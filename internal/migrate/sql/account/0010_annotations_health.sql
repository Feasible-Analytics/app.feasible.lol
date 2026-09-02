--
-- 0010_annotations_health.sql
-- Annotations on the graph, and the record behind the ingestion health panel.
--
-- Created: 2026-08-31
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Requires: the deployed topology through account migration 0009. M9 remains
-- account migration 0010 and does not rewrite topology-owned ingest tables.
--
-- Both of these are site-scoped configuration and history, which is why they
-- live in the account database beside the events they describe rather than in
-- system.db. A health panel that had to open two files to answer "how many
-- events did I drop yesterday" would be a cross-database join on the one page
-- somebody opens when they already think the product is broken.

-- A dated note rendered as a marker on the main graph. The date is stored as
-- 'YYYY-MM-DD' in the site's own timezone rather than as an instant, because an
-- annotation is about a day — "we launched", "the outage" — and converting an
-- instant back to a day would move the marker for anybody reading the dashboard
-- from another continent.
CREATE TABLE annotations (
    id             INTEGER PRIMARY KEY,
    site_id        INTEGER NOT NULL,
    shown_on       TEXT NOT NULL,
    body           TEXT NOT NULL,

    -- Who wrote it, denormalised. The users table is in another database, and
    -- a marker's tooltip must still say "Sam" after Sam has left the team.
    author_user_id INTEGER NOT NULL DEFAULT 0,
    author_name    TEXT NOT NULL DEFAULT '',

    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

CREATE INDEX annotations_site_date ON annotations(site_id, shown_on);

-- What happened to the traffic, per site, per observed second. The ingest process counts
-- in memory and flushes deltas in here, because a row per event would double
-- the write volume of the whole system. Events sharing a second still collapse
-- into one row while the panel keeps a precise rolling-day boundary.
--
-- kind separates the three facts that a single count would blur together:
-- accepted, thrown away, filed as a bot but still stored, and kept but cut
-- short. Telling a customer "we classified this as a bot" and "we threw this
-- away" with the same number is how somebody concludes their data is gone.
CREATE TABLE ingest_health (
    id      INTEGER PRIMARY KEY,
    site_id INTEGER NOT NULL,

    -- Exact unix second. A coarse hour bucket cannot answer a rolling 24-hour
    -- query at 12:37 without including traffic from 12:00 the previous day.
    observed_at INTEGER NOT NULL,

    kind    TEXT NOT NULL CHECK (kind IN ('accepted', 'dropped', 'classified', 'truncated')),

    -- One of the closed reason sets in internal/ingest/counters.go. Empty for
    -- the accepted count, which has no reason.
    reason  TEXT NOT NULL DEFAULT '',

    count   INTEGER NOT NULL DEFAULT 0,

    UNIQUE (site_id, observed_at, kind, reason)
);

CREATE INDEX ingest_health_window ON ingest_health(site_id, observed_at);

-- The last request each site sent us, fully derived. This is the debug output
-- the customer would otherwise have to produce with curl and the
-- X-Debug-Request header, kept where they can read it: which address we
-- resolved, and which header we believed. Four separate incumbent bugs share
-- that one root cause, and every one of them failed silently behind a 202.
CREATE TABLE ingest_last_request (
    site_id          INTEGER PRIMARY KEY,
    received_at      INTEGER NOT NULL,
    client_ip        TEXT NOT NULL DEFAULT '',
    client_ip_source TEXT NOT NULL DEFAULT '',
    trusted_proxy    INTEGER NOT NULL DEFAULT 0,
    hostname         TEXT NOT NULL DEFAULT '',
    pathname         TEXT NOT NULL DEFAULT '',
    user_agent       TEXT NOT NULL DEFAULT '',
    tracker_version  INTEGER NOT NULL DEFAULT 0,
    drop_reason      TEXT NOT NULL DEFAULT '',

    -- The whole derived event as JSON, so the panel can show every field
    -- without this table having to grow a column per derivation step.
    debug            TEXT NOT NULL DEFAULT '{}'
);

-- Rolling counts of the things a warning is derived from. They are counted
-- rather than sampled because every warning on the panel names a number, and a
-- warning that cannot say "on 412 events" is a warning nobody acts on.
CREATE TABLE ingest_observations (
    id            INTEGER PRIMARY KEY,
    site_id       INTEGER NOT NULL,

    -- Exact unix second for the same rolling-boundary guarantee as the health
    -- counts above.
    observed_at   INTEGER NOT NULL,

    -- unknown_hostname  a hostname sending events that is not on the allow-list
    -- tracker_version   which build of the script is in the wild
    -- ip_source         which header the client address came from
    kind          TEXT NOT NULL CHECK (kind IN ('unknown_hostname', 'tracker_version', 'ip_source')),

    value         TEXT NOT NULL,
    count         INTEGER NOT NULL DEFAULT 0,
    first_seen_at INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL,

    UNIQUE (site_id, observed_at, kind, value)
);

CREATE INDEX ingest_observations_recent ON ingest_observations(site_id, observed_at, kind);
