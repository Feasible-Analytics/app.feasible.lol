--
-- 0004_rollups.sql
-- The pre-aggregated report tables and the record of which buckets are built.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- A report over a month of a busy account reads two and a half million event
-- rows to answer a question whose answer is fifteen thousand numbers. These
-- tables hold those numbers, so the same report reads fifteen thousand rows.
--
-- Four decisions govern the shape, and each one is a bug we are choosing not to
-- have later.
--
-- 1. Numerator and denominator, never an average. `bounces` and `visits` are
--    both stored; `bounce_rate` is not. An average cannot be re-aggregated —
--    the mean of two daily means is not the weekly mean unless the days had
--    identical traffic — while a ratio of two sums re-aggregates perfectly.
--
-- 2. `bucket` is the start of the local period expressed in *local* seconds,
--    which is the timestamp column plus the site's UTC offset at that instant.
--    Storing local seconds rather than the UTC instant means the label a report
--    draws is `date(bucket, 'unixepoch')` with no timezone arithmetic at read
--    time, and it is the identical string the raw path produces from the same
--    events. The offset used is recorded implicitly by rollup_state.timezone:
--    a query in a different timezone cannot read these rows and falls back to
--    raw.
--
-- 3. Both grains live in one table behind a `grain` discriminator, keyed so
--    that the two never mix in a scan. Hourly rows are pruned after a fortnight
--    and daily rows are kept forever.
--
-- 4. The `_carried` columns are what make a distinct count re-aggregate. A
--    visitor id lives for one UTC day and a visit for as long as somebody keeps
--    clicking, so either can appear in two adjacent buckets and be counted
--    twice when the buckets are added up. `_carried` records how many of a
--    bucket's distinct entities were already present in the bucket before it,
--    so summing a range and subtracting every bucket's carry except the first
--    gives the exact distinct count. Without it a 28-day visitor total is a few
--    per cent too high on any site whose day does not start at UTC midnight.
--
-- Every table has the same columns even where half of them are always zero.
-- Uniformity is worth more here than the handful of bytes SQLite spends on a
-- zero: one builder, one reader, and no table that is subtly different from the
-- other ten.

-- rollup_visitors carries the whole-site totals — one row per bucket, dimension
-- 0, value 0. It is the table the headline numbers and the main graph read, and
-- keeping it separate from the breakdowns means the cheapest, most-run query in
-- the product touches the smallest table.
CREATE TABLE rollup_visitors (
    site_id                INTEGER NOT NULL,
    grain                  INTEGER NOT NULL, -- 0 = day, 1 = hour
    bucket                 INTEGER NOT NULL, -- local seconds at the start of the period
    dimension              INTEGER NOT NULL,
    value_id               INTEGER NOT NULL,

    -- Event grain: counted over `events`, one row per hit.
    pageviews              INTEGER NOT NULL DEFAULT 0,
    events                 INTEGER NOT NULL DEFAULT 0,
    event_visitors         INTEGER NOT NULL DEFAULT 0,
    event_visitors_carried INTEGER NOT NULL DEFAULT 0,
    event_visits           INTEGER NOT NULL DEFAULT 0,
    event_visits_carried   INTEGER NOT NULL DEFAULT 0,

    -- Visit grain: counted over `sessions`, one row per visit, placed by when
    -- the visit started.
    visits                 INTEGER NOT NULL DEFAULT 0,
    visitors               INTEGER NOT NULL DEFAULT 0,
    visitors_carried       INTEGER NOT NULL DEFAULT 0,
    bounces                INTEGER NOT NULL DEFAULT 0,
    visit_duration         INTEGER NOT NULL DEFAULT 0, -- seconds, summed
    session_pageviews      INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (site_id, grain, dimension, bucket, value_id)
) WITHOUT ROWID;

