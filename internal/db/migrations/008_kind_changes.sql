-- blygd migration 008: changes of kind seen in imports (§13.3).
--
-- An item's kind is fixed at its first publication (§5.3, decision #100):
-- a fragment is never republished as a thread, nor a thread as a
-- fragment, withdrawal aside. A reader that sees one SHOULD record and
-- surface it, and a client quoting such an item SHOULD tell the author
-- before baking it, since a former fragment can now carry third parties'
-- words into the quoter's bake. The change is adopted, like a stealth edit
-- (§13.3), and kept here once seen, so a publish can warn. Plane 2: written
-- only by the importer, read by reader.Snapshots. Nothing is relaxed.

ALTER TABLE imported_items ADD COLUMN IF NOT EXISTS kind_changed text;
