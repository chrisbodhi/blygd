-- blygd migration 007: subscription imports.
--
-- A blogroll (§11) or another reader's OPML export becomes one row per
-- entry, resolved in the background (§12.1), so a long list neither holds
-- a request open nor spends the user's subscribe budget. Plane 3, the
-- private overlay: written and read only by internal/reader.

CREATE TABLE IF NOT EXISTS subscription_imports (
  id                bigserial PRIMARY KEY,
  user_id           bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  url               text NOT NULL,   -- the entry's xmlUrl, or its htmlUrl when it has none
  title             text,            -- the entry's text, as written: a display title (§11)
  status            text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'subscribed', 'failed')),
  remote_origin_id  bigint REFERENCES remote_origins(id),   -- the subscription, once made
  error             text,            -- the last failure
  attempts          int NOT NULL DEFAULT 0,
  next_attempt_at   timestamptz NOT NULL DEFAULT now(),     -- pushed forward while leased
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, url),
  CHECK ((status = 'subscribed') = (remote_origin_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS subscription_imports_due_idx ON subscription_imports (next_attempt_at) WHERE status = 'pending';
