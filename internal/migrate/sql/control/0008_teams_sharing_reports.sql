--
-- 0008_teams_sharing_reports.sql
-- Guest invitations, the hostname allow-list, shared links and the notifier ledger.
--
-- Created: 2026-08-31
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Requires: the real control migrations 0006 and 0007 supplied by the shared
-- branch topology. This M9 migration must not manufacture either predecessor.
--
-- Same two conventions as 0001: every timestamp is unix seconds in UTC, and
-- nothing secret is stored in a form we can read back.

-- A site's analytics database and its current owner are separate facts after
-- an ownership transfer. account_id remains the immutable storage account so
-- historical rows never move or disappear; owner_team_id is the live access,
-- billing and administration boundary.
ALTER TABLE sites ADD COLUMN owner_team_id INTEGER REFERENCES teams(id) ON DELETE CASCADE;

UPDATE sites SET owner_team_id = account_id WHERE owner_team_id IS NULL;

CREATE INDEX sites_owner_team ON sites(owner_team_id);

-- Older schemas permitted more than one owner. Keep the earliest ownership
-- row and retain every other owner's administrative access without preserving
-- an ambiguous multi-owner state.
UPDATE team_memberships
SET role = 'admin'
WHERE role = 'owner'
  AND id NOT IN (
      SELECT MIN(id) FROM team_memberships WHERE role = 'owner' GROUP BY team_id
  );

CREATE UNIQUE INDEX team_memberships_single_owner
    ON team_memberships(team_id) WHERE role = 'owner';

-- Refresh tokens outlive their paired access tokens. Keeping a distinct
-- deadline prevents an expired one-hour access token from either killing a
-- thirty-day refresh grant or accidentally making that grant immortal.
ALTER TABLE mcp_oauth_tokens ADD COLUMN refresh_expires_at INTEGER NOT NULL DEFAULT 0;

UPDATE mcp_oauth_tokens
SET refresh_expires_at = created_at + (30 * 24 * 60 * 60)
WHERE refresh_expires_at = 0;

-- Every rotated pair stays in one lineage. Re-presenting any revoked refresh
-- token is evidence that the family was copied, so the token endpoint revokes
-- every descendant through this stable id in the same writer transaction.
ALTER TABLE mcp_oauth_tokens ADD COLUMN token_family_id TEXT NOT NULL DEFAULT '';
ALTER TABLE mcp_oauth_tokens ADD COLUMN parent_token_id INTEGER REFERENCES mcp_oauth_tokens(id);

UPDATE mcp_oauth_tokens
SET token_family_id = token_hash
WHERE token_family_id = '';

CREATE INDEX mcp_oauth_tokens_family ON mcp_oauth_tokens(token_family_id, id);

-- Provider deletion is a separate durable step from deleting local rows. The
-- audit survives the team cascade, so a restarted worker can rediscover and
-- lease failed Stripe work without a subscriptions or teams row to consult.
ALTER TABLE account_deletions ADD COLUMN provider_status TEXT NOT NULL DEFAULT 'pending'
    CHECK (provider_status IN ('pending', 'deleting', 'completed'));
ALTER TABLE account_deletions ADD COLUMN provider_lease_token TEXT NOT NULL DEFAULT '';
ALTER TABLE account_deletions ADD COLUMN provider_lease_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE account_deletions ADD COLUMN provider_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE account_deletions ADD COLUMN provider_error TEXT NOT NULL DEFAULT '';
ALTER TABLE account_deletions ADD COLUMN provider_next_attempt_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE account_deletions ADD COLUMN provider_completed_at INTEGER;
ALTER TABLE account_deletions ADD COLUMN local_completed_at INTEGER;

UPDATE account_deletions
SET local_completed_at = completed_at,
    provider_status = CASE
        WHEN stripe_customer_id = '' THEN 'completed'
        WHEN completed_at IS NOT NULL AND notes NOT LIKE '%NOT removed%' THEN 'completed'
        ELSE 'pending'
    END,
    provider_completed_at = CASE
        WHEN stripe_customer_id = '' THEN completed_at
        WHEN completed_at IS NOT NULL AND notes NOT LIKE '%NOT removed%' THEN completed_at
        ELSE NULL
    END,
    provider_next_attempt_at = CASE
        WHEN stripe_customer_id <> '' AND notes LIKE '%NOT removed%' THEN started_at
        ELSE 0
    END,
    completed_at = CASE
        WHEN stripe_customer_id <> '' AND notes LIKE '%NOT removed%' THEN NULL
        ELSE completed_at
    END;

CREATE INDEX account_deletions_provider_due
    ON account_deletions(provider_status, provider_next_attempt_at, provider_lease_until);

