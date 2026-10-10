# blygd — design

blygd is a Blygger studio as one Go binary over Postgres: publisher,
reader, hoppers and Webmentions together. It is headless — every client
(the SwiftUI app, ygg.el, an agent) talks to it over the write API — and
multi-tenant, with each user owning one origin.

Spec references (§) are to `blygger-spec` `docs/protocol-v0.3.md`, read at
commit `c2da39f` (2026-10-07) for Phase 1 and `d008e21` (2026-10-07) for
Phases 2 and 3.

## Three planes

A published feed is not a subset of a hopper. The two overlap only when
one of your own items happens to be in one of your hoppers. blygd keeps
three planes apart, and the separation is structural:

| Plane | What | Tables | Authority |
|---|---|---|---|
| 1. Own origins | Items this instance publishes, and its blogroll | `origins`, `items`, `item_versions`, `media`, `blogroll_entries`, `surface_files` | Authoritative |
| 2. Import mirror | Other origins' items, one copy per origin, shared by every subscriber | `remote_origins`, `imported_items`, `imported_pins` | Reconciled copy (§13) |
| 3. Private overlay | Per-user: subscriptions, hoppers, reads | `subscriptions`, `hoppers`, `hopper_members`, `reads` | Private, never on the wire |

- **The renderer reads only plane 1.** Imported items can never be
  re-emitted (§13.5) because nothing that writes `surface_files` queries
  plane 2. A thread may quote an imported item (§10.2), and that is the
  one door: `studio.Imports`, implemented by `reader.Snapshots`, hands a
  publish the snapshots the publisher's own subscriptions hold. It runs
  before the transaction. What it returns is baked once into the new
  version and stored in plane 1, where it is the publisher's bytes from
  then on.
- **Hopper members are references**, exactly one of `own_item_id` or
  `imported_item_id`. A copy would outlive a withdrawal, which §13.4
  forbids. Own members may be drafts.
- **The mirror is shared** so N users following one blyg cost that blyg
  one poll (§12.3), not N.
- **The separation runs both ways.** `internal/studio` never queries the
  mirror or the overlay, and `internal/reader` never queries plane-1
  tables or writes the surface. A test in each package fails if a query
  crosses the line.
- **The blogroll is plane 1.** Publishing a reading choice is a publishing
  act (§11). Opting a subscription in copies its outline (title, feed URL,
  site URL) into `blogroll_entries`, and the renderer reads only that. The
  copy is a snapshot: re-sending `in_blogroll: true` refreshes it.

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
schema (triggers in `internal/db/migrations/`), not only in Go, and each
has a test.

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
| Media bytes never change | §5.4 | content-addressed path + trigger `media_guard` |
| Media any published version references is kept forever | §8 rules 2 and 4 | `version_media` foreign key (RESTRICT) + trigger `version_media_guard` |
| Media only drafts use is never served | §4 | the surface serves media only when `version_media` lists it |
| A response shows only while approved and verified | §15.5 | CHECK `approved_at IS NULL OR status IN ('verified', 'pending')`; render filter |
| An approval does not survive a changed response | §15.5 | verifier clears `approved_at` unless relation, origin, page and author match |
| A public hopper's slug never changes | §13.5 (reference `slug_frozen`) | CHECK `NOT public OR slug_frozen` + trigger `hoppers_guard` |
| Public hoppers are never protocol surface | §13.5 | served live, never in `surface_files`; structural test in `internal/hopper` |
| Pinned version pages stay 200 forever | §8.4 rule 1 | `surface_files.permanent` (re-renderable, not write-once) |
| Imported items are never re-emitted | §13.5 | renderer reads plane 1 only; structural test |
| The import watermark never regresses | §13.3 | trigger `imported_items_guard` |
| Retained pins never change | §8 rule 1, §13.4 | trigger `imported_pins_guard` |
| A withdrawn import keeps no content | §13.4 | CHECK `state = 'current' OR doc IS NULL` |
| A version's provenance never changes | §10.3, §10.6 rule 4, §5.7 rule 5 | trigger `item_versions_guard` |
| Threads carry `transclusions`, fragments never | §10.3 | CHECK constraints |
| Stubs are threads | §10.6 | CHECK constraints on `items` and `item_versions` |
| A tenant quotes only what its own subscriptions imported | §10.2 step 2 | `reader.Snapshots` joins `subscriptions`; e2e test |

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
re-queues only new or changed references; a target named twice (a stub
that quotes its target) is one row, recorded by its first relation: fork,
stub, transclusion. Withdrawing an item re-sends every mention it made,
once, so each target finds the endcap and marks its mention gone (§15.7). A worker then:

