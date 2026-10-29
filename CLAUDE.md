# CLAUDE.md — working on blygd

blygd is a Blygger studio: publisher, reader, hoppers and Webmentions in
one Go binary over Postgres. Read `docs/DESIGN.md` before changing
behaviour and `docs/PLAN.md` before starting a phase.

## The spec is the authority

The normative text is `blygger/blygger-spec` `docs/protocol-v0.4.md`.
Cite sections (§) in code comments where a rule comes from the spec. When
the spec and this code disagree, the spec wins, unless DESIGN.md lists the
difference as a deliberate departure. The spec moves often, so read the
current text rather than relying on memory or on these docs.

## Commands

```sh
make test          # unit tests; database tests skip without a database
make test-all      # everything, against a local Postgres 18
make vuln          # govulncheck: known vulnerabilities on paths blygd calls
make run           # serve on :8080 against BLYGD_DATABASE_URL
make parity REFERENCE=path/to/blygger-studio   # differential tests vs the reference
make interop REFERENCE=path/to/blygger-studio  # live: both studios, each subscribed to the other

Rerun both when the spec or the reference moves (scripts/interop/README.md).
```

Database tests need `BLYGD_TEST_DATABASE_URL` pointing at a Postgres 18
database whose user may `CREATE DATABASE`. `make test-all` defaults it to
`postgres://localhost:5432/postgres` as the current OS user; override it
on the command line or in the environment. Each test gets a fresh database.

## Rules of the house

- **Plane separation.** Nothing that writes `surface_files` may query the
  import mirror (`remote_origins`, `imported_items`, `imported_pins`).
  That is how imported items are never re-emitted (§13.5). The one
  sanctioned path from the mirror into plane 1 is `studio.Imports`,
  implemented by `reader.Snapshots`: a read-only lookup, scoped to the
  publisher's own subscriptions and called before the publish
  transaction, whose results are only ever baked as a §10.2 snapshot, an
  `[[id]]` anchor, or a frozen `cited`. Do not widen it. The reverse
  holds too: `internal/reader` never queries plane-1 tables, and reads a
  same-instance origin through its rendered surface. `internal/mention`
  reads the mirror only in `importedPage` and never writes plane 1. A
  planes test in each package checks its SQL, and `internal/archtest`
  allow-lists who may import whom, so a new edge between packages fails
  until its row says why.
- **Render inside the transaction.** Any change to plane 1 re-renders its
  surface files in the same transaction. The HTTP layer never renders.
- **Permanent things stay permanent.** Never weaken the schema's triggers
  (`internal/db/migrations/`). If a change needs one relaxed, stop and ask.
- **No network inside a transaction.** Fetch first, then open the
  transaction (see `Publish` and its fork-pin check). Every transaction
  starts in `tx.Run`, which marks its context, and `internal/fetch`
  refuses a marked one. Name the closure's context parameter `ctx`, so the
  unmarked one outside is shadowed.
- **All outbound HTTP goes through `internal/fetch`.** Never use
  `http.DefaultClient` for a URL someone else chose. `internal/archtest`
  fails on any other client, transport or dial, and on a transaction
  started anywhere but `tx.Run`.
- **Store verbatim, sanitize at render** for anything imported (§14).
  That includes our own thread pages: a bake carries another origin's
  bytes, so pages go through `blyg.PageHTML`, never raw `template.HTML`.
- Every new invariant gets a test, and the test should fail when the rule
  is broken: try it.
- **Errors say their kind.** A domain error the caller should see is an
  `apperr` kind (or `studio.Error`, which has one). Handlers return it as
  is, and `handleErr` picks the status. Never map a status from an
  error's text, or treat "not a database error" as the caller's fault:
  anything unkinded is a 500.
- **The API is described.** Any change to an `/api/` route, its scope, or
  a request or response shape edits `internal/web/openapi.json` in the
  same change. `openapi_test.go` and the e2e suite fail until it does.
- **Migrations are append-only.** A schema change is a new numbered file
  in `internal/db/migrations/` (tern, run at startup or by
  `blygd migrate`). Never edit one that has shipped: databases already
  ran it. Record each new file in `internal/db/migrations.sum`
  (`shasum -a 256` run inside `migrations/`), or the test fails. No `{{` in a
  migration, because tern parses them as Go templates.

## Layout

```
cmd/blygd            CLI: serve, user add, token mint, rebuild
internal/blyg        protocol: wire types, ids, hashing, markdown, feed, pages,
                     thread grammar and composition, fork flattening (pure)
internal/db          pool + embedded tern migrations (migrations/, migrations.sum)
internal/studio      plane 1: item lifecycle, pins, outbox, render-on-publish
internal/mention     §15: receiver, verifier, sender, lease workers
internal/reader      §12–13: resolution, importer, poller, reading overlay
internal/fetch       bounded, SSRF-guarded HTTP client; answers this instance in-process; refuses to run inside a transaction
internal/tx          starts every transaction, and marks its context
internal/apperr      what kind of failure an error is; internal/web maps kinds to statuses in one place
internal/surface     locates another origin's surface from its manifest (§6.1)
internal/account     users, scoped tokens
internal/hopper      hopper CRUD over references; public hopper view (§13.5, live)
internal/web         HTTP: origin surface, /api, /studio (embedded plain client), router; end-to-end tests
internal/testdb      per-test databases
internal/testblyg    a fake static blyg for import tests
internal/archtest    source-reading tests of the house rules: import graph, SQL by plane, HTTP, transactions
```
