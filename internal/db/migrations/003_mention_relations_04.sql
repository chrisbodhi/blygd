-- blygd migration 003: protocol 0.4's mention relations and classes.
--
-- Two relations join stub, transclusion and fork, one per wire construct
-- that names a target (§15.4 step 4): source, a remote generation source
-- (§5.7 rule 9), and link, a disclosed [[id]] link (§10.3a, decision #79).
-- Outbound rows may carry them too (§15.2).
--
-- A verified mention is a response or a notice (§15.5, decisions #79 and
-- #80): a link, or a stub or transclusion from a document with no words of
-- its own (a repost), is a notice. The receiver derives the class; class is
-- set exactly when relation is. Notices never appear among responses, so
-- one can never be approved for display. Rows verified before 0.4 are
-- responses: their relations predate both kinds of notice, though a
-- re-verification may reclassify a repost.

ALTER TABLE inbound_mentions DROP CONSTRAINT IF EXISTS inbound_mentions_relation_check;
ALTER TABLE inbound_mentions ADD CONSTRAINT inbound_mentions_relation_check
  CHECK (relation IN ('stub', 'transclusion', 'fork', 'source', 'link'));

ALTER TABLE inbound_mentions ADD COLUMN IF NOT EXISTS class text;
UPDATE inbound_mentions SET class = 'response' WHERE relation IS NOT NULL AND class IS NULL;
ALTER TABLE inbound_mentions DROP CONSTRAINT IF EXISTS inbound_mentions_class_check;
ALTER TABLE inbound_mentions ADD CONSTRAINT inbound_mentions_class_check
  CHECK (class IN ('response', 'notice') AND (relation <> 'link' OR class = 'notice'));
ALTER TABLE inbound_mentions DROP CONSTRAINT IF EXISTS inbound_mentions_class_with_relation;
ALTER TABLE inbound_mentions ADD CONSTRAINT inbound_mentions_class_with_relation
  CHECK ((relation IS NULL) = (class IS NULL));
ALTER TABLE inbound_mentions DROP CONSTRAINT IF EXISTS inbound_mentions_notices_unshown;
ALTER TABLE inbound_mentions ADD CONSTRAINT inbound_mentions_notices_unshown
  CHECK (approved_at IS NULL OR class = 'response');

ALTER TABLE outbound_mentions DROP CONSTRAINT IF EXISTS outbound_mentions_relation_check;
ALTER TABLE outbound_mentions ADD CONSTRAINT outbound_mentions_relation_check
  CHECK (relation IN ('stub', 'transclusion', 'fork', 'source', 'link', 'url'));
