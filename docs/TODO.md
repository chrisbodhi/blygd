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

- [ ] `internal/web/tenancy_e2e_test.go`: every operation with an id in its
  path, called by one user with another user's id, is a 404 and changes
  nothing. Driven from `openapi.json`, so a new route with a path id fails
  until it has a case. Break one owner check on purpose and see it fail.
- [ ] Ids in request bodies: hopper members, `forked_from`, `stub_of`,
  `![[id]]` and `[[id]]` naming another user's draft, read marks, events.
- [ ] Anything that fails here comes before every other track.

## Track B — studio fixes (Saturday)

- [ ] **Approving a response always fails.** The studio's "Show on your
  page" (`app.js`, `PUT /api/mentions/{mid}/approval`) needs
  `origin:write`, which `STUDIO_SCOPES` does not hold, so every
  studio-minted token gets 403. Add the scope, and update the comment
  beside it and DESIGN.md "The studio" (its own token).
- [ ] **Settings:** title, description and author through
  `PATCH /api/origin`, which the API already describes.

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
