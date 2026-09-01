--
-- 0007_imports_shields_paths.sql
-- Imported history, exports, shield rules, path cleaning and the Google links.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Everything here is site-scoped configuration or site-scoped data, so it lives
-- in the account database rather than in control.db. That is the same rule the
-- first control migration states: only the routing index is central, so a
-- dashboard query never has to open two files.

-- One row per import run, whatever the source. Progress and the failure reason
-- are columns rather than log lines because "my import is stuck" and "my import
-- failed and I do not know why" are the two support tickets this feature
-- generates, and both are answerable only if the state is queryable.
CREATE TABLE imports (
    id             INTEGER PRIMARY KEY,
    site_id        INTEGER NOT NULL,

    -- csv | ga4 | search_console.
    source         TEXT NOT NULL,

    -- What the customer sees in the list: the uploaded filename, or the GA4
    -- property. Free text, because it is a label and never a key.
    label          TEXT NOT NULL DEFAULT '',

    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'running', 'completed', 'failed', 'cancelled')),

    -- Progress is two counters rather than a percentage so the UI can say
    -- "4 of 11 files" instead of a bar that jumps from 0 to 100.
    progress_done  INTEGER NOT NULL DEFAULT 0,
    progress_total INTEGER NOT NULL DEFAULT 0,
    rows_written   INTEGER NOT NULL DEFAULT 0,

    -- The dimensions this import genuinely carries, as a JSON array of query
    -- dimension names. A filter on anything outside it cannot be answered from
    -- imported rows, and the query engine reports that as a labelled gap rather
    -- than quietly contributing zero.
    dimensions     TEXT NOT NULL DEFAULT '[]',

    -- Where a resumable import got to. A GA4 or Search Console run is a walk
    -- over days, and restarting one from the beginning after a token refresh
    -- would double every number it had already written.
    cursor         TEXT NOT NULL DEFAULT '',

    -- The sentence shown to the customer when status is failed. It is written
    -- for them, not for a log: "row 412 of imported_pages.csv: date 2026-13-02
    -- is not a date" rather than a stack trace.
    failure        TEXT NOT NULL DEFAULT '',

    -- The uploaded file, already copied into the data directory. It is a copy
    -- and never a rename: rename(2) fails with EXDEV across a device boundary,
    -- which is the normal shape of a Docker bind mount and of a NAS.
    upload_path    TEXT NOT NULL DEFAULT '',

    range_start    INTEGER,
    range_end      INTEGER,

    created_at     INTEGER NOT NULL,
    started_at     INTEGER,
    completed_at   INTEGER
);

CREATE INDEX imports_site ON imports(site_id, created_at);

