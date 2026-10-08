# blygd — design

blygd is a Blygger studio as one Go binary over Postgres: publisher,
reader, hoppers and Webmentions together. It is headless — every client
(the SwiftUI app, ygg.el, an agent) talks to it over the write API — and
multi-tenant, with each user owning one origin.

Spec references (§) are to `blygger-spec` `docs/protocol-v0.3.md`, read at
commit `c2da39f` (2026-10-07).

## Three planes

A published feed is not a subset of a hopper. The two overlap only when
one of your own items happens to be in one of your hoppers. blygd keeps
three planes apart, and the separation is structural:

| Plane | What | Tables | Authority |
|---|---|---|---|
| 1. Own origins | Items this instance publishes | `origins`, `items`, `item_versions`, `media`, `surface_files` | Authoritative |
| 2. Import mirror | Other origins' items, one copy per origin, shared by every subscriber | `remote_origins`, `imported_items`, `imported_pins` | Reconciled copy (§13) |
| 3. Private overlay | Per-user: subscriptions, hoppers, reads | `subscriptions`, `hoppers`, `hopper_members`, `reads` | Private, never on the wire |

- **The renderer reads only plane 1.** Imported items can never be
  re-emitted (§13.5) because nothing that writes `surface_files` queries
  plane 2.
- **Hopper members are references**, exactly one of `own_item_id` or
  `imported_item_id`. A copy would outlive a withdrawal, which §13.4
  forbids. Own members may be drafts.
- **The mirror is shared** so N users following one blyg cost that blyg
  one poll (§12.3), not N.

## Render on publish

Every change to plane 1 re-renders the affected surface files **inside the
same transaction**: the item document, its pins, its permalink page, and
the aggregates (index, feed, manifest, feed page). The HTTP layer serves
stored bytes and never renders. Consequences:

- What readers fetch is exactly what was committed, and `content_hash`
  covers exactly what is served.
- ETags are content hashes, so conditional GETs (§12.3) are free.
- `SELECT path, body FROM surface_files WHERE origin_id = …` is a
  conformant static blyg (§4). Static export is a dump, not a renderer.
- Publishes on one origin serialize on `SELECT … FOR UPDATE` of the origin
  row, because the aggregates are rebuilt from the whole origin.

Rendering every aggregate on every publish is O(items). That is fine to
thousands of items; past that, the index is the one to make incremental.

## Invariants and where they are enforced

The protocol makes several things permanent. Each is enforced in the
schema (triggers in `schema.sql`), not only in Go, and each has a test.

| Invariant | Spec | Enforced by |
|---|---|---|
| Published versions are immutable and never deleted | §5.2 | trigger `item_versions_guard` |
| Versions advance by exactly 1 | §5.2 | trigger `items_guard` |
| Pins are irrevocable | §8 rule 1 | trigger `item_versions_guard` |
| Endcaps are never pinned, and say nothing | §8 rule 2, §9 | CHECK constraints |
| Published items are never deleted, only withdrawn | §9 | trigger `items_guard` |
| `forked_from` is immutable | §5.6 rule 3 | trigger `items_guard` |
| `page` is fixed at first publish | §5.8 (SHOULD; we make it MUST) | trigger `items_guard` |
| Item documents and pins stay 200 forever | §4, §8 | `surface_files.permanent` + trigger |
| Pin bytes never change | §8 | `surface_files.immutable` + insert-or-ignore |
| Drafts and unpinned versions 404 | §4, §8.4 | nothing renders them |
| Imported items are never re-emitted | §13.5 | renderer reads plane 1 only |

## Origins and routing

- An origin is a **full base URL** (`origins.base_url`), normalized. Today
  every origin is path-mounted under `BLYGD_PUBLIC_URL`:
  `https://host/{handle}/`. Custom domains later need no schema change.
- Request routing finds the origin whose `base_url` is the longest prefix
  of the request URL, then serves `surface_files` at the remainder. The one
  dynamic path is `{origin}webmention` (§15.1).
- The API is **host-rooted** at `/api/`, never inside an origin: a studio
  and its API are client internals, not protocol (§15.1, §16.6). Handles
  that would collide with host-rooted routes are reserved.