CREATE TABLE rollup_sources (
    site_id                INTEGER NOT NULL,
    grain                  INTEGER NOT NULL,
    bucket                 INTEGER NOT NULL,
    dimension              INTEGER NOT NULL,
    value_id               INTEGER NOT NULL,
    pageviews              INTEGER NOT NULL DEFAULT 0,
    events                 INTEGER NOT NULL DEFAULT 0,
    event_visitors         INTEGER NOT NULL DEFAULT 0,
    event_visitors_carried INTEGER NOT NULL DEFAULT 0,
    event_visits           INTEGER NOT NULL DEFAULT 0,
    event_visits_carried   INTEGER NOT NULL DEFAULT 0,
    visits                 INTEGER NOT NULL DEFAULT 0,
    visitors               INTEGER NOT NULL DEFAULT 0,
    visitors_carried       INTEGER NOT NULL DEFAULT 0,
    bounces                INTEGER NOT NULL DEFAULT 0,
    visit_duration         INTEGER NOT NULL DEFAULT 0,
    session_pageviews      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, grain, dimension, bucket, value_id)
) WITHOUT ROWID;

CREATE TABLE rollup_pages (
    site_id                INTEGER NOT NULL,
    grain                  INTEGER NOT NULL,
    bucket                 INTEGER NOT NULL,
    dimension              INTEGER NOT NULL,
    value_id               INTEGER NOT NULL,
    pageviews              INTEGER NOT NULL DEFAULT 0,
    events                 INTEGER NOT NULL DEFAULT 0,
    event_visitors         INTEGER NOT NULL DEFAULT 0,
    event_visitors_carried INTEGER NOT NULL DEFAULT 0,
    event_visits           INTEGER NOT NULL DEFAULT 0,
    event_visits_carried   INTEGER NOT NULL DEFAULT 0,
    visits                 INTEGER NOT NULL DEFAULT 0,
    visitors               INTEGER NOT NULL DEFAULT 0,
    visitors_carried       INTEGER NOT NULL DEFAULT 0,
    bounces                INTEGER NOT NULL DEFAULT 0,
    visit_duration         INTEGER NOT NULL DEFAULT 0,
    session_pageviews      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, grain, dimension, bucket, value_id)
) WITHOUT ROWID;

-- Entry and exit pages are visit-grain facts and live in their own tables. The
-- split is not tidiness: a bounce rate beside a page is measured over the
-- visits that *entered* on it, so the session half of a page breakdown is keyed
-- by entry page while the event half is keyed by the page that was viewed.
-- Two keyings cannot share one row.
CREATE TABLE rollup_entry_pages (
    site_id                INTEGER NOT NULL,
    grain                  INTEGER NOT NULL,
    bucket                 INTEGER NOT NULL,
    dimension              INTEGER NOT NULL,
    value_id               INTEGER NOT NULL,
    pageviews              INTEGER NOT NULL DEFAULT 0,
    events                 INTEGER NOT NULL DEFAULT 0,
    event_visitors         INTEGER NOT NULL DEFAULT 0,
    event_visitors_carried INTEGER NOT NULL DEFAULT 0,
    event_visits           INTEGER NOT NULL DEFAULT 0,
    event_visits_carried   INTEGER NOT NULL DEFAULT 0,
    visits                 INTEGER NOT NULL DEFAULT 0,
    visitors               INTEGER NOT NULL DEFAULT 0,
    visitors_carried       INTEGER NOT NULL DEFAULT 0,
    bounces                INTEGER NOT NULL DEFAULT 0,
    visit_duration         INTEGER NOT NULL DEFAULT 0,
    session_pageviews      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, grain, dimension, bucket, value_id)
) WITHOUT ROWID;

CREATE TABLE rollup_exit_pages (
    site_id                INTEGER NOT NULL,
    grain                  INTEGER NOT NULL,
    bucket                 INTEGER NOT NULL,
    dimension              INTEGER NOT NULL,
    value_id               INTEGER NOT NULL,
    pageviews              INTEGER NOT NULL DEFAULT 0,
    events                 INTEGER NOT NULL DEFAULT 0,
    event_visitors         INTEGER NOT NULL DEFAULT 0,
    event_visitors_carried INTEGER NOT NULL DEFAULT 0,
    event_visits           INTEGER NOT NULL DEFAULT 0,
    event_visits_carried   INTEGER NOT NULL DEFAULT 0,
    visits                 INTEGER NOT NULL DEFAULT 0,
    visitors               INTEGER NOT NULL DEFAULT 0,
    visitors_carried       INTEGER NOT NULL DEFAULT 0,
    bounces                INTEGER NOT NULL DEFAULT 0,
    visit_duration         INTEGER NOT NULL DEFAULT 0,
    session_pageviews      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, grain, dimension, bucket, value_id)
) WITHOUT ROWID;

