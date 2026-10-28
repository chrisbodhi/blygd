-- blygd migration 006: links[] (§10.3a, decision #79).
--
-- Each resolved [[id]] in a version produces a links[] entry: the target's
-- id, and its origin when remote. It is provenance, part of the version
-- like transclusions and generated, so the version guard is extended to
-- cover it: this tightens the trigger and relaxes nothing. A withdrawal
-- endcap omits it (§9).

ALTER TABLE item_versions ADD COLUMN IF NOT EXISTS links jsonb;

ALTER TABLE item_versions DROP CONSTRAINT IF EXISTS item_versions_endcap_no_links;
ALTER TABLE item_versions ADD CONSTRAINT item_versions_endcap_no_links
  CHECK (kind <> 'withdrawn' OR links IS NULL);

CREATE OR REPLACE FUNCTION blygd_item_versions_guard() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'published versions are permanent (item %, v%)', OLD.item_id, OLD.version;
  END IF;
  IF NEW.item_id <> OLD.item_id OR NEW.version <> OLD.version OR NEW.kind <> OLD.kind
     OR NEW.at <> OLD.at OR NEW.note IS DISTINCT FROM OLD.note
     OR NEW.content_md <> OLD.content_md
     OR NEW.content_html <> OLD.content_html OR NEW.content_hash <> OLD.content_hash
     -- Provenance is part of the version (§10.3, §10.3a, §10.6 rule 4, §5.7 rule 5).
     OR NEW.note_generated <> OLD.note_generated OR NEW.author IS DISTINCT FROM OLD.author
     OR NEW.transclusions IS DISTINCT FROM OLD.transclusions OR NEW.stub_of IS DISTINCT FROM OLD.stub_of
     OR NEW.generated IS DISTINCT FROM OLD.generated OR NEW.links IS DISTINCT FROM OLD.links THEN
    RAISE EXCEPTION 'published versions are immutable (item %, v%)', OLD.item_id, OLD.version;
  END IF;
  -- §8 rule 1: a pin is irrevocable.
  IF OLD.pinned_at IS NOT NULL AND (NEW.pinned_at IS DISTINCT FROM OLD.pinned_at) THEN
    RAISE EXCEPTION 'pins are irrevocable (item %, v%)', OLD.item_id, OLD.version;
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;
