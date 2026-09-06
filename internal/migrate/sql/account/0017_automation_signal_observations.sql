--
-- 0017_automation_signal_observations.sql
-- Record which automation letters the tracker reported.
--
-- Created: 2026-09-06
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- The classifier reads a one-letter signal string off every event and never
-- stores it, so a visitor classified as automated leaves no record of what was
-- actually reported. That makes the classifier unfalsifiable: when it is wrong
-- there is nothing to query.
--
-- SQLite cannot alter a CHECK constraint, so the table is rebuilt.

CREATE TABLE ingest_observations_new (
    id            INTEGER PRIMARY KEY,
    site_id       INTEGER NOT NULL,

    -- Exact unix second for the same rolling-boundary guarantee as the health
    -- counts beside it.
    observed_at   INTEGER NOT NULL,

    -- unknown_hostname    a hostname sending events that is not on the allow-list
    -- tracker_version     which build of the script is in the wild
    -- ip_source           which header the client address came from
    -- automation_signals  the letters the tracker reported about the browser
    kind          TEXT NOT NULL CHECK (kind IN (
                      'unknown_hostname', 'tracker_version', 'ip_source', 'automation_signals')),

    value         TEXT NOT NULL,
    count         INTEGER NOT NULL DEFAULT 0,
    first_seen_at INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL,

    UNIQUE (site_id, observed_at, kind, value)
);

INSERT INTO ingest_observations_new
    (id, site_id, observed_at, kind, value, count, first_seen_at, last_seen_at)
SELECT id, site_id, observed_at, kind, value, count, first_seen_at, last_seen_at
FROM ingest_observations;

DROP TABLE ingest_observations;

ALTER TABLE ingest_observations_new RENAME TO ingest_observations;

CREATE INDEX ingest_observations_recent ON ingest_observations(site_id, observed_at, kind);
