# Opening blygd to testers

The weekend of 2026-11-14. Goal: up to 10 people testing from Monday
2026-11-16, and a demo at the Protocol Institute hackathon launch on Friday
2026-11-20.

blygd is already multi-tenant underneath. Each user owns one origin, items
are scoped by `origin_id`, and hoppers, subscriptions and tokens by
`user_id`. Request budgets are per token and per user. What remains is
proving the isolation, fixing what testers will hit first, letting people
in, and running the instance for people other than its operator.

Status: `[ ]` open, `[~]` in progress on a branch, `[x]` done.
Each track names the branch it is on.

## Decisions open

- [ ] **Host and domain.** Origins are identities and published items are
  permanent (§4, §8), so whatever host testers publish on, they publish on
  for good. Choose it before the first tester's origin exists. A sandbox
  that is wiped later breaks the promise to anyone who subscribed or
  quoted, the reference studio included.
- [ ] **How users are mounted.** Today an origin is
  `https://host/{handle}/`, so every handle competes with host-rooted
  routes, which is what `ReservedHandles` is for. Namespacing users
  (`https://host/{x}/{handle}/`) leaves one name to reserve instead of a
  list that has to anticipate every future route. Points to weigh:
  - `h/` is already the public hopper prefix inside an origin
    (`{origin}h/{slug}/`), and `h` is reserved at the host root. Users under
    `h/` would give `https://host/h/alice/h/reading/`. Another letter
    (`u/`), `~alice` or `@alice` avoids reading `h` two ways.
  - The operator's origin already lives at `https://host/{handle}/` and
    stays there (its `base_url` is permanent). Routing is by longest
    `base_url` prefix, so mixed mounts work, but the new mount must be
    decided before any tester's origin is made.
  - `CreateUser` builds `base + handle + "/"`. The mount becomes a setting
    or a constant there, and DESIGN.md "Origins and routing" changes.
  - On a new domain with no existing origins, the mount can be chosen
    freely.
- [ ] **Reserved handles.** Depends on the mount. With the path at the
  host root, add `invite`, `login`, `signup`, `docs`, `status`, `help` and
  `about` before anyone chooses a handle, since a taken handle cannot be
  taken back. Under a namespace, the list mostly stops mattering.
- [ ] **Invites or the CLI.** Invites if people at the launch should get an
  account on the spot; otherwise `blygd user add` and a one-time secret
  link to send the owner token.

## Track A — isolation, proved (Saturday)

`[x]` on `claude/adoring-cray-iexclk-a-isolation`, 2026-11-14.

`TestTenantsAreIsolated` in `internal/web/tenancy_e2e_test.go`. Four
users on one instance; Alice has one of everything, Carol and Dave give
her a subscription, a received mention and an import. Every user holds a
`*` owner token, so no scope check can answer before the ownership check
does.

- [x] **Alice's fixture, one of everything:** a draft, a published item
  pinned at v1, a thread whose `[[id]]` link sends Carol a mention, a
  hopper with an own and an imported member, media, a subscription with a
  private display title and a read mark, a verified mention from Carol's
  stub, a block, a second token, and an OPML import worked off.
- [x] **Path ids, all 26 operations.** As Bob, with Alice's ids, each
  answers 404, and everything Alice can read of her own things (15 API
  reads, and the ETags of 5 surface files) is the same afterwards.
