--
-- 0009_report_notification_leases.sql
-- Durable recurring-job slots and leased notification delivery state.
--
-- Created: 2026-08-31
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Requires: control 0008_teams_sharing_reports.
--
-- A live-job unique key cannot protect a recurring bucket after its job has
-- completed. The slot is therefore its own durable fact: creating the slot and
-- its job happens in one transaction, so a crash leaves both or neither.

-- Site jobs carry their scope structurally. A reset or deletion discovers and
-- removes every site-scoped control row through the schema; leaving the site id
-- buried only in JSON would let a queued import recreate data after that sweep.
ALTER TABLE jobs ADD COLUMN site_id INTEGER;

UPDATE jobs
SET site_id = CAST(json_extract(args, '$.site_id') AS INTEGER)
WHERE json_valid(args) AND CAST(json_extract(args, '$.site_id') AS INTEGER) > 0;

CREATE INDEX jobs_site ON jobs(site_id, state);

CREATE TABLE cron_slots (
    id         INTEGER PRIMARY KEY,
    queue      TEXT NOT NULL,
    kind       TEXT NOT NULL,
    bucket     INTEGER NOT NULL,
    job_id     INTEGER REFERENCES jobs(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,

    UNIQUE (queue, kind, bucket)
);

-- Carry forward buckets made by the old live-job key. Completed jobs are
-- included because their key is precisely the history the partial unique index
-- stopped protecting. GROUP BY also collapses any bucket that already raced
-- through quick completion before this migration existed.
INSERT INTO cron_slots (queue, kind, bucket, job_id, created_at)
SELECT queue,
       kind,
       CAST(substr(unique_key, length('cron:' || kind || ':') + 1) AS INTEGER),
       MIN(id),
       MIN(scheduled_at)
FROM jobs
WHERE unique_key LIKE ('cron:' || kind || ':%')
  AND CAST(substr(unique_key, length('cron:' || kind || ':') + 1) AS INTEGER) > 0
GROUP BY queue, kind, CAST(substr(unique_key, length('cron:' || kind || ':') + 1) AS INTEGER);

-- One logical report or alert. Pending work is leased rather than represented
-- by a permanent "sent" row, so a process killed after claiming it cannot
-- suppress it forever. Alert rows are also the rate-limit slots: their count
-- and insertion happen under the same SQLite write transaction.
CREATE TABLE notification_claims (
    id           INTEGER PRIMARY KEY,
    site_id      INTEGER NOT NULL REFERENCES sites(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL CHECK (kind IN ('weekly', 'monthly', 'spike', 'drop')),
    period_key   TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'completed')),
    lease_token  TEXT NOT NULL DEFAULT '',
    lease_until  INTEGER NOT NULL DEFAULT 0,
    recipients   INTEGER NOT NULL DEFAULT 0,
    payload      TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    completed_at INTEGER
);

CREATE UNIQUE INDEX notification_claims_period
    ON notification_claims(site_id, kind, period_key)
    WHERE period_key <> '';

CREATE INDEX notification_claims_alert_rate
    ON notification_claims(site_id, created_at)
    WHERE kind IN ('spike', 'drop');

CREATE INDEX notification_claims_pending
    ON notification_claims(state, lease_until);

-- Every destination is durable state of its own. A retry therefore skips an
-- email or Slack webhook that succeeded before another destination failed.
CREATE TABLE notification_destinations (
    id              INTEGER PRIMARY KEY,
    notification_id INTEGER NOT NULL REFERENCES notification_claims(id) ON DELETE CASCADE,
    destination_key TEXT NOT NULL,
    channel         TEXT NOT NULL CHECK (channel IN ('email', 'slack')),
    target          TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'sent')),
    sent_at         INTEGER,

    UNIQUE (notification_id, destination_key)
);

CREATE INDEX notification_destinations_pending
    ON notification_destinations(notification_id, state);

-- Preserve completed history from the M9 ledger. Rows with zero recipients
-- were abandoned claims rather than deliveries; intentionally omitting them is
-- what lets the leased implementation recover those periods.
INSERT INTO notification_claims
    (site_id, kind, period_key, state, recipients, created_at, completed_at)
SELECT site_id, kind, period_key, 'completed', recipients, sent_at, sent_at
FROM notifications_sent
WHERE recipients > 0;

DELETE FROM notifications_sent WHERE recipients = 0;

-- PBKDF2 protects stored passwords but makes every online guess intentionally
-- expensive. Attempts are bounded per source and per link so one source cannot
-- exhaust a core, while spoofing traffic against one link cannot deny access
-- to every other shared dashboard.
CREATE TABLE share_password_attempts (
    link_id           INTEGER NOT NULL REFERENCES shared_links(id) ON DELETE CASCADE,
    source_key        TEXT NOT NULL,
    window_started_at INTEGER NOT NULL,
    attempts          INTEGER NOT NULL,

    PRIMARY KEY (link_id, source_key)
);

CREATE INDEX share_password_attempts_window
    ON share_password_attempts(window_started_at);
