# Deploys

What is live at `blyg.newschematic.org`, newest first. Add a row each time
you ship (see "Upgrading" in [DEPLOY.md](DEPLOY.md)). The commit is the one
the binary was built from, and the SHA-256 is of the file installed at
`/usr/local/bin/blygd`, so `sha256sum` on the server says whether it is
this one. Migrations are the schema version the database was at after the
restart (`blygd migrate status`).

| Date (UTC) | Commit | Go / x/net | SHA-256 (first 16) | Migrations | Notes |
|---|---|---|---|---|---|
| 2026-10-28 | `b683c83` | go1.26.10 / v0.62.0 | `0e956dc3830fd53a` | 002 | First public deploy. IPv6 fetch-guard ranges, `/api/me` origin docs, vuln patches (GO-2026-6860, GO-2026-6809). |