- discovers the endpoint: manifest `webmention` key first, then W3C
  discovery (Link header via HEAD, then GET, then the first `<link>` or
  `<a>`);
- POSTs without following redirects;
- applies §15.2's schedule: 2xx is sent, 4xx is failed, 5xx/429/network
  are retried at 15 m, 1 h, 4 h, 12 h, 24 h and then abandoned. A 429's
  `Retry-After` sooner than the next step is honored (never under a
  minute): the receiver knows when it will listen.

**Workers** use a lease, not a job table. A claim is one
`UPDATE … WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED)` that pushes
`next_attempt_at` forward and bumps `attempts`. The network work then runs
outside any transaction, and a crashed worker's rows come due again on
their own.

## The reader (Phase 2)

**Resolution** (§12.1) is `reader.Resolve`: the six steps in order, at
most six fetches, under a 45 s overall deadline because a subscribe
request waits on it. The identity is the URL the manifest was fetched
from, after redirects, minus `blyg.json` (§12.2). A manifest `site` that
disagrees is stored as `site_discrepancy` and never adopted. A failure
returns the trail of URLs tried. Only the HTML `<link rel="blyg">` is
read; the optional HTTP `Link` header is not.

**Identity changes are never automatic.** `POST
/api/subscriptions/{id}/resolve` re-runs resolution and only reports.
`POST …/move {to}` moves the subscription, and only if `to` resolves to
exactly the identity named, so the user confirms the identity they saw,
not a URL that redirects somewhere else.

**Polling now.** `POST /api/subscriptions/{id}/poll` polls one origin
synchronously, within §12.3. It is refused (429, `Retry-After`) while the
origin backs off after failures (polling never tightens on failure),
within a minute of its last poll (the mirror is shared, so one tenant
cannot hammer an origin for everyone), and while another poll of it
runs. `poll_lease_until` marks a poll in flight.

**The poller** leases due remote origins with the mention workers'
pattern, and only origins someone subscribes to. One poll of one blyg:

1. Conditional GET of `feed.xml`. Entries are triggers: an id we lack, or
   a version above ours, queues a document fetch. The feed never writes
   state (§13.2).
2. Reconcile against `items/index.json` when due: on first poll, every
   24 h, when the newest guid from last time has left the window, or when
   the feed does not parse. Any entry whose version differs from ours is
   fetched. A 404 index marks the subscription `lossy`.
3. During reconciliation, re-fetch the 10 live documents fetched longest
   ago. Stealth edits show up nowhere else (§13.3).
4. Only a fetched document advances state, in its own short transaction
   after its fetch.

A poll fetches at most 200 documents. A large first backfill continues a
minute later. Feed and index validators are saved only once everything
they triggered has been fetched, so a 304 cannot hide an unfinished
entry. Failure backs off exponentially from the 30-minute interval, up to
24 h. It honors `Retry-After` and never polls sooner than the steady
interval (§12.3). A poll failure never changes item state.

**Applying a document** (`ApplyDocument`):

- It must come from exactly `{origin}items/{id}.json`, after redirects.
  The same rule as Webmention verification (§15.4).
- Unknown kinds are ignored. The document is stored verbatim as `jsonb`:
  key order and whitespace are not kept, every value is.
- A lower version than the watermark is not adopted. It sets
  `discrepancy`.