-- Imported history as roll-up rows. One row is one day and one combination of
-- the dimensions that day's source actually reported, with a counter per
-- metric — not one row per pageview, which for a site with 60M of them is not a
-- workable import at all.
--
-- The column that makes this different from the incumbent's marginal totals is
-- `covered`. Every row records which dimensions it genuinely carries, so a
-- filtered query can tell "this row does not match the filter" apart from "this
-- row cannot answer the filter". The first narrows; the second is reported to
-- the reader as a labelled gap. The incumbent has neither distinction, which is
-- why applying any filter zeroes their imported data out.
CREATE TABLE imported_rollups (
    id                 INTEGER PRIMARY KEY,
    import_id          INTEGER NOT NULL REFERENCES imports(id) ON DELETE CASCADE,
    site_id            INTEGER NOT NULL,

    -- Unix seconds at the site's local midnight, so the same bucket expression
    -- every other report uses puts this row in the same local day the customer
    -- saw in the product it came from.
    timestamp          INTEGER NOT NULL,

    -- Bitmask of the dimensions this row carries; see the query package for the
    -- bit assignment. Zero means a row that carries only a date.
    covered            INTEGER NOT NULL DEFAULT 0,

    name_id            INTEGER NOT NULL DEFAULT 0,
    hostname_id        INTEGER NOT NULL DEFAULT 0,
    pathname_id        INTEGER NOT NULL DEFAULT 0,
    entry_page_id      INTEGER NOT NULL DEFAULT 0,
    exit_page_id       INTEGER NOT NULL DEFAULT 0,
    page_title_id      INTEGER NOT NULL DEFAULT 0,
    referrer_id        INTEGER NOT NULL DEFAULT 0,
    source_id          INTEGER NOT NULL DEFAULT 0,
    channel_id         INTEGER NOT NULL DEFAULT 0,
    utm_source_id      INTEGER NOT NULL DEFAULT 0,
    utm_medium_id      INTEGER NOT NULL DEFAULT 0,
    utm_campaign_id    INTEGER NOT NULL DEFAULT 0,
    country_id         INTEGER NOT NULL DEFAULT 0,
    region_id          INTEGER NOT NULL DEFAULT 0,
    city_id            INTEGER NOT NULL DEFAULT 0,
    device_type_id     INTEGER NOT NULL DEFAULT 0,
    screen_size_id     INTEGER NOT NULL DEFAULT 0,
    browser_id         INTEGER NOT NULL DEFAULT 0,
    browser_version_id INTEGER NOT NULL DEFAULT 0,
    os_id              INTEGER NOT NULL DEFAULT 0,
    os_version_id      INTEGER NOT NULL DEFAULT 0,
    language_id        INTEGER NOT NULL DEFAULT 0,

    visitors           INTEGER NOT NULL DEFAULT 0,
    visits             INTEGER NOT NULL DEFAULT 0,
    pageviews          INTEGER NOT NULL DEFAULT 0,
    events             INTEGER NOT NULL DEFAULT 0,
    exits              INTEGER NOT NULL DEFAULT 0,
    bounces            INTEGER NOT NULL DEFAULT 0,

    -- Totals rather than averages. An average of averages is wrong the moment
    -- two rows are added together, and every roll-up row is added to another.
    duration_total     INTEGER NOT NULL DEFAULT 0, -- seconds across `visits`
    engagement_total   INTEGER NOT NULL DEFAULT 0  -- milliseconds
);

CREATE INDEX imported_rollups_range ON imported_rollups(site_id, timestamp);
CREATE INDEX imported_rollups_import ON imported_rollups(import_id);

-- Search Console rows. They are kept out of `imported_rollups` because a search
-- query is not one of our dimensions and never will be: it is Google's data
-- about Google's index, not a property of a visit we measured.
CREATE TABLE search_console_daily (
    id          INTEGER PRIMARY KEY,
    site_id     INTEGER NOT NULL,
    timestamp   INTEGER NOT NULL, -- site-local midnight, as unix seconds
    query       TEXT NOT NULL,
    page        TEXT NOT NULL DEFAULT '',
    country     TEXT NOT NULL DEFAULT '',
    device      TEXT NOT NULL DEFAULT '',
    clicks      INTEGER NOT NULL DEFAULT 0,
    impressions INTEGER NOT NULL DEFAULT 0,

    -- The average position weighted by impressions, kept as a sum so that two
    -- days can be added together without averaging two averages.
    position_x1000_total INTEGER NOT NULL DEFAULT 0,

    UNIQUE (site_id, timestamp, query, page, country, device)
);

CREATE INDEX search_console_range ON search_console_daily(site_id, timestamp);

-- A prepared export and the window it can be downloaded in. The token is stored
-- hashed for the same reason every other token in this system is: a link that
-- leaks out of a mail spool or a browser history must not be replayable.
CREATE TABLE exports (
    id           INTEGER PRIMARY KEY,
    site_id      INTEGER NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    path         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    bytes        INTEGER NOT NULL DEFAULT 0,
    failure      TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    completed_at INTEGER,

    -- Twenty-four hours from creation. A prepared export is a full copy of a
    -- customer's traffic sitting on disk behind a URL, so it expires whether or
    -- not anybody downloaded it.
    expires_at   INTEGER NOT NULL
);

