--
-- 0012_drop_stored_salts.sql
-- Removes the obsolete app-side salt authority table.
--
-- Created: 2026-09-01
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Ingest processes now derive daily material from FEASIBLE_INGEST_SALT and the
-- UTC day. No salt material needs to be stored, replicated, or rotated by an
-- app shard.

DROP TABLE salts;
