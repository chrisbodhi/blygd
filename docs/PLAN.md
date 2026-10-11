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
  - **Close the local shortcut.** `reader.LocalFirst` answers this
    instance's origins from the database without the network, trusting
    `base_url` instead of DNS. That is safe today only because every
    origin's host is `BLYGD_PUBLIC_URL`'s. It must treat an origin as
    local only when its host is the instance's own or its custom
    `base_url` is verified, and go through the network otherwise. Without
    this, another tenant subscribing to `https://blyg.google.com/` would
    read the impostor's items, and so would quotes and snapshots. Test it:
    an unverified `base_url` must not be read locally.
  - An export is just files, and anyone can hand-write a manifest
    claiming any site. It speaks for an origin only where DNS serves it,
    which is why the checks above live in serving and reading, not in
    rendering.
- **Studio discovery** via an HTML `rel` link on a studio page;
  OAuth-style token approval beside paste-a-token (§16.6); an MCP
  transport over the same operations and scopes. *(Discovery done:
  `rel="service-desc"` to the OpenAPI document. Approval and MCP open;
  the reference's shape is in its `docs/client-access.md`.)*

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
