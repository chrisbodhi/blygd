-- blygd migration 008: reader-side filters (DESIGN.md "Filters").
--
-- A reader names topics ("US politics", "spoilers for Severance S3") and
-- chooses what happens to posts about them. A classifier the operator
-- configured judges each imported item against them. None of this is on
-- the wire: AI is never in the protocol, and a reader needs no models or
-- keys (protocol invariant 3). Written and read only by internal/reader.
--
-- filters and label_overrides are plane 3, the private overlay.
-- item_labels and label_leases are derived from plane 2: answers about an
-- imported item's text, shared by every reader who asked the same question
-- in the same words, as the mirror itself is shared.

CREATE TABLE IF NOT EXISTS filters (
  id                bigserial PRIMARY KEY,
  user_id           bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name              text NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
  description       text NOT NULL DEFAULT '' CHECK (length(description) <= 500),
  -- classify.Topic.Hash: the normalized words and the prompt's version.
  -- Answers are keyed by it, so editing the words asks again.
  def_hash          text NOT NULL,
  -- What counts as a match: mentions (any mention) or mainly (the post is
  -- about it), at or above threshold. Decided when read, so a new
  -- threshold needs no new answer.
  match             text NOT NULL DEFAULT 'mainly' CHECK (match IN ('mentions', 'mainly')),
  threshold         double precision NOT NULL DEFAULT 0.5 CHECK (threshold > 0 AND threshold < 1),
  action            text NOT NULL DEFAULT 'fold' CHECK (action IN ('fold', 'hide')),
  -- Before an answer arrives: show the post, or hold it back for a while
  -- (spoilers: a spoiler shown and then folded has already spoiled).
  pending           text NOT NULL DEFAULT 'show' CHECK (pending IN ('show', 'hold')),
  -- Only this subscription's items; NULL for every subscription.
  remote_origin_id  bigint REFERENCES remote_origins(id),
  enabled           boolean NOT NULL DEFAULT true,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS filters_user_idx ON filters (user_id);
CREATE UNIQUE INDEX IF NOT EXISTS filters_unique_idx ON filters (user_id, def_hash, coalesce(remote_origin_id, 0));
CREATE INDEX IF NOT EXISTS filters_def_idx ON filters (def_hash) WHERE enabled;

-- One answer per item, question and model. version, content_hash and
-- input_hash say which text it is about: it applies only while the item
-- is still at that version and hash, so an edit (or a stealth edit,
-- §13.3) is never judged by an answer about other words. Deleted when the
-- item is withdrawn (§13.4).
CREATE TABLE IF NOT EXISTS item_labels (
  imported_item_id  bigint NOT NULL REFERENCES imported_items(id),
  def_hash          text NOT NULL,
  model             text NOT NULL,
  version           integer NOT NULL,
  content_hash      text,
  input_hash        text NOT NULL,      -- classify.Input.Hash: what was sent
  p_mentions        double precision NOT NULL CHECK (p_mentions BETWEEN 0 AND 1),
  p_mainly          double precision NOT NULL CHECK (p_mainly BETWEEN 0 AND 1),
  score             double precision NOT NULL,
  confidence        double precision NOT NULL,
  probabilities     jsonb NOT NULL,     -- each level's probability, lowest first
  classified_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (imported_item_id, def_hash, model)
);

-- The worker's claim on an item: one request covers every question asked
-- of it. A row is per item, for the version and hash it was leased at;
-- an item that moves on is due again at once.
CREATE TABLE IF NOT EXISTS label_leases (
  imported_item_id  bigint PRIMARY KEY REFERENCES imported_items(id),
  version           integer NOT NULL,
  content_hash      text,
  attempts          int NOT NULL DEFAULT 0,
  next_attempt_at   timestamptz NOT NULL DEFAULT now(),
  failed            boolean NOT NULL DEFAULT false,   -- this version will not classify; shown unclassified
  error             text,
  updated_at        timestamptz NOT NULL DEFAULT now()
);

-- A reader's own judgment of an item, which wins over the classifier's.
CREATE TABLE IF NOT EXISTS label_overrides (
  filter_id         bigint NOT NULL REFERENCES filters(id) ON DELETE CASCADE,
  imported_item_id  bigint NOT NULL REFERENCES imported_items(id),
  matched           boolean NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (filter_id, imported_item_id)
);

-- Requests per UTC day, so a runaway worker meets a ceiling
-- (BLYGD_SUBQ_DAILY_CALLS). No prompt or answer is kept here.
CREATE TABLE IF NOT EXISTS classifier_usage (
  day               date PRIMARY KEY,
  calls             int NOT NULL DEFAULT 0,
  input_tokens      bigint NOT NULL DEFAULT 0
);