-- Local erasure is deliberately separate from the transaction that makes an
-- account unreachable. A restarted worker finds committed control deletions
-- through this index and resumes the idempotent directory removal.
CREATE INDEX account_deletions_local_due
    ON account_deletions(local_completed_at, team_id);

-- Deleting either team in a cross-account transfer would destroy the control
-- row through account_id's original cascade while the analytics database still
-- contains the site's history. Application deletion paths refuse this state as
-- well, but the schema guard makes that durability independent of every future
-- caller remembering the check.
CREATE TRIGGER teams_preserve_transferred_site_storage
BEFORE DELETE ON teams
WHEN EXISTS (
    SELECT 1 FROM sites
    WHERE account_id <> COALESCE(owner_team_id, account_id)
      AND (account_id = OLD.id OR owner_team_id = OLD.id)
)
BEGIN
    SELECT RAISE(ABORT, 'team participates in transferred site storage');
END;

-- Older provisioning callers name only account_id. Keep those inserts safe
-- during a rolling deployment while every current caller writes both fields.
CREATE TRIGGER sites_default_owner_after_insert
AFTER INSERT ON sites
WHEN NEW.owner_team_id IS NULL
BEGIN
    UPDATE sites SET owner_team_id = NEW.account_id WHERE id = NEW.id;
END;

-- A destructive operation remains visible while analytics data and control
-- state are changed in separate databases. Transfers consult this ledger in
-- their own write transaction, so ownership cannot move between erasing a
-- site's storage and deleting or resetting its control row. No foreign keys
-- are intentional: a tombstone must survive the row whose deletion it tracks.
CREATE TABLE destructive_operations (
    resource_type     TEXT NOT NULL CHECK (resource_type IN ('site', 'team')),
    resource_id       INTEGER NOT NULL,
    kind              TEXT NOT NULL CHECK (kind IN (
                          'site_reset', 'site_delete', 'account_purge', 'account_delete')),
    owner_team_id     INTEGER NOT NULL,
    storage_account_id INTEGER NOT NULL,
    state             TEXT NOT NULL DEFAULT 'claimed'
                      CHECK (state IN ('claimed', 'analytics_deleted', 'control_deleted')),
    lease_token       TEXT NOT NULL,
    lease_until       INTEGER NOT NULL,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    PRIMARY KEY (resource_type, resource_id)
);

CREATE INDEX destructive_operations_owner
    ON destructive_operations(owner_team_id, resource_type, resource_id);

-- An invitation can now be to a team or to a single site, so it needs a
-- site_id and the two guest roles. SQLite cannot widen a CHECK constraint in
-- place, so the table is rebuilt: the constraint is the thing keeping a typo in
-- a role string from failing open in an authorisation check, and dropping it to
-- avoid a rebuild would trade the whole point of the column for a shorter
-- migration.
CREATE TABLE team_invitations_rebuilt (
    id                 INTEGER PRIMARY KEY,
    team_id            INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,

    -- Set only for a guest invitation. A guest is invited to one site and can
    -- see nothing else about the team that owns it, which is the whole reason
    -- agencies can hand a client a login at all.
    site_id            INTEGER REFERENCES sites(id) ON DELETE CASCADE,

    email              TEXT NOT NULL COLLATE NOCASE,
    role               TEXT NOT NULL CHECK (role IN (
                           'admin', 'editor', 'billing', 'viewer',
                           'guest_editor', 'guest_viewer')),
    token_hash         TEXT NOT NULL UNIQUE,
    invited_by_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at         INTEGER NOT NULL,

    -- 48 hours after it was written. It is a stored deadline rather than a
    -- lifetime computed at read time so that expiry is one comparison
    -- everywhere, including in the query that deletes the dead rows.
    expires_at         INTEGER NOT NULL,

    -- A guest role without a site would grant access to nothing, and a team
    -- role with a site would grant access to a whole team through a site-level
    -- invitation. Both are the kind of mistake that only shows up as somebody
    -- seeing data they should not.
    CHECK ((role IN ('guest_editor', 'guest_viewer')) = (site_id IS NOT NULL))
);

INSERT INTO team_invitations_rebuilt
    (id, team_id, site_id, email, role, token_hash, invited_by_user_id, created_at, expires_at)
SELECT id, team_id, NULL, email, role, token_hash, invited_by_user_id, created_at, expires_at
FROM team_invitations
WHERE role <> 'owner';

DROP TABLE team_invitations;

ALTER TABLE team_invitations_rebuilt RENAME TO team_invitations;

-- One live invitation per address per target. site_id is coalesced because
-- SQLite treats two NULLs as distinct in a UNIQUE constraint, which would let
-- the same address be invited to the same team any number of times.
CREATE UNIQUE INDEX team_invitations_target
    ON team_invitations(team_id, email, COALESCE(site_id, 0));

