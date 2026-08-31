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
