--
-- 0021_bot_reason_cleared_from_session_sampling.sql
-- Carry a bot verdict that is taken back into the session sampling fact.
--
-- Created: 2026-09-14
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- A visit-level verdict is cleared when a later event shows a person was there,
-- and session_sampling.is_bot has to follow it back to 0 unless another event of
-- the visit still carries a reason of its own.

CREATE TRIGGER events_sampling_bot_reason_cleared
AFTER UPDATE OF bot_reason_id ON events
WHEN OLD.bot_reason_id <> 0 AND NEW.bot_reason_id = 0
BEGIN
    UPDATE session_sampling
    SET is_bot = EXISTS (
        SELECT 1 FROM events e INDEXED BY events_session_bot
        WHERE e.session_id = NEW.session_id AND e.bot_reason_id <> 0
    )
    WHERE session_id = NEW.session_id;
END;
