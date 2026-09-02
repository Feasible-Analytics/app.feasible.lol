--
-- 0003_accounts_and_sites.sql
-- What the sign-in, site management and onboarding screens need on top of 0001.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- Every column here is nullable or carries a default, so the migration runs on
-- a database that already holds accounts without a rewrite and without a
-- backfill job.

-- Ordering and pinning for the sites list. Both are per-team display state
-- rather than anything the ingest path reads, but they live beside the site
-- because a separate preferences table would need its own row lifecycle for a
-- feature whose entire state is two integers.
--
-- position is a sparse rank, not a dense index: a drag that lands a site
-- between two others rewrites one row rather than renumbering the list.
ALTER TABLE sites ADD COLUMN position INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sites ADD COLUMN pinned_at INTEGER;

-- The 72-hour dual-write window for a domain change. Both the old and the new
-- domain resolve to this site until previous_domain_until passes, so a snippet
-- nobody has updated yet keeps collecting instead of silently dropping every
-- pageview the moment the setting is saved.
--
-- Nothing site-scoped is keyed on the domain string — goals, funnels and
-- segments are all keyed on sites.id — so changing a domain moves the routing
-- entry and touches nothing else. Keying any of them on the domain is what
-- turned the incumbent's change-domain feature into a silent wipe of every
-- configured goal.
ALTER TABLE sites ADD COLUMN previous_domain TEXT NOT NULL DEFAULT '';
ALTER TABLE sites ADD COLUMN previous_domain_until INTEGER;

-- Where onboarding got to. A site that has never been installed is offered the
-- snippet again; one that was skipped is left alone, because trapping someone
-- in a wizard they have already declined is how a product earns a support
-- ticket instead of a customer.
ALTER TABLE sites ADD COLUMN onboarded_at INTEGER;

-- Manual ordering for folders, same sparse-rank rule as sites.
ALTER TABLE site_folders ADD COLUMN position INTEGER NOT NULL DEFAULT 0;

-- The team-wide two-factor policy. It is a team column rather than a per-user
-- flag because the decision belongs to whoever owns the account: an admin
-- turning it on has to be able to lock out a member who has not enrolled yet,
-- and a per-user flag can only ever describe a member's own choice.
ALTER TABLE teams ADD COLUMN require_2fa INTEGER NOT NULL DEFAULT 0;

-- A site is looked up by its previous domain on every routing-map rebuild, and
-- by an operator asking "who used to own this domain" during a support call.
CREATE INDEX sites_previous_domain ON sites(previous_domain)
    WHERE previous_domain <> '';
