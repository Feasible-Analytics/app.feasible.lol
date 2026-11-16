--
-- 0020_local_avatars.sql
-- Keep only the pictures people brought with them from Google.
--
-- Created: 2026-11-16
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- A person with no stored picture is drawn as their initials on a colour, by
-- this process. Every row that is not a picture Google returned with a
-- sign-in goes: pictures looked up from an email address, and the empty rows
-- that recorded an address with nothing behind it.

DELETE FROM user_avatars WHERE source <> 'google' OR etag = '' OR bytes IS NULL;
