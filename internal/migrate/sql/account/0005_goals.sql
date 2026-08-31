--
-- 0005_goals.sql
-- Goals, funnels, the property allow-list and the currency rates every revenue report divides by.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- All five tables live in the account database rather than in control.db for
-- the same reason the facts do: every query that reads a goal also reads the
-- events it matches, and a definition in another file would mean a cross-
-- database join on the one path that has to stay a single scan.
--
-- Definitions are per site inside the account. site_id cannot be a foreign key
-- for the same reason it is not one on `events`: sites live in control.db.

-- One goal. A goal is either a path pattern or an event name, never both, and
-- which one it is decides how it is matched.
CREATE TABLE goals (
    id           INTEGER PRIMARY KEY,
    site_id      INTEGER NOT NULL,

    -- 'page' or 'event'.
    kind         TEXT NOT NULL,

    -- What the customer calls it. Empty means "describe it from the pattern",
    -- which is what an automatic goal wants: renaming the event later should
    -- not leave a stale label behind.
    display_name TEXT NOT NULL DEFAULT '',

    -- The path pattern for a page goal, with * matching inside one path
    -- segment and ** matching across them. Stored already trimmed: a leading
    -- or trailing space is invisible in a text box and silently prevents every
    -- match, and it is the real cause behind reports that "wildcards interfere
    -- with each other".
    page_pattern TEXT NOT NULL DEFAULT '',

    -- The event name for a custom-event goal, matched exactly and also stored
    -- trimmed.
    event_name   TEXT NOT NULL DEFAULT '',

    -- A revenue-bearing goal reports money as well as conversions. The
    -- currency is the one the customer sets the goal up in; an event arriving
    -- in another currency is converted for reporting, never rewritten.
    is_revenue   INTEGER NOT NULL DEFAULT 0,
    currency     TEXT NOT NULL DEFAULT '',

    -- Created by us rather than by a person: the 404 goal, outbound clicks,
    -- downloads and form submissions. It is a column rather than a guess from
    -- the name so that a customer who renames one keeps it.
    is_automatic INTEGER NOT NULL DEFAULT 0,

    -- Goals do not backfill. Every conversion query starts at the later of the
    -- report range and this instant, so a goal created today cannot claim last
    -- month's traffic — and the UI says so at creation time rather than
    -- letting somebody discover it from an empty report.
    created_at   INTEGER NOT NULL,

    -- Everything that decides which events this goal matches, rendered as one
    -- string: the kind, the pattern or name, and the property constraints in a
    -- fixed order. It exists because the constraints live in another table and
    -- a unique index cannot reach them, and without it "Purchase" and
    -- "Purchase where plan is growth" would be the same row.
    signature    TEXT NOT NULL DEFAULT ''
);

-- One definition per site, so creating the same goal twice is a no-op rather
-- than a report that counts every conversion twice.
CREATE UNIQUE INDEX goals_definition ON goals(site_id, signature);
CREATE INDEX goals_site ON goals(site_id);

-- Property constraints on a goal: "Purchase where plan is growth". At most
-- three per goal, enforced in code — the limit is a product decision rather
-- than a storage one, and a CHECK constraint could not count rows anyway.
CREATE TABLE goal_properties (
    id      INTEGER PRIMARY KEY,
    goal_id INTEGER NOT NULL REFERENCES goals(id) ON DELETE CASCADE,
    name    TEXT NOT NULL,
    value   TEXT NOT NULL
);

CREATE INDEX goal_properties_goal ON goal_properties(goal_id);

-- The allow-list of custom properties, each with the scope it was registered
-- under. The scope is the whole point of the table: an event-scoped property
-- describes one hit ("which product"), a session-scoped one describes the
-- whole visit ("which A/B variant"), and a conversion rate filtered by the
-- second must divide by the visitors in that variant rather than by everybody.
-- Without a declared scope there is no way to tell the two apart, and the
-- denominator is silently wrong for one of them.
CREATE TABLE allowed_properties (
    id         INTEGER PRIMARY KEY,
    site_id    INTEGER NOT NULL,

    -- The property name as the tracker sends it, up to 300 characters.
    name       TEXT NOT NULL,

    -- 'event' or 'session'.
    scope      TEXT NOT NULL,

    created_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX allowed_properties_name ON allowed_properties(site_id, name);

-- A funnel is an ordered list of goals. Two to eight steps: one step is a goal
-- report and nine is a chart nobody can read.
CREATE TABLE funnels (
    id           INTEGER PRIMARY KEY,
    site_id      INTEGER NOT NULL,
    name         TEXT NOT NULL,

    -- Strict order means the steps have to happen in sequence within one
    -- visit. With it off, a visit that did all of them in any order counts.
    -- The two answer different questions — "did the flow work" and "did they
    -- get there at all" — and neither is a superset of the other.
    strict_order INTEGER NOT NULL DEFAULT 1,

    created_at   INTEGER NOT NULL
);

CREATE UNIQUE INDEX funnels_name ON funnels(site_id, name);

-- The steps, in position order. Position starts at one so a step number in the
-- database reads the same as the step number on the chart.
CREATE TABLE funnel_steps (
    funnel_id INTEGER NOT NULL REFERENCES funnels(id) ON DELETE CASCADE,
    position  INTEGER NOT NULL,
    goal_id   INTEGER NOT NULL REFERENCES goals(id),

    PRIMARY KEY (funnel_id, position)
) WITHOUT ROWID;

-- Exchange rates, refreshed on a timer. A report that mixes currencies has to
-- convert somewhere, and doing it at read time against a stored rate is the
-- only version that can be re-run: converting at ingest would bake one
-- afternoon's rate into the stored amount forever.
--
-- Amounts stay integers in their own currency. The rate is a float because a
-- rate is not money — it is a ratio, it has no minor unit, and storing it as
-- scaled integers would only move the rounding somewhere less obvious.
CREATE TABLE currency_rates (
    -- The currency the money is in, and the currency the report is in.
    base       TEXT NOT NULL,
    quote      TEXT NOT NULL,

    rate       REAL NOT NULL,

    -- When we fetched it. Rates go stale, so a report can say how old the
    -- number it converted with is instead of implying it is live.
    fetched_at INTEGER NOT NULL,

    PRIMARY KEY (base, quote)
) WITHOUT ROWID;
