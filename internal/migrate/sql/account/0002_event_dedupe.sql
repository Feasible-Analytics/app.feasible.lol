--
-- 0002_event_dedupe.sql
-- The permanent idempotency table that makes writing an event twice harmless.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- The moment a connection can fail after a commit, retries are guaranteed. A
-- browser can lose the acknowledgement, replay its locally held request, and
-- otherwise write the pageview twice. Retrofitting idempotency after real data
-- exists is unpleasant, so this table predates every retry path.
--
-- Receipts are retained for the life of the account database. A browser can
-- retain a failed request indefinitely, so any timed deletion would eventually
-- make a lost acknowledgement count twice.

CREATE TABLE recent_event_ids (
    -- The uuid the ingest tier stamped on the event when it derived it, stored
    -- as its 16 raw bytes rather than 36 characters of text.
    event_uuid  BLOB PRIMARY KEY,

    -- When we first accepted it. It remains useful for incident reconstruction;
    -- correctness never expires it.
    received_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE INDEX recent_event_ids_received ON recent_event_ids(received_at);