CREATE TABLE rollup_locations (
    site_id                INTEGER NOT NULL,
    grain                  INTEGER NOT NULL,
    bucket                 INTEGER NOT NULL,
    dimension              INTEGER NOT NULL,
    value_id               INTEGER NOT NULL,
    pageviews              INTEGER NOT NULL DEFAULT 0,
    events                 INTEGER NOT NULL DEFAULT 0,
    event_visitors         INTEGER NOT NULL DEFAULT 0,
    event_visitors_carried INTEGER NOT NULL DEFAULT 0,
    event_visits           INTEGER NOT NULL DEFAULT 0,
    event_visits_carried   INTEGER NOT NULL DEFAULT 0,
    visits                 INTEGER NOT NULL DEFAULT 0,
    visitors               INTEGER NOT NULL DEFAULT 0,
    visitors_carried       INTEGER NOT NULL DEFAULT 0,
    bounces                INTEGER NOT NULL DEFAULT 0,
    visit_duration         INTEGER NOT NULL DEFAULT 0,
    session_pageviews      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, grain, dimension, bucket, value_id)
) WITHOUT ROWID;

CREATE TABLE rollup_devices (
    site_id                INTEGER NOT NULL,
    grain                  INTEGER NOT NULL,
    bucket                 INTEGER NOT NULL,
    dimension              INTEGER NOT NULL,
    value_id               INTEGER NOT NULL,
    pageviews              INTEGER NOT NULL DEFAULT 0,
    events                 INTEGER NOT NULL DEFAULT 0,
    event_visitors         INTEGER NOT NULL DEFAULT 0,
    event_visitors_carried INTEGER NOT NULL DEFAULT 0,
    event_visits           INTEGER NOT NULL DEFAULT 0,
    event_visits_carried   INTEGER NOT NULL DEFAULT 0,
    visits                 INTEGER NOT NULL DEFAULT 0,
    visitors               INTEGER NOT NULL DEFAULT 0,
    visitors_carried       INTEGER NOT NULL DEFAULT 0,
    bounces                INTEGER NOT NULL DEFAULT 0,
    visit_duration         INTEGER NOT NULL DEFAULT 0,
    session_pageviews      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, grain, dimension, bucket, value_id)
) WITHOUT ROWID;

CREATE TABLE rollup_browsers (
    site_id                INTEGER NOT NULL,
    grain                  INTEGER NOT NULL,
    bucket                 INTEGER NOT NULL,
    dimension              INTEGER NOT NULL,
    value_id               INTEGER NOT NULL,
    pageviews              INTEGER NOT NULL DEFAULT 0,
    events                 INTEGER NOT NULL DEFAULT 0,
    event_visitors         INTEGER NOT NULL DEFAULT 0,
    event_visitors_carried INTEGER NOT NULL DEFAULT 0,
    event_visits           INTEGER NOT NULL DEFAULT 0,
    event_visits_carried   INTEGER NOT NULL DEFAULT 0,
    visits                 INTEGER NOT NULL DEFAULT 0,
    visitors               INTEGER NOT NULL DEFAULT 0,
    visitors_carried       INTEGER NOT NULL DEFAULT 0,
    bounces                INTEGER NOT NULL DEFAULT 0,
    visit_duration         INTEGER NOT NULL DEFAULT 0,
    session_pageviews      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, grain, dimension, bucket, value_id)
) WITHOUT ROWID;

