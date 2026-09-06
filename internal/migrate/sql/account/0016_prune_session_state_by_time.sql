--
-- 0016_prune_session_state_by_time.sql
-- Indexes ordered for the prune that runs on every write batch.
--
-- Created: 2026-09-06
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Both tables already carry an index that starts (site_id, user_id, …), which
-- is right for the read path: it looks up one visitor. The prune asks a
-- different question — everything for this site older than a cutoff — and with
-- user_id sitting between the site and the time column, SQLite can only seek to
-- the site and then scan every row it has.
--
-- The retention window is 48 hours, so that scan is every session the site has
-- had in two days, on every batch. The cost of writing a batch grows with the
-- site's own traffic and a busy site writes more batches, so the total grows
-- with roughly the square of it.
--
-- These two index exactly the rows the prune deletes. Both tables are small and
-- high-churn, so the write amplification is a row's worth of index per session
-- against a scan that has no bound but the site's popularity.

CREATE INDEX ingest_session_state_expiry
    ON ingest_session_state(site_id, last_seen_at);

CREATE INDEX ingest_orphan_engagements_expiry
    ON ingest_orphan_engagements(site_id, timestamp);
