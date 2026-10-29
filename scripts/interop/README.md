# Interop with the reference client

Two checks against [blygger-studio](https://github.com/blygger/blygger-studio),
the reference client. Rerun both whenever the spec or the reference moves.

Both need a built reference checkout:

```sh
git clone https://github.com/blygger/blygger-studio
cd blygger-studio && npm ci && npm run build
```

Both run the reference on your `node`, and refuse one without the Brotli
decoder fix (CVE-2026-111497): 26.12.1, 24.22.1, 22.23.4 or later in their
line (`scripts/check-node.sh`).

npm 11 blocks dependency install scripts by default. The reference needs
two of them, esbuild and workerd (the Workers runtime), which download
native binaries: `npm install-scripts approve esbuild workerd && npm rebuild esbuild workerd`.

## Differential: `make parity REFERENCE=…`

`internal/blyg/parity_test.go` feeds the same inputs to the reference's
TypeScript and to our Go, and compares the outputs: §10.2 text content,
quote normalization, partial location, and §5.6 rule 6 fork flattening.
It takes a second. Deliberate differences are listed in the corpus with
their reason and are reported, not failed.

## Live: `make interop REFERENCE=…`

`run.sh` starts both studios on localhost, blygd on :18090 and the
reference under Miniflare on :18787 (cron control on :18788). Each
subscribes to the other, and `check.py` runs seven scenarios:

1. blygd imports the reference: hashes, retained pins, `page`.
2. blygd quotes the reference (partial, whole, nested, `[[id]]`); the
   reference verifies the mentions.
3. The reference imports, quotes and stubs blygd, including a quote
   cycle; blygd verifies.
4. Each side forks the same pinned threads; the flattened drafts must be
   byte-identical, and fork mentions verify both ways.
5. Withdrawal: mentions go `gone`, imports roll to null, bakes stay, and
   a withdrawn import cannot be quoted.
6. A relocated surface (§6.1). Neither studio publishes one, so
   `check.py` serves a static third origin, Sol, on :18789. Its manifest is
   found through a `rel="blyg"` link (§12.1 step 4), and its index, feed,
   item documents and pins are all off their default paths. Its item has
   no page. Both studios import it, quote it and fork its pin. The forks
   must be byte-identical. Every mention must reach Sol's endpoint naming
   the item document (#85), and nothing may ask for a default path. A
   second origin, Luna, on :18790, is the same with no `blyg.json` at all:
   both studios read it, and blygd refuses to fork it (see DESIGN.md,
   departures).
7. Labels, `links[]` and remote generation sources, published by blygd
   (§10.1, §10.3a, §5.7 rules 8 and 9). The reference verifies the
   `source` mention and the labelled quote. blygd verifies the
   reference's labelled quote.

A **gap** is a check of something the spec asks of the reference that it
does not do yet: verifying a `link` mention as a notice, emitting
`links[]`, and baking `data-blyg-label`. Gaps are printed and counted but
never fail the run. When the reference catches up they print PASS, and
then they should become checks.

It takes about two minutes, most of it the reference's 60 s
cooldown on re-sent mention pairs. It needs node, python3, go, and a
Postgres 18 whose user may `CREATE DATABASE` (set `PSQL` and
`BLYGD_PG_BASE` if yours differ). It creates and drops its own database,
and leaves nothing running.

The run's waits are a few minutes long in all, so a machine that sleeps
partway fails it: on a Mac, run it as `caffeinate -i make interop …`,
with the lid open or on power. `INTEROP_LOGS=dir` keeps both studios'
full logs there; a failed run otherwise shows only their last lines.

Two settings exist for this run only: `BLYGD_ALLOW_PRIVATE_FETCH=true`
(both studios are on 127.0.0.1) and `BLYGD_MANUAL_POLL_FLOOR=2`. Neither
belongs in a deployment.
