-- blygd migration 009: the hoppers a manifest names (§6.1, §13.5).
--
-- From 0.4's eighth revision (decision #86) a manifest MAY list its
-- public hoppers in `hoppers`, each with its page and its hopper feed, and
-- MUST NOT list one that is not public. Making a list public is a
-- publishing act, as the blogroll is (§11), so the list is copied into
-- plane 1 at that act and the renderer reads only the copy: the studio
-- never queries the overlay. internal/web keeps the order safe, adding a
-- row after the hopper goes public and removing it before it goes
-- private, so a failure between the two can only leave a public hopper
-- unlisted, never a private one listed. The hopper feed is served live,
-- and only for a row here.

CREATE TABLE IF NOT EXISTS public_hoppers (
  origin_id   bigint NOT NULL REFERENCES origins(id),
  slug        text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]*$'),
  title       text NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
  PRIMARY KEY (origin_id, slug)
);

-- Hoppers public before this migration were made public by their owners:
-- list them. Manifests name them from the next render of each origin,
-- which a deploy's rebuild does (DEPLOY.md, step 4).
-- A user's origin is their first (the studio's OriginForOwner).
INSERT INTO public_hoppers (origin_id, slug, title)
  SELECT (SELECT o.id FROM origins o WHERE o.owner_id = h.user_id ORDER BY o.id LIMIT 1), h.slug, h.name
  FROM hoppers h
  WHERE h.public AND EXISTS (SELECT 1 FROM origins o WHERE o.owner_id = h.user_id)
  ON CONFLICT DO NOTHING;
