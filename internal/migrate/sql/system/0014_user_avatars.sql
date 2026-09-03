--
-- 0014_user_avatars.sql
-- Stores each person's picture as bytes we serve ourselves.
--
-- Created: 2026-09-02
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- The picture comes from Google or Gravatar, and neither may be linked to from
-- a page. A browser fetching an image hands that provider the viewer's address,
-- their user agent, and the page they are on, on every load — which is the
-- thing our customers left the incumbents to stop doing. Google's picture URL
-- also rotates, so a stored URL rots.
--
-- A row with a fetch time and no bytes is a remembered miss: an address with no
-- Gravatar, or a fetch that failed. It stops us asking the provider again on
-- every page load.

ALTER TABLE users ADD COLUMN avatar_bytes BLOB;
ALTER TABLE users ADD COLUMN avatar_type TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN avatar_etag TEXT NOT NULL DEFAULT '';

-- 'google', 'gravatar', or empty for a remembered miss. Google's picture wins,
-- so this is what says whether a Gravatar lookup would be an improvement.
ALTER TABLE users ADD COLUMN avatar_source TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN avatar_fetched_at INTEGER;
