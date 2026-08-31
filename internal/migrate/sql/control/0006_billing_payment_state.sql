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
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
