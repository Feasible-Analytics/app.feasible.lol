--
-- 0018_imported_wide_grains.sql
-- Week and month summaries of imported history.
--
-- Created: 2026-09-06
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Imported history is one row per day per shape. A wide report over a large
-- archive adds up millions of them to reach a number that a few dozen hold.
--
-- The arithmetic is a plain SUM. Unlike the native roll-ups there is no
-- carried-visitor correction to apply, because a daily total is all an import
-- ever supplies: the source counted its own visitors per day and we cannot
-- un-count anybody who appeared on two of them. Summing the days is exactly
-- what reading the days does, so the answer does not change.
--
-- It is a table of its own rather than a `grain` column on imported_rollups.
-- Several readers outside the query engine scan that table with no grain
-- predicate, and a coarse row one of them picked up would silently double a
-- customer's history.

CREATE TABLE imported_wide (
    id                  INTEGER PRIMARY KEY,
    import_id           INTEGER NOT NULL REFERENCES imports(id) ON DELETE CASCADE,
    site_id             INTEGER NOT NULL,

    -- 2 = week, 3 = month, matching query.Grain. Day is not stored here: it is
    -- imported_rollups, unchanged.
    grain               INTEGER NOT NULL,

    -- Unix seconds at the site's local start of the bucket, the same clock
    -- imported_rollups.timestamp is on.
    timestamp           INTEGER NOT NULL,

    covered             INTEGER NOT NULL DEFAULT 0,

    name_id             INTEGER NOT NULL DEFAULT 0,
    hostname_id         INTEGER NOT NULL DEFAULT 0,
    pathname_id         INTEGER NOT NULL DEFAULT 0,
    entry_page_id       INTEGER NOT NULL DEFAULT 0,
    exit_page_id        INTEGER NOT NULL DEFAULT 0,
    page_title_id       INTEGER NOT NULL DEFAULT 0,
    referrer_id         INTEGER NOT NULL DEFAULT 0,
    source_id           INTEGER NOT NULL DEFAULT 0,
    channel_id          INTEGER NOT NULL DEFAULT 0,
    utm_source_id       INTEGER NOT NULL DEFAULT 0,
    utm_medium_id       INTEGER NOT NULL DEFAULT 0,
    utm_campaign_id     INTEGER NOT NULL DEFAULT 0,
    country_id          INTEGER NOT NULL DEFAULT 0,
    region_id           INTEGER NOT NULL DEFAULT 0,
    city_id             INTEGER NOT NULL DEFAULT 0,
    device_type_id      INTEGER NOT NULL DEFAULT 0,
    screen_size_id      INTEGER NOT NULL DEFAULT 0,
    browser_id          INTEGER NOT NULL DEFAULT 0,
    browser_version_id  INTEGER NOT NULL DEFAULT 0,
    os_id               INTEGER NOT NULL DEFAULT 0,
    os_version_id       INTEGER NOT NULL DEFAULT 0,
    language_id         INTEGER NOT NULL DEFAULT 0,

    property_key        TEXT NOT NULL DEFAULT '',
    property_value      TEXT NOT NULL DEFAULT '',

    visitors            INTEGER NOT NULL DEFAULT 0,
    visits              INTEGER NOT NULL DEFAULT 0,
    pageviews           INTEGER NOT NULL DEFAULT 0,
    events              INTEGER NOT NULL DEFAULT 0,
    exits               INTEGER NOT NULL DEFAULT 0,
    bounces             INTEGER NOT NULL DEFAULT 0,
    duration_total      INTEGER NOT NULL DEFAULT 0,
    engagement_total    INTEGER NOT NULL DEFAULT 0,
    engagement_visits   INTEGER NOT NULL DEFAULT 0,
    scroll_depth_total  INTEGER NOT NULL DEFAULT 0,
    scroll_depth_visits INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX imported_wide_range ON imported_wide(site_id, grain, timestamp);
CREATE INDEX imported_wide_import ON imported_wide(import_id);

-- Which grains an import has been summarised into, so a reader can tell a
-- finished summary from one that is still being built. A half-finished backfill
-- has to be slow rather than wrong.
ALTER TABLE imports ADD COLUMN wide_grains INTEGER NOT NULL DEFAULT 0;

-- The timezone the buckets were cut in. A week is a week in one zone and a
-- different seven days in another, so a summary read under a zone it was not
-- cut in reports one month's traffic as the next one's.
ALTER TABLE imports ADD COLUMN wide_timezone TEXT NOT NULL DEFAULT '';
