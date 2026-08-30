--
-- 0001_initial.sql
-- The control database: who the people are, what they own, and what we owe them.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Two conventions hold everywhere in this file.
--
-- Every timestamp is unix seconds in UTC, stored as INTEGER. Analytics rows use
-- the same unit, and a system that mixed ISO strings here with integers there
-- would need a conversion at every boundary and would eventually get one wrong.
--
-- Nothing secret is stored in a form we can read back. Session cookies, API
-- keys, invitation tokens and reset tokens are all kept as hashes, so a stolen
-- copy of this file cannot be replayed against the running service.
--
-- An account and a team are the same row. `teams.id` is the account id, and it
-- names the per-account analytics database at data/accounts/<id>/analytics.db.
-- Keeping them as one entity is what makes "a team's dashboard, export and
-- billing usage span its sites" a single-file query.

-- People who can sign in. The password hash is empty for someone who only ever
-- signs in with Google, and google_sub is NULL for someone who never links it;
-- both are allowed because either one alone is a complete identity.
CREATE TABLE users (
    id                  INTEGER PRIMARY KEY,
    email               TEXT NOT NULL COLLATE NOCASE UNIQUE,
    name                TEXT NOT NULL DEFAULT '',
    password_hash       TEXT NOT NULL DEFAULT '',
    google_sub          TEXT UNIQUE,
    email_verified_at   INTEGER,
    theme               TEXT NOT NULL DEFAULT 'system',

    -- Two-factor state. The recovery codes are a JSON array of hashes, not of
    -- codes: a recovery code is a password and is treated as one.
    totp_secret         TEXT NOT NULL DEFAULT '',
    totp_recovery_codes TEXT NOT NULL DEFAULT '',
    totp_enabled_at     INTEGER,

    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    last_seen_at        INTEGER
);

-- Login sessions, one row per signed-in browser. Expiry is a rolling 14-day
-- inactivity window pushed forward on use, so the column is a deadline rather
-- than a fixed lifetime, and the index on it is what lets a cleanup job delete
-- the expired rows without scanning the table.
CREATE TABLE user_sessions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    device_label TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
);

CREATE INDEX user_sessions_user ON user_sessions(user_id);
CREATE INDEX user_sessions_expiry ON user_sessions(expires_at);

-- A team owns sites, the subscription and one analytics database. The trial
-- lives entirely in these two columns because no card is collected up front,
-- so there is no payment provider record to ask about a trial that has not
-- become a subscription yet.
CREATE TABLE teams (
    id                   INTEGER PRIMARY KEY,
    name                 TEXT NOT NULL,
    trial_ends_at        INTEGER,

    -- How long we keep accepting events after a subscription lapses. Dropping
    -- a paying customer's traffic the instant a card fails loses data they can
    -- never get back, so a lapse costs access to the dashboard first.
    accept_traffic_until INTEGER,

    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);

-- Who is in a team and what they may do. The role list is a CHECK constraint
-- rather than a convention, because a typo in a role string would otherwise
-- fail open somewhere in an authorisation check.
CREATE TABLE team_memberships (
    id         INTEGER PRIMARY KEY,
    team_id    INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'editor', 'billing', 'viewer')),
    created_at INTEGER NOT NULL,

    UNIQUE (team_id, user_id)
);

CREATE INDEX team_memberships_user ON team_memberships(user_id);

-- Outstanding invitations. The row is the invitation: accepting it creates a
-- membership and deletes this, so an invitation cannot be redeemed twice.
CREATE TABLE team_invitations (
    id                 INTEGER PRIMARY KEY,
    team_id            INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    email              TEXT NOT NULL COLLATE NOCASE,
    role               TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'editor', 'billing', 'viewer')),
    token_hash         TEXT NOT NULL UNIQUE,
    invited_by_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at         INTEGER NOT NULL,
    expires_at         INTEGER NOT NULL,

    UNIQUE (team_id, email)
);

