--
-- 0003_public_api.sql
-- The public API: keys people can actually use, webhooks, and MCP's OAuth clients.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Everything here lives in control.db rather than in an account database for one
-- reason each, and the reason matters more than the convention:
--
--   * A key, an OAuth client and a rate limit are checked before we know which
--     account a request is for. Putting them in an account database would mean
--     opening a file to find out whether the caller may open it.
--   * A webhook delivery is a job, and the job queue is here. A delivery row in
--     one file and its job row in another cannot be enqueued in one transaction,
--     and a delivery that exists with no job is a payload nobody will ever send.
--   * Tracker configuration is read by the script route, which runs in processes
--     that hold no account database at all.

-- The Stats and Sites API keys, rebuilt so that the shipped default limit is one
-- real integrations can live inside. The incumbent's default is 600 an hour,
-- hard-coded even for people running it on their own hardware, and the
-- documented workaround is an UPDATE against their database.
--
-- The table is rebuilt rather than altered because the change is to a column
-- default, which SQLite can only express by writing the table again.
CREATE TABLE api_keys_new (
    id           INTEGER PRIMARY KEY,
    team_id      INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL DEFAULT '',

    -- SHA-256 of the key, hex. The key itself is shown once, at creation, and
    -- is never recoverable: a stolen copy of this file must not be replayable
    -- against the running service.
    key_hash     TEXT NOT NULL UNIQUE,

    -- The first few characters of the key, kept in the clear so the dashboard
    -- and the CLI can show somebody which of their keys a row is. Without it
    -- the only way to identify a key is to revoke it and see what breaks.
    key_prefix   TEXT NOT NULL DEFAULT '',

    -- A JSON array. Empty means every scope: one key type, self-serve, works
    -- for stats and for provisioning, because making integrators email support
    -- for a second kind of key is a tax on the people trying hardest to adopt.
    scopes       TEXT NOT NULL DEFAULT '[]',

    -- Requests per hour. Zero means "whatever the deployment is configured for",
    -- so raising the limit for everybody is one environment variable rather than
    -- an UPDATE over every row.
    hourly_limit INTEGER NOT NULL DEFAULT 0,

    last_used_at INTEGER,
    created_at   INTEGER NOT NULL,
    revoked_at   INTEGER
);

INSERT INTO api_keys_new (id, team_id, user_id, name, key_hash, scopes, hourly_limit, last_used_at, created_at, revoked_at)
SELECT id, team_id, user_id, name, key_hash, scopes, 0, last_used_at, created_at, revoked_at FROM api_keys;

DROP TABLE api_keys;
ALTER TABLE api_keys_new RENAME TO api_keys;

CREATE INDEX api_keys_team ON api_keys(team_id);

-- Per-site tracker configuration, which is what the snippet the customer copies
-- is built from. It is one row per site rather than columns on `sites` so that a
-- new tracker option is a JSON key rather than a migration against the table the
-- routing map is rebuilt from every fifteen seconds.
CREATE TABLE site_tracker_config (
    site_id           INTEGER PRIMARY KEY REFERENCES sites(id) ON DELETE CASCADE,

    -- Where the script posts. Empty means the origin it was loaded from, which
    -- is what makes a reverse proxy work with no second setting to keep in sync.
    api_endpoint      TEXT NOT NULL DEFAULT '',

    -- Track pages whose URL only differs after the #, for single-page apps that
    -- route on the hash.
    hash_routing      INTEGER NOT NULL DEFAULT 0,

    -- Send no pageview until the page calls the tracker itself.
    manual_tagging    INTEGER NOT NULL DEFAULT 0,

    -- Count outbound links, file downloads and 404s as events.
    outbound_links    INTEGER NOT NULL DEFAULT 0,
    file_downloads    INTEGER NOT NULL DEFAULT 0,
    track_404         INTEGER NOT NULL DEFAULT 0,

    -- Report from localhost too. Off by default because a developer's own
    -- reloads are otherwise indistinguishable from real traffic.
    track_localhost   INTEGER NOT NULL DEFAULT 0,

    -- Comma-separated path patterns to leave out, and file extensions to count
    -- as downloads. Free text because both are the customer's vocabulary.
    excluded_pages    TEXT NOT NULL DEFAULT '',
    file_types        TEXT NOT NULL DEFAULT '',

    updated_at        INTEGER NOT NULL
);

-- The custom properties a site is allowed to report, which is what the property
-- picker offers and what an export knows to include. It is an allow list rather
-- than a discovered list so that a typo'd property name in one deploy does not
-- become a permanent column in everybody's dashboard.
CREATE TABLE site_custom_properties (
    id         INTEGER PRIMARY KEY,
    site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    created_at INTEGER NOT NULL,

    UNIQUE (site_id, key)
);

