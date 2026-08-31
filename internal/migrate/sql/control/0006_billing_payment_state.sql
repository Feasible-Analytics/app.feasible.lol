--
-- 0006_billing_payment_state.sql
-- Payment evidence for delayed Stripe checkout methods.
--
-- Created: 2026-08-31
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--

-- A Managed Payments subscription can report active while its asynchronous
-- checkout payment is still pending or has failed. Keep the final payment
-- evidence separately so a later subscription update cannot grant access on
-- status alone.
ALTER TABLE subscriptions ADD COLUMN payment_state TEXT NOT NULL DEFAULT ''
    CHECK (payment_state IN ('', 'pending', 'paid', 'failed'));

-- The first failed event in the current lapse is the contractual day zero.
-- Keeping it beside the payment state makes delayed delivery and process
-- restarts preserve the provider's clock instead of substituting local time.
ALTER TABLE subscriptions ADD COLUMN payment_failed_at INTEGER;

-- These fields are a durable compare-and-swap fence for webhook ordering.
-- Provider object creation orders separate invoices/sessions, event creation
-- orders changes to one object, and rank breaks exact timestamp ties.
ALTER TABLE subscriptions ADD COLUMN evidence_source_created INTEGER NOT NULL DEFAULT 0;
ALTER TABLE subscriptions ADD COLUMN evidence_event_created INTEGER NOT NULL DEFAULT 0;
ALTER TABLE subscriptions ADD COLUMN evidence_rank INTEGER NOT NULL DEFAULT 0;
ALTER TABLE subscriptions ADD COLUMN reconciled_event_created INTEGER NOT NULL DEFAULT 0;

-- A database-backed lease serialises one account across independently running
-- app and CLI processes. The token prevents an expired worker from releasing a
-- lease that a newer worker has already acquired.
CREATE TABLE billing_account_leases (
    team_id    INTEGER PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
    token      TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Every reversible provider mutation made during the day-90 check is recorded
-- before Stripe is called. A replacement process can therefore restore a paid
-- account even when the original process died after pausing collection.
CREATE TABLE billing_quiescence_objects (
    team_id      INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    object_type  TEXT NOT NULL CHECK (object_type IN ('subscription', 'invoice')),
    stripe_id    TEXT NOT NULL,
    created_at   INTEGER NOT NULL,

    PRIMARY KEY (team_id, object_type, stripe_id)
);

-- A checkout claim is written before calling Stripe. Retries reuse the same
-- idempotency key, including after a crash, so at most one customer/subscription
-- can be created for an account while a checkout is open or settling.
CREATE TABLE billing_checkouts (
    team_id         INTEGER PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
    plan             TEXT NOT NULL CHECK (plan IN ('monthly', 'yearly')),
    stripe_price_id  TEXT NOT NULL,
    idempotency_key  TEXT NOT NULL UNIQUE,
    session_id       TEXT NOT NULL DEFAULT '',
    session_url      TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'creating'
                     CHECK (status IN ('creating', 'open', 'complete', 'expired')),
    claim_token      TEXT NOT NULL,
    claim_expires_at INTEGER NOT NULL,
    customer_id      TEXT NOT NULL DEFAULT '',
    billing_email    TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

-- A provider response can arrive after its checkout claim was replaced. Keep
-- that session until Stripe confirms it is expired, so a transient cleanup
-- failure cannot leave a second usable subscription checkout untracked.
CREATE TABLE billing_checkout_cleanup (
    session_id   TEXT PRIMARY KEY,
    team_id      INTEGER NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

-- Payment transitions and warning sends use this second account-scoped lease.
-- Holding it through the bounded mail transport prevents a payment from racing
-- an already-claimed obsolete deletion warning.
CREATE TABLE lifecycle_account_leases (
    team_id    INTEGER PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
    token      TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Lifecycle delivery is a leased outbox rather than a permanent pre-send
-- claim. It deliberately has no team foreign key: the day-90 confirmation is
-- created in the same transaction that removes the team and must survive that
-- cascade. Warning rows are removed during deletion once their audit purpose
-- has ended.
CREATE TABLE lifecycle_outbox (
    id               INTEGER PRIMARY KEY,
    team_id          INTEGER NOT NULL,
    started_at       INTEGER NOT NULL,
    template         TEXT NOT NULL,
    recipient        TEXT NOT NULL,
    message_key      TEXT NOT NULL UNIQUE,
    payload          TEXT NOT NULL DEFAULT '',
    lease_token      TEXT NOT NULL DEFAULT '',
    lease_expires_at INTEGER NOT NULL DEFAULT 0,
    attempts         INTEGER NOT NULL DEFAULT 0,
    completed_at     INTEGER,
    outcome          TEXT NOT NULL DEFAULT '',
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,

    UNIQUE (team_id, started_at, template)
);

CREATE INDEX lifecycle_outbox_pending
    ON lifecycle_outbox(completed_at, lease_expires_at, team_id);

-- Existing successful deliveries remain complete. A legacy row with no
-- outcome, or an explicitly failed outcome, becomes immediately retryable;
-- those are precisely the sends the old permanent claim could strand.
INSERT INTO lifecycle_outbox
    (team_id, started_at, template, recipient, message_key, attempts,
     completed_at, outcome, created_at, updated_at)
SELECT team_id, started_at, template, recipient,
       'lifecycle-' || team_id || '-' || started_at || '-' || template,
       1,
       CASE WHEN outcome <> '' AND instr(outcome, 'failed:') = 0 THEN sent_at END,
       outcome,
       sent_at,
       sent_at
FROM lifecycle_emails;

-- Deletion is complete only after local data and the provider customer are both
-- gone. These checkpoints survive team removal and make either half retryable.
ALTER TABLE account_deletions ADD COLUMN local_removed_at INTEGER;
ALTER TABLE account_deletions ADD COLUMN provider_removed_at INTEGER;
ALTER TABLE account_deletions ADD COLUMN control_removed_at INTEGER;

-- SQLite reuses a deleted maximum INTEGER PRIMARY KEY unless AUTOINCREMENT is
-- present. Rebuilding the heavily referenced teams table would be destructive,
-- so this one-row sequence permanently reserves every assigned team id instead.
CREATE TABLE team_id_sequence (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    last_id   INTEGER NOT NULL
);

INSERT INTO team_id_sequence (singleton, last_id)
SELECT 1, MAX(
    COALESCE((SELECT MAX(id) FROM teams), 0),
    COALESCE((SELECT MAX(team_id) FROM account_deletions), 0)
);
