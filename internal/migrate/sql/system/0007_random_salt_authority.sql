--
-- 0007_random_salt_authority.sql
-- Marks random salt material created by the deployment authority.
--
-- Created: 2026-08-31
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Existing rows are invalidated deliberately. The app salt authority creates
-- fresh random current and next-day material marked as authority-owned before
-- it becomes ready.

ALTER TABLE salts ADD COLUMN source_shard INTEGER NOT NULL DEFAULT -1;

DELETE FROM salts;
