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

Requirements: Go 1.24+ and Postgres 18.

```sh
go mod tidy    # once, to fill go.sum
createdb blygd # on a local Postgres 18, e.g. brew install postgresql@18

export BLYGD_DATABASE_URL='postgres://localhost:5432/blygd?sslmode=disable'
export BLYGD_PUBLIC_URL='http://localhost:8080/'

go run ./cmd/blygd user add chris "New Schematic"   # prints your origin and an owner token
go run ./cmd/blygd serve
```

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
| `BLYGD_TRUST_PROXY` | `false` | Honor `X-Forwarded-Proto` (set behind Caddy or nginx) |
| `BLYGD_FRAGMENT_CAP` | `2000` | Characters (§5.3) |
| `BLYGD_LEVEL` | `2` | Informative manifest claim (§3.2) |
| `BLYGD_GENERATOR_URL` | this repo | Manifest `generator_url` |
| `BLYGD_ALLOW_PRIVATE_FETCH` | `false` | Development only: lets fetches reach private addresses |

## API

The API is described by an OpenAPI 3.1 document, served without a token
at `GET /api/openapi.json` (source: `internal/web/openapi.json`). Every
other route takes `Authorization: Bearer <token>` and needs at most one
scope, named in the document's security requirement:

| Scope | Allows |
|---|---|
| `items:read` / `items:write` | reading and editing drafts and items |
| `items:publish` / `items:withdraw` / `items:pin` | the public acts, each separately; pins are irrevocable |
| `hoppers:read` / `hoppers:write` | private hoppers |
| `mentions:read` | inbound and outbound Webmentions |
| `reader:read` / `reader:write` | subscriptions, the reading feed, read marks |
| `origin:write` | origin settings, and publishing a blogroll entry |
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
