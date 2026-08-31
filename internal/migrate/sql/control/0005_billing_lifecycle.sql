--
-- 0005_billing_lifecycle.sql
-- The account lifecycle clock, the emails it sends, the Stripe log and the volume ladder.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Every timestamp here is unix seconds in UTC, matching 0001.
--
-- One idea runs through the whole file: the lifecycle clock is a single instant
-- per account, and every phase, deadline and email date is arithmetic on it.
-- Storing the phase, or a per-phase set of dates, would let two of them disagree
-- and there is no way to tell afterwards which one was right — and the code that
-- reads them permanently deletes customer data.

-- The lifecycle clock. One row per account, and the row exists only once the
-- account has been enrolled — a team with no row here is not on a clock at all.
--
-- started_at is day 0 and never moves while a clock runs. Both triggers set it
-- once: a trial sets it at signup, a lapse sets it at the FIRST failed charge
-- rather than at the end of the payment provider's retry window. A second
-- failure mid-grace must not push the deadline out, because the customer has
-- already been told the date.
CREATE TABLE account_lifecycle (
    team_id    INTEGER PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,

    -- Why the clock is running. Empty means it is not: the account is Active.
    -- 'trial' and 'lapse' follow the identical timetable and differ only in the
    -- words the emails use.
    trigger    TEXT NOT NULL DEFAULT '' CHECK (trigger IN ('', 'trial', 'lapse')),

    started_at INTEGER,

    -- Set the moment destruction begins, before anything is removed. A crash
    -- part-way through must leave the account excluded from the sweep rather
    -- than half-deleted and still ticking.
    deleted_at INTEGER,

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Accounts whose clock is running, which is the only set the sweeper reads.
CREATE INDEX account_lifecycle_running ON account_lifecycle(started_at)
    WHERE started_at IS NOT NULL AND deleted_at IS NULL;

-- One row per lifecycle email that has been sent. This table IS the idempotency
-- guarantee: the unique constraint below is what makes sending twice impossible
-- even when a job retries after the message has left but before the row landed.
--
-- The row is keyed by the clock it belongs to, not just the account, so that an
-- account that lapses, pays, and lapses again a year later gets the full set of
-- warnings the second time rather than being silently skipped.
CREATE TABLE lifecycle_emails (
    id         INTEGER PRIMARY KEY,
    team_id    INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    started_at INTEGER NOT NULL,
    template   TEXT NOT NULL,
    recipient  TEXT NOT NULL,

    -- What the transport actually reported. A send function returning without
    -- an error is not delivery, so the transport's own answer is recorded and
    -- an empty string here means we never got one.
    outcome    TEXT NOT NULL DEFAULT '',
    sent_at    INTEGER NOT NULL,

    UNIQUE (team_id, started_at, template)
);

CREATE INDEX lifecycle_emails_team ON lifecycle_emails(team_id);

-- What survives a deletion, and nothing else does. Every other row belonging to
-- the account is destroyed by the cascade off `teams`, including the lifecycle
-- row above, so this table deliberately carries NO foreign key: it is the only
-- record that the account ever existed and that we destroyed it on purpose.
--
-- The contact address is kept, and the day-90 email says so in as many words.
-- Without it we could not answer "did you delete my account, and when", which
-- is a question a data subject is entitled to ask after the fact.
CREATE TABLE account_deletions (
    id                 INTEGER PRIMARY KEY,
    team_id            INTEGER NOT NULL UNIQUE,
    team_name          TEXT NOT NULL DEFAULT '',
    contact_email      TEXT NOT NULL DEFAULT '',
    stripe_customer_id TEXT NOT NULL DEFAULT '',
    clock_started_at   INTEGER NOT NULL,
    started_at         INTEGER NOT NULL,
    completed_at       INTEGER,

    -- When the confirmation was sent. It is what makes that final email
    -- idempotent once every other row for the account has gone.
    notified_at        INTEGER,

    -- What actually happened, step by step, so a support person can answer
    -- "was the payment provider's customer removed too".
    notes              TEXT NOT NULL DEFAULT ''
);

-- Every webhook we have received, kept whether or not we could act on it. The
-- unique event id is the deduplication: the payment provider retries, delivers
-- out of order, and occasionally delivers twice, and a handler that acted on
-- each delivery would double-apply.
--
-- The payload is stored verbatim because this table is what a support person
-- reads when a customer says they paid and the account still says otherwise.
CREATE TABLE stripe_events (
    id          INTEGER PRIMARY KEY,
    event_id    TEXT NOT NULL UNIQUE,
    type        TEXT NOT NULL,
    team_id     INTEGER,
    payload     TEXT NOT NULL,
    received_at INTEGER NOT NULL,
    handled_at  INTEGER,

    -- 'applied', 'ignored', 'duplicate' or 'error', so a support query can find
    -- the events that did nothing without reading every payload.
    outcome     TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX stripe_events_team ON stripe_events(team_id, received_at);
CREATE INDEX stripe_events_type ON stripe_events(type, received_at);

-- One row per volume-ladder email already sent for one account in one calendar
-- month. Same reasoning as lifecycle_emails: the constraint is the guarantee,
-- not a check in the job.
CREATE TABLE usage_notices (
    id        INTEGER PRIMARY KEY,
    team_id   INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    period    TEXT NOT NULL,
    threshold TEXT NOT NULL CHECK (threshold IN ('warn', 'near', 'reached', 'second_month')),
    sent_at   INTEGER NOT NULL,

    UNIQUE (team_id, period, threshold)
);

-- The state of the conversation with an account that has gone over two months
-- running. Going over is not a payment failure and never touches the deletion
-- clock, so it needs its own state rather than a phase on the lifecycle row.
CREATE TABLE usage_overages (
    team_id        INTEGER PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,

    -- The month the second consecutive overage was observed, as 'YYYY-MM'.
    period         TEXT NOT NULL,

    -- When we asked them to reply, and the date the dashboard locks if nobody
    -- does. Two weeks, named in the email, and nothing happens before it.
    asked_at       INTEGER,
    reply_deadline INTEGER,

    -- Set when a human replies. It is a person's judgement rather than an
    -- automatic signal because "talk to us about Enterprise" has no machine
    -- answer, and locking somebody who did reply would be unforgivable.
    replied_at     INTEGER,

    locked_at      INTEGER,
    updated_at     INTEGER NOT NULL
);

-- The plan the account is on and whether it is set to stop at the period end.
-- Both are read straight off the payment provider's current state, so that the
-- handler is a function of that state rather than of whichever event woke it.
ALTER TABLE subscriptions ADD COLUMN stripe_price_id TEXT NOT NULL DEFAULT '';
ALTER TABLE subscriptions ADD COLUMN cancel_at_period_end INTEGER NOT NULL DEFAULT 0;

-- The account's billing contact. It is here rather than derived from the team
-- owner because the owner can leave, and a deletion warning that bounces is the
-- one email in the product that must not.
ALTER TABLE subscriptions ADD COLUMN billing_email TEXT NOT NULL DEFAULT '';

-- The window the dormant phase left in the data, so the graph can draw it as a
-- labelled gap. Zeroes would read as "nobody visited", which is a different and
-- much worse thing to tell somebody who has just paid to come back.
CREATE TABLE collection_gaps (
    id         INTEGER PRIMARY KEY,
    team_id    INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    started_at INTEGER NOT NULL,
    ended_at   INTEGER,
    reason     TEXT NOT NULL DEFAULT 'dormant',

    UNIQUE (team_id, started_at)
);

CREATE INDEX collection_gaps_team ON collection_gaps(team_id, started_at);
