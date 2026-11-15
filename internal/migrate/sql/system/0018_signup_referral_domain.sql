--
-- 0018_signup_referral_domain.sql
-- Keep only the domain of the page that sent each new account.
--
-- Created: 2026-11-15
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- The referrer column holds a hostname and nothing else: no path, no query
-- string and no port. A full address can carry a search term or a token that
-- belongs to the visitor, and the hostname answers the only question this
-- table is asked.

-- Drop the scheme.
UPDATE user_referrals
   SET referrer = substr(referrer, instr(referrer, '://') + 3)
 WHERE instr(referrer, '://') > 0;

-- Cut everything from the first path, query, fragment or port separator.
UPDATE user_referrals SET referrer = substr(referrer, 1, instr(referrer, '/') - 1) WHERE instr(referrer, '/') > 0;
UPDATE user_referrals SET referrer = substr(referrer, 1, instr(referrer, '?') - 1) WHERE instr(referrer, '?') > 0;
UPDATE user_referrals SET referrer = substr(referrer, 1, instr(referrer, '#') - 1) WHERE instr(referrer, '#') > 0;
UPDATE user_referrals SET referrer = substr(referrer, 1, instr(referrer, ':') - 1) WHERE instr(referrer, ':') > 0;

UPDATE user_referrals SET referrer = lower(referrer);

-- The landing page is a path, so its query string goes too.
UPDATE user_referrals SET landing_page = substr(landing_page, 1, instr(landing_page, '?') - 1) WHERE instr(landing_page, '?') > 0;

-- The arrival time was carried by a browser cookie that no longer exists.
ALTER TABLE user_referrals DROP COLUMN first_seen_at;
