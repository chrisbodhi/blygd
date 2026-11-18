-- blygd migration 010: invites (docs/TODO.md, Track D).
--
-- An operator mints codes (`blygd invite`); someone holding one makes
-- their own user and origin (POST /api/invites/redeem), with no token.
-- Only the SHA-256 of a code is stored, as for tokens: the code is shown
-- once, at mint time. A code makes one user, in the transaction that
-- makes them, so redeemed_at and redeemed_by are set together or not at
-- all. Users are never deleted (their origins are permanent), so
-- redeemed_by needs no ON DELETE.

CREATE TABLE IF NOT EXISTS invites (
  id           bigserial PRIMARY KEY,
  code_hash    bytea NOT NULL UNIQUE,
  note         text NOT NULL DEFAULT '' CHECK (length(note) <= 200),
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  redeemed_at  timestamptz,
  redeemed_by  bigint UNIQUE REFERENCES users(id),
  CHECK ((redeemed_at IS NULL) = (redeemed_by IS NULL))
);