CREATE TABLE rollup_operating_systems (
    site_id                INTEGER NOT NULL,
    grain                  INTEGER NOT NULL,
    bucket                 INTEGER NOT NULL,
    dimension              INTEGER NOT NULL,
    value_id               INTEGER NOT NULL,
    pageviews              INTEGER NOT NULL DEFAULT 0,
    events                 INTEGER NOT NULL DEFAULT 0,
    event_visitors         INTEGER NOT NULL DEFAULT 0,
    event_visitors_carried INTEGER NOT NULL DEFAULT 0,
    event_visits           INTEGER NOT NULL DEFAULT 0,
    event_visits_carried   INTEGER NOT NULL DEFAULT 0,
    visits                 INTEGER NOT NULL DEFAULT 0,
    visitors               INTEGER NOT NULL DEFAULT 0,
    visitors_carried       INTEGER NOT NULL DEFAULT 0,
    bounces                INTEGER NOT NULL DEFAULT 0,
    visit_duration         INTEGER NOT NULL DEFAULT 0,
    session_pageviews      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, grain, dimension, bucket, value_id)
) WITHOUT ROWID;

CREATE TABLE rollup_languages (
    site_id                INTEGER NOT NULL,
    grain                  INTEGER NOT NULL,
    bucket                 INTEGER NOT NULL,
    dimension              INTEGER NOT NULL,
    value_id               INTEGER NOT NULL,
    pageviews              INTEGER NOT NULL DEFAULT 0,
    events                 INTEGER NOT NULL DEFAULT 0,
    event_visitors         INTEGER NOT NULL DEFAULT 0,
    event_visitors_carried INTEGER NOT NULL DEFAULT 0,
    event_visits           INTEGER NOT NULL DEFAULT 0,
    event_visits_carried   INTEGER NOT NULL DEFAULT 0,
    visits                 INTEGER NOT NULL DEFAULT 0,
    visitors               INTEGER NOT NULL DEFAULT 0,
    visitors_carried       INTEGER NOT NULL DEFAULT 0,
    bounces                INTEGER NOT NULL DEFAULT 0,
    visit_duration         INTEGER NOT NULL DEFAULT 0,
    session_pageviews      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, grain, dimension, bucket, value_id)
) WITHOUT ROWID;

CREATE TABLE rollup_custom_events (
    site_id                INTEGER NOT NULL,
    grain                  INTEGER NOT NULL,
    bucket                 INTEGER NOT NULL,
    dimension              INTEGER NOT NULL,
    value_id               INTEGER NOT NULL,
    pageviews              INTEGER NOT NULL DEFAULT 0,
    events                 INTEGER NOT NULL DEFAULT 0,
    event_visitors         INTEGER NOT NULL DEFAULT 0,
    event_visitors_carried INTEGER NOT NULL DEFAULT 0,
    event_visits           INTEGER NOT NULL DEFAULT 0,
    event_visits_carried   INTEGER NOT NULL DEFAULT 0,
    visits                 INTEGER NOT NULL DEFAULT 0,
    visitors               INTEGER NOT NULL DEFAULT 0,
    visitors_carried       INTEGER NOT NULL DEFAULT 0,
    bounces                INTEGER NOT NULL DEFAULT 0,
    visit_duration         INTEGER NOT NULL DEFAULT 0,
    session_pageviews      INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (site_id, grain, dimension, bucket, value_id)
) WITHOUT ROWID;

-- What has actually been built, per site and grain. A roll-up is a cache, so a
-- reader must never assume a bucket exists: it asks this table first and reads
-- raw events for anything outside the covered window. The timezone is stored
-- because the buckets are local days — a query asking for a different timezone
-- is asking about different days, and has to go to raw.
CREATE TABLE rollup_state (
    site_id         INTEGER NOT NULL,
    grain           INTEGER NOT NULL,
    timezone        TEXT NOT NULL,

    -- Local seconds. covered_from is inclusive, covered_through exclusive.
    covered_from    INTEGER NOT NULL,
    covered_through INTEGER NOT NULL,

    built_at        INTEGER NOT NULL, -- unix seconds, UTC

    PRIMARY KEY (site_id, grain)
) WITHOUT ROWID;

-- The builder has to know which visits contained automated traffic, because
-- `sessions` carries no bot flag of its own and the reports exclude bots by
-- default. Without this index that question is a full scan of the events table
-- on every build; with it, it is a range scan over the one and a half per cent
-- of rows that are bots.
CREATE INDEX events_bots ON events(site_id, session_id) WHERE bot_reason_id <> 0;
