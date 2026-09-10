--
-- 0019_bot_reason_reaches_session_sampling.sql
-- Carry a bot verdict reached after the insert into the session sampling fact.
--
-- Created: 2026-09-10
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- session_sampling.is_bot is what a visit-grain query reads to exclude
-- automated traffic, and it was maintained only on insert and on a session
-- repoint. A verdict that can only be reached once a whole visit is visible --
-- one that fired custom events on more than one path without ever loading one
-- -- is set by updating rows that are already stored, so it never arrived, and
-- the visit stayed in the visitor count it was supposed to leave.
--
-- Only is_bot is touched. The entry-title columns beside it are a function of
-- the event's name, path and timestamp, none of which a verdict changes.

CREATE TRIGGER events_sampling_bot_reason
AFTER UPDATE OF bot_reason_id ON events
WHEN OLD.bot_reason_id = 0 AND NEW.bot_reason_id <> 0
BEGIN
    UPDATE session_sampling
    SET is_bot = 1
    WHERE session_id = NEW.session_id;
END;