-- One customer endpoint we push events to.
--
-- The signing secret is stored in a form we can read, unlike every other secret
-- in this database. That is not an oversight: this is a key we sign *with*, not
-- a credential somebody presents *to* us, so a hash would make it useless. The
-- previous secret is kept for a grace period so that rotating does not drop the
-- deliveries already in flight against a receiver that has not redeployed yet.
CREATE TABLE webhook_endpoints (
    id                      INTEGER PRIMARY KEY,
    team_id                 INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,

    -- Optional. A webhook scoped to one site only fires for that site's events;
    -- NULL means every site the team owns.
    site_id                 INTEGER REFERENCES sites(id) ON DELETE CASCADE,

    url                     TEXT NOT NULL,
    description             TEXT NOT NULL DEFAULT '',

    -- JSON array of event type names. An empty array means every type, so a
    -- customer who just wants everything does not have to keep the list up to
    -- date as we add types.
    event_types             TEXT NOT NULL DEFAULT '[]',

    secret                  TEXT NOT NULL,
    previous_secret         TEXT NOT NULL DEFAULT '',
    previous_secret_until   INTEGER,

    enabled                 INTEGER NOT NULL DEFAULT 1,

    -- Reset to zero by any delivery that succeeds. The warning email is sent
    -- when this crosses the warning threshold and the endpoint is disabled when
    -- it crosses the disable one, which is the whole of "tell them before, not
    -- after".
    consecutive_failures    INTEGER NOT NULL DEFAULT 0,
    warned_at               INTEGER,
    disabled_at             INTEGER,
    disabled_reason         TEXT NOT NULL DEFAULT '',

    created_at              INTEGER NOT NULL,
    updated_at              INTEGER NOT NULL
);

CREATE INDEX webhook_endpoints_team ON webhook_endpoints(team_id);

-- The delivery log the customer reads. One row per delivery of one event to one
-- endpoint, kept whether it succeeded or not: a webhook that silently never
-- arrived is the single most common integration complaint, and the only cure is
-- a log the customer can open themselves.
CREATE TABLE webhook_deliveries (
    id              INTEGER PRIMARY KEY,
    endpoint_id     INTEGER NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,

    -- Stable across retries and redeliveries, and sent in the request headers,
    -- so a receiver can make its own handling idempotent.
    event_id        TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    payload         TEXT NOT NULL,

    state           TEXT NOT NULL DEFAULT 'pending'
                    CHECK (state IN ('pending', 'delivered', 'failed')),

    attempt         INTEGER NOT NULL DEFAULT 0,
    max_attempts    INTEGER NOT NULL DEFAULT 12,

    response_status INTEGER,

    -- Truncated. A receiver that answers with a megabyte of HTML on error must
    -- not be able to fill our disk one retry at a time.
    response_body   TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    duration_ms     INTEGER NOT NULL DEFAULT 0,

    created_at      INTEGER NOT NULL,
    attempted_at    INTEGER,
    next_attempt_at INTEGER,
    delivered_at    INTEGER
);

CREATE INDEX webhook_deliveries_endpoint ON webhook_deliveries(endpoint_id, id DESC);

-- OAuth 2.1 clients registered against the MCP endpoint. Registration is dynamic
-- because that is what an assistant does when somebody pastes our URL into it:
-- there is no human to fill in a developer portal, and a server that needs one
-- is a server nobody connects.
CREATE TABLE mcp_oauth_clients (
    id                         INTEGER PRIMARY KEY,
    client_id                  TEXT NOT NULL UNIQUE,

    -- SHA-256, or empty for a public client. Public clients are the normal case
    -- here: a desktop assistant cannot keep a secret, which is why PKCE is
    -- mandatory on every authorisation rather than only when a secret is absent.
    client_secret_hash         TEXT NOT NULL DEFAULT '',

    client_name                TEXT NOT NULL DEFAULT '',
    redirect_uris              TEXT NOT NULL DEFAULT '[]',
    grant_types                TEXT NOT NULL DEFAULT '[]',
    token_endpoint_auth_method TEXT NOT NULL DEFAULT 'none',
    scope                      TEXT NOT NULL DEFAULT '',
    created_at                 INTEGER NOT NULL
);

-- Authorisation codes, hashed and single-use. They live for a minute, which is
-- long enough for a redirect and far too short to be worth stealing.
CREATE TABLE mcp_oauth_codes (
    id                    INTEGER PRIMARY KEY,
    code_hash             TEXT NOT NULL UNIQUE,
    client_id             TEXT NOT NULL,
    team_id               INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    api_key_id            INTEGER REFERENCES api_keys(id) ON DELETE CASCADE,
    redirect_uri          TEXT NOT NULL,
    scope                 TEXT NOT NULL DEFAULT '',

    -- PKCE. Required on every authorisation, including confidential clients:
    -- an authorisation code intercepted on the loopback redirect of a desktop
    -- app is the exact attack this closes.
    code_challenge        TEXT NOT NULL,
    code_challenge_method TEXT NOT NULL DEFAULT 'S256',

    created_at            INTEGER NOT NULL,
    expires_at            INTEGER NOT NULL,
    consumed_at           INTEGER
);

-- Access and refresh tokens, hashed like every other bearer credential here.
CREATE TABLE mcp_oauth_tokens (
    id                 INTEGER PRIMARY KEY,
    token_hash         TEXT NOT NULL UNIQUE,
    refresh_token_hash TEXT UNIQUE,
    client_id          TEXT NOT NULL,
    team_id            INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    api_key_id         INTEGER REFERENCES api_keys(id) ON DELETE CASCADE,
    scope              TEXT NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL,
    expires_at         INTEGER NOT NULL,
    revoked_at         INTEGER
);

CREATE INDEX mcp_oauth_tokens_team ON mcp_oauth_tokens(team_id);
