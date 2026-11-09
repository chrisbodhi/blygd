# blygd — design

blygd is a Blygger studio as one Go binary over Postgres: publisher,
reader, hoppers and Webmentions together. It is headless — every client
(the SwiftUI app, ygg.el, an agent) talks to it over the write API — and
multi-tenant, with each user owning one origin.

Spec references (§) are to `blygger-spec` `docs/protocol-v0.4.md`. This
document was last checked against it at commit `b31d0ef` (2026-11-09),
whose text is the eleventh revision (2026-11-07). Phase 5 was built
against the sixth revision, at `24b9770` (2026-10-20), and Phases 1–3
against `docs/protocol-v0.3.md` at `c2da39f` and `d008e21` (2026-10-07).
Section numbers carry over, with §10.3a new in 0.4. blygd emits
`"blyg": "0.4"` (§3.1), and reads any `0.x`.

What the revisions after the sixth changed, and where blygd stands:

- **Seventh** (2026-11-01, decision #87): `{origin}blyg.json` MUST serve
  the manifest at every identity origin (§4, §6.1, §12.1 step 4). blygd
  always serves it there, and already located other origins only there
  (Other origins' surfaces, below), so nothing changed.
- **Eighth** (2026-11-01, decision #86): hopper feeds. **Not
  implemented.** blygd emits no `hoppers` manifest key (§6.1) and no
  hopper feed (§4, §13.5 rules (a)–(d)), and its reader has no hopper
  match (§12.1 step 7), so no hopper subscription (§12.2, §13.6) either.
  Both are OPTIONAL to emit, so the publisher side is conformant; a hopper
  page's URL resolves to the whole blyg, the degradation step 7 itself
  names for readers without it.
- **Ninth** (2026-11-03): §10.1's rule that a directive line is never a
  lazy continuation line, so it ends a partial selection, is **not yet
  implemented** (Threads, below). §13.2's clause that a version with no
  words of its own MAY be queued as maintenance, and §13.7's folding of a
  burst under a thread published with it, are MAYs blygd does not take.
  A gap older than the sixth revision is recorded under the reader:
  §13.2's SHOULD NOT on resurfacing a same-hash republish.
- **Tenth** (2026-11-05, decision #93) and **eleventh** (2026-11-07,
  decisions #95 and #96) are clarifications blygd already meets: every
  reader gets the same stored bytes (§4), and nothing in blygd reads
  meaning into the words of a changelog note (§5.2).
- Ruled shapes that are not yet normative (§16.9 `pinned_at` and `cid`,
  §16.10 author-sized images, §16.13 `moved_from`) are neither emitted nor
  read.

## Three planes

A published feed is not a subset of a hopper. The two overlap only when
one of your own items happens to be in one of your hoppers. blygd keeps
three planes apart, and the separation is structural:

| Plane | What | Tables | Authority |
|---|---|---|---|
| 1. Own origins | Items this instance publishes, and its blogroll | `origins`, `items`, `item_versions`, `media`, `blogroll_entries`, `surface_files` | Authoritative |
| 2. Import mirror | Other origins' items, one copy per origin, shared by every subscriber | `remote_origins`, `imported_items`, `imported_pins` | Reconciled copy (§13) |
| 3. Private overlay | Per-user: subscriptions, hoppers, reads | `subscriptions`, `subscription_imports`, `hoppers`, `hopper_members`, `reads` | Private, never on the wire |

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
| An approval does not survive a changed response | §15.5 | verifier clears `approved_at` unless relation, class, origin, page and author match |
| A notice is never shown among responses | §15.5 | CHECK `approved_at IS NULL OR class = 'response'`; a link is always a notice; class is set exactly with relation |
| A public hopper's slug never changes | §13.5 (reference `slug_frozen`) | CHECK `NOT public OR slug_frozen` + trigger `hoppers_guard` |
| Public hopper pages are served live and never stored | §13.5 (curation display) | nothing writes them to `surface_files`; structural test in `internal/hopper` |
| Pinned version pages stay 200 forever | §8.4 rule 1 | `surface_files.permanent` (re-renderable, not write-once) |
| Imported items are never re-emitted | §13.5 | renderer reads plane 1 only; structural test |
| The import watermark never regresses | §13.3 | trigger `imported_items_guard` |
| Retained pins never change | §8 rule 1, §13.4 | trigger `imported_pins_guard` |
| A withdrawn import keeps no content | §13.4 | CHECK `state = 'current' OR doc IS NULL` |
| A version's provenance never changes | §10.3, §10.6 rule 4, §5.7 rule 5 | trigger `item_versions_guard` |
| Threads carry `transclusions`, fragments never | §10.3 | CHECK constraints |
| Stubs are threads | §10.6 | CHECK constraints on `items` and `item_versions` |
| A tenant quotes only what its own subscriptions imported | §10.2 step 2 | `reader.Snapshots` joins `subscriptions`; e2e test |

Three house rules are not the protocol's, and live in Go rather than the
schema, so they are checked by reading the source (`internal/archtest`
and a planes test in each package):

| Rule | Checked by |
|---|---|
| Nothing that writes the surface reads the mirror | per-package SQL rules over folded string constants, failing on any table named through a variable; an import allow-list, with `studio` unable to reach `reader`, `hopper` or `mention` |
| No network inside a transaction | every transaction starts in `tx.Run`, whose marked context `fetch` refuses at runtime; no `pgx.BeginTxFunc` or `.Begin` elsewhere |
| All outbound HTTP goes through `fetch` | no default client, `http.Client` or `Transport` literal, or `net.Dial` outside it |

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
- `/llms.txt` (llmstxt.org) is host-rooted too: a static, embedded
  description of the service and where to read more (the spec, the
  reference client, `/api/openapi.json`, `/studio/`). It names no origin
  and lists no users, so it says nothing about who publishes here. No
  handle can contain a dot, and no surface file is called `llms.txt`, so
  it shadows nothing, even at a future root-mounted custom domain.

## Webmentions (§15)

**Receiving.** `POST {origin}webmention` runs §15.3:

1. Syntactic checks return 400. The target must be one of this origin's
   items by an address it serves for it (§15.3 step 1, decision #84): its
   page, its document, or a pinned version's page or document. That is a
   membership test; blygd never relocates its surface, so every such
   address is also inside the origin. A withdrawn target is fine.
2. Three rate caps return 429 and the claim is never queued: per source
   host (60/h), per registrable domain (240/h), global (2000/h). The caps
   are **instance-wide**, because verification fetches cost the machine,
   not the tenant.
3. Otherwise 202, and the claim is upserted as `pending`. A re-sent pair
   re-verifies rather than duplicating.

**Verifying.** A worker runs §15.4 with at most two bounded fetches, or
three under templates. The item document must have been fetched from
exactly `{origin}items/{id}.json` for its own origin and id, or, when the
asserted origin's own `blyg.json` carries an `item` template (§6.1), from
exactly that template's expansion. That one rule rejects mirrors, pinned
files, and path-mounted neighbours speaking for each other. The relation
check is `stub`, `transclusion`, `fork`, `source`, then `link`; a fork
counts only if it names a version pinned here, and an unpinned one falls
through to the rest. A reference must name this origin's identity as it
is now: blygd's manifest carries no `moved_from`, and the receiver
accepts no former identity (§16.13, decision #88, ruled but not yet
normative).

The receiver also derives a class (§15.5). A `link`, or a stub or
transclusion whose document has no words of its own (a repost, §15.4), is
a **notice**; the rest are **responses**. The repost test sets aside
directive lines, labels included, and the partial selection attached to
each, reading how far a selection runs from the bake as fork flattening
does, so a document from a client that ends quotes at the first unmarked
line keeps its words. Notices can be listed (`?class=notice`) but never
approved or shown.

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

**Resolution** (§12.1) is `reader.Resolve`: steps 1–6 in order, at
most six fetches. Step 7, the hopper match, is not implemented (it costs
no fetch, so the bound is the same), and a hopper's URL resolves to the
whole blyg. Resolution runs under a 45 s overall deadline because a
subscribe request waits on it. The identity is the URL the manifest was fetched
from, after redirects, minus its last path segment (§12.2), and that URL
is kept as `manifest_url` so polls re-read the manifest where it was
found. A `rel="blyg"` target whose body parses as a manifest is the
manifest; otherwise it is a base URL probed for `blyg.json`, as at 0.3
(§12.1 step 4). A manifest at a URL with a query string has no identity
and is refused (#64). A manifest `site` that disagrees is stored as
`site_discrepancy` and never adopted. A failure returns the trail of URLs
tried. Only the HTML `<link rel="blyg">` is read; the optional HTTP `Link`
header is not. Step 4 can now cost two fetches, so a page with a dead rel
link spends the bound on the conventional mounts before step 6 can
autodiscover its feed. Both the order and the bound are the spec's.

**Identity changes are never automatic.** `POST
/api/subscriptions/{id}/resolve` re-runs resolution and only reports.
`POST …/move {to}` moves the subscription, and only if `to` resolves to
exactly the identity named, so the user confirms the identity they saw,
not a URL that redirects somewhere else. A manifest that answers a poll
from anywhere but where it was found, by any redirect, is recorded as the
subscription's error with a prompt to re-resolve, and the poll goes on
with the manifest it holds. §16.13 (decision #88, ruled, not yet
normative) would make a permanent redirect to a manifest whose
`moved_from` lists the old identity a move to offer the user; blygd
neither reads `moved_from` nor tells a permanent redirect from any other.

**Polling now.** `POST /api/subscriptions/{id}/poll` polls one origin
synchronously, within §12.3. It is refused (429, `Retry-After`) while the
origin backs off after failures (polling never tightens on failure),
within a minute of its last poll (the mirror is shared, so one tenant
cannot hammer an origin for everyone), and while another poll of it
runs. `poll_lease_until` marks a poll in flight.

**The poller** leases due remote origins with the mention workers'
pattern, and only origins someone subscribes to. Every URL it fetches
comes from the stored manifest through `blyg.Surface`: `feed` and `items`
as given, item and pin documents from the `item` and `pin` templates, and
the 0.3 paths where the manifest names none (§4, §6.1). A reconciliation
re-reads the manifest first, and the rest of that poll uses the surface
it names. One poll of one blyg:

1. Conditional GET of the feed. Entries are triggers: an id we lack, or
   a version above ours, queues a document fetch. The feed never writes
   state (§13.2).
2. Reconcile against the archive index when due: on first poll, every
   24 h, when the newest guid from last time has left the window, or when
   the feed does not parse. Any entry whose version differs from ours is
   fetched. A 404 index marks the subscription `lossy`.
3. During reconciliation, re-fetch the 10 live documents fetched longest
   ago. Stealth edits show up nowhere else (§13.3).
4. Only a fetched document advances state, in its own short transaction
   after its fetch.

A poll fetches at most 200 documents, within a 5-minute deadline for the
whole poll, kept under the 15-minute lease so two polls of one origin
never overlap. A poll cut short keeps what it applied, saves no
validators and counts as a failure, so an origin that is always too slow
backs off instead of holding a slot. A batch's origins are polled side by
side, so a slow one delays only itself. A large first backfill continues a
minute later. Feed and index validators are saved only once everything
they triggered has been fetched, so a 304 cannot hide an unfinished
entry. Failure backs off exponentially from the 30-minute interval, up to
24 h. It honors `Retry-After` and never polls sooner than the steady
interval (§12.3). A poll failure never changes item state.

**Applying a document** (`ApplyDocument`):

- It must come from exactly the surface's URL for it,
  `{origin}items/{id}.json` or the `item` template's expansion, after
  redirects. The same rule as Webmention verification (§15.4). Retained
  pins likewise come from the `pin` template's expansion. A relative
  `page` is resolved against the origin; an absolute one is kept as given
  (§5.8).
- Unknown kinds are ignored. The document is stored verbatim as `jsonb`:
  key order and whitespace are not kept, every value is.
- A lower version than the watermark is not adopted. It sets
  `discrepancy`.
- Same version, different hash: a stealth edit, adopted and recorded.
  Except after an endcap: content at the endcap's version is a replay,
  not adopted, because the watermark survives withdrawal (§13.4).
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

**Maintenance** (§13.2) is not told apart yet. A version whose
`content_hash` equals the one before it (decision #77) is unread again
and sorts by its new `updated` like any edit, though §13.2 says readers
SHOULD NOT resurface one as new: a gap, not a choice. The importer
already reads the held version's hash before it applies the next. The
two MAY clauses beside it are not taken either: an edit below a
reader-local threshold (#83), and a version with no words of its own
(the ninth revision), which uses §15.4's repost test, the one the
receiver already runs (`internal/blyg`).

**Same-instance fetches** (subscriptions, forks, mentions in both
directions) are answered in-process. `fetch.Client.Local` hands any
request for `BLYGD_PUBLIC_URL`'s own scheme and host to the origin-surface
handler, the one a remote client reaches, minus the public request
budgets. The importer sees exactly the bytes a remote reader would, the
sender's POST reaches the real receiver, and nothing depends on whether
the instance can dial its own public address. Behind a private reverse
proxy, it can't. Only that one host is local, so an origin on any other
host is fetched over the network and vouched for by DNS.

**Outbound targets** (§15.2, #85): the publish transaction, which reads
neither the mirror nor the network, writes the target's default item
document. The send worker settles the address before sending: the
imported item's `page` when the mirror holds one under the target's
origin, else the item document from the target origin's `item` template.
It never guesses `f/{id}/`. The manifest it reads for the template is the
one endpoint discovery reads first, so it is fetched once. A manifest
that cannot be read for a reason that may pass (no response, 429, 5xx) is
retried, because guessing the default path would aim at the wrong
address on a templated origin.

**Other origins' surfaces** are located by `internal/surface`, which
reads `{origin}blyg.json` and nothing else: the one name every identity
origin MUST serve the manifest at (§4, §12.1 step 4, decision #87). It
depends only on `blyg` and `fetch`, so plane 1 may use it without
reaching the mirror. The verifier, the sender, the fork pin check, fork
copying and the attribution link all use it. The reader keeps its own
copy of each subscribed manifest.

**Reading API**: subscriptions, a merged feed with keyset paging and an
unread filter, single items with retained pins, and read marks per
version (an item read at v3 is unread again at v4). "Mark all read"
takes back the `as_of` a feed page carries and marks every item whose
current version was first seen by then (`version_seen_at`), not every
item sorted below some point: a backdated edit sorts among items already
read (§13.7), and must stay unread until it is shown. Every imported byte
is returned verbatim with a `notice` that clients must sanitize (§14).
Hopper members that are imported items carry the verbatim document, or
none once withdrawn.

**Subscription imports**: an OPML list, a blogroll (§11) or another
reader's export, sent inline or fetched from a URL (an OPML file, or a
blyg whose manifest names a blogroll), becomes one
`subscription_imports` row per entry, up to 1,000. A worker beside the
poller leases due rows the poller's way and resolves each through
`Subscribe`, so §12.1 and the subscription cap apply unchanged, and a
blyg's feed upgrades to the blyg (§11). It takes one entry per user per
batch, so one long list cannot hold back another user's. A failure
waits 5 minutes, then 20, and fails on the third try; the cap fails an
entry at once. An entry's title becomes the display title only when it
differs from the origin's own and the user has not named it, and
nothing imported joins the user's blogroll (§11: opt-in). Each outcome
is a `subscribe` hint to that user alone.

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
   **Gap:** since the ninth revision, §10.1 says a directive line (or the
   reserved `![[id@vN]]`) is never a lazy continuation line, so it ends
   the selection. blygd's `attachedQuote` still asks goldmark alone, which
   reads a directive straight after a `>` line as lazy continuation: the
   directive becomes selected text and never bakes. Fork flattening and
   the repost test use the same function to find a selection's extent.
3. The prose between directives is rendered with `[[id]]` links swapped
   for sentinels, generated spans are wrapped in `blyg-tk-gen`, and the
   links are spliced back as absolute anchors. A link inside another
   link's text becomes its label, and one inside a URL or a tag goes back
   to the author's literal.
4. Each resolved link is disclosed in `links[]`, one entry per link in
   document order (§10.3a, #79). Generated blocks render apart from the
   prose around them, so the order is read from the wire `content_md` by
   the same grammar. A remote entry sends a `link` mention, a notice. A
   markdown link to a blyg page is never an entry.

**Labels** (§10.1, #70 and #81) follow the id and a pipe and run to the
first `]]` on the line, so a label may hold a pipe or a single bracket but
never `]]`; a line that tries is literal. Brackets with anything but a pipe
after the id are literal too. On a link the label is the anchor text. On a
directive it is baked as `data-blyg-label`, escaped, and nothing else
sees it: not `transclusions[]`, not the target. Fork flattening uses it
as the attribution's anchor text, with the host and version beside it.

Code spans and code blocks are found with goldmark's own parser, so the
grammar is inert in exactly the places the renderer shows code (§10.1).
`SelectionText` follows the reference client's normalizer rule for rule,
because any reader may re-run the partial check.

**Resolution** (§10.2) is the studio's `resolver`. It looks in this
origin first: a published item of either kind, then the local closure
check for threads, walking the `transclusions[]` of the versions actually
baked, within this origin only. A local id never falls through to an
import, and one also held as an import is ambiguous (§5.1), a publish
error. After that it looks at imports fetched before the transaction.
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
from the instruction's `![[id]]`s, labels allowed. A source resolves
exactly as a directive does, so it may be another origin's item (rule
8). It goes through `studio.Imports` like a quote: only its id, version,
origin and a frozen `cited` reach plane 1, never its words. A remote
source carries its origin and `cited`, and sends a `source` mention,
which is a response (rule 9). The model and time come from `PATCH /api/items/{id}
{generation: [{model, at}]}`, by scope position.
`[TK]impyrt <model>=text[/TK]` is text generated elsewhere (rule 7). A
publish with `note_generated: true` marks the changelog note as
client-written (§5.2).

**Forks** (§5.6 rule 6). `POST /api/items {forked_from}` without
`content_md` fetches the pinned document before any transaction (or
reads it from plane 1 when it is our own), from the source origin's `pin`
template (§6.1). `blyg.FlattenFork` then
builds the draft:

- own prose is copied byte-exact;
- each bake becomes a plain blockquote with an attribution line, nested
  quotes nested, and partiality read from the bake's class;
- `[[id]]` links become the anchors the bake rendered;
- generated spans become `[TK]impyrt …[/TK]`, so the fork discloses them
  again.

A quote's attribution line links the quoted item's page where we hold
it, else its item document from its origin's `item` template, with each
origin's manifest read once per flattening.

When the prose cannot be matched to the bake, the whole draft is rebuilt
from the HTML. A fork's page shows its lineage (rule 7). For a remote
source the link is the pinned file where the first publish's pin check
found it, through the `pin` template, kept in `items.forked_from_url`
(migration 005). Rendering runs inside transactions and must work
offline for a rebuild, so it never fetches. The URL is written once, and
only when the check confirmed the file. Otherwise the page links the
default path.

**Preview** (`POST /api/items/{id}/preview`) is the publish that would
happen now, stored nowhere. It shares publish's composition
(`Studio.compose`), so it looks up imports the same way before its
transaction (`studio.Imports`, its one other sanctioned caller), and runs
in a read-only transaction, so Postgres itself refuses a write. Every
refusal a publish would make is a problem in the response rather than an
error, and `ComposeInput.Preview` marks a directive that cannot bake
where it stands, in the reference client's `blyg-transclusion
unresolved` block. It skips the fork pin check, the one fetch a publish
makes. The HTML is returned verbatim with a notice, like `/api/items`'
`content_html`: clients sanitize.

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
  survives only if relation, class, source origin, page and author are
  unchanged.
  Otherwise, or if it goes `failed`/`gone`, approval is cleared, and the
  page is re-rendered in the same transaction (`Studio.ChangeDisplay`,
  under the origin's lock): if the render fails, the approval stands and
  the claim is retried.
- **Little to show.** An entry is a verb ("responded", "quoted this",
  "forked this", "drew on this"), the source's author name (as asserted, cut to 80
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
shown list with attribution and links, with no protocol surface beyond
the manifest's `hoppers` list (§6.1) and a hopper feed (§13.5 rules
(a)–(d)). A public hopper is that display, at `{origin}h/{slug}/`, and
blygd uses neither of the two: both are OPTIONAL, so the manifest lists
no hoppers and no hopper has a feed. The page carries `<link rel="blyg">`
to its origin, as §13.5 asks; with no feed, there is no `rel="alternate"`
to carry.

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

## Static export (Phase 4)

`blygd export <handle>` and `GET /api/origin/export` (scope `items:read`:
it is all public) write an origin as a gzipped tar a plain static host can
serve: every surface file at its origin-relative path, directory pages as
`index.html`, and the media a published version references.

- **No endpoint.** A static deployment advertises none (§15.1, §15.7), so
  the export re-renders the origin with `accept_mentions` off into memory:
  no manifest `webmention` key, no `rel="webmention"` link. The renders
  write through `surfaceOut`; the database writer is the usual one.
- **Pins are their stored bytes**, as `putFile` keeps them: write-once.
- **Writes nothing.** One read-only, repeatable-read transaction, so the
  stored pins and the fresh renders are one snapshot.
- **Served at the base URL only.** Every URL inside is absolute against
  the origin's `base_url`; a copy served elsewhere points back here. To
  publish at another URL, the origin's `base_url` must be that URL.
- Not included: drafts, public hopper pages (served live) and the
  webmention endpoint. Exporting sends no mentions (§15.7 permits it, does
  not require it).

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

**Draft revisions.** Every item carries `draft_rev`, a hash of what an
edit changes and a publish reads (`draft_md`, `draft_note`, generation).
`PATCH` and publish take it back as an optional precondition and answer
`409 stale` when the draft has moved on, so two clients cannot overwrite
each other unseen. It is a body field, not an `ETag`: an `ETag` names the
whole representation, which pins and publishes change while the draft
does not, and a field also reaches clients through listings. A hash
rather than a counter needs no column, and two edits that arrive
at the same draft agree.

Bearer tokens are stored as SHA-256 hashes, shown once at mint time,
revocable, and scoped. Consequential public acts have their own scopes, so
a drafting tool or an agent can be given `items:write` without the power
to publish, withdraw, or pin (pinning is irrevocable). A token can only
mint scopes it holds. `blygd user add` mints the owner's `*` token; that
is the paste-a-token path §16.6 requires beside any future OAuth-style
flow.

Tokens may expire (`expires_at`; `expires_in` at mint, `--expires` on the
CLI), and an expired token is a 401. A minted token never outlives its
minter, as it never holds a scope its minter lacks: absent or later
expiries are cut to the minter's (`Principal.ExpiryFor`). The owner token
never expires; it is the root credential and belongs offline. Any token
may revoke itself (`DELETE /api/tokens/self`), which is signing out;
revoking another takes `tokens:write`. Issuing stays separate from the
policy: a login, when there are users who log in, is one more way to mint
an expiring, scoped token.

Tools discover the write surface from the studio page (§16.6), never the
manifest: `/studio/` carries `<link rel="service-desc"
type="application/openapi+json" href="/api/openapi.json">` (RFC 8631),
and the description is readable without a token. The reference links
OAuth endpoints (`authorization_endpoint`, `oauth-metadata`) instead;
blygd will add those when it has an approval flow.

## Request budgets

In-memory token buckets (`internal/limit`), so budgets are per process;
blygd runs one. `BLYGD_RATE_LIMITS=false` turns them off for harnesses.

- **API, per token and per user.** Each route has a cost class
  (`routeCosts`, stated as `x-blygd-cost` on every operation in
  openapi.json, and the two are compared by a test): `default` 120 a
  minute in bursts of 60, `fetch` 12 (subscribe, resolve, move, poll,
  import subscriptions, create an item, publish: each can fetch other
  hosts), `export` 2,
  `upload` 30 in bursts of 20 (media, and `PATCH /api/origin`, which
  re-renders every item), and `stream`, which has no per-request budget
  but at most 8 open streams. A user's budget is twice a token's, across
  all their tokens, so one runaway tool cannot starve the owner and many
  tokens cannot multiply a user. Over budget is 429 with `Retry-After`.
- **Failed authentication, per address.** 30 at once, then 10 a minute.
  Past that the address is refused before its token is looked up, good
  token or not.
- **Everything public, per address.** Origin files, hopper pages, the
  studio and llms.txt share 600 a minute in bursts of 300; webmention posts
  have 30 a minute on top of §15.3's per-source caps. `/healthz` is free.
- **Addresses.** The peer, or with `BLYGD_TRUST_PROXY` the last
  `X-Forwarded-For` entry, which is the one the proxy saw. Public IPv6 is
  charged per /64; loopback and private addresses are kept whole, since a
  tailnet puts every device in one /64. Each limiter holds at most 100,000 keys: refilled buckets are
  swept first, and if every bucket is in debt a new address goes through
  unbudgeted rather than evict someone mid-penalty.
- **Outbound.** A user may hold 1,000 subscriptions
  (`Reader.MaxSubscriptions`), checked before resolving, so a refused
  subscribe fetches nothing. With the `fetch` budget and the poll-now
  floor, no token can steer blygd at a third party faster than that.
- **Visibility.** The request log carries the client address on every
  line and the token id when there was one, so 401s, 403s and 429s count
  per token and per address.

## Live hints (`GET /api/events`)

A Server-Sent Events stream of "something changed" hints, per user. A
hint says what changed, never the state, so a client re-fetches what it
shows, and a dropped connection (a phone asleep) loses nothing: on
reconnect, re-fetch.

- **Sources:** the poller after each batch (`import`: `items`, `more`,
  `error`); the receiver and verifier (`mention`); the sender
  (`outbound`); the studio, on every change to an own item (`item`:
  `change`, and `version` or `draft_rev` where they apply, so a client
  can tell its own edit from another's); the importer, per entry tried
  (`subscribe`, to the importing user only). A studio event is sent
  inside the change's transaction, so it is heard on commit and never
  for a change that rolled back. Progress is never stored: it is
  transient, and the counts are in the API.
- **Transport:** Postgres `LISTEN/NOTIFY` on `blygd_events`, so any
  process can send and every process's streams hear. `internal/events`
  has one hijacked listening connection and a fan-out hub. A slow
  subscriber drops hints rather than slowing others.
- **Per stream:** events are filtered by scope and ownership (imports
  of the user's subscriptions, and their own subscription imports, with
  `reader:read`; their own items with `items:read`; their origin's
  mentions and sends with `mentions:read`). The server's write deadline
  is lifted for the stream, there is a heartbeat every 25 s, and at most
  8 streams per user. Requests share the process context, so a shutdown
  ends streams at once.
- Browsers read it with `fetch()`, because `EventSource` cannot send the
  `Authorization` header.

## The studio

`/studio/` is a deliberately plain client, embedded in the binary: one
HTML page, CSS and vanilla JS, with no build step. It talks only to
`/api/`.

**The API comes first; the studio is one client of it.** blygd exists to
let different interfaces be tried against one service, so the studio gets
no back door. Every request it makes is an operation `openapi.json`
describes, and it fetches nothing outside `/api/` (`studio_test.go`).
Its Go side is a static file handler (`studio.go`) and nothing more: no
template, no route, no response shape that exists for the studio's sake.
Whatever a new UI needs is added to the API, described, and is then
there for every client. The studio could be deleted, or replaced by
another directory of files, without touching a handler.

- **Its own token.** A pasted token that may mint is traded at once for a
  studio token: only the scopes the studio calls (items, reader,
  mentions; no `tokens:write`, origin or hopper settings), 30 days, named
  for the device. Only that one is kept, in `localStorage`. A stolen
  studio token cannot mint, and it ends on its own. A pasted token that
  cannot mint is used as it is. Sign out revokes the token.

- Its CSP allows scripts and styles from itself only, connections to
  this host only, and no framing.
- It never parses a string into markup, so imported content is only ever
  read as text. `studio_test.go` fails on any use of `innerHTML` or its
  kin.
- The token shares an origin with the public pages. Those pages sanitize
  every `content_html` they show and run no script under a sandboxing CSP
  (HTML safety). A real client should still live on its own origin
  (Phase 4's OAuth-style approval).

## HTML safety

- **Our own content:** the markdown renderer escapes raw HTML (the
  reference client's `html: false`), neuters `javascript:` URLs, and
  absolutizes every link (§5.2, decision #53).
- **Our own pages:** a thread's `content_html` carries other origins'
  bytes, baked verbatim as §10.2 requires. Pages share a host with every
  tenant and the API, so `blyg.PageHTML` sanitizes every `content_html`
  shown on a page (bluemonday's UGC policy, plus the protocol's own
  classes and `data-blyg-*`). The wire keeps the bytes.
- **Second wall:** every public HTML page is served with a CSP that runs
  no script (`default-src 'none'`) and sandboxes the page into an opaque
  origin, so even a sanitizer bypass could not read the studio's token
  from the same host. Pages may load inline styles and images (the
  origin's media, and `https:` images authors embed); public hopper pages,
  which show strangers' excerpts, load no images. `TestPagesRunNoScript`
  holds every page to it.
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
  reach old pins. Pinned pages advertise no Webmention endpoint of their
  own: a sender finds it in the manifest first (§15.2), and the receiver
  accepts a pinned version's page or document as a target (§15.3,
  decision #84).
- A partial quote's blockquote has CommonMark's extent, lazy continuation
  included (§10.1, decision #76), except that a directive line ends it,
  which blygd does not yet do (the gap under Threads). The reference
  (0.48.2) still ends it at the first line without `>`, so a document it
  composed selected less than we would read. Fork flattening takes the extent from the bake: lazy
  lines are the quote's only when the bake holds them, else the author's
  prose, so neither reading loses or repeats words.
- Fork flattening puts a blank line after each flattened quote. The
  reference does not, and the prose after a quote then joins it by lazy
  continuation.
- An unlabelled `[[id]]` anchor is labelled the way the reference labels
  it (leading heading, else a quoted excerpt), using our own excerpt
  rule. A label the author wrote always wins (§10.1).
- Feed-title excerpts treat inline tags as zero-width. The reference turns
  every tag into a space, giving "**x**." → "x .".
- The mirror is shared between users, so a discrepancy (§13.3) is
  recorded once per item for everyone subscribed.
- The user-confirmed reset after a regression (§13.3, MAY) is not
  offered yet. The discrepancy is surfaced and the watermark holds.
- Adding a subscription to the blogroll needs `origin:write` as well as
  `reader:write`, because it publishes a reading choice (§11).
- Another origin's surface is located through `{origin}blyg.json`
  everywhere outside the reader, never through the reader's stored
  manifest location. Since the seventh revision this is the spec's own
  rule, not a departure from it: `{origin}blyg.json` MUST serve the
  manifest at every identity origin, as the manifest or by a redirect to
  one with the same identity (§4, §6.1, §12.1 step 4; decision #87, on
  blygger-spec#21), and the parties that hold only an identity (a
  receiver under templates, §15.4; a sender building a target, §15.2; a
  forker checking a pin, §5.6 rule 2) all probe it. An origin whose
  manifest is only where a `rel="blyg"` link names it is now
  nonconforming. blygd can still subscribe to and read it, but not fork
  it: both the copy and the publish-time pin check refuse, naming the
  missing file. Mentions to it fall back to the default layout and W3C
  discovery, so they usually find no endpoint. Plane 1 knows only the
  identity, and passing the reader's location through `studio.Imports`
  would widen the one door between the planes. What remains a departure
  is from the reference client, which reuses its subscription's stored
  surface and so reaches such an origin.
- The manifest claims `level: 2` (§3). With threads it is honest: every
  L1 and L2 publisher construct is implemented. The claim stays
  informative (§3.2).