CREATE INDEX team_invitations_expiry ON team_invitations(expires_at);

-- The hostnames a site will accept events from. An empty list means "any
-- hostname", which is what almost every site wants; a non-empty list is what a
-- customer sets when somebody has copied their snippet onto a staging domain
-- or a scraper mirror and polluted their numbers.
--
-- It lives in the control database rather than the account one because the
-- ingest tier has to consult it per event, and the ingest tier never opens an
-- account database. It reaches the hot path inside the same in-memory routing
-- snapshot the domain lookup uses.
CREATE TABLE site_allowed_hostnames (
    id         INTEGER PRIMARY KEY,
    site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    hostname   TEXT NOT NULL COLLATE NOCASE,
    created_at INTEGER NOT NULL,

    UNIQUE (site_id, hostname)
);

-- A shared link's password is a password, so it gets a per-link salt and a
-- derivation cost rather than a bare digest. The salt is a separate column
-- because it is not a secret and does not belong inside the hash string we
-- compare.
ALTER TABLE shared_links ADD COLUMN password_salt TEXT NOT NULL DEFAULT '';

-- Who made the link. Revoking somebody's access should let whoever is left see
-- which of the outstanding links were theirs.
ALTER TABLE shared_links ADD COLUMN created_by_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL;

-- Saved segments are immutable filter sets a shared link may pin. Keeping the
-- filters in control.db lets the authorization layer apply them before a stats
-- request reaches the account database; trusting the browser to resend them
-- would let a reader delete the filters and widen the shared view.
CREATE TABLE saved_segments (
    id         INTEGER PRIMARY KEY,
    site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    name       TEXT NOT NULL DEFAULT '',
    filters    TEXT NOT NULL DEFAULT '[]',
    created_at INTEGER NOT NULL
);

CREATE INDEX saved_segments_site ON saved_segments(site_id, id);

-- Scheduled reports, at most one of each kind per site. The recipient list is
-- JSON because it is read and written whole and never joined against; a second
-- table would buy nothing but a join on every send.
CREATE TABLE report_subscriptions (
    id                INTEGER PRIMARY KEY,
    site_id           INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    kind              TEXT NOT NULL CHECK (kind IN ('weekly', 'monthly')),
    recipients        TEXT NOT NULL DEFAULT '[]',
    slack_webhook_url TEXT NOT NULL DEFAULT '',
    enabled           INTEGER NOT NULL DEFAULT 1,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,

    UNIQUE (site_id, kind)
);

-- Spike and drop alerts. The threshold and window are columns rather than
-- constants because the sane number for a site with a thousand visitors an hour
-- is nonsense for one with ten a day, and a fixed default would make the
-- feature useless for one of the two.
CREATE TABLE alert_rules (
    id                INTEGER PRIMARY KEY,
    site_id           INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    kind              TEXT NOT NULL CHECK (kind IN ('spike', 'drop')),
    threshold         INTEGER NOT NULL,

    -- How far back a drop alert looks, in hours. A spike alert reads current
    -- visitors and ignores this.
    window_hours      INTEGER NOT NULL DEFAULT 12,

    recipients        TEXT NOT NULL DEFAULT '[]',
    slack_webhook_url TEXT NOT NULL DEFAULT '',
    enabled           INTEGER NOT NULL DEFAULT 1,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,

    UNIQUE (site_id, kind)
);

-- Every notification we have sent. It answers two different questions with one
-- table, and both of them are the difference between a useful alert and a
-- mailbox flood:
--
--   "have we already sent this site's report for this week"  — period_key
--   "have we sent this site more than twice in the last day" — sent_at
--
-- An incident that trips an alert trips it every time the job runs, so without
-- the second question a single outage sends a message an hour until somebody
-- filters the sender.
CREATE TABLE notifications_sent (
    id         INTEGER PRIMARY KEY,
    site_id    INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,

    -- The local period this delivery covers, as 'YYYY-Www' or 'YYYY-MM'.
    -- Empty for an alert, which has no period and is rate-limited instead.
    period_key TEXT NOT NULL DEFAULT '',

    recipients INTEGER NOT NULL DEFAULT 0,
    sent_at    INTEGER NOT NULL
);

CREATE INDEX notifications_sent_rate ON notifications_sent(site_id, sent_at);

-- A scheduled report is sent once per site per period, enforced here rather
-- than by the scheduler's arithmetic. The hourly job runs on every process in a
-- deployment, and two of them agreeing that Monday has arrived is the normal
-- case, not the exception.
CREATE UNIQUE INDEX notifications_sent_period
    ON notifications_sent(site_id, kind, period_key)
    WHERE period_key <> '';
