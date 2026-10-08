# CLAUDE.md — working on blygd

blygd is a Blygger studio: publisher, reader, hoppers and Webmentions in
one Go binary over Postgres. Read `docs/DESIGN.md` before changing
behaviour and `docs/PLAN.md` before starting a phase.

## The spec is the authority

The normative text is `blygger/blygger-spec` `docs/protocol-v0.3.md`.
Cite sections (§) in code comments where a rule comes from the spec. When
the spec and this code disagree, the spec wins, unless DESIGN.md lists the
difference as a deliberate departure. The spec moves often, so read the
current text rather than relying on memory or on these docs.

## Commands

```sh
make test          # unit tests; database tests skip without a database
make test-all      # everything, against a local Postgres 18
make run           # serve on :8080 against BLYGD_DATABASE_URL
```

Database tests need `BLYGD_TEST_DATABASE_URL` pointing at a Postgres 18
database whose user may `CREATE DATABASE`. `make test-all` defaults it to
`postgres://localhost:5432/postgres` as the current OS user; override it
on the command line or in the environment. Each test gets a fresh database.

## Rules of the house

- **Plane separation.** Nothing that writes `surface_files` may read the
  import mirror (`remote_origins`, `imported_items`, `imported_pins`).
  That is how imported items are never re-emitted (§13.5).
- **Render inside the transaction.** Any change to plane 1 re-renders its
  surface files in the same transaction. The HTTP layer never renders.
- **Permanent things stay permanent.** Never weaken the triggers in
  `schema.sql`. If a change needs one relaxed, stop and ask.
- **No network inside a transaction.** Fetch first, then open the
  transaction (see `Publish` and its fork-pin check).
- **All outbound HTTP goes through `internal/fetch`.** Never use
  `http.DefaultClient` for a URL someone else chose.
- **Store verbatim, sanitize at render** for anything imported (§14).
- Every new invariant gets a test, and the test should fail when the rule
  is broken: try it.
- No migrations yet. Schema changes edit `schema.sql` and assume a fresh
  dev database (PLAN.md, Operations).

## Layout

```
cmd/blygd            CLI: serve, user add, token mint, rebuild
internal/blyg        protocol: wire types, ids, hashing, markdown, feed, pages (pure)
internal/db          pool + embedded schema.sql
internal/studio      plane 1: item lifecycle, pins, outbox, render-on-publish
internal/mention     §15: receiver, verifier, sender, lease workers
internal/fetch       bounded, SSRF-guarded HTTP client
internal/account     users, scoped tokens
internal/hopper      hopper CRUD over references
internal/web         HTTP: origin surface, /api, router; end-to-end tests
internal/testdb      per-test databases
```
