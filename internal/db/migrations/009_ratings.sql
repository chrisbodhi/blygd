-- blygd migration 009: a reader's ratings tune their filters
-- (DESIGN.md "Filters", ratings).
--
-- A rating is the reader's own judgment of an item under one of their
-- filters (label_overrides, 008). It now also keeps what the classifier
-- said when the reader rated, about which question and from which model,
-- so ratings can pick the filter's threshold without asking anything
-- again. A rating made before any answer arrived keeps no probabilities:
-- it still decides its item, and tunes nothing. Plane 3, the private
-- overlay, as before: written and read only by internal/reader.

ALTER TABLE label_overrides
  ADD COLUMN IF NOT EXISTS def_hash   text,               -- the filter's question when rated
  ADD COLUMN IF NOT EXISTS model      text,               -- the model whose answer was rated
  ADD COLUMN IF NOT EXISTS p_mentions double precision CHECK (p_mentions BETWEEN 0 AND 1),
  ADD COLUMN IF NOT EXISTS p_mainly   double precision CHECK (p_mainly BETWEEN 0 AND 1);

-- auto_tune: each rating may move the threshold to the one that agrees
-- with the most of the reader's ratings. Setting a threshold by hand turns
-- it off. tuned_at: when ratings last moved it.
ALTER TABLE filters
  ADD COLUMN IF NOT EXISTS auto_tune boolean NOT NULL DEFAULT true,
  ADD COLUMN IF NOT EXISTS tuned_at  timestamptz;
