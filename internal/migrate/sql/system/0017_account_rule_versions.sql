--
-- 0017_account_rule_versions.sql
-- When an account last changed a rule the ingest path has to know about.
--
-- Created: 2026-09-06
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Two background loops used to open every account database on the box every
-- fifteen seconds to re-read shield and path-cleaning rules that almost never
-- change. This row is what lets them read one table on system.db instead and
-- open only the accounts whose rules actually moved.
--
-- An account with no row here has never had a rule saved by a process that
-- stamps one, which is indistinguishable from never having had a rule saved.
-- The hourly full pass is what covers the difference.

CREATE TABLE account_rule_versions (
    account_id INTEGER PRIMARY KEY,
    changed_at INTEGER NOT NULL
) WITHOUT ROWID;