- Same version, different hash: a stealth edit, adopted and recorded.
- A `content_hash` that does not match `content_md`: recorded, adopted.
- An endcap rolls the item to null: `doc` is cleared, `page_url` kept.
- Pins the changelog names are fetched once into `imported_pins` and
  kept through withdrawal (§13.4). A pin that 404s is a discrepancy.

**L0** (§13.6): ids are `l0:` plus 32 hex characters of SHA-256 over the
GUID (or link), which can never be a blyg id. A wrapped entry stores
title, link, the summary HTML verbatim (capped at 64 KB) and a 1,000
character plain-text cut. Changed content bumps a local version; entries
that leave the window are kept.

**Ordering** (§13.7): `sort_at = least(claimed updated, when we first
saw this version)`, a stored generated column. Edits resurface; a
future-dated origin sorts no later than now.

**Same-instance subscriptions** go through `reader.LocalFirst`, which
serves URLs inside an own origin from `surface_files` with the same
status codes and ETags as HTTP. The importer sees exactly the bytes a
remote reader would.

**Outbound targets** (§15.2): the publish transaction writes the
reference convention, because it does not read the mirror. The send
worker, which writes no surface, swaps in the imported item's `page`
when the mirror holds one.

**Reading API**: subscriptions, a merged feed with keyset paging and an
unread filter, single items with retained pins, and read marks per
version (an item read at v3 is unread again at v4). Every imported byte
is returned verbatim with a `notice` that clients must sanitize (§14).
Hopper members that are imported items carry the verbatim document, or
none once withdrawn.

## Threads (Phase 3)

**Composition** is `blyg.Compose`, pure apart from a resolver callback,
and it runs inside the publish transaction:

1. `[TK]` scopes are stripped to their output. That is the wire
   `content_md` the hash covers (§5.7 rule 2).
2. A thread is split at its directive lines. Each directive resolves and
   bakes. A directive with an attached blockquote is a partial quote: the
   selection must be a substring of the target's text content (§10.2),
   and it bakes as escaped `<p>` lines with `blyg-partial`. The selector
   records the first match with 32 characters of context either side.
3. The prose between directives is rendered with `[[id]]` links swapped
   for sentinels, generated spans are wrapped in `blyg-tk-gen`, and the
   links are spliced back as absolute anchors. A link inside another
   link's text becomes its label, and one inside a URL or a tag goes back
   to the author's literal.

Code spans and code blocks are found with goldmark's own parser, so the
grammar is inert in exactly the places the renderer shows code (§10.1).
`SelectionText` follows the reference client's normalizer rule for rule,
because any reader may re-run the partial check.

**Resolution** (§10.2) is the studio's `resolver`. It looks in this
origin first: a published item of either kind, then the local closure
check for threads, walking stored `transclusions[]` within this origin
only. After that it looks at imports fetched before the transaction.
L0 rows, items withdrawn with nothing retained, and ids imported from
two origins are publish errors. A withdrawn import with a retained pin
bakes the latest pin. Every problem is reported at once, as `422` with a
`problems` list. A remote entry carries a `cited` frozen at publish, and
the response warns when a remote quote is unpinned (§10.2 SHOULD).
Republishing a thread whose text is unchanged is allowed when its bake
changed, because quotes re-resolve.

**Stubs** (§10.6). `POST /api/items {stub_of}` resolves the target and
freezes `stub_of` with its `cited` on the draft (`draft_stub_of`). Its
first body is `![[id]]`. Each publish copies it to the version after
version agreement (rule 3): the baked version when the body quotes the
target, else the version set at creation. A `{url}` stub carries the
client's `cited`, dated, with its excerpt held to 200 characters.

**Generated provenance** (§5.7). blygd runs no model, so it reads the
reference client's `[TK]` grammar as the client's declaration, and
discloses every scope. `[TK]instruction[=]output[/TK]` takes its sources
from the instruction's `![[id]]`s, which must be own published items
(rule 1). The model and time come from `PATCH /api/items/{id}
{generation: [{model, at}]}`, by scope position.
`[TK]impyrt <model>=text[/TK]` is text generated elsewhere (rule 7). A
publish with `note_generated: true` marks the changelog note as
client-written (§5.2).

