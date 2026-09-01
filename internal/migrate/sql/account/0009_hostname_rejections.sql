--
-- 0009_hostname_rejections.sql
-- Rejected hostnames, permanent event receipts, and shared session identities.
--
-- Created: 2026-08-31
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.

-- Rejected events are counted rather than stored. The writer caps distinct
-- hostnames per site and UTC day and folds overflow into an "other" row.
CREATE TABLE hostname_rejections (
    site_id  INTEGER NOT NULL,
    hostname TEXT NOT NULL,
    day      INTEGER NOT NULL,
    events   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, day, hostname)
) WITHOUT ROWID;

-- Session ids once came from a process-local counter seeded from MAX(id).
-- Reserving disjoint ranges in SQLite prevents independent writers from using
-- the same identity for different visitors.
CREATE TABLE session_id_allocator (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    next_id   INTEGER NOT NULL CHECK (next_id > 0)
);

INSERT INTO session_id_allocator (singleton, next_id)
SELECT 1, COALESCE(MAX(id), 0) + 1 FROM sessions;

-- Event receipts are permanent, so the pruning index is obsolete.
DROP INDEX recent_event_ids_received;
