package studio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/chrisbodhi/blygd/internal/blyg"
	"github.com/chrisbodhi/blygd/internal/fetch"
)

// checkForkPin applies §5.6 rule 2 before a fork's first publish: the
// referenced version must be pinned at its origin. Refuse on evidence (a 4xx,
// or a 200 that is not that pinned version); proceed with a warning when the
// network says nothing (failure, timeout, 5xx), so a stranger's outage is
// never a veto. Returns the target's kind when it learns it.
func (s *Studio) checkForkPin(ctx context.Context, originID int64, raw json.RawMessage) ([]string, string, error) {
	var ref blyg.Reference
	if err := json.Unmarshal(raw, &ref); err != nil {
		return nil, "", fmt.Errorf("stored forked_from is malformed: %w", err)
	}
	own, err := s.GetOrigin(ctx, originID)
	if err != nil {
		return nil, "", err
	}
	if blyg.SameOrigin(ref.Origin, own.BaseURL) {
		var kind string
		err := s.Pool.QueryRow(ctx, `SELECT i.authored_kind FROM item_versions v JOIN items i ON i.id = v.item_id
			WHERE i.origin_id = $1 AND v.item_id = $2 AND v.version = $3 AND v.pinned_at IS NOT NULL`,
			originID, ref.ID, ref.Version).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", invalid("forked_from names v%d of %s, which is not pinned here (§5.6 rule 2)", ref.Version, ref.ID)
		}
		return nil, kind, err
	}
	if s.Fetch == nil {
		return []string{"lineage not checked: no fetch client configured"}, "", nil
	}
	pinURL := blyg.PinURL(ref.Origin, ref.ID, ref.Version)
	resp, err := s.Fetch.Get(ctx, pinURL, fetch.VerifyLimits, "application/json")
	if err != nil {
		return []string{fmt.Sprintf("could not confirm %s is pinned (%v); published anyway", pinURL, err)}, "", nil
	}
	switch {
	case resp.Status >= 500:
		return []string{fmt.Sprintf("could not confirm %s is pinned (HTTP %d); published anyway", pinURL, resp.Status)}, "", nil
	case resp.Status >= 400:
		return nil, "", invalid("forked_from is not pinned at its origin: %s returned %d (§5.6 rule 2)", pinURL, resp.Status)
	case resp.Status != 200:
		return nil, "", invalid("forked_from pin check: %s returned %d", pinURL, resp.Status)
	}
	var doc blyg.PinDoc
	if err := json.Unmarshal(resp.Body, &doc); err != nil || doc.ID != ref.ID || doc.Version != ref.Version {
		return nil, "", invalid("forked_from: %s is not v%d of %s (§5.6 rule 2)", pinURL, ref.Version, ref.ID)
	}
	return nil, doc.Kind, nil
}

// outRef is one remote reference a version makes (§15.2).
type outRef struct {
	key           string
	relation      string // stub, transclusion, fork, url
	targetURL     string
	targetOrigin  string
	targetID      string
	targetVersion int
}

// remoteRefs lists the remote references of a newly published version:
// cross-origin stub_of, transclusions[] entries carrying an origin, and a
// cross-origin forked_from; plus a {url} stub. Same-origin references never
// generate mentions.
func remoteRefs(own string, it *Item, v *Version, forkKind string) []outRef {
	var out []outRef
	add := func(rel string, ref blyg.Reference, kind string) {
		if ref.Origin == "" || blyg.SameOrigin(ref.Origin, own) {
			return
		}
		origin, err := blyg.NormalizeOrigin(ref.Origin)
		if err != nil || !blyg.ValidID(ref.ID) {
			return
		}
		// §15.2: the target's page if the sender holds it, else the
		// reference convention. The publish transaction never reads the
		// import mirror, so this writes the convention and the send worker
		// swaps in the imported page when there is one.
		out = append(out, outRef{
			key:           "blyg " + origin + " " + ref.ID,
			relation:      rel,
			targetURL:     origin + blyg.DefaultPage(kind, ref.ID),
			targetOrigin:  origin,
			targetID:      ref.ID,
			targetVersion: ref.Version,
		})
	}
	if it.ForkedFrom != nil {
		var ref blyg.Reference
		if json.Unmarshal(it.ForkedFrom, &ref) == nil {
			add("fork", ref, forkKind)
		}
	}
	if v.StubOf != nil {
		var stub struct {
			blyg.Reference
			URL string `json:"url"`
		}
		if json.Unmarshal(v.StubOf, &stub) == nil {
			if stub.URL != "" {
				if u, err := blyg.NormalizeURL(stub.URL); err == nil {
					out = append(out, outRef{key: "url " + u, relation: "url", targetURL: u})
				}
			} else {
				add("stub", stub.Reference, "")
			}
		}
	}
	if v.Transclusions != nil {
		var refs []blyg.Reference
		if json.Unmarshal(v.Transclusions, &refs) == nil {
			for _, r := range refs {
				add("transclusion", r, "")
			}
		}
	}
	return out
}

// enqueueOutbound writes the outbox rows for a new version, inside the
// publish transaction. A reference whose target version is unchanged since
// the last send is left alone; new or changed ones go (back) to pending.
func enqueueOutbound(ctx context.Context, tx pgx.Tx, o *Origin, it *Item, v *Version, forkKind string) error {
	if it.Page == nil {
		return errors.New("enqueueOutbound: item has no page")
	}
	source := o.BaseURL + *it.Page
	seen := map[string]bool{}
	for _, r := range remoteRefs(o.BaseURL, it, v, forkKind) {
		// One row per target. A stub usually quotes its target too; the
		// first relation wins (fork, then stub, then transclusion), so the
		// row says stub. Verification checks every relation anyway (§15.4).
		if seen[r.key] {
			continue
		}
		seen[r.key] = true
		var targetVersion any
		if r.targetVersion > 0 {
			targetVersion = r.targetVersion
		}
		_, err := tx.Exec(ctx, `INSERT INTO outbound_mentions
			(origin_id, source_item_id, ref_key, relation, source_url, target_url, target_origin, target_id, target_version, status, next_attempt_at)
			VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), $9, 'pending', now())
			ON CONFLICT (source_item_id, ref_key) DO UPDATE SET
				status = 'pending', attempts = 0, next_attempt_at = now(), updated_at = now(),
				relation = EXCLUDED.relation, target_url = EXCLUDED.target_url, target_version = EXCLUDED.target_version
			WHERE outbound_mentions.target_version IS DISTINCT FROM EXCLUDED.target_version
			   OR outbound_mentions.relation <> EXCLUDED.relation`,
			o.ID, it.ID, r.key, r.relation, source, r.targetURL, r.targetOrigin, r.targetID, targetVersion)
		if err != nil {
			return fmt.Errorf("enqueue mention: %w", err)
		}
	}
	return nil
}
