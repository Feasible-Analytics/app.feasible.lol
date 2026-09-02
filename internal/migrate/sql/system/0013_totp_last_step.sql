--
-- 0013_totp_last_step.sql
-- Remembers the last accepted authenticator time step so a code cannot be replayed.
--
-- Created: 2026-09-02
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- A six-digit code is valid for about ninety seconds. Without a record of the
-- step that was last accepted, a code read over somebody's shoulder works a
-- second time inside that window. The verifier only accepts a step later than
-- this one.

ALTER TABLE users ADD COLUMN totp_last_used_step INTEGER NOT NULL DEFAULT 0;