-- Folders group sites for someone managing dozens of them. A site with no
-- folder is at the top level, which is why folder_id is nullable rather than
-- pointing at a magic "root" row.
CREATE TABLE site_folders (
    id         INTEGER PRIMARY KEY,
    team_id    INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

-- The site index. This is the routing table for the whole system: an ingestor
-- turns a domain into an account id here, and that id is the analytics
-- database the event belongs in. account_id is teams.id under the name the
-- rest of the system uses for it.
--
-- Only the index lives here. Site-scoped configuration — goals, funnels,
-- allowed props, shield rules, segments, annotations, imports — lives in the
-- account database, so a dashboard query never has to open two files.
CREATE TABLE sites (
    id               INTEGER PRIMARY KEY,
    account_id       INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    domain           TEXT NOT NULL UNIQUE,
    display_name     TEXT NOT NULL DEFAULT '',

    -- IANA name. Reports are bucketed by the site's local day, so this decides
    -- what "yesterday" means for every number on the dashboard.
    timezone         TEXT NOT NULL DEFAULT 'Etc/UTC',

    -- A public dashboard needs no login at all. It is off by default because
    -- turning it on publishes the site's traffic to anyone with the URL.
    is_public        INTEGER NOT NULL DEFAULT 0,

    folder_id        INTEGER REFERENCES site_folders(id) ON DELETE SET NULL,

    -- The first day with real data, as a unix timestamp at the site's local
    -- midnight. Date pickers use it so nobody is offered a range that predates
    -- the site and comes back empty.
    stats_start_date INTEGER,

    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

CREATE INDEX sites_account ON sites(account_id);

-- Access to one site for someone who is not in the team at all. Agencies and
-- clients need this: a client should see their own site and nothing else about
-- the team that manages it.
CREATE TABLE guest_memberships (
    id         INTEGER PRIMARY KEY,
    site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('guest_editor', 'guest_viewer')),
    created_at INTEGER NOT NULL,

    UNIQUE (site_id, user_id)
);

CREATE INDEX guest_memberships_user ON guest_memberships(user_id);

-- Billing state, one row per team. Everything here is a mirror of the payment
-- provider's records: the provider is the source of truth, and this copy
-- exists so that a page load does not make a network call.
CREATE TABLE subscriptions (
    id                     INTEGER PRIMARY KEY,
    team_id                INTEGER NOT NULL UNIQUE REFERENCES teams(id) ON DELETE CASCADE,
    stripe_customer_id     TEXT,
    stripe_subscription_id TEXT,
    status                 TEXT NOT NULL DEFAULT 'none',
    plan                   TEXT NOT NULL DEFAULT '',
    current_period_end     INTEGER,
    created_at             INTEGER NOT NULL,
    updated_at             INTEGER NOT NULL
);

-- Billable volume per team per month, keyed by 'YYYY-MM' in UTC. It is counted
-- here rather than derived from the analytics databases because the limit
-- check runs on the ingest path, which must never open an account database,
-- and because usage has to survive a site being deleted.
CREATE TABLE usage_counters (
    id            INTEGER PRIMARY KEY,
    team_id       INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    period        TEXT NOT NULL,
    pageviews     INTEGER NOT NULL DEFAULT 0,
    custom_events INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL,

    UNIQUE (team_id, period)
);

-- API keys for the stats API. The key is stored as a hash and shown to its
-- owner exactly once. scopes is a JSON array so a key can be narrowed later
-- without a migration for every new scope.
CREATE TABLE api_keys (
    id           INTEGER PRIMARY KEY,
    team_id      INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL DEFAULT '',
    key_hash     TEXT NOT NULL UNIQUE,
    scopes       TEXT NOT NULL DEFAULT '[]',
    hourly_limit INTEGER NOT NULL DEFAULT 600,
    last_used_at INTEGER,
    created_at   INTEGER NOT NULL,
    revoked_at   INTEGER
);

CREATE INDEX api_keys_team ON api_keys(team_id);

-- Public dashboard links, one per shared view of a site. The password is
-- optional and hashed; the pinned segment is optional so a link can expose one
-- slice of a site rather than all of it.
CREATE TABLE shared_links (
    id            INTEGER PRIMARY KEY,
    site_id       INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    name          TEXT NOT NULL DEFAULT '',
    slug          TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL DEFAULT '',
    segment_id    INTEGER,
    created_at    INTEGER NOT NULL
);

CREATE INDEX shared_links_site ON shared_links(site_id);

-- The rotating fingerprint salts. Treat this table as being as sensitive as
-- raw IP logs: the visitor hash is pseudonymous, not anonymous, and anyone
-- holding a salt plus the stored hashes can brute-force the inputs back out.
-- Rows are deleted after 48 hours, and that deletion is the entire basis of
-- the "no cookies, no persistent identifiers" claim. Do not weaken it.
CREATE TABLE salts (
    id         INTEGER PRIMARY KEY,
    salt       BLOB NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX salts_created ON salts(created_at);

-- The background job queue. Every queue runs at concurrency 1, which suits a
-- system with one SQLite writer per account and makes batch work deliberately
-- serial.
CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY,
    queue        TEXT NOT NULL,
    kind         TEXT NOT NULL,
    args         TEXT NOT NULL,
    state        TEXT NOT NULL DEFAULT 'available'
                 CHECK (state IN ('available', 'executing', 'completed', 'discarded')),
    attempt      INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 20,
    scheduled_at INTEGER NOT NULL,
    attempted_at INTEGER,
    completed_at INTEGER,
    last_error   TEXT,

    -- Set by jobs that must not double-enqueue. An import that runs twice
    -- doubles a customer's numbers, and no later check can tell which half was
    -- the duplicate.
    unique_key   TEXT
);

-- The claim query: the oldest available job in a queue whose time has come.
CREATE INDEX jobs_claim ON jobs(state, queue, scheduled_at);

-- Uniqueness only applies while a job is live. Once it has completed or been
-- discarded the same key must be enqueueable again, otherwise an hourly job
-- could only ever run once.
CREATE UNIQUE INDEX jobs_unique_key ON jobs(unique_key)
    WHERE unique_key IS NOT NULL AND state IN ('available', 'executing');

-- Short-lived codes emailed to prove an address. Stored hashed and consumed on
-- use, so a code that leaks from a mail spool cannot be replayed.
CREATE TABLE email_verification_codes (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash   TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    consumed_at INTEGER
);

CREATE INDEX email_verification_codes_user ON email_verification_codes(user_id);

-- Password reset tokens. Same rules as the verification codes, kept separate
-- because a reset token grants far more than proving an address does.
CREATE TABLE password_reset_tokens (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    consumed_at INTEGER
);