**Forks** (§5.6 rule 6). `POST /api/items {forked_from}` without
`content_md` fetches the pinned document before any transaction (or
reads it from plane 1 when it is our own). `blyg.FlattenFork` then
builds the draft:

- own prose is copied byte-exact;
- each bake becomes a plain blockquote with an attribution line, nested
  quotes nested, and partiality read from the bake's class;
- `[[id]]` links become the anchors the bake rendered;
- generated spans become `[TK]impyrt …[/TK]`, so the fork discloses them
  again.

When the prose cannot be matched to the bake, the whole draft is rebuilt
from the HTML. A fork's page shows its lineage (rule 7).

## Media (Phase 4)

Bytes live in Postgres (`media.bytes`). Backups stay one `pg_dump`, and
publishing a version and recording its media share a transaction. Uploads
are capped (`BLYGD_MEDIA_MAX_BYTES`, default 10 MiB) and limited to PNG,
JPEG, GIF and WebP. SVG is left out because it can carry script. The
declared type must match the sniffed bytes.

- **Content-addressed:** the path is `media/{first 32 hex of sha256}.{ext}`.
  A path can never name other bytes (§5.4), and the same upload dedupes.
- **Referenced, not attached:** markdown writes `![alt](media/….png)`, and
  the renderer resolves `media/…` against the origin, not the page (§5.4:
  media URLs are origin-relative). At publish the studio collects every
  `{origin}media/…` the new `content_html` references, own baked quotes
  included. It refuses unknown ones, records them in `version_media`, and
  the item document's `media[]` is that list, with each `<img>` alt.
  Clients never maintain `media[]`.
- **Permanent once published, not once pinned.** §8 rule 4 keeps a pinned
  version's media forever, but any published version may be pinned later
  (rule 2). So media any version references cannot be deleted: the
  foreign key refuses, and the reference rows themselves are guarded.
  Uploads that only drafts use 404 on the surface and may be deleted.
- Served with `immutable` caching, `nosniff`, CORS, and a
  `default-src 'none'; sandbox` CSP.

## Responses and harassment (Phase 4)

§15.5 lets a publisher show verified mentions on an item's page as
curation. Structural verification stops *fake* mentions, not unwelcome
ones: a harasser who really publishes a stub of your item verifies like
anyone else. So the defaults assume a response may be hostile.

- **Approval is the gate.** A verified mention shows only after
  `PUT /api/mentions/{id}/approval`. That publishes, so it needs
  `origin:write` beside `mentions:write`. Hiding needs only
  `mentions:write`.
- **No swap after approval.** A re-sent mention re-verifies, and approval
  survives only if relation, source origin, page and author are unchanged.
  Otherwise, or if it goes `failed`/`gone`, approval is cleared, and the
  worker's `OnDisplayChange` hook re-renders the page at once.
- **Little to show.** An entry is a verb ("responded", "quoted this",
  "forked this"), the source's author name (as asserted, cut to 80
  characters), its host, and a `nofollow ugc` link. No words from the
  source, which is never stored.
- **Blocks** by host or by registrable domain (harassers mint
  subdomains). A blocked source's claim gets the same `202` as any other
  and is dropped: nothing stored, nothing fetched, so the block is not
  revealed. Mentions already received lose their approval and leave the
  default list (`?include_blocked=true` shows them). Unblocking restores
  nothing.
- Responses live in the stored permalink page: `inbound_mentions` is not
  the import mirror, and every change re-renders inside a transaction.
  Withdrawn items keep their responses (§15.7).

## Public hoppers (Phase 4)

§13.5 lets a reader display imported content publicly as curation: a
shown list with attribution and links, and no protocol surface. A public
hopper is that, at `{origin}h/{slug}/`.

- **Served live, never stored.** The web layer renders it per request
  from `hopper.Store.Public`, with an ETag and `no-cache`. Nothing reaches
  `surface_files`, so the renderer still never reads the mirror, a
  withdrawal shows on the next request with nothing to re-render, and a
  static export never holds anyone else's words. The page advertises no
  Webmention endpoint and carries a strict CSP.
