--
-- 0019_onboarding_emails.sql
-- When the onboarding emails began, and which accounts asked for them to stop.
--
-- Created: 2026-11-15
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- The sent markers for these emails are rows in lifecycle_outbox, keyed by the
-- team's creation instant. Only the two facts that table cannot hold live here.

-- The instant this migration ran. Only a team created at or after it is ever
-- sent an onboarding email, so an account that signed up before the feature
-- existed is never written to about a sign-up that happened weeks ago.
CREATE TABLE onboarding_start (
    singleton  INTEGER PRIMARY KEY CHECK (singleton = 1),
    started_at INTEGER NOT NULL
);

INSERT INTO onboarding_start (singleton, started_at)
VALUES (1, CAST(strftime('%s', 'now') AS INTEGER));

-- One row per team whose owner clicked "stop these tips". The row is the whole
-- preference: its presence stops every onboarding email, and nothing else.
CREATE TABLE onboarding_optouts (
    team_id    INTEGER PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL
);
