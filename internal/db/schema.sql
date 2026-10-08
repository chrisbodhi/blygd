-- blygd schema — Postgres 18.
--
-- Applied idempotently at startup. There is deliberately no migration tool
-- yet: while blygd is pre-release, changing a table means recreating the dev
-- database. See docs/PLAN.md ("Migrations").
--
-- Three planes (docs/DESIGN.md):
--   1. own origins   — what this instance publishes (authoritative)
--   2. import mirror — other origins' items, one copy per origin, shared
--   3. overlay       — per-user, private: subscriptions, hoppers, reads
-- plus mentions (inbound, outbound) and the rendered public surface.
--
-- Spec references are to blygger-spec docs/protocol-v0.3.md.

-- ───────────────────────── accounts ─────────────────────────

CREATE TABLE IF NOT EXISTS users (
  id          bigserial PRIMARY KEY,
  handle      text NOT NULL UNIQUE CHECK (handle ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
  created_at  timestamptz NOT NULL DEFAULT now()
);

-- Per-tool, scoped, revocable credentials (§16.6). Only the SHA-256 of the
-- secret is stored; the secret is shown once, at mint time.
CREATE TABLE IF NOT EXISTS tokens (
  id            bigserial PRIMARY KEY,
  user_id       bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name          text NOT NULL,
  secret_hash   bytea NOT NULL UNIQUE,
  scopes        text[] NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_used_at  timestamptz,
  revoked_at    timestamptz
);

-- ───────────────────── plane 1: own origins ─────────────────────

CREATE TABLE IF NOT EXISTS origins (
  id               bigserial PRIMARY KEY,
  owner_id         bigint NOT NULL REFERENCES users(id),
  -- The identity origin (§2, §12.2): a full base URL, normalized, ending in
  -- '/'. Path-mounted today (https://host/{handle}/); custom domains later
  -- need no schema change because this is the whole URL.
  base_url         text NOT NULL UNIQUE CHECK (base_url ~ '^https?://[^/]+/(.*/)?$'),
  title            text NOT NULL DEFAULT '',
  description      text NOT NULL DEFAULT '',
  author           jsonb,   -- manifest publication identity (§6.1)
  byline           jsonb,   -- default per-item author (§5.5), opaque
  accept_mentions  boolean NOT NULL DEFAULT true,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS items (
  -- §5.1: 26 chars of lowercase Crockford base32.
  id                  text PRIMARY KEY CHECK (id ~ '^[0-9a-hjkmnp-tv-z]{26}$'),
  origin_id           bigint NOT NULL REFERENCES origins(id),
  authored_kind       text NOT NULL CHECK (authored_kind IN ('fragment', 'thread')),
  -- Studio-private authoring state. Never on the wire.
  draft_md            text NOT NULL DEFAULT '',
  draft_note          text,
  -- Latest published version; 0 means never published (a draft, which
  -- §4 requires to 404 and §9 lets us hard-delete).
  version             integer NOT NULL DEFAULT 0 CHECK (version >= 0),
  withdrawn           boolean NOT NULL DEFAULT false,
  -- §5.8: origin-relative permalink, fixed at first publish.
  page                text,
  -- §5.6: lineage, fixed at creation, immutable. A reference (§5.9).
  forked_from         jsonb,
  first_published_at  timestamptz,
  published_at        timestamptz,   -- latest publish event
  created_at          timestamptz NOT NULL DEFAULT now(),
  draft_updated_at    timestamptz NOT NULL DEFAULT now(),
  CHECK (version > 0 OR (NOT withdrawn AND page IS NULL))
);
CREATE INDEX IF NOT EXISTS items_origin_idx ON items (origin_id, published_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS items_page_idx ON items (origin_id, page) WHERE page IS NOT NULL;

-- Every published version, kept privately forever: §5.2 withholds unpinned
-- history from the wire, but §8 rule 2 allows pinning retroactively, so the
-- bytes must survive here.
CREATE TABLE IF NOT EXISTS item_versions (
  item_id         text NOT NULL REFERENCES items(id),
  version         integer NOT NULL CHECK (version >= 1),
  kind            text NOT NULL CHECK (kind IN ('fragment', 'thread', 'withdrawn')),
  at              timestamptz NOT NULL,
  note            text,
  note_generated  boolean NOT NULL DEFAULT false,
  content_md      text NOT NULL,
  content_html    text NOT NULL,
  content_hash    text NOT NULL CHECK (content_hash ~ '^sha256:[0-9a-f]{64}$'),
  author          jsonb,   -- as asserted at publish (§8 rule 6)
  transclusions   jsonb,   -- threads only (§10.3)
  stub_of         jsonb,   -- threads only (§10.6)
  generated       jsonb,   -- §5.7
  pinned_at       timestamptz,
  PRIMARY KEY (item_id, version),
  -- §8 rule 2: endcaps are never pinned.
  CHECK (kind <> 'withdrawn' OR pinned_at IS NULL),
  -- §9: an endcap says nothing.
  CHECK (kind <> 'withdrawn' OR (content_md = '' AND content_html = '' AND generated IS NULL AND stub_of IS NULL))
);

-- Invariants the protocol makes permanent are enforced here, not just in Go.
CREATE OR REPLACE FUNCTION blygd_item_versions_guard() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'published versions are permanent (item %, v%)', OLD.item_id, OLD.version;
  END IF;
  IF NEW.item_id <> OLD.item_id OR NEW.version <> OLD.version OR NEW.kind <> OLD.kind
     OR NEW.at <> OLD.at OR NEW.note IS DISTINCT FROM OLD.note
     OR NEW.content_md <> OLD.content_md
     OR NEW.content_html <> OLD.content_html OR NEW.content_hash <> OLD.content_hash THEN
    RAISE EXCEPTION 'published versions are immutable (item %, v%)', OLD.item_id, OLD.version;
  END IF;
  -- §8 rule 1: a pin is irrevocable.
  IF OLD.pinned_at IS NOT NULL AND (NEW.pinned_at IS DISTINCT FROM OLD.pinned_at) THEN
    RAISE EXCEPTION 'pins are irrevocable (item %, v%)', OLD.item_id, OLD.version;
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS item_versions_guard ON item_versions;
CREATE TRIGGER item_versions_guard BEFORE UPDATE OR DELETE ON item_versions
  FOR EACH ROW EXECUTE FUNCTION blygd_item_versions_guard();

CREATE OR REPLACE FUNCTION blygd_items_guard() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.version > 0 THEN
      RAISE EXCEPTION 'published items cannot be deleted, only withdrawn (item %)', OLD.id;
    END IF;
    RETURN OLD;
  END IF;
  IF NEW.id <> OLD.id OR NEW.origin_id <> OLD.origin_id OR NEW.authored_kind <> OLD.authored_kind THEN
    RAISE EXCEPTION 'item identity is permanent (item %)', OLD.id;
  END IF;
  -- §5.2: versions only go up, by exactly one.
  IF NEW.version <> OLD.version AND NEW.version <> OLD.version + 1 THEN
    RAISE EXCEPTION 'version must advance by exactly 1 (item %: % -> %)', OLD.id, OLD.version, NEW.version;
  END IF;
  -- §5.8 (SHOULD be stable): we make it MUST.
  IF OLD.page IS NOT NULL AND NEW.page IS DISTINCT FROM OLD.page THEN
    RAISE EXCEPTION 'page is fixed at first publish (item %)', OLD.id;
  END IF;
  -- §5.6 rule 3: lineage is immutable.
  IF NEW.forked_from IS DISTINCT FROM OLD.forked_from THEN
    RAISE EXCEPTION 'forked_from is immutable (item %)', OLD.id;
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS items_guard ON items;
CREATE TRIGGER items_guard BEFORE UPDATE OR DELETE ON items
  FOR EACH ROW EXECUTE FUNCTION blygd_items_guard();

-- §5.4: media objects are immutable; bytes referenced by a pin are kept
-- forever (§8 rule 4). Upload API is Phase 4.
CREATE TABLE IF NOT EXISTS media (
  origin_id   bigint NOT NULL REFERENCES origins(id),
  path        text NOT NULL CHECK (path ~ '^media/[0-9a-z]+\.[0-9a-z]+$'),
  mime        text NOT NULL,
  bytes       bytea NOT NULL,
  sha256      bytea NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (origin_id, path)
);

-- §11: the blogroll is a publishing act, so it lives in plane 1. Entries are
-- written when the owner opts a subscription in, with the display values
-- copied at that moment, so the renderer never reads the import mirror.
CREATE TABLE IF NOT EXISTS blogroll_entries (
  origin_id         bigint NOT NULL REFERENCES origins(id),
  remote_origin_id  bigint NOT NULL,   -- which subscription it came from; never joined by the renderer
  text              text NOT NULL,
  xml_url           text NOT NULL,
  html_url          text NOT NULL,
  added_at          timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (origin_id, remote_origin_id)
);

-- The rendered public surface (§4). Every publish writes the affected files
-- here once; the HTTP layer only ever serves these bytes. A dump of this
-- table per origin is a conformant static blyg.
CREATE TABLE IF NOT EXISTS surface_files (
  origin_id     bigint NOT NULL REFERENCES origins(id),
  path          text NOT NULL,   -- origin-relative; '' is the feed page
  content_type  text NOT NULL,
  body          bytea NOT NULL,
  etag          text NOT NULL,
  -- items/{id}.json and pins: 200 forever once written (§4, §8).
  permanent     boolean NOT NULL DEFAULT false,
  immutable     boolean NOT NULL DEFAULT false,   -- pins: bytes never change
  updated_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (origin_id, path)
);

CREATE OR REPLACE FUNCTION blygd_surface_guard() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.permanent THEN
      RAISE EXCEPTION 'surface file is permanent: %', OLD.path;
    END IF;
    RETURN OLD;
  END IF;
  IF OLD.permanent AND NOT NEW.permanent THEN
    RAISE EXCEPTION 'surface file is permanent: %', OLD.path;
  END IF;
  IF OLD.immutable AND NEW.body <> OLD.body THEN
    RAISE EXCEPTION 'surface file is immutable: %', OLD.path;
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS surface_guard ON surface_files;
CREATE TRIGGER surface_guard BEFORE UPDATE OR DELETE ON surface_files
  FOR EACH ROW EXECUTE FUNCTION blygd_surface_guard();

-- ─────────────────── plane 2: import mirror ───────────────────
-- Shared across users: one poll and one copy per remote origin, however
-- many users subscribe (§12.3 politeness). Written only by internal/reader.

CREATE TABLE IF NOT EXISTS remote_origins (
  id                  bigserial PRIMARY KEY,
  kind                text NOT NULL CHECK (kind IN ('blyg', 'l0')),
  -- §12.2 identity origin for a blyg (the post-redirect URL the manifest
  -- came from, minus blyg.json); the final feed URL for an L0 subscription.
  identity_url        text NOT NULL UNIQUE,
  manifest            jsonb,   -- verbatim, display-advisory (§12.2)
  title               text,
  site_url            text,    -- L0: the feed's channel link (§11 htmlUrl)
  -- §12.2: a manifest `site` that disagrees with the identity origin is
  -- surfaced here and never adopted.
  site_discrepancy    text,
  manifest_etag       text,
  feed_etag           text,
  feed_last_modified  text,
  index_etag          text,
  index_last_modified text,
  -- §13.2 gap detection: the newest feed guid seen last poll. If it has left
  -- the window, the poller falls back to an index diff.
  feed_newest_guid    text,
  reconciled_at       timestamptz,   -- last complete index diff
  -- §13.2: an otherwise-live blyg whose items/index.json 404s.
  lossy               boolean NOT NULL DEFAULT false,
  next_poll_at        timestamptz NOT NULL DEFAULT now(),
  poll_failures       integer NOT NULL DEFAULT 0,
  last_polled_at      timestamptz,
  last_error          text,
  created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS remote_origins_poll_idx ON remote_origins (next_poll_at);

CREATE TABLE IF NOT EXISTS imported_items (
  id                bigserial PRIMARY KEY,
  remote_origin_id  bigint NOT NULL REFERENCES remote_origins(id),
  -- Origin-scoped (§13.1 rule 7). For L0 it is synthetic, 'l0:' + hex, and
  -- so can never be a valid blyg id (§13.6).
  item_id           text NOT NULL,
  state             text NOT NULL CHECK (state IN ('current', 'withdrawn')),
  watermark         integer NOT NULL CHECK (watermark >= 1),   -- §13.3: highest version ever seen
  version           integer NOT NULL CHECK (version >= 1),
  kind              text NOT NULL CHECK (kind IN ('fragment', 'thread', 'withdrawn', 'l0')),
  -- The latest document verbatim; NULL once rolled to null (§13.4). Store
  -- verbatim, sanitize at render (§14): nothing here is trusted HTML.
  doc               jsonb,
  content_hash      text,               -- as served, for stealth-edit detection (§13.3)
  page_url          text,               -- absolute (§5.8); the entry link for L0
  claimed_updated   timestamptz,        -- self-asserted (§5.2)
  first_seen_at     timestamptz NOT NULL DEFAULT now(),
  version_seen_at   timestamptz NOT NULL DEFAULT now(),   -- when this version was first observed
  -- §13.7: an item sorts no newer than when we actually saw it.
  sort_at           timestamptz GENERATED ALWAYS AS (least(claimed_updated, version_seen_at)) STORED,
  fetched_at        timestamptz NOT NULL DEFAULT now(),
  discrepancy       text,               -- §13.3 rewrite / stealth edit / bad hash, surfaced to the user
  discrepancy_at    timestamptz,
  CHECK (version <= watermark),
  CHECK (state = 'current' OR doc IS NULL),
  UNIQUE (remote_origin_id, item_id)
);
CREATE INDEX IF NOT EXISTS imported_items_sort_idx ON imported_items (remote_origin_id, sort_at DESC, id DESC) WHERE state = 'current';

-- §13.4: the only content retainable past withdrawal. A pin's bytes never
-- change at the origin (§8 rule 1), so neither do ours.
CREATE TABLE IF NOT EXISTS imported_pins (
  remote_origin_id  bigint NOT NULL REFERENCES remote_origins(id),
  item_id           text NOT NULL,
  version           integer NOT NULL,
  doc               jsonb NOT NULL,
  fetched_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (remote_origin_id, item_id, version)
);

-- §13.3: the watermark never regresses, whatever the code above it does.
CREATE OR REPLACE FUNCTION blygd_imported_items_guard() RETURNS trigger AS $$
BEGIN
  IF NEW.watermark < OLD.watermark THEN
    RAISE EXCEPTION 'watermark never regresses (imported item %: % -> %)', OLD.item_id, OLD.watermark, NEW.watermark;
  END IF;
  IF NEW.remote_origin_id <> OLD.remote_origin_id OR NEW.item_id <> OLD.item_id THEN
    RAISE EXCEPTION 'imported item identity is permanent (%)', OLD.item_id;
  END IF;
  RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS imported_items_guard ON imported_items;
CREATE TRIGGER imported_items_guard BEFORE UPDATE ON imported_items
  FOR EACH ROW EXECUTE FUNCTION blygd_imported_items_guard();

CREATE OR REPLACE FUNCTION blygd_imported_pins_guard() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'retained pins are immutable (% v%)', OLD.item_id, OLD.version;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS imported_pins_guard ON imported_pins;
CREATE TRIGGER imported_pins_guard BEFORE UPDATE ON imported_pins
  FOR EACH ROW EXECUTE FUNCTION blygd_imported_pins_guard();

-- ─────────────────── plane 3: private overlay ───────────────────

CREATE TABLE IF NOT EXISTS subscriptions (
  user_id           bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  remote_origin_id  bigint NOT NULL REFERENCES remote_origins(id),
  display_title     text,
  created_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, remote_origin_id)
);
CREATE INDEX IF NOT EXISTS subscriptions_remote_idx ON subscriptions (remote_origin_id);

CREATE TABLE IF NOT EXISTS hoppers (
  id           bigserial PRIMARY KEY,
  user_id      bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
  slug         text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]*$'),
  -- Public display (§13.5) is Phase 4. Once public, the slug is someone's
  -- link and never changes again (reference studio's slug_frozen rule).
  public       boolean NOT NULL DEFAULT false,
  slug_frozen  boolean NOT NULL DEFAULT false,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, slug)
);

-- Members are references, never copies: a copy would outlive a withdrawal,
-- which §13.4 forbids. Exactly one side is set.
CREATE TABLE IF NOT EXISTS hopper_members (
  id                bigserial PRIMARY KEY,
  hopper_id         bigint NOT NULL REFERENCES hoppers(id) ON DELETE CASCADE,
  own_item_id       text REFERENCES items(id) ON DELETE CASCADE,   -- drafts may be hard-deleted
  imported_item_id  bigint REFERENCES imported_items(id),
  added_at          timestamptz NOT NULL DEFAULT now(),
  CHECK ((own_item_id IS NULL) <> (imported_item_id IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS hopper_members_own_idx ON hopper_members (hopper_id, own_item_id) WHERE own_item_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS hopper_members_imp_idx ON hopper_members (hopper_id, imported_item_id) WHERE imported_item_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS reads (
  user_id           bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  imported_item_id  bigint NOT NULL REFERENCES imported_items(id),
  version           integer NOT NULL,
  read_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, imported_item_id)
);

-- ───────────────────────── mentions ─────────────────────────

-- §15.3–15.4. A verified mention is a pointer: no content is stored.
CREATE TABLE IF NOT EXISTS inbound_mentions (
  id               bigserial PRIMARY KEY,
  origin_id        bigint NOT NULL REFERENCES origins(id),
  target_item_id   text NOT NULL REFERENCES items(id),
  source_url       text NOT NULL,
  target_url       text NOT NULL,
  status           text NOT NULL CHECK (status IN ('pending', 'verified', 'failed', 'gone')),
  ever_verified    boolean NOT NULL DEFAULT false,
  relation         text CHECK (relation IN ('stub', 'transclusion', 'fork')),
  source_origin    text,
  source_item_id   text,
  source_version   integer,
  source_kind      text,
  source_author    jsonb,   -- pass-through (§5.5)
  source_page      text,
  failure          text,
  attempts         integer NOT NULL DEFAULT 0,
  next_attempt_at  timestamptz,
  received_at      timestamptz NOT NULL DEFAULT now(),
  verified_at      timestamptz,
  -- §15.3 step 3: a re-sent pair re-verifies rather than duplicating.
  UNIQUE (origin_id, source_url, target_url)
);
CREATE INDEX IF NOT EXISTS inbound_mentions_due_idx ON inbound_mentions (next_attempt_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS inbound_mentions_item_idx ON inbound_mentions (target_item_id);

-- One row per accepted claim, for §15.3 step 2's per-host, per-domain and
-- global caps. Instance-wide: verification fetches cost the instance, so
-- spreading claims across tenants must not raise the ceiling.
CREATE TABLE IF NOT EXISTS mention_claims (
  id             bigserial PRIMARY KEY,
  at             timestamptz NOT NULL DEFAULT now(),
  source_host    text NOT NULL,
  source_domain  text NOT NULL
);
CREATE INDEX IF NOT EXISTS mention_claims_at_idx ON mention_claims (at);
CREATE INDEX IF NOT EXISTS mention_claims_host_idx ON mention_claims (source_host, at);
CREATE INDEX IF NOT EXISTS mention_claims_domain_idx ON mention_claims (source_domain, at);

-- §15.2 outbox. Rows are written in the publish transaction; a worker sends.
-- One row per (source item, reference target), carrying the target version
-- last sent, so a republish re-sends only new or changed references.
CREATE TABLE IF NOT EXISTS outbound_mentions (
  id               bigserial PRIMARY KEY,
  origin_id        bigint NOT NULL REFERENCES origins(id),
  source_item_id   text NOT NULL REFERENCES items(id),
  ref_key          text NOT NULL,   -- 'blyg {origin} {id}' or 'url {url}'
  relation         text NOT NULL CHECK (relation IN ('stub', 'transclusion', 'fork', 'url')),
  source_url       text NOT NULL,
  target_url       text NOT NULL,
  target_origin    text,
  target_id        text,
  target_version   integer,
  status           text NOT NULL CHECK (status IN ('pending', 'sent', 'failed', 'no_endpoint', 'abandoned')),
  endpoint         text,
  attempts         integer NOT NULL DEFAULT 0,
  next_attempt_at  timestamptz,
  last_result      text,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (source_item_id, ref_key)
);
CREATE INDEX IF NOT EXISTS outbound_mentions_due_idx ON outbound_mentions (next_attempt_at) WHERE status = 'pending';
