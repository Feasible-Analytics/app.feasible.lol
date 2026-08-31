--
-- 0002_salt_rotation.sql
-- One salt per UTC day, enforced by the database rather than by a code path.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Every process refreshes its own salts, so at 00:00 UTC every process in a
-- deployment tries to create the same day's salt at the same moment. Without a
-- constraint they all succeed, the fingerprint depends on which process served
-- the request, and one visitor becomes several with nothing reporting an error.
--
-- created_at is unix seconds in UTC, so dividing by 86400 is the UTC day number
-- with no timezone arithmetic anywhere. A local-midnight rotation would give two
-- accounts different visitor identities for the same person, which is the exact
-- failure this expression exists to make impossible.

CREATE UNIQUE INDEX salts_day ON salts(created_at / 86400);
