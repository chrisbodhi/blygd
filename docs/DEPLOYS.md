# Deploys

What is live at `blyg.newschematic.org`, newest first. Add a row each time
you ship (see "Upgrading" in [DEPLOY.md](DEPLOY.md)). The commit is the one
the binary was built from, and the SHA-256 is of the file installed at
`/usr/local/bin/blygd`, so `sha256sum` on the server says whether it is
this one. Migrations are the schema version the database was at after the
restart (`blygd migrate status`).

| Date (UTC) | Commit | Go / x/net | SHA-256 (first 16) | Migrations | Notes |
|---|---|---|---|---|---|
| 2026-11-18 | `1362c5b` | go1.26.11 / v0.62.0 | `d0217f72338b4907` | 010 | Invites (Track D, migration 010): `blygd invite` mints single-use, expiring codes, each printed beside a `/studio/#invite/<code>` link; `POST /api/invites/redeem`, the one tokenless `/api/` operation, makes the user, origin and owner token in the transaction that spends the code, charged to the address's budgets as a wrong token is. "Have an invite?" on the studio sign-in and host home pages. No open sign-up. |
| 2026-11-17 | `9cc3d3f` | go1.26.11 / v0.62.0 | `8fc89b90eed3468e` | 009 | Opening to testers (Tracks A, B, C, E, F): tenants proved isolated, responses and origin settings in the studio, users mounted under `~`, CORS on `/api/`, queue depths, suspension and a backup ping. Spec revisions 7–16 (migrations 008, 009): kind changes recorded, public hoppers named in the manifest with their feeds, math never passed through. go1.26.11, for GO-2026-6859. |
| 2026-10-30 | `185627e` | go1.26.10 / v0.62.0 | `ab3339f0f0e71ace` | 007 | Gaps a second client found (migration 007): conditional draft edits (`draft_rev`, 409 `stale`), `item` and `subscribe` events, mark-all-read as of a feed's `as_of`, subscription imports from OPML or a blogroll, draft preview, `Subscription.author`, schemas for imported documents, and every error code described. API reference page at `/studio/api/`. Preflight on a copy of production first; origin rebuilt. |
| 2026-10-29 | `2697b25` | go1.26.10 / v0.62.0 | `4de71a12e1f4ca3f` | 006 | Protocol 0.4 (migrations 003–006): mention relations and classes, relocated manifests and pins, `links[]`, labels. Architecture-review hardening: error kinds, lease-bound claims, draining shutdown, plane checks. First deploy through `scripts/deploy.sh`, with a preflight on a copy of production; origin rebuilt. |
| 2026-10-28 | `b683c83` | go1.26.10 / v0.62.0 | `0e956dc3830fd53a` | 002 | First public deploy. IPv6 fetch-guard ranges, `/api/me` origin docs, vuln patches (GO-2026-6860, GO-2026-6809). |
