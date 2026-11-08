--
-- 0022_hostname_filter_index.sql
-- Let a hostname filter seek the events it matches.
--
-- Created: 2026-11-07
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- A filter cannot use the daily summaries, so it reads the events table. Page,
-- source and country each have an index that seeks by site, value and time.
-- Hostname is filtered the same way, and without this index that read walks
-- every event in the date range and checks the hostname afterwards.

CREATE INDEX events_hostname ON events(site_id, hostname_id, timestamp);
