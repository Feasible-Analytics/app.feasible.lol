--
-- 0002_event_dedupe.sql
-- The 24-hour idempotency table that makes writing an event twice harmless.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- The moment there is a retry anywhere — and store-and-forward adds retries by
-- design — duplicates are guaranteed. The classic case is a shard that commits
-- and then loses the acknowledgement in transit: the sender retries, and the
-- event is written twice. A duplicated pageview is a wrong number with no
-- obvious cause, and retrofitting idempotency once real data is in the table is
-- unpleasant, so this table exists before anything can generate a duplicate.
--
-- Twenty-four hours is the retention because the realistic redelivery window is
-- seconds to minutes. The bound is not about correctness; it is what keeps the
-- index small enough that the lookup stays cheap on the write path.

CREATE TABLE recent_event_ids (
    -- The uuid the ingest tier stamped on the event when it derived it, stored
    -- as its 16 raw bytes rather than 36 characters of text.
    event_uuid  BLOB PRIMARY KEY,

    -- When we first accepted it. The only reason this column exists is so the
    -- pruning job can find rows to delete without a full scan.
    received_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE INDEX recent_event_ids_received ON recent_event_ids(received_at);