CREATE INDEX exports_site ON exports(site_id, created_at);
CREATE INDEX exports_expiry ON exports(expires_at);

-- Shield rules: traffic the customer does not want counted. Four kinds, thirty
-- rules each per site, and the cap is enforced in code rather than here because
-- a CHECK constraint cannot count sibling rows and a trigger that could would
-- fail with a message nobody can act on.
--
-- IP rules are evaluated during request derivation, the only place the raw
-- address still exists. Country, page and hostname rules are evaluated by the
-- authoritative account writer, where this table is live.
CREATE TABLE shield_rules (
    id         INTEGER PRIMARY KEY,
    site_id    INTEGER NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('ip', 'country', 'page', 'hostname')),

    -- An address or CIDR block, an ISO country code, a path or path prefix, or
    -- a hostname. Stored in the normalised form the evaluator compares against
    -- so that matching never depends on how somebody typed it.
    value      TEXT NOT NULL,

    note       TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,

    UNIQUE (site_id, kind, value)
);

CREATE INDEX shield_rules_site ON shield_rules(site_id, kind);

-- Path cleaning rules, in the order they are tried. First match wins, which is
-- what lets a specific rule sit above a general one without the general one
-- swallowing it.
CREATE TABLE path_clean_rules (
    id          INTEGER PRIMARY KEY,
    site_id     INTEGER NOT NULL,
    position    INTEGER NOT NULL,
    pattern     TEXT NOT NULL,
    replacement TEXT NOT NULL,
    label       TEXT NOT NULL DEFAULT '',

    -- The trailing-slash rule ships disabled. Merging /about and /about/ is
    -- right for most sites and wrong for the few that serve different content
    -- at each, so it is one click rather than a decision made for them.
    is_enabled  INTEGER NOT NULL DEFAULT 1,

    created_at  INTEGER NOT NULL,

    UNIQUE (site_id, position)
);

-- The materialised result of applying the rules to every path this account has
-- ever interned. Query time reads this map rather than running a regular
-- expression per row, and because it maps ids rather than rewriting them, the
-- stored events are untouched: changing a rule changes every historical report
-- at once, and deleting every rule puts the original paths back.
CREATE TABLE path_clean_map (
    site_id   INTEGER NOT NULL,
    source_id INTEGER NOT NULL,
    target_id INTEGER NOT NULL,

    PRIMARY KEY (site_id, source_id)
) WITHOUT ROWID;

-- One Google connection per site and provider, never one per Google account.
-- Sharing a token row between two sites is what let connecting a second site
-- invalidate the first site's refresh token on an incumbent's self-hosted
-- build, and the UNIQUE below is what makes that impossible here.
CREATE TABLE google_connections (
    id            INTEGER PRIMARY KEY,
    site_id       INTEGER NOT NULL,

    -- The owning account, carried so a connection can be found and revoked
    -- without a join back to the control database.
    account_id    INTEGER NOT NULL,

    provider      TEXT NOT NULL CHECK (provider IN ('ga4', 'search_console')),

    -- Which Google account authorised it, for display only. Two sites may hold
    -- two independent grants from the same Google account, which is the entire
    -- point of keying this table the way it is.
    google_email  TEXT NOT NULL DEFAULT '',

    -- The GA4 property id or the Search Console site URL.
    property      TEXT NOT NULL DEFAULT '',

    refresh_token TEXT NOT NULL DEFAULT '',
    access_token  TEXT NOT NULL DEFAULT '',
    expires_at    INTEGER,
    scopes        TEXT NOT NULL DEFAULT '',

    -- connected | needs_reconnect. A refresh that comes back invalid_grant sets
    -- the second, and the settings page turns that into a reconnect button
    -- instead of an import that fails every night with nobody watching.
    status        TEXT NOT NULL DEFAULT 'connected'
                  CHECK (status IN ('connected', 'needs_reconnect')),

    failure       TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,

    UNIQUE (site_id, provider)
);
