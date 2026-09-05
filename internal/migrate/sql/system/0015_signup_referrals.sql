--
-- 0015_signup_referrals.sql
-- Where each person came from, recorded once, when their account is created.
--
-- Created: 2026-09-05
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- It is first touch, not last. The referrer on the registration POST is always
-- our own form, so recording that would make every account look self-referred;
-- what matters is the site that sent them to us the first time, days earlier.
-- A first-party cookie carries it across that gap and this row is where it
-- lands.
--
-- Its own table rather than columns on users, for the reason user_avatars is
-- its own table: users is read on every authenticated request, and this is
-- written once and read by nobody on that path.

CREATE TABLE user_referrals (
    user_id       INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,

    -- The external page that sent them, with our own hostname already removed.
    referrer      TEXT NOT NULL DEFAULT '',

    -- The campaign parameters, as they arrived. They are stored separately from
    -- the referrer because a tagged link carries both and they answer different
    -- questions: the referrer is where the click happened, the campaign is what
    -- we were running when it did.
    source        TEXT NOT NULL DEFAULT '',
    medium        TEXT NOT NULL DEFAULT '',
    campaign      TEXT NOT NULL DEFAULT '',

    -- The first page of ours they landed on. It is the part that survives when
    -- a browser sends no referrer at all, which is most of them.
    landing_page  TEXT NOT NULL DEFAULT '',

    -- When they first arrived, and when the account was created. Both are kept
    -- because the gap between them is how long somebody thought about it, and
    -- that is the number this table exists to make answerable.
    first_seen_at INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);

-- Answering "where did last month's signups come from" without reading every
-- row of the table.
CREATE INDEX user_referrals_source ON user_referrals(source, created_at);
