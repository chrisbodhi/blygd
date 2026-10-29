# blygd — plan

## Phase 1 — publish side, mentions, hoppers (done)

- Fragments: draft, publish, edit-publish, withdraw, return, pin.
- Rendered surface: item documents, pins, index, feed, manifest, permalink
  and feed pages, with ETags and CORS.
- Webmention receive (§15.3), structural verification (§15.4), and
  sending through an outbox (§15.2). Forks exercise the outbound path end
  to end.
- `forked_from` with the §5.6 rule 2 pin check: refuse on evidence,
  proceed with a warning on silence.
- Scoped tokens; hopper CRUD over references to own items.
- Tests: unit tests for protocol, fetch and mention code, plus end-to-end
  tests on a real Postgres. Two deliberate rule breakages were caught.

## Phase 2 — the reader (done)

The import mirror and the overlay that reads it. This is what makes blygd
a frontend-agnostic reader for the Apple client and ygg.el. DESIGN.md
("The reader") describes what was built.

Left for later, all optional in the spec:

- The user-confirmed reset to origin state after a regression (§13.3 MAY).
- Running re-resolution on its own when surfaces 404 persistently (§12.2
  MAY). Today the poll error says to re-resolve, and the user does.
- The HTTP `Link` header form of `rel="blyg"` (§12.1 step 4, OPTIONAL).
- Garbage-collecting mirror rows nobody subscribes to any more.
- Upgrading an L0 subscription when its feed later gains
  `<blyg:manifest>`.

1. **Resolution** (§12.1), with the six-fetch bound. Subscription identity
   is the final fetch origin (§12.2), and identity changes require user
   confirmation.
2. **Poller** over `remote_origins`, using the same lease pattern as the
   mention workers:
   - conditional GETs, exponential backoff, `Retry-After`, never tightening
     on failure (§12.3);
   - the feed is a cheap trigger; the archive index is the reconciliation
     surface (§13.2); only fetched item documents advance state.
3. **Watermark and discrepancies** (§13.3): refuse silent regression,
   surface rewrites and stealth edits, verify `content_hash`.
4. **Withdrawal** (§13.4): roll to null, delete stored content, retain pin
   files only.
5. **L0 wrapper** (§13.6): synthetic ids, never a transclusion source.
6. **Reading API:** a merged reverse-chron feed ordered by
   `min(claimed updated, first seen)` (§13.7), reads, and imported hopper
   members. Return verbatim HTML and say clients must sanitize (§14).
7. **Blogroll** (§11): opt-in per subscription, rendered as
   `blogroll.opml`, with the manifest key only when non-empty.
8. **Same-instance subscriptions** go through the importer like any other
   origin, reading the target's `surface_files` in-process. Never join
   across tenants' plane-1 tables: that would bypass §13.3–13.4 for local
   origins only.
9. **Outbound targets:** use the imported item's `page` (§15.2) instead of
   the reference convention.

**Answered question that shapes this phase:** do clients need offline
snapshots? no, they do not, so thin version pointers.

## Phase 3 — threads (done)

DESIGN.md ("Threads") describes what was built. Thread composition is
pure (`blyg.Compose`). Imported snapshots reach a publish only through
`studio.Imports`, read before the transaction and scoped to the
publisher's subscriptions. Pages sanitize, because bakes carry other
origins' bytes.

- Directive grammar and resolution (§10.1–10.2): local first, then
  imported from a subscribed origin; ambiguity is a publish error.
- Baking `blyg-transclusion` / `blyg-partial`, including the text-content
  substring check for partial quotes.
- The local closure check; `transclusions[]` provenance with `selector`
  and frozen `cited`.
- `[[id]]` plain links, rendered as absolute anchors and silent on the
  wire.
- Stubs: `stub_of` on versions, both shapes, `cited` with `retrieved`, and
  the version-agreement rule (§10.6 rule 3).
- `generated[]` and `blyg-tk-gen` wrapping (§5.7), through the reference
  client's `[TK]` grammar; generated changelog notes (§5.2).
- Fork flattening (§5.6 rule 6) when a fork is created without content,
  and lineage on the fork's page (rule 7).
- The publish guard against `[[` is gone, and the manifest's `level: 2`
  is now honest.

