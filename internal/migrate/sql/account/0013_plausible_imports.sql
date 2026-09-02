--
-- 0013_plausible_imports.sql
-- Plausible-specific roll-up fields needed for lossless archive migration.
--
-- Created: 2026-09-02
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--

ALTER TABLE imported_rollups ADD COLUMN property_key TEXT NOT NULL DEFAULT '';
ALTER TABLE imported_rollups ADD COLUMN property_value TEXT NOT NULL DEFAULT '';
ALTER TABLE imported_rollups ADD COLUMN engagement_visits INTEGER NOT NULL DEFAULT 0;
ALTER TABLE imported_rollups ADD COLUMN scroll_depth_total INTEGER NOT NULL DEFAULT 0;
ALTER TABLE imported_rollups ADD COLUMN scroll_depth_visits INTEGER NOT NULL DEFAULT 0;

CREATE INDEX imported_rollups_property ON imported_rollups(site_id, property_key, property_value);
