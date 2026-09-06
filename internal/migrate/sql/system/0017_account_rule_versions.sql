--
-- 0017_account_rule_versions.sql
-- When an account last changed a rule the ingest path has to know about.
--
-- Created: 2026-09-06
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- One row read from system.db is what lets the shield and path-cleaning caches
-- open only the accounts whose rules moved, rather than every account on the
-- box on every refresh.
--
-- Nanoseconds, because two edits inside the same second are two edits and a
-- comparison at second resolution would miss the later one until the hourly
-- full pass.
--
-- An account with no row here has never had a rule saved by a process that
-- stamps one, which is indistinguishable from never having had a rule saved.
-- The hourly full pass is what covers the difference.

CREATE TABLE account_rule_versions (
    account_id INTEGER PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
    changed_at INTEGER NOT NULL
);