Left for later:

- A draft preview endpoint that renders unresolvable directives in place
  instead of refusing (the reference client has one). Today the publish
  `422` lists every problem.
- A frozen `cited` on `forked_from` (§5.9 allows it; nothing sets it).
- A `{url}` stub sends a plain W3C Webmention through the existing outbox
  (`relation = 'url'`). Nothing yet stubs an L0 entry by id: clients pass
  its link as `{url}`.

## Phase 4 — surface and access

- **Media** upload: immutable, `media/{key}.{ext}`, retained forever when
  a pin references it (§5.4, §8 rule 4). *(Done, in Postgres with a 10 MiB
  images-only cap. Kept forever once any published version references it,
  since any version may be pinned later. Static export must include
  published media.)*
- **Pinned version pages** `{page}v{n}/` (§8.4), plus pin citations on the
  live page. *(Done: sanitized rather than verbatim, see DESIGN.md
  departures; own forks' lineage links them.)*
- **Public hoppers** at `/h/{slug}/` (§13.5): freeze the slug on first
  publicity, attribution and origin links, no protocol surface. *(Done:
  served live, excerpts only. Not yet linked from the feed page, which is
  stored surface and must not read the overlay.)*
- **Per-item display** of verified mentions (§15.5), opt-in. *(Done:
  per-mention approval, no swap after approval, host and domain blocks.
  See DESIGN.md "Responses and harassment".)*
- **Static export:** tar `surface_files`, omitting the manifest
  `webmention` key (§15.7). *(Done: `blygd export` and
  `GET /api/origin/export`, with published media and no
  `rel="webmention"` either. Valid only at the origin's `base_url`.)*
- **Custom domains** with automatic TLS (certmagic). `base_url` already
  carries the full URL. The threat is impersonation: a user setting
  `base_url` to an address they do not control (`https://blyg.google.com/`).
  On the open web the protocol already defeats it, because identity is
  where a document was fetched from (§12.2; §15.4 rule 2): readers and
  mention receivers fetch from the real host, and ACME will not issue for
  it. That holds only if blygd keeps up its side:
  - **Verify before use.** A custom `base_url` takes effect only once
    control is proven: a DNS TXT record for a host, or a token served at
    the new base URL's path, which also covers a path on a host the user
    owns (`https://newschematic.org/blyg/`). Until then the origin keeps
    its handle URL.
  - **Per path, not per host.** The identity is the full base URL (§4,
    §12.2), and two blygs on one host are ordinary (§13.5): verifying
    `newschematic.org/blyg/` grants nothing at `newschematic.org/other/`.
  - **Recheck.** Domains lapse and DNS moves. Recheck control on a
    schedule, and stop serving the origin under a host it no longer
    controls.
  - **Route by host only.** A custom origin is served only when the
    request's host and path match its `base_url`: no `/{handle}/`
    fallback, and no other path to its pages.
  - **Close the local shortcut.** Without this, another tenant
    subscribing to `https://blyg.google.com/` would read the impostor's
    items, and so would quotes and snapshots.
    *(Done by construction, 2026-10-29. `reader.LocalFirst` is gone.
    `fetch.Client.Local` answers in-process only for `BLYGD_PUBLIC_URL`'s
    own scheme and host, so a custom-domain origin is fetched over the
    network. If custom domains should also be served in-process once
    verified, that is a deliberate widening, with a test that an
    unverified `base_url` is not.)*
  - An export is just files, and anyone can hand-write a manifest
    claiming any site. It speaks for an origin only where DNS serves it,
    which is why the checks above live in serving and reading, not in
    rendering.
- **Studio discovery** via an HTML `rel` link on a studio page;
  OAuth-style token approval beside paste-a-token (§16.6); an MCP
  transport over the same operations and scopes. *(Discovery done:
  `rel="service-desc"` to the OpenAPI document. Approval and MCP open;
  the reference's shape is in its `docs/client-access.md`.)*

## Phase 5 — protocol 0.4

Move from `protocol-v0.3.md` at `d008e21` (0.3's last revision) to
`protocol-v0.4.md` at `24b9770` (its sixth revision, 2026-10-20). Every 0.4
construct is additive, so blygd today is a 0.4 publisher that emits none
of them. The gaps are in reading, in receiving mentions, and in three 0.3
rules that 0.4 states precisely. Steps are in order: each is shippable on
its own, and the version string moves last.

1. **Rules 0.4 makes precise**, all from decision #76, and none of them a
   wire change:
   - A partial quote's blockquote has CommonMark's extent, lazy
     continuation included (§10.1). `attachedQuote` stops at the first
     line without `>`.
   - A line ends at LF or CRLF (§10.1). The grammar splits on `\n` alone,
     so a CRLF directive line does not match.
   - The local closure check walks the versions actually baked, from each
     `transclusions[]` entry to that version's own (§10.2). Today
     `closureContains` reads each item's current version.
   - Check, and add tests for: a local id (draft or published) never
     falls through to an import; `transclusions[]` has one entry per
     directive; `stub_of.version` never moves backwards (§10.6 rule 3); a
     document at or below the withdrawal endcap's version is a replay
     (§13.4).

   *(Done, 2026-10-28. Two of the "check" items were not yet true: a stub
   that stopped quoting fell back to its creation version, and content at a
   withdrawal endcap's version was adopted as a stealth edit. A published
   local id that is also held as an import is now a publish error for
   every use, not only directives. `make parity` against blygger-studio
   0.48.2 (`b93ac12`): 56 cases, no unexpected differences. The reference
   still ends a partial quote at the first line without `>`, which parity
   caught as a fork dropping the author's prose; fork flattening now reads
   the quote's extent from the bake, so either reading flattens whole.)*
2. **Receiving mentions:**
   - The target check is membership, not location (§15.3 step 1, #84): a
     target may also be a pinned version's document or page. Revisit the
     DESIGN.md departure that pinned pages advertise no endpoint.
   - Verification under templates (§15.4 step 2, #61): when the final URL
     is not the default, fetch `{origin}blyg.json`, expand its `item`
     template and accept an exact match. The bound rises to 3 fetches.
   - Relations `source`, then `link`, after `fork` (§15.4 step 4).
   - Reposts and notices (§15.4, §15.5, #80): a `link` mention, or a stub
     or transclusion whose source has no words of its own, is a notice. A
     notice never appears in a responses list. The receiver classifies; no
     sender marks it.
   - A migration widening the `relation` CHECKs in `001_initial.sql`
     (inbound and outbound), plus a column for the class. Both API changes
     edit `openapi.json`.

   *(Done, 2026-10-28: migration 003. A notice is refused approval and the
   schema refuses one shown. Approval now also survives only an unchanged
   class. Mentions verified before 0.4 are recorded as responses until
   they re-verify. The repost test reads a partial selection's extent from
   the bake, as fork flattening does. `blyg.Surface` reads a manifest's
   surface keys, and step 3 reuses it.)*
3. **Reading a relocated surface** (§4, §6.1, §12, #51 and #64):
   - The manifest's `feed` and `items` are authoritative, and its `item`
     and `pin` templates (RFC 6570 level 1) replace
     `{origin}items/{id}.json` and `{origin}items/{id}/v{n}.json` for
     remote origins. The poller hard-codes all four today, and
     `refreshManifest` assumes `identity + "blyg.json"`.
   - §12.1 step 4: a `rel="blyg"` href whose body parses as a manifest is
     the manifest. The identity is its final URL minus its last path
     segment (§12.2).
   - `page` may be absolute (§5.8). Resolve it; never prefix it.
   - The fake blyg in `internal/testblyg` gains a templated mode, with
     query-string templates, so tests have something to subscribe to.
   - Our own surface does not change. We keep the default paths and emit
     no template keys, as §6.1 recommends.

   *(Done, 2026-10-28: migration 004 stores where each manifest was found,
   and polls re-read it there. The poller, importer, retained-pin URLs and
   blogroll `xmlUrl` all take URLs from the stored manifest, and a
   reconciliation uses the surface its refreshed manifest names. A manifest
   at a query-string URL is refused (#64). An absolute `page` already
   resolved correctly; it now has a test. A dead `rel="blyg"` link now costs
   two fetches, which can spend the bound of 6 before step 6. Plane 1 still
   builds remote pin and document URLs from the defaults (the fork pin
   check, fork copying, the attribution link's fallback, a fork's
   rendered link). Each needs the target's manifest, fetched outside the
   mirror, so they move to step 4.)*
4. **Sending mentions:** with no `page` held, the fallback target is the
   target's item document, expanded from its `item` template, and never
   the `f/{id}/` / `t/{id}/` convention (§15.2, #85). The outbox writes the
   convention today and the send worker swaps in the page; the swap moves
   to the item document, from the manifest the worker already reads.
   - Plane 1's other remote URLs follow the target's templates too: the
     fork pin check and fork copy (`outbox.go`, `threads.go`) fetch
     `pin`, and the attribution link's fallback (`quoteLink`) and a fork's
     rendered link name `item` and `pin`. Each fetches
     `{origin}blyg.json` outside the transaction, as the verifier does,
     and falls back to the defaults when it has no template.

   *(Done, 2026-10-28. `internal/surface.Locate` is the one manifest
   read, used by the verifier, the sender, the fork pin check, fork
   copying and `quoteLink`, which caches per origin for one call.
   - **Correction to the plan, from the review:** a fork's rendered link
     cannot fetch, because rendering runs inside transactions and
     `Rebuild` works offline. The pin check's confirmed URL is kept in
     `items.forked_from_url` (migration 005), written once with no trigger.
   - **The sender:** it retries a manifest it cannot read yet, rather than
     guessing the default path. It still names a held page only when the
     page is under the target's origin. An absolute page elsewhere (§5.8)
     falls back to the item document, which every 0.4 receiver accepts
     (#84), so a stranger's page claim can never aim our mention at a
     third party.
   - **The snapshot page fallback:** with no `page` held, it is the item
     document, never `f/{id}/`.
   - **What changed for existing tests:** `TestForkMentionRoundTrip`'s
     hand re-send now names the item document. A sender holding no page
     targets that address now.
   - **Verification:** parity, 56 cases with no unexpected differences;
     interop, 0 failed.)*
5. **Emitting the new constructs:**
   - Disclosed links (§10.3a, #79, which reverses #32): each resolved
     `[[id]]` adds a `links[]` entry, and a remote one sends a `link`
     mention. A markdown link stays silent. Item documents gain the
     member; endcaps omit it.
   - Labels (§10.1, #70 and #81): `[[id | text]]` and `![[id | text]]`.
     On a link the label is the anchor text, ahead of the DESIGN.md
     labelling departure. On a directive it is baked as `data-blyg-label`,
     and fork flattening uses it in the attribution line. A label that
     would contain `]]` is rewritten.
   - Remote generation sources (§5.7 rules 1, 8 and 9): a `[TK]` source
     resolves like a directive, so it may be imported. `GenSource` gains
     `origin` and `cited`, and a remote source sends a `source` mention.
     It goes through `studio.Imports` like a quote: the door stays as
     narrow as it is.

   *(Done, 2026-10-28: migration 006 adds `item_versions.links` and
   extends the version guard to it, which tightens the trigger.
   - **Labels:** one scanner reads both forms. A link's label is its
     anchor text, and a directive's is baked as `data-blyg-label` and used
     in a fork's attribution.
   - **`links[]`:** one entry per link, in document order, read from the
     wire text, because generated blocks render out of order.
   - **Remote sources:** they resolve like directives and freeze `cited`.
     The outbox sends `source` and `link` mentions. When a version both
     quotes and draws on one target, it sends one mention, as the quote
     (§15.2). A link carries no target version, so keeping it across a
     republish re-sends nothing.
   - **What else changed:** `openapi.json` gains `Link` and `links`, and
     `GenSource` gains `origin` and `cited`.
   - **The reference:** blygger-studio 0.48.2 parses labels but emits
     neither `links[]` nor `data-blyg-label`, so parity has no labelled
     cases yet.)*
6. **Bump** `ProtocolVersion` to `"0.4"`, move DESIGN.md's spec citation,
   and rerun `make parity` and `make interop` against a reference that
   speaks 0.4.

   *(Done, 2026-10-28. Manifests, item documents and pins now say
   `"blyg": "0.4"`. CLAUDE.md, DESIGN.md and `openapi.json` (now API
   version 0.4.0) cite `protocol-v0.4.md`. The fake test blyg still serves
   `"0.3"`, which covers reading an older publisher.
   - **Against blygger-studio 0.48.2 (`b93ac12`):** parity, 56 cases with
     no unexpected differences; interop, all five scenarios, 0 failed.
   - **Interop for 0.4 (2026-10-28):** two new scenarios, 0 failed.
     Scenario 6 is a relocated surface: a static third origin that
     `check.py` serves, reached through `rel="blyg"`, with every file
     templated or renamed and no page. Scenario 7 covers labels,
     `links[]` and a remote generation source. The reference verifies
     blygd's `source` mention. It is behind on three things, which the
     run reports as gaps rather than failures: it doesn't verify `link`
     mentions (it answers 202 and keeps nothing), it emits no `links[]`,
     and it bakes no `data-blyg-label`.
   - **Found by the new scenarios:** an origin whose manifest is only
     reachable through `rel="blyg"`, with nothing at `{origin}blyg.json`
     (§12.2's `/wp-json/…` case), can be read by blygd but not forked or
     notified. `surface.Locate` knows only the identity, so the pin fetch
     and the sender fall back to the default layout. The reader records
     the manifest's URL in `remote_origins.manifest_url`, but plane 1
     can't read it without widening `studio.Imports`. The reference uses
     its subscription's stored surface. The spec also leans on the fixed
     name: §4 calls `blyg.json` at the origin protocol-fixed, and §15.3
     verification fetches `{origin}blyg.json`. **Decided (2026-10-28):**
     outside the reader, blygd requires `{origin}blyg.json`. A fork of
     such an origin is refused with a message naming the missing file, and
     DESIGN.md lists this as a departure. The inconsistency is raised
     upstream as blygger-spec#21. Sol serves the manifest at both URLs, and
     `check.py` adds a second origin with only the rel-named manifest:
     the reference's fork of it is reported as a gap check, and blygd's
     refusal is a check.)*

**Open question:** §10.2 step 2 (#82) resolves against any snapshot the
publisher holds, not only a subscribed origin's. `reader.Snapshots` joins
through `subscriptions`, which is also what keeps one tenant from quoting
another's imports. Responding from anywhere needs a per-user one-off
snapshot with no subscription behind it, and a decision on how that fits
the plane-separation door before any code.

Left for later, all optional in the spec:

- Queueing maintenance republishes low, by hash or by a small edit
  (§13.2, #77 and #83), and folding items published together (§13.7,
  #78).
- Re-sending a version's mentions when it is pinned (§15.2, #73).
- §16.9 `pinned_at` and `cid`, and §16.10 image sizes: ruled shapes, not
  yet normative.

Not in the spec at all:

- **Running `[TK]` generation.** `[TK]` is the reference client's
  authoring grammar, not protocol: the spec defines only the wire result,
  `generated[]` and the `blyg-tk-gen` spans (§5.7). blygd parses the
  grammar, strips scopes to their output and discloses each one, but runs
  no model. A `[TK]instruction[=][/TK]` scope publishes only once a client
  has written the output in. Generating it server-side would need a model
  provider, per-user keys and a review step before publish, so it
  starts with a design note.

## Hardening (from the architecture review, 2026-10-28)

The review is `reports/architecture-review-entire-project-2026-10-28-171955.md`.
Its two critical findings come before Phase 5 step 4, and neither touches
step 4's files. The rest wait until after the 0.4 bump unless step 4 needs
them.

1. **A withdrawn approval re-rendered outside its transaction** (C1). The
   mention worker committed `approved_at = NULL` and then re-rendered the
   page in a second transaction. A failed render left the page showing a
   response the database no longer approved, and nothing retried it.
   *(Done, 2026-10-28. `Workers.Display` replaces `OnDisplayChange`, and
   `Studio.ChangeDisplay` runs the worker's update under the origin's lock
   and renders before the commit. That is the same lock order as
   `SetMentionApproval`, origin then mention. A failed render now rolls
   both back, and the claim comes due again when its lease lapses. Every
   verification now takes the origin lock briefly. Taking it only when an
   approval might drop would race an approval made between the read and
   the update.)*
2. **One origin can stall the poller** (C2). Polls run one at a time with
   no deadline per poll. 200 fetches at 20 s is about 67 minutes, past
   the 15-minute lease, so `PollFor` can start a second poll of the same
   origin.
   *(Done, 2026-10-28. `Reader.PollDeadline`, 5 minutes by default and
   capped at the lease, bounds one poll. The poll stops at the next fetch
   by the same path as a 429, keeps what it applied, saves no validators
   and counts as a failure for backoff. `PollBatch` polls its leased
   origins concurrently, at most `Batch` (5) at once. A user's `POST
   …/poll` still waits for its poll, now at most the deadline. The fake
   blyg gained `SetDelay`.)*
3. **The house rules were conventions** (D3, P3). The planes tests were
   regexes over each package's raw source, `internal/mention` had none,
   and nothing checked the import graph, outbound HTTP or where
   transactions start.
   *(Done, 2026-10-29. `internal/archtest` does three things:
   - It folds string constants into the SQL they build, using `go/ast`,
     and fails on a table named through a variable. All four planes tests
     use it. The new one in `mention` allows mirror reads in
     `importedPage` only, and bans writes to plane 1.
   - It allow-lists each package's internal imports, and states
     outright that `studio` can't reach a package that reads the mirror.
   - It bans HTTP clients outside `fetch`, and transactions outside
     `tx.Run`.

   `tx.Run` marks its context and `fetch` refuses a marked one, so the
   no-network rule now holds at runtime too. All 19 transaction sites
   moved to it, and `ChangeDisplay`'s callback now receives the marked
   context. For P3, `blyg.ItemPath`, `blyg.PinPath` and `ManifestFile`
   name what the renderer writes. Tests check that the reader's default
   `Surface` and the receiver's path patterns agree with them. Every
   rule was broken on purpose and its test failed.)*
4. **Same-instance fetches dialed the public address** (P2). Only the
   reader went local. The studio's pin checks and copies and the mention
   workers' sends and verifications went out over the network to this
   same instance, and behind a private reverse proxy the address guard
   refused them. A fork's pin check then counted as "could not confirm"
   and published anyway.
   *(Done, 2026-10-29. `fetch.Client` gains `Local` and `LocalBase`. A
   request for the instance's own scheme and host goes to the
   origin-surface handler in-process, through the client's usual
   redirects and limits, outside the public request budgets. The receiver's
   own mention limits still apply. `reader.LocalFirst` is removed. The
   two same-instance e2e tests run with private addresses refused, so
   they pass only in-process; turning the path off fails both.)*
5. **Four error-to-status conventions** (P1). `studio.Error` had a code
   table, `reader` and `hopper` had sentinels with a wrapper to remember,
   `TooSoon` and `ResolveError` had their own handlers, and the
   bad-cursor check compared error text. Three handlers called anything
   that wasn't a database error a 400, so a cancelled request or a lost
   pool from `hopper.Create`, `Rename` or `Accounts.Mint` was blamed on
   the caller.
   *(Done, 2026-10-29. A new `internal/apperr` has four kinds: bad
   request, not found, invalid and conflict. `studio.Error` reports one,
   and the sentinels and validation errors in `reader`, `hopper` and
   `account` are kinded. `handleErr` maps kinds, `ResolveError` and
   `TooSoon` in one place, and handlers return errors as they come.
   Anything unkinded is a 500. Statuses and codes are unchanged for every
   kinded error. `TestHandleErr` covers each mapping, and
   `TestServerFailuresAreNotTheCallersFault` covers the old bug.)*
6. **Leases shorter than the work they cover** (D5). A verify batch held
   10 claims for 2 minutes and could need 150 s. A send batch fit in its
   10 minutes only by arithmetic nobody checked. A second process would
   have claimed rows twice and spent their retries early.
   *(Done, 2026-10-29. `batch.Each` works leased claims in order and
   starts each one only if its worst case still ends before the lease
   lapses. Each claim runs under that worst case as its deadline. The
   worst cases come from the fetch limits: a verification is 3 fetches at
   5 s plus 15 s, a send is 5 fetches at 10 s plus 30 s. Claims it doesn't
   start, and those after an error, go back due at once with their
   attempt refunded, even during shutdown. Lease lengths are now
   parameters rather than SQL literals. The poller was already safe: its
   polls run side by side, each capped by `PollDeadline`. One unified
   backoff type is left undone, since the three schedules answer
   different specs. `TestEachKeepsWithinTheLease` and
   `TestVerifyBatchStaysWithinItsLease` cover the change.)*
7. **Two composition roots** (P4). `newEnv` re-wired by hand what `serve`
   wired, so a dependency added in one place left the e2e suite testing
   a different graph.
   *(Done, 2026-10-29. `web.New(cfg, pool, log)` builds the whole
   instance, including its fetch client and the in-process local route.
   `serve`, the other commands and `newEnv` all call it. Tests adjust it
   with a `config.Config`, plus the few knobs the config doesn't hold
   (mention limits, reconcile interval). `sameInstanceOnly` is now one
   field on the shared client. `cmd/blygd` may import only `account`,
   `config`, `db` and `web`, so `archtest` fails if main starts wiring
   again.)*
8. **Fire-and-forget workers** (P5). `Workers.Run` and `Reader.Run`
   started goroutines with no handle, and `serve` closed the pool while
   batches could still be querying. A claim cut off by the shutdown kept
   its spent attempt and waited out its lease.
   *(Done, 2026-10-29. Both `Run`s block until their loops return, and
   `App.Run` waits for them and the event hub. `serve` stops the
   listener, then waits up to 15 s for the background work before the
   pool closes. `batch.Each` counts a claim interrupted by its context
   ending as not started, so it is released due with its attempt
   refunded. A send interrupted after its POST is therefore sent again
   later, and the receiver re-verifies it as an update (§15.4 rule 6).
   `TestShutdownHandsBackTheClaimInFlight` and
   `TestEachHandsBackTheClaimAShutdownInterrupts` cover the change.)*
9. **Plane-1 schema outside the studio** (D4). `mention` read `items` and
   `item_versions` to resolve targets and pins. Handlers in `web` held the
   mention-list SQL and an overlay query. `account.CreateUser` inserted
   the origin itself and left its first render to the caller, in a second
   transaction, so a failed `Rebuild` left an origin with no `blyg.json`.
   *(Done, 2026-10-29. `studio.TargetItem` and `studio.Pinned` answer the
   receiver and verifier, and the path patterns moved with them.
   `mention.Store` lists received and sent mentions, and validates the
   filters as `apperr` kinds. `reader.Subscribed` answers the event
   stream. `CreateUser` asks an `account.OriginMaker`, which the studio
   implements, to insert and render the origin in the user's
   transaction. The CLI and the e2e harness no longer rebuild after it.
   Planes tests now cover this: `mention` may not read the studio's
   tables, `account` may not touch plane 1, and `web` may hold no SQL.
   `TestNewOriginIsServedAtOnce` and `TestFailedOriginLeavesNoUser` cover
   the account change.)*

## Interop (2026-10-10)

Checked against the reference client, blygger-studio 0.39.0 (`9f6bab8`).
Both checks are in the repo and are rerun when the spec or the reference
moves (`scripts/interop/README.md`):

- **`make parity`**, differential: 56 cases through both implementations'
  pure functions (§10.2 text content, quote normalization, partial
  location, §5.6 rule 6 flattening). There are no unexpected differences.
  The documented ones are our lazy-continuation fix and entity decoding
  (the reference decodes a fixed subset); neither renderer emits those
  entities, so the two agree on each other's content.
- **`make interop`**, live: both studios on localhost, each subscribed to
  the other, 32 checks in five scenarios. All pass.
  - import, with hashes, pins and `page`;
  - quotes of every shape, including a cycle;
  - stubs;
  - mentions verified as stub, transclusion and fork;
  - forks byte-identical to each other's flattening;
  - withdrawal end to end.

What it changed in blygd:

- `POST /api/subscriptions/{id}/poll`, within §12.3. The first run had to
  nudge `next_poll_at` by hand.
- The mention sender honors a 429's `Retry-After` when it is sooner than
  §15.2's next step (never under a minute). The reference refuses a pair
  re-sent within 60 s, so a quick withdrawal reached it a schedule step
  (15 minutes) late.

Observed, not bugs:

- The reference retains a pin past withdrawal only for items in a hopper
  (§13.4 MAY).
- It serves pinned version pages, so a cross-origin fork's lineage line
  could link one when known.

## Live hints (done, 2026-10-10)

`GET /api/events` (SSE over `LISTEN/NOTIFY`, DESIGN.md "Live hints"),
and the studio follows it: backfill progress in place, a dot on tabs
with news, and a refresh bar instead of re-rendering under the reader's
thumb.

## Operations

- **API description (done).** `internal/web/openapi.json`, OpenAPI 3.1,
  hand-written, embedded, served unauthenticated at
  `GET /api/openapi.json`. `openapi_test.go` fails when a registered route
  is missing from the document, the document names a route that does not
  exist, or a route's scope differs between the two. Every `/api/`
  response the e2e suite receives is also checked against the documented
  schema for its status: required members, undocumented members, and
  types. Every operation is exercised.

- **Migrations (done).** tern, embedded. `001_initial.sql` is the schema
  as of 2026-10-10. `blygd serve` (and every command) migrates first;
  `BLYGD_MIGRATE_ON_START=false` makes `blygd migrate` the only way, and
  an out-of-date schema is then refused. Two cases are refused: a
  database newer than the binary, and one built before migrations
  (recreate it, once). Shipped migrations are append-only, enforced by
  `migrations.sum`.
- **Deploy.** One binary under systemd, Caddy in front for TLS until
  custom domains move TLS in-process, nightly `pg_dump`. Set
  `BLYGD_TRUST_PROXY=true` behind Caddy.
  Hardening it (2026-10-29), before the 0.3 → 0.4 deploy, which carries
  migrations 003–006:
  1. Rehearse each migrating release on a restored copy of production.
  2. `scripts/deploy.sh`: build from a clean checkout, back up, dry-run
     the migrations on a scratch restore of that backup, install keeping
     `.prev`, check health and the running checksum, and put the old
     binary back on failure when nothing migrated.
  3. The binary says what it is. *(Done: `blygd version` prints the
     commit, Go and x/net the toolchain stamped; the "listening" log line
     carries the commit; `-dirty` marks a tree with changes.)*
  4. Caddy holds requests across a restart (`lb_try_duration`) instead of
     answering 502.
  Then alerts (an uptime check, and a ping from each good backup), a
  provisioning script with a timed restore drill, and CI once the code
  has a host.
- **Security posture: malicious and overeager requests.** Today's limits
  are local: mention receipt per host, domain and globally (§15.3), 8
  event streams per user, a 1 MiB JSON body cap, the media cap, server
  timeouts, and bounded outbound fetches. The API itself has no request
  rate limit, so one token (a buggy script, an agent in a loop) or one
  stranger can spend as much as it likes. Before the first public tenant:
  - **Per-token and per-user API budgets**, with 429 and `Retry-After`,
    and stricter ones for the expensive routes: export (renders the whole
    origin in memory; a large one can also outrun the 60 s write
    timeout), rebuild, subscribe and poll-now (each turns one request
    into several outbound fetches), and media upload.
  - **Per-address limits on the unauthenticated surface:** origin files,
    live hopper pages, the webmention endpoint and failed bearer
    attempts, which are logged but not throttled.
  - **Outbound amplification:** a cap on subscriptions per user and on
    fetches per user per hour, so blygd cannot be steered into hammering
    a third party.
  - **Abuse visibility:** count 401s, 403s and 429s per token and address
    in the request logs, so a misbehaving tool is easy to spot and
    revoke.
  - A pass over every route for what it costs and who can call it, run
    again whenever the MCP transport lands, since agents call tools
    eagerly.

  *(Done, 2026-10-11, except where noted: budgets per token and user by
  cost class, failed-auth and public budgets per address, a subscription
  cap, address and token in the request log, and the cost of every route
  in openapi.json. See DESIGN.md "Request budgets". Open: budgets are per
  process, a full limiter lets new addresses through without saying so in
  the logs, and the route review is due again with MCP.)*
- **Observability.** Request logs exist. Add queue-depth gauges for the
  mention tables, and the poller, before the first public tenant. The
  poller records `last_error`, `poll_failures` and `lossy` per remote
  origin, and the subscriptions API shows them.
