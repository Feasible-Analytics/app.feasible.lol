--
-- 0002_event_dedupe.sql
-- The idempotency table that makes writing an event twice harmless.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- The moment a connection can fail after a commit, retries are guaranteed. A
-- browser can lose the acknowledgement, replay its locally held request, and
-- otherwise write the pageview twice. Retrofitting idempotency after real data
-- exists is unpleasant, so this table predates every retry path.
--
-- A receipt is kept for thirty days, against the seven a browser may hold a
-- failed event for. The gap is margin: a wrong client clock, a tab open across
-- the boundary, a prune that has not run, a browser on a cached older tracker.
-- Bounding the client is what makes the table prunable at all — see
-- 0015_prune_event_receipts.sql.

CREATE TABLE recent_event_ids (
    -- The uuid the ingest tier stamped on the event when it derived it, stored
    -- as its 16 raw bytes rather than 36 characters of text.
    event_uuid  BLOB PRIMARY KEY,

    -- When we first accepted it, and what the prune reads.
    received_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE INDEX recent_event_ids_received ON recent_event_ids(received_at);
