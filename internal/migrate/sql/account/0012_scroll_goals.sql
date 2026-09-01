--
-- 0012_scroll_goals.sql
-- Scroll-depth conversion definitions.
--
-- Created: 2026-09-01
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--

ALTER TABLE goals ADD COLUMN scroll_depth INTEGER NOT NULL DEFAULT 0;

