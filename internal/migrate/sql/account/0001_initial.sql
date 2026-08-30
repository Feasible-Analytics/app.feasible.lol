--
-- 0001_initial.sql
-- One account's analytics database: the two fact tables and their dimensions.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Two decisions govern this schema, and both exist to make queries read less
-- off disk. They are cheap now and a painful rewrite later, which is why they
-- are in the first migration.
--
-- 1. Dimension strings are interned. Every *_id column below is an integer into
--    a small dim_* table rather than a repeated string. A popular path is the
--    same 25 characters written on forty thousand rows; as an integer it is a
--    sixth of the space, and GROUP BY on an integer beats GROUP BY on text,
--    which is what every report does. Rows go from roughly 300 bytes to 80.
--
-- 2. Hot and cold columns are split. SQLite reads the whole row off disk even
--    when a query wants three columns, so a props blob in the same row is
--    dragged through every scan that never looks at it.
--
-- site_id points at sites.id in the control database. It cannot be a foreign
-- key because that table lives in a different file, and that is the trade the
-- per-account layout makes: no cross-database joins on the query path, at the
-- cost of one reference the database cannot enforce for us.

-- The hot table, append-only. Every row carries a full copy of its session's
-- acquisition, geo and device attributes. The duplication is deliberate: disk
-- is cheap and it removes a join from every dashboard query.
CREATE TABLE events (
    id                 INTEGER PRIMARY KEY,
    site_id            INTEGER NOT NULL,
    timestamp          INTEGER NOT NULL,          -- unix seconds, UTC
    name_id            INTEGER NOT NULL,          -- dim_event_name: pageview | engagement | custom
    user_id            INTEGER NOT NULL,          -- SipHash fingerprint, not a person
    session_id         INTEGER NOT NULL,

    hostname_id        INTEGER NOT NULL DEFAULT 0,
    pathname_id        INTEGER NOT NULL DEFAULT 0,
    page_title_id      INTEGER NOT NULL DEFAULT 0,

    referrer_id        INTEGER NOT NULL DEFAULT 0,
    source_id          INTEGER NOT NULL DEFAULT 0,
    channel_id         INTEGER NOT NULL DEFAULT 0,
    utm_source_id      INTEGER NOT NULL DEFAULT 0,
    utm_medium_id      INTEGER NOT NULL DEFAULT 0,
    utm_campaign_id    INTEGER NOT NULL DEFAULT 0,

    country_id         INTEGER NOT NULL DEFAULT 0,
    region_id          INTEGER NOT NULL DEFAULT 0,

    -- Not interned: a geoname id is already an integer from the geolocation
    -- database, so a lookup table would only add a level of indirection to a
    -- number that is stable across every account we run.
    city_geoname_id    INTEGER NOT NULL DEFAULT 0,

    device_type_id     INTEGER NOT NULL DEFAULT 0,
    screen_size_id     INTEGER NOT NULL DEFAULT 0,
    browser_id         INTEGER NOT NULL DEFAULT 0,
    browser_version_id INTEGER NOT NULL DEFAULT 0,
    os_id              INTEGER NOT NULL DEFAULT 0,
    os_version_id      INTEGER NOT NULL DEFAULT 0,
    language_id        INTEGER NOT NULL DEFAULT 0,

    -- 255 means "never reported", which is outside the 0-100 range a real
    -- measurement can take, so the average of real scroll depths never has to
    -- exclude a NULL.
    scroll_depth       INTEGER NOT NULL DEFAULT 255,
    engagement_time    INTEGER NOT NULL DEFAULT 0, -- milliseconds

    bot_reason_id      INTEGER NOT NULL DEFAULT 0, -- 0 = human
    is_imported        INTEGER NOT NULL DEFAULT 0,

    -- Whether this row has an event_details partner. The common query path
    -- checks the flag instead of attempting a join that would miss.
    has_details        INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX events_main ON events(site_id, timestamp, name_id);
CREATE INDEX events_session ON events(session_id);

-- Filtered queries cannot use roll-ups, so these three are what keep them off a
-- full scan. They are the dimensions people actually filter on.
CREATE INDEX events_page ON events(site_id, pathname_id, timestamp);
CREATE INDEX events_source ON events(site_id, source_id, timestamp);
CREATE INDEX events_country ON events(site_id, country_id, timestamp);

-- The cold table. Written only when there is something to write, and read only
-- when a query actually asks for props, revenue or the long-tail UTM fields.
CREATE TABLE event_details (
    event_id         INTEGER PRIMARY KEY REFERENCES events(id),
    props            TEXT,    -- JSON object
    revenue_amount   INTEGER, -- minor units, never float
    revenue_currency TEXT,
    utm_content      TEXT,
    utm_term         TEXT,
    full_url         TEXT     -- when the customer opts into full-URL capture
);

-- Sessions are mutable: one row per session, updated in place. A column store
-- that cannot UPDATE needs a sign column and a collapsing merge to fake this,
-- and every average becomes a ratio of signed sums. We just update the row.
CREATE TABLE sessions (
    id                 INTEGER PRIMARY KEY,
    site_id            INTEGER NOT NULL,
    user_id            INTEGER NOT NULL,

    started_at         INTEGER NOT NULL,
    last_seen_at       INTEGER NOT NULL,

    -- Recomputed from started_at on every event rather than accumulated, so a
    -- retried or out-of-order event cannot inflate it.
    duration           INTEGER NOT NULL DEFAULT 0, -- seconds

    -- Starts true and becomes false once a second pageview or an interactive
    -- event arrives. Once false it is never true again.
    is_bounce          INTEGER NOT NULL DEFAULT 1,

    pageviews          INTEGER NOT NULL DEFAULT 0,
    events             INTEGER NOT NULL DEFAULT 0,

    entry_page_id      INTEGER NOT NULL DEFAULT 0,
    exit_page_id       INTEGER NOT NULL DEFAULT 0,
    entry_hostname_id  INTEGER NOT NULL DEFAULT 0,
    exit_hostname_id   INTEGER NOT NULL DEFAULT 0,

    -- The first event's properties, kept so a funnel or goal report can filter
    -- whole sessions by how they started.
    entry_props        TEXT,

    -- The same interned acquisition, geo and device block as events, fixed at
    -- the session's first event. A visitor's source does not change mid-visit,
    -- and holding it here is what makes a session-grain report a single scan.
    referrer_id        INTEGER NOT NULL DEFAULT 0,
    source_id          INTEGER NOT NULL DEFAULT 0,
    channel_id         INTEGER NOT NULL DEFAULT 0,
    utm_source_id      INTEGER NOT NULL DEFAULT 0,
    utm_medium_id      INTEGER NOT NULL DEFAULT 0,
    utm_campaign_id    INTEGER NOT NULL DEFAULT 0,

    country_id         INTEGER NOT NULL DEFAULT 0,
    region_id          INTEGER NOT NULL DEFAULT 0,
    city_geoname_id    INTEGER NOT NULL DEFAULT 0,

    device_type_id     INTEGER NOT NULL DEFAULT 0,
    screen_size_id     INTEGER NOT NULL DEFAULT 0,
    browser_id         INTEGER NOT NULL DEFAULT 0,
    browser_version_id INTEGER NOT NULL DEFAULT 0,
    os_id              INTEGER NOT NULL DEFAULT 0,
    os_version_id      INTEGER NOT NULL DEFAULT 0,
    language_id        INTEGER NOT NULL DEFAULT 0,

    is_imported        INTEGER NOT NULL DEFAULT 0
);

-- The ingest path's only session query: does this visitor already have a live
-- session on this site? Ordering by last_seen_at inside the key means the most
-- recent row is found without a sort.
CREATE INDEX sessions_visitor ON sessions(site_id, user_id, last_seen_at);

-- Session-grain reports — visits, bounce rate, visit duration — scan a date
-- range rather than a visitor.
CREATE INDEX sessions_range ON sessions(site_id, started_at);

-- Dimension tables, one per interned column, shared across every site in the
-- account. They are small, hot and stay in page cache.
--
-- Id 0 is always the empty string in every one of them, so "not set" is an
-- ordinary id rather than a NULL that every query, index and GROUP BY would
-- have to handle specially. It is seeded here because the interning cache
-- assumes it, and the cache is on the ingest hot path.
CREATE TABLE dim_event_name      (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_hostname        (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_pathname        (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_page_title      (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_referrer        (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_source          (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_channel         (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_utm_source      (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_utm_medium      (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_utm_campaign    (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_country         (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_region          (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_device_type     (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_screen_size     (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_browser         (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_browser_version (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_os              (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_os_version      (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_language        (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_bot_reason      (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);

INSERT INTO dim_event_name      (id, value) VALUES (0, '');
INSERT INTO dim_hostname        (id, value) VALUES (0, '');
INSERT INTO dim_pathname        (id, value) VALUES (0, '');
INSERT INTO dim_page_title      (id, value) VALUES (0, '');
INSERT INTO dim_referrer        (id, value) VALUES (0, '');
INSERT INTO dim_source          (id, value) VALUES (0, '');
INSERT INTO dim_channel         (id, value) VALUES (0, '');
INSERT INTO dim_utm_source      (id, value) VALUES (0, '');
INSERT INTO dim_utm_medium      (id, value) VALUES (0, '');
INSERT INTO dim_utm_campaign    (id, value) VALUES (0, '');
INSERT INTO dim_country         (id, value) VALUES (0, '');
INSERT INTO dim_region          (id, value) VALUES (0, '');
INSERT INTO dim_device_type     (id, value) VALUES (0, '');
INSERT INTO dim_screen_size     (id, value) VALUES (0, '');
INSERT INTO dim_browser         (id, value) VALUES (0, '');
INSERT INTO dim_browser_version (id, value) VALUES (0, '');
INSERT INTO dim_os              (id, value) VALUES (0, '');
INSERT INTO dim_os_version      (id, value) VALUES (0, '');
INSERT INTO dim_language        (id, value) VALUES (0, '');
INSERT INTO dim_bot_reason      (id, value) VALUES (0, '');