## Webmentions (§15)

**Receiving.** `POST {origin}webmention` runs §15.3:

1. Syntactic checks return 400. The target must be inside this origin and
   name a published item, by page or by document URL. A withdrawn target
   is fine.
2. Three rate caps return 429 and the claim is never queued: per source
   host (60/h), per registrable domain (240/h), global (2000/h). The caps
   are **instance-wide**, because verification fetches cost the machine,
   not the tenant.
3. Otherwise 202, and the claim is upserted as `pending`. A re-sent pair
   re-verifies rather than duplicating.

**Verifying.** A worker runs §15.4 with at most two bounded fetches. The
item document must have been fetched from exactly
`{origin}items/{id}.json` for its own origin and id. That one rule rejects
mirrors, pinned files, and path-mounted neighbours speaking for each other.
The relation check is `stub`, then `transclusion`, then `fork`; a fork
counts only if it names a version pinned here.

- No content is stored; a verified mention is a pointer.
- A mention that verified before and fails now becomes `gone`, and the
  record is kept.
- A network failure is retried (1 m, 10 m, 1 h) before it counts as a
  failure.

**Sending.** Publish writes outbox rows in the same transaction, one per
(source item, reference target), carrying the target version. A republish
re-queues only new or changed references. Withdrawing a stub re-queues it
once. A worker then:

- discovers the endpoint: manifest `webmention` key first, then W3C
  discovery (Link header via HEAD, then GET, then the first `<link>` or
  `<a>`);
- POSTs without following redirects;
- applies §15.2's schedule: 2xx is sent, 4xx is failed, 5xx/429/network
  are retried at 15 m, 1 h, 4 h, 12 h, 24 h and then abandoned.

**Workers** use a lease, not a job table. A claim is one
`UPDATE … WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED)` that pushes
`next_attempt_at` forward and bumps `attempts`. The network work then runs
outside any transaction, and a crashed worker's rows come due again on
their own.

## Outbound fetching

Every outbound request goes through `internal/fetch`. Every URL it touches
is chosen by someone else, so it is bounded on every axis:

- redirects, time, and body size are capped (§15.4's 3 / 5 s / 1 MB for
  verification);
- only http(s) is fetched;
- non-public addresses are refused **at connect time on the resolved IP**,
  so DNS rebinding cannot slip past;
- environment proxies are ignored, so the address check is checking the
  target and not the proxy.

`BLYGD_ALLOW_PRIVATE_FETCH=true` lifts the address check. Use it for
development only.

## Write API and scopes

Bearer tokens are stored as SHA-256 hashes, shown once at mint time,
revocable, and scoped. Consequential public acts have their own scopes, so
a drafting tool or an agent can be given `items:write` without the power
to publish, withdraw, or pin (pinning is irrevocable). A token can only
mint scopes it holds. `blygd user add` mints the owner's `*` token; that
is the paste-a-token path §16.6 requires beside any future OAuth-style
flow.

## HTML safety

- **Our own content:** the markdown renderer escapes raw HTML (the
  reference client's `html: false`), neuters `javascript:` URLs, and
  absolutizes every link (§5.2, decision #53). That is why own
  `content_html` is trusted in our pages.
- **Imported content (Phase 2):** stored verbatim and never trusted. §14
  requires readers to sanitize at render, so the API will hand clients
  verbatim HTML plus the obligation to sanitize. A server-rendered reading
  page, if one is ever added, sanitizes there.

## Deliberate departures from the reference client

- `page` is immutable; the reference lets a never-public slug move. Ours
  are `f/{id}/` and `t/{id}/`, which never need to.
- A publish with no content change is refused (409). It would cost every
  reader a new feed entry and guid for nothing.
- Content containing `[[id]]` or `![[id]]` is refused until Phase 3,
  because §10.1 makes an unresolvable link a publish error, and nothing
  resolves yet.
- Feed-title excerpts treat inline tags as zero-width. The reference turns
  every tag into a space, giving "**x**." → "x .".
- The manifest claims `level: 2`. That claim is informative (§3.2), but
  honest only once threads land (Phase 3); see PLAN.md.
