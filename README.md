# blygd

A [Blygger](https://github.com/blygger/blygger-spec) studio as one Go
binary over Postgres: it publishes your blyg, reads everyone else's,
keeps your hoppers private, and sends and verifies Webmentions. It is
headless — clients talk to it over a scoped-token API — and
multi-tenant, with each user owning one path-mounted origin.

Status: Phases 1 and 2 of [docs/PLAN.md](docs/PLAN.md). Fragments, pins,
withdrawal, the full static surface, Webmentions in both directions,
hoppers, and the reader (subscriptions, the import mirror, a merged
reading feed, the blogroll) work. Threads (Phase 3) are next.

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
| `POST /api/hoppers/{hid}/members` `{own_item_id}`, `{imported: {origin, id}}` or `{imported: {subscription_id, id}}` | `hoppers:write` |
| `DELETE /api/hoppers/{hid}/members/{mid}` | `hoppers:write` |
| `GET /api/mentions?status=…`, `GET /api/outbound` | `mentions:read` |
| `GET` / `POST /api/tokens`, `DELETE /api/tokens/{tid}` | `tokens:write` |
| `GET /api/subscriptions`, `GET /api/subscriptions/{sid}` | `reader:read` |
| `POST /api/subscriptions` `{url}` (resolves per §12.1; 422 with the trail when nothing resolves) | `reader:write` |
| `PATCH /api/subscriptions/{sid}` `{display_title?, in_blogroll?}` | `reader:write`, plus `origin:write` for `in_blogroll` |
| `DELETE /api/subscriptions/{sid}` | `reader:write` |
| `POST /api/subscriptions/{sid}/resolve` `{url?}` (reports, changes nothing) | `reader:read` |
| `POST /api/subscriptions/{sid}/move` `{to}` (the confirmed identity change, §12.2) | `reader:write` |
| `GET /api/reader/feed?limit=&before=&subscription=&unread=true` | `reader:read` |
| `GET /api/reader/items/{sid}/{item_id}` | `reader:read` |
| `PUT` / `DELETE /api/reader/items/{sid}/{item_id}/read` | `reader:write` |

Imported content comes back verbatim, exactly as the origin served it.
**Clients must sanitize or sandbox it before rendering** (§14); every
reading response says so in a `notice` field.

Subscribe to another blyg:

```sh
curl -s -H "Authorization: Bearer $T" localhost:8080/api/subscriptions \
     -d '{"url": "https://example.com/blyg/"}'
curl -s -H "Authorization: Bearer $T" localhost:8080/api/reader/feed
```

See [docs/DESIGN.md](docs/DESIGN.md) for how it works and why.
