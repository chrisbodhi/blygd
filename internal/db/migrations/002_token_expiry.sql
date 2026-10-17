-- blygd migration 002: tokens may expire.
--
-- A token with expires_at authenticates only before it. NULL never expires:
-- existing tokens, and the owner token `blygd user add` mints, which is the
-- root credential and belongs offline. A minted token never outlives the
-- token that minted it (account.Mint).

ALTER TABLE tokens ADD COLUMN IF NOT EXISTS expires_at timestamptz;
ALTER TABLE tokens DROP CONSTRAINT IF EXISTS tokens_expiry_after_creation;
ALTER TABLE tokens ADD CONSTRAINT tokens_expiry_after_creation CHECK (expires_at IS NULL OR expires_at > created_at);