- **Excerpts, not content.** Each entry is plain text (280 characters),
  the source's title and the item's own author, with links to the item's
  page and its origin. No imported HTML reaches the page.
- **What shows:** own items only while published. Imported items only
  from origins the owner still subscribes to, because only a polled copy
  learns of a withdrawal. A withdrawn import shows its retained pin
  (§13.4), marked, or nothing. L0 entries are marked as plain RSS.
- **Going public is a publishing act:** `PATCH /api/hoppers/{id}
  {public}` needs `origin:write` as well, as the blogroll does. The first
  time a hopper goes public, its slug freezes for good, through renames
  and going private again.

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

The API is described by a hand-written OpenAPI 3.1 document at
`GET /api/openapi.json`. Each operation's bearer requirement names its one
scope (3.1 permits scopes on non-OAuth schemes). The router is built from
a route table, and the tests compare that table with the document both
ways, scopes included. They also validate every response the e2e suite
sees against the documented schema.

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
  absolutizes every link (§5.2, decision #53).
- **Our own pages:** a thread's `content_html` carries other origins'
  bytes, baked verbatim as §10.2 requires. Pages share a host with every
  tenant and the API, so `blyg.PageHTML` sanitizes every `content_html`
  shown on a page (bluemonday's UGC policy, plus the protocol's own
  classes and `data-blyg-*`). The wire keeps the bytes.
- **Imported content:** stored verbatim and never trusted. §14 requires
  readers to sanitize at render, so the API hands clients verbatim HTML
  plus the obligation to sanitize. A server-rendered reading page, if one
  is ever added, sanitizes there. §13.6 calls the L0 summary "sanitized";
  we store it verbatim too and leave sanitizing to the client, like
  everything else imported.

## Deliberate departures from the reference client

- `page` is immutable; the reference lets a never-public slug move. Ours
  are `f/{id}/` and `t/{id}/`, which never need to.
- A publish with no content change is refused (409). It would cost every
  reader a new feed entry and guid for nothing.
- A fragment containing a live `![[id]]` is refused. Only threads
  transclude (§10), and printing the directive literally would surprise
  the author.
- Every `[TK]` scope is disclosed. The reference wraps only scopes its own
  generate endpoint produced; blygd has none, so a scope is the client's
  declaration. A directive inside generated output is refused (§5.7
  rule 3 keeps quotes and generated prose apart), and an inline span that
  crosses a paragraph is closed and reopened, so the HTML stays well
  nested.
- Pinned version pages (§8.4) show the pinned `content_html` sanitized,
  not verbatim as rule 2 says. A pinned thread carries other origins'
  bytes, and a page we must serve forever is the worst place for their
  scripts. The pin's promise is its JSON file, which stays byte-exact and
  is linked from the page. For the same reason the page is permanent but
  not write-once: a rebuild re-renders it, so sanitizer and chrome fixes
  reach old pins. Pinned pages advertise no Webmention endpoint, because
  §15.3 accepts a target only as an item's page or document.
- Fork flattening puts a blank line after each flattened quote. The
  reference does not, and the prose after a quote then joins it by lazy
  continuation.
- A remote `[[id]]` anchor is labelled the way the reference labels it
  (leading heading, else a quoted excerpt), using our own excerpt rule.
- Feed-title excerpts treat inline tags as zero-width. The reference turns
  every tag into a space, giving "**x**." → "x .".
- The mirror is shared between users, so a discrepancy (§13.3) is
  recorded once per item for everyone subscribed.
- The user-confirmed reset after a regression (§13.3, MAY) is not
  offered yet. The discrepancy is surfaced and the watermark holds.
- Adding a subscription to the blogroll needs `origin:write` as well as
  `reader:write`, because it publishes a reading choice (§11).
- The manifest claims `level: 2` (§3). With threads it is honest: every
  L1 and L2 publisher construct is implemented. The claim stays
  informative (§3.2).
