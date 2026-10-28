# blygd

A [Blygger](https://github.com/blygger/blygger-spec) studio as one Go
binary over Postgres: it publishes your blyg, reads everyone else's,
keeps your hoppers private, and sends and verifies Webmentions. It is
headless — clients talk to it over a scoped-token API — and
multi-tenant, with each user owning one path-mounted origin.

Status: Phases 1–3 of [docs/PLAN.md](docs/PLAN.md). Fragments, pins,
withdrawal, the full static surface, Webmentions in both directions,
hoppers, the reader (subscriptions, the import mirror, a merged reading
feed, the blogroll), and threads (transclusion across origins, partial
quotes, `[[id]]` links, stubs, generation provenance, fork flattening)
work. Media, public hoppers and custom domains (Phase 4) are next.

## Run it

To run it on a server, see [docs/DEPLOY.md](docs/DEPLOY.md): one small VM
with systemd, Caddy and Postgres 18, and nightly offsite backups.

Requirements: Go 1.26+ and Postgres 18.

```sh
go mod tidy    # once, to fill go.sum
createdb blygd # on a local Postgres 18, e.g. brew install postgresql@18

export BLYGD_DATABASE_URL='postgres://localhost:5432/blygd?sslmode=disable'
export BLYGD_PUBLIC_URL='http://localhost:8080/'

go run ./cmd/blygd user add chris "New Schematic"   # prints your origin and an owner token
go run ./cmd/blygd serve
```

Then open `/studio/` in a browser (on the same host, e.g.
`http://localhost:8080/studio/`) and paste the owner token. It is a plain,
phone-friendly client for writing, publishing, reading and handling
mentions. The studio trades the owner token at once for a 30-day token of
its own, with only the scopes it uses, and forgets the one you pasted:
keep the owner token somewhere safe, such as a password manager, to sign
in again. Other tools get their own tokens:
`blygd token mint chris my-tool --expires 30d items:read`.

Every command applies pending schema migrations first (tern, embedded in
the binary). `blygd migrate status` reports where a database stands, and
`blygd migrate` applies migrations explicitly. Set
`BLYGD_MIGRATE_ON_START=false` to make that the only way. A database
created before migrations existed (before 2026-10-10) is refused: drop
and recreate it once.

Publish a fragment:

```sh
T='blygd_…'   # the owner token
curl -s -H "Authorization: Bearer $T" localhost:8080/api/items \
     -d '{"content_md": "Hello, *network*."}'                      # → {"id": …}
curl -s -H "Authorization: Bearer $T" -X POST localhost:8080/api/items/$ID/publish \
     -d '{"note": "first"}'
curl -s localhost:8080/chris/items/$ID.json
```

## Configuration

| Variable | Default | |
|---|---|---|
| `BLYGD_DATABASE_URL` | (required) | Postgres connection URL |
| `BLYGD_PUBLIC_URL` | (required) | Where origins mount: `https://host/` gives `https://host/{handle}/` |
| `BLYGD_LISTEN` | `:8080` | |
| `BLYGD_TRUST_PROXY` | `false` | Honor `X-Forwarded-Proto`, and charge budgets to the last `X-Forwarded-For` address. Set it only behind Caddy or nginx, never when clients reach blygd directly |
| `BLYGD_FRAGMENT_CAP` | `2000` | Characters (§5.3) |
| `BLYGD_LEVEL` | `2` | Informative manifest claim (§3.2) |
| `BLYGD_MIGRATE_ON_START` | `true` | Apply pending migrations before any command; `false` leaves it to `blygd migrate` |
| `BLYGD_MANUAL_POLL_FLOOR` | `60` | Seconds between user-asked polls of one origin; lower only for development |
| `BLYGD_MEDIA_MAX_BYTES` | `10485760` | Largest accepted upload (PNG, JPEG, GIF, WebP) |
| `BLYGD_GENERATOR_URL` | this repo | Manifest `generator_url` |
| `BLYGD_ALLOW_PRIVATE_FETCH` | `false` | Development only: lets fetches reach private addresses |
| `BLYGD_RATE_LIMITS` | `true` | Request budgets (docs/DESIGN.md); `false` for test harnesses only |

## API

The API is described by an OpenAPI 3.1 document, served without a token
at `GET /api/openapi.json` (source: `internal/web/openapi.json`). Every
other route takes `Authorization: Bearer <token>` and needs at most one
scope, named in the document's security requirement:

| Scope | Allows |
|---|---|
| `items:read` / `items:write` | reading and editing drafts and items, uploading media |
| `items:publish` / `items:withdraw` / `items:pin` | the public acts, each separately; pins are irrevocable |
| `hoppers:read` / `hoppers:write` | private hoppers |
| `mentions:read` / `mentions:write` | inbound and outbound Webmentions; hiding responses and blocking sources (showing one also needs `origin:write`) |
| `reader:read` / `reader:write` | subscriptions, the reading feed, read marks |
| `origin:write` | origin settings, publishing a blogroll entry, making a hopper public |
| `tokens:write` | listing, minting (a subset of your own scopes) and revoking tokens |
| `*` | everything: the owner token |

A publish that cannot resolve every `![[id]]` and `[[id]]` (§10.2)
returns `422` with a `problems` list naming each one.

Imported content comes back verbatim, exactly as the origin served it.
**Clients must sanitize or sandbox it before rendering** (§14); every
reading response says so in a `notice` field.

Subscribe to another blyg:

```sh
curl -s -H "Authorization: Bearer $T" localhost:8080/api/subscriptions \
     -d '{"url": "https://example.com/blyg/"}'
curl -s -H "Authorization: Bearer $T" localhost:8080/api/reader/feed
```

Quote it in a thread. A `>` block straight after the directive quotes
part of it:

```sh
curl -s -H "Authorization: Bearer $T" localhost:8080/api/items \
     -d '{"kind": "thread", "content_md": "Worth reading:\n\n![[<item id>]]"}'
```

See [docs/DESIGN.md](docs/DESIGN.md) for how it works and why.
