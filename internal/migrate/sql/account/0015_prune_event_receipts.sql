--
-- 0015_prune_event_receipts.sql
-- The index that makes an event receipt prunable.
--
-- Created: 2026-09-06
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- recent_event_ids is WITHOUT ROWID over a random uuid, so the table is the
-- index and every insert lands at a random position in a tree that only grows.
-- While it fits in memory that costs nothing; once it does not, every accepted
-- event pays a random disk read on the write path, for ever.
--
-- The tracker gives up on a stashed event after seven days, so a receipt is only
-- needed for as long as a browser can still replay one. Thirty days here leaves
-- margin for a wrong client clock, a tab open across the boundary, a prune that
-- has not run, and a browser on a cached older tracker — so the server is never
-- the reason a legitimate replay is counted twice.

CREATE INDEX recent_event_ids_received ON recent_event_ids(received_at);
