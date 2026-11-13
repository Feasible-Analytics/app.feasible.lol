--
-- 0023_rollup_generation.sql
-- Let a roll-up build notice that the data it read was erased.
--
-- Created: 2026-11-13
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- A roll-up aggregates a snapshot and publishes the result in a later
-- transaction. Erasing a site's data advances this number in the erasing
-- transaction, and a build that sees it move between its snapshot and its
-- publish discards its work. It is one row per account rather than per site,
-- so erasing a site leaves nothing of that site behind in it.

CREATE TABLE rollup_generation (
    singleton  INTEGER PRIMARY KEY CHECK (singleton = 1),
    generation INTEGER NOT NULL
);

INSERT INTO rollup_generation (singleton, generation) VALUES (1, 0);
