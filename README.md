# blygd

A [Blygger](https://github.com/blygger/blygger-spec) studio as one Go
binary over Postgres: it publishes your blyg, will read everyone else's,
keeps your hoppers private, and sends and verifies Webmentions. It is
headless — clients talk to it over a scoped-token API — and
multi-tenant, with each user owning one path-mounted origin.

Status: Phase 1 of [docs/PLAN.md](docs/PLAN.md). Fragments, pins,
withdrawal, the full static surface, Webmentions in both directions, and
hoppers over your own items work. The reader (Phase 2) and threads
(Phase 3) are next.

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

Every route takes `Authorization: Bearer <token>` and needs one scope.

| Route | Scope |
|---|---|
| `GET /api/me` | any |
| `GET /api/origin` / `PATCH /api/origin` | `items:read` / `origin:write` |
| `GET /api/items?state=draft\|published\|withdrawn` | `items:read` |
| `POST /api/items` `{kind?, content_md, forked_from?}` | `items:write` |
| `GET` / `PATCH` / `DELETE /api/items/{id}` | `items:read` / `items:write` / `items:write` (drafts only) |
| `POST /api/items/{id}/publish` `{note?}` | `items:publish` |
| `POST /api/items/{id}/withdraw` `{note?}` | `items:withdraw` |
| `POST /api/items/{id}/versions/{n}/pin` | `items:pin` (irrevocable) |
| `GET` / `POST /api/hoppers`, `GET` / `PATCH` / `DELETE /api/hoppers/{hid}` | `hoppers:read` / `hoppers:write` |
| `POST /api/hoppers/{hid}/members` `{own_item_id}` or `{imported: {origin, id}}` | `hoppers:write` |
| `DELETE /api/hoppers/{hid}/members/{mid}` | `hoppers:write` |
| `GET /api/mentions?status=…`, `GET /api/outbound` | `mentions:read` |
| `GET` / `POST /api/tokens`, `DELETE /api/tokens/{tid}` | `tokens:write` |

See [docs/DESIGN.md](docs/DESIGN.md) for how it works and why.
