--
-- 0011_account_comps.sql
-- Durable complimentary access for accounts that should never enter billing lifecycle enforcement.
--
-- Created: 2026-09-01
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--

CREATE TABLE account_comps (
    team_id          INTEGER PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
    owner_email      TEXT NOT NULL COLLATE NOCASE,
    comped_at        INTEGER NOT NULL
);
