--
-- 0008_ingest_session_state.sql
-- Durable fold ownership and orphan engagement storage shared by every writer.
--
-- Created: 2026-08-31
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- The sessions fact row is optimized for reports and does not retain every
-- timestamp and tie marker needed for an order-independent fold. This
-- companion state makes SQLite, rather than one process's cache,
-- authoritative when two writers overlap or a process restarts mid-visit.

CREATE TABLE ingest_session_state (
    session_id   INTEGER PRIMARY KEY,
    site_id      INTEGER NOT NULL,
    user_id      INTEGER NOT NULL,
    started_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    payload      BLOB NOT NULL
);

CREATE INDEX ingest_session_state_visitor
    ON ingest_session_state(site_id, user_id, last_seen_at);

-- Engagement can arrive before its pageview. Persisting the complete derived
-- event and its permanent receipt in one transaction lets any later writer
-- for the same visitor adopt it without process memory or redelivery.
CREATE TABLE ingest_orphan_engagements (
    event_uuid BLOB PRIMARY KEY,
    site_id    INTEGER NOT NULL,
    user_id    INTEGER NOT NULL,
    timestamp  INTEGER NOT NULL,
    payload    BLOB NOT NULL
) WITHOUT ROWID;

CREATE INDEX ingest_orphan_engagements_visitor
    ON ingest_orphan_engagements(site_id, user_id, timestamp);
