--
-- 0020_utm_content_and_term.sql
-- utm_content and utm_term become ordinary interned dimensions.
--
-- Created: 2026-09-11
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- The tracker has always captured all five UTM tags, but only three of them
-- reached a column anything can group by. Content and term went to
-- event_details as free text, where nothing could break down by them, filter on
-- them, or roll them up — so a campaign split across two creatives was one row
-- on the dashboard.
--
-- They join their three siblings: an interned id on both events and sessions,
-- and their own dim table. The values already captured are carried across
-- rather than abandoned, and the free-text columns they came from go, so there
-- is one place a UTM tag lives.

CREATE TABLE dim_utm_content (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);
CREATE TABLE dim_utm_term    (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);

INSERT INTO dim_utm_content (id, value) VALUES (0, '');
INSERT INTO dim_utm_term    (id, value) VALUES (0, '');

ALTER TABLE events   ADD COLUMN utm_content_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events   ADD COLUMN utm_term_id    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN utm_content_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN utm_term_id    INTEGER NOT NULL DEFAULT 0;

ALTER TABLE imported_rollups ADD COLUMN utm_content_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE imported_rollups ADD COLUMN utm_term_id    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE imported_wide    ADD COLUMN utm_content_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE imported_wide    ADD COLUMN utm_term_id    INTEGER NOT NULL DEFAULT 0;

-- Carry across what was already captured. Every distinct value becomes an id,
-- then the events holding it point at that id. Only events that carried a tag
-- are touched: the default of zero already means "not set" for the rest.
INSERT OR IGNORE INTO dim_utm_content (value)
SELECT DISTINCT utm_content FROM event_details WHERE utm_content IS NOT NULL AND utm_content <> '';

INSERT OR IGNORE INTO dim_utm_term (value)
SELECT DISTINCT utm_term FROM event_details WHERE utm_term IS NOT NULL AND utm_term <> '';

UPDATE events SET utm_content_id = (
    SELECT d.id FROM dim_utm_content d
    JOIN event_details ed ON ed.utm_content = d.value
    WHERE ed.event_id = events.id
)
WHERE id IN (SELECT event_id FROM event_details WHERE utm_content IS NOT NULL AND utm_content <> '');

UPDATE events SET utm_term_id = (
    SELECT d.id FROM dim_utm_term d
    JOIN event_details ed ON ed.utm_term = d.value
    WHERE ed.event_id = events.id
)
WHERE id IN (SELECT event_id FROM event_details WHERE utm_term IS NOT NULL AND utm_term <> '');

-- A visit's acquisition is fixed at its first event, which is how the three
-- columns beside these were filled when the session was written. The earliest
-- event carrying a tag is that event.
UPDATE sessions SET utm_content_id = (
    SELECT e.utm_content_id FROM events e
    WHERE e.session_id = sessions.id AND e.utm_content_id <> 0
    ORDER BY e.timestamp, e.id LIMIT 1
)
WHERE id IN (SELECT DISTINCT session_id FROM events WHERE utm_content_id <> 0);

UPDATE sessions SET utm_term_id = (
    SELECT e.utm_term_id FROM events e
    WHERE e.session_id = sessions.id AND e.utm_term_id <> 0
    ORDER BY e.timestamp, e.id LIMIT 1
)
WHERE id IN (SELECT DISTINCT session_id FROM events WHERE utm_term_id <> 0);

-- The free-text originals go. Two places to read a UTM tag from is two answers
-- to "which creative won", and the interned column is the one every report,
-- filter and roll-up now reads.
ALTER TABLE event_details DROP COLUMN utm_content;
ALTER TABLE event_details DROP COLUMN utm_term;
