-- blygd migration 004: where a remote origin's manifest was found.
--
-- At 0.4 the identity is the manifest's final URL minus its last path
-- segment (§12.2), and the manifest need not be named blyg.json when a
-- rel="blyg" link or a feed's <blyg:manifest> names it directly (§12.1
-- steps 3 and 4). The poller re-reads it where resolution found it. NULL
-- means {identity}blyg.json, which is where every row resolved before this
-- migration is assumed to have come from.

ALTER TABLE remote_origins ADD COLUMN IF NOT EXISTS manifest_url text;
