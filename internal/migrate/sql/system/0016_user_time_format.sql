--
-- 0016_user_time_format.sql
-- Whether each person reads a 12-hour or a 24-hour clock.
--
-- Created: 2026-09-05
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- A column on users rather than a table of its own, because it is one short
-- string read on the same path that already reads the theme beside it.
--
-- It is a personal preference and nothing else. It is deliberately not derived
-- from the language, the country or the site's timezone: two people who both
-- read English and both sit in the same office can want different clocks, and
-- guessing from any of those gets one of them wrong on every screen.
--
-- 'system' means "whatever this person set their own device to", which only the
-- browser can answer. The feasible_clock cookie carries that answer back.

ALTER TABLE users ADD COLUMN time_format TEXT NOT NULL DEFAULT 'system';