- [x] **The list comes from `openapi.json`.** The test fails on an
  operation with a path id and no case, and on a case whose path does not
  match the operation it is filed under. (That second check caught three
  of the test's own paths.)
- [x] **Media is per origin.** Bob uploading Alice's bytes gets the same
  path; deleting his copy leaves hers.
- [x] **Ids in request bodies:** Bob's hopper refuses Alice's draft, her
  published item, and an import named through her subscription.
  `stub_of`, `![[id]]`, `[[id]]` and `forked_from` naming her draft never
  resolve, whether or not Bob subscribes to her. Her published item
  resolves for Bob only once he subscribes (§10.2). `POST
  /api/reader/read` with her subscription is a 404. Bob's
  `DELETE /api/subscriptions/imports` deletes only his own.
- [x] **Events:** Bob's stream, open from the start, hears nothing, while
  Alice's hears `item`, `mention`, `outbound`, `import` and `subscribe`.
- [x] **Listings:** none of Bob's 12 listings names Alice's items, media,
  or the origins she follows.
- [x] **Nothing Bob is answered carries Alice's private words:** her
  draft, hopper name, token name, block, or display title.
- [x] **Tried:** each of these breakages fails the test, and was put back:
  - `DeleteMedia`'s `DELETE` without `origin_id`;
  - `AddOwn` without `o.owner_id`;
  - hopper `Delete` without `user_id`;
  - the studio's item read without `origin_id`;
  - `Revoke` without `user_id`;
  - the event filter without its origin or user check, for `item`,
    `mention` and `subscribe` hints;
  - reverting the fix below.
- [x] **Found and fixed:** `DELETE /api/reader/items/{sid}/{iid}/read`
  answered 204 for an item the caller does not hold. It changed nothing
  (the delete was already scoped to the caller), but every other operation
  answers 404, as `PUT` on the same path does. `Reader.MarkRead` now
  answers 404 when the caller holds no such item, and still 204 for an
  item already unread. `openapi.json` says so.
- [x] **Noted, not changed:** a subscription's `id` in the API is the
  remote origin's row id, which every subscriber to that origin shares.
  It is not a key to anything (every query also checks the caller's
  `user_id`), but ids are sequential and the mirror is shared, so
  subscribing to an origin tells a user whether someone on the instance
  already follows it, as the instant backfill already does. That comes
  with the shared mirror (DESIGN.md "Three planes"), and is fine for
  testers. Revisit if the instance ever hosts people who must not learn
  that about each other.
- [x] `go test ./...` green against Postgres 16 (this container cannot
  install 18; nothing in the migrations needs it). Run `make test-all` on
  18 before deploying.
- [ ] Merge back into `claude/adoring-cray-iexclk`.

## Track B — studio fixes (Saturday)

`[x]` on `claude/adoring-cray-iexclk-b-studio`, 2026-11-14.

No API change: everything the studio now does is an operation
`openapi.json` already describes.

- [x] **Approving a response always failed.** `STUDIO_SCOPES` lacked
  `origin:write`, which approval needs beside `mentions:write` (§15.5),
  so every studio-minted token got 403.
  - Added, with the comment above it rewritten. DESIGN.md "The studio"
    says what the token holds and why.
  - A sign-in from before the change lacks the scope. The mentions view
    then says to sign in again, rather than show a button that is refused.
  - `TestStudioHoldsWhatItCalls` reads `STUDIO_SCOPES` and every `api()`
    call out of `app.js`. It fails when a call needs a scope the list
    lacks, counting approval's extra `origin:write`. Calls made with the
    pasted token (`call()`, to mint) are exempt. Without the scope it
    names approval and `PATCH /api/origin`.
  - `TestStudioTokenShowsResponsesAndEditsSettings`: a token minted with
    exactly those scopes approves a verified response, which then shows
    on the page, and retitles the origin. Without the scope it gets 403.
- [x] **Found on the way, fixed:**
  - Show, Hide, Delete (a draft) and Unsubscribe never refreshed the view on
    success. `attempt()` returned the response body, and a 204 has none,
    so the caller read success as failure. It now returns `true` for an
    empty success.
  - `GET /api/mentions` lists notices too, and the studio offered "Show"
    on them, which always answers 409 (§15.5). Notices now carry a
    badge and no Show button, and the view says what they are. The
    verbs gain `source` ("drew on") and `link` ("linked to").
  - `operationFor`, the studio tests' path matcher, now prefers a literal
    segment to a parameter, as the router does. `/api/tokens/self` matched
    `/api/tokens/{tid}` at random before.
- [x] **Settings tab** (`#settings`): the permanent origin URL; title;
  description; name and link as the origin's `author` (§6.1), with
  "Sign your items with this name" setting the same object as the
  `byline` (§5.5); accepting mentions; and sign out. It sends only what
  changed, keeps any other members of `author`, says saving rebuilds
  every page, and disables Save while it runs.
- [x] **Tried in a browser** (headless Chromium, a local instance on
  Postgres 16): signing in with an owner token gives a studio token with
  `origin:write`. Show re-renders with the "shown" badge and the response
  is on the public page, and Hide brings Show back. Save with nothing
  changed says so; a real save reaches `blyg.json`. No page errors. All
  five tabs fit at 360 px.
- [x] `go test ./...` green against Postgres 16. Run `make test-all` on
  18 before deploying.
- [ ] Merge back into `claude/adoring-cray-iexclk`. Testers already
  signed in must sign out and in once.

## Track C — reserved handles or a mount (blocked on the decisions above)

- [ ] Whichever is decided, with a test that a handle colliding with a
  host-rooted route is refused, and DESIGN.md "Origins and routing"
  updated.

## Track D — invites (Sunday)

- [ ] Migration 008: `invites` in `internal/account`, holding the SHA-256
  of the code (never the code), `expires_at`, `redeemed_at`,
  `redeemed_by`, `note`. Recorded in `migrations.sum`.
- [ ] `blygd invite [--expires 7d] [--count N] [--note …]` prints codes
  once.
- [ ] `POST /api/invites/redeem {code, handle, title}`, no token. Charged
  to the `fetch` cost class and to the failed-authentication budget per
  address. The invite is redeemed in the transaction that creates the
  user, through a `CreateUser` that takes the caller's transaction.
  Returns the owner token once. Described in `openapi.json`.
- [ ] Studio: "Have an invite?" shows the permanent URL before submitting,
  shows the owner token once with a prompt to save it, then trades it for
  a studio token as today.
- [ ] Tests: a second redemption fails; an expired invite is refused; a
  taken or reserved handle leaves the invite unused; a wrong code costs
  the address's budget.

## Track E — CORS on `/api/` (Sunday)

So that a client on another origin (a hackathon team's web app) can call
the API, as the studio does from this one. Safe because the API
authenticates only by a bearer token in a header, never by cookie: a page
on another origin cannot act as a user without being handed their token.

- [ ] Every `/api/` response, errors and 429s included, carries
  `Access-Control-Allow-Origin: *`. Never `Allow-Credentials`.
- [ ] `OPTIONS` preflights for `/api/` are answered before routing,
  authentication and the token budgets (a preflight carries no
  `Authorization`), charged to the public per-address budget.
  Allow `GET, POST, PUT, PATCH, DELETE`; headers `Authorization` and
  `Content-Type`; `Access-Control-Max-Age` of a day.
- [ ] Expose `Retry-After` and `WWW-Authenticate`, so a browser client can
  back off and tell an expired token from a missing one.
- [ ] `/api/events` works from another origin with `fetch()` (it already
  avoids `EventSource`, which cannot send the header).
- [ ] The studio's CSP is unchanged: it still connects only to this host.
- [ ] Tests: a preflight from an arbitrary origin succeeds without a token;
  a 401, a 404 and a 429 all carry the header; no response carries
  `Access-Control-Allow-Credentials`.
- [ ] DESIGN.md: a "Cross-origin clients" note under "Write API and
  scopes", with why it is safe. `openapi.json` mentions it in its
  description.

## Track F — running it for others (Sunday)

- [ ] Uptime check on `/healthz` (it pings the database) from an external
  monitor.
- [ ] `backup.sh` pings a success URL (`BLYGD_BACKUP_PING_URL` in
  `backup.env`) after `pg_restore --list` passes. No user data goes to
  either service.
- [ ] A log line each minute with queue depths: inbound mentions pending,
  outbox due and failed, remote origins due and failing, subscription
  imports pending. Each package counts its own tables; `internal/web`
  holds no SQL.
- [ ] `blygd user suspend <handle>` revokes every token the user holds.
- [ ] DEPLOY.md "Operating for other people": a lost owner token
  (`blygd token mint <handle> owner '*'`), suspension, taking content down
  (mint `items:withdraw` for the handle; the endcap stays), and that the
  operator can act as any user, so testers are told.
- [ ] If invites are for the launch: a per-user cap on total media bytes,
  checked in the upload transaction.

## The week

- [ ] Saturday night: deploy A and B (no migration).
- [ ] Sunday night: preflight (`scripts/deploy.sh --check`), deploy D, E, F,
  and add the DEPLOYS.md row.
- [ ] Monday: three testers. Tuesday: the rest, if nothing broke.
- [ ] An OPML of every tester's origin, imported by each
  (`POST /api/subscriptions/imports`), so everyone follows everyone.
- [ ] A one-page tester guide: what is permanent, pinning, the draft and
  preview loop, quoting and forking each other, where to report bugs.
- [ ] Wednesday midnight: freeze.
- [ ] Thursday: rehearse on the live host (publish, quoted by another
  tester, mention verifies, approve, shown, fork, withdraw and endcap);
  a timed restore drill; `free -m` against DEPLOY.md's 400 MB; check
  whether `blygger-spec` has moved past `24b9770`.
- [ ] Friday: demo.

## Not this week

Custom domains, OAuth-style approval and MCP, account deletion (the schema
forbids it), rate limits across processes, hoppers in the studio, and the
§10.2 open question.
