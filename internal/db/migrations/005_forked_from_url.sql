-- blygd migration 005: where a fork's remote source is pinned.
--
-- A fork's page links to the pinned version it descends from (§5.6 rule 7).
-- At 0.4 another origin's `pin` template may put that file anywhere
-- (§4, §6.1), and only its manifest says where. Rendering runs inside
-- transactions and must work offline (rebuild), so the URL is found before
-- the first publish, by the same fetch that checks the pin (§5.6 rule 2),
-- and kept here. NULL means it was never confirmed, and pages fall back to
-- the default path. The studio writes it once, at first publish.

ALTER TABLE items ADD COLUMN IF NOT EXISTS forked_from_url text;
