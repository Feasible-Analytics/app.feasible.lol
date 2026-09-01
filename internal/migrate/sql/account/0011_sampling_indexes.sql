--
-- 0011_sampling_indexes.sql
-- Materialized, indexed fact samples and bounded session query facts.
-- Predecessor: 0010
-- Requires: 0008-0010
--
-- Created: 2026-08-31
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--

-- A cleaned target is resolved back to its source paths after aggregation. The
-- primary key supports source -> target reads; this reverse index keeps the
-- enrichment lookup proportional to displayed paths rather than all mappings.
CREATE INDEX path_clean_map_target
    ON path_clean_map(site_id, target_id, source_id);

-- Sampling membership is data, not a query-time expression over a
-- caller-controlled fact id. Each site/day stratum advances an ordinal, folds
-- every higher ten-bit block into its low bits, and applies an odd permutation.
-- Every aligned run of 1,024 writes therefore contains every bucket once, while
-- periodic deletion patterns are spread across buckets by the higher blocks.
-- Keeping the allocator separate from row ids makes restores, signed ids and
-- sparse imports unable to create the catastrophic id-bit bias.
CREATE TABLE sampling_strata (
    fact_kind    TEXT NOT NULL CHECK (fact_kind IN ('event', 'session')),
    site_id      INTEGER NOT NULL,
    day          INTEGER NOT NULL,
    next_ordinal INTEGER NOT NULL,

    PRIMARY KEY (fact_kind, site_id, day)
) WITHOUT ROWID;

-- One narrow row per event is the only population a sampled event query opens
-- first. The fact row is then fetched by primary key, after the bounded indexed
-- membership seek has selected it.
CREATE TABLE event_sampling (
    event_id  INTEGER PRIMARY KEY REFERENCES events(id) ON DELETE CASCADE,
    site_id   INTEGER NOT NULL,
    timestamp INTEGER NOT NULL,
    bucket    INTEGER NOT NULL CHECK (bucket BETWEEN 0 AND 1023)
);

CREATE INDEX event_sampling_seek
    ON event_sampling(site_id, bucket, timestamp, event_id);

-- Session sampling also carries the facts that used to require an event scan
-- per selected visit. A sampled query can now exclude bot sessions and read an
-- entry title with one bounded membership seek, even when one session owns two
-- million events.
CREATE TABLE session_sampling (
    session_id          INTEGER PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    site_id             INTEGER NOT NULL,
    started_at          INTEGER NOT NULL,
    bucket              INTEGER NOT NULL CHECK (bucket BETWEEN 0 AND 1023),
    is_bot              INTEGER NOT NULL DEFAULT 0 CHECK (is_bot IN (0, 1)),
    entry_page_title_id INTEGER NOT NULL DEFAULT 0,
    entry_title_at      INTEGER,
    entry_title_event   INTEGER
);

CREATE INDEX session_sampling_seek
    ON session_sampling(site_id, bucket, started_at, is_bot, session_id);

-- These two indexes are write-path costs paid to keep rare event deletion and
-- session repair bounded. Dashboard statements do not use them: they read the
-- precomputed session_sampling row.
CREATE INDEX events_session_bot
    ON events(session_id, bot_reason_id);

CREATE INDEX events_session_entry
    ON events(session_id, pathname_id, name_id, timestamp, id, page_title_id);

-- Exact per-site UTC-day counts make the automatic decision range-aware. A
-- partial day deliberately reads the whole day's count as an upper bound; a
-- current spike therefore cannot be diluted by old quiet days.
CREATE TABLE sampling_daily_counts (
    site_id      INTEGER NOT NULL,
    day          INTEGER NOT NULL,
    event_rows   INTEGER NOT NULL DEFAULT 0 CHECK (event_rows >= 0),
    session_rows INTEGER NOT NULL DEFAULT 0 CHECK (session_rows >= 0),

    PRIMARY KEY (site_id, day)
) WITHOUT ROWID;

INSERT INTO sampling_daily_counts (site_id, day, event_rows, session_rows)
SELECT site_id, day, SUM(event_rows), SUM(session_rows)
FROM (
    SELECT site_id, CAST(strftime('%s', date(timestamp, 'unixepoch')) AS INTEGER) AS day,
           COUNT(*) AS event_rows, 0 AS session_rows
    FROM events
    GROUP BY site_id, day
    UNION ALL
    SELECT site_id, CAST(strftime('%s', date(started_at, 'unixepoch')) AS INTEGER) AS day,
           0 AS event_rows, COUNT(*) AS session_rows
    FROM sessions
    GROUP BY site_id, day
)
GROUP BY site_id, day;

-- Existing rows are stratified by site/day and chronological fact position.
-- The odd multiplier permutes all 1,024 buckets; site/day shifts keep separate
-- populations from selecting the same ordinal positions.
WITH ranked AS (
    SELECT id, site_id, timestamp,
           CAST(strftime('%s', date(timestamp, 'unixepoch')) AS INTEGER) AS day,
           ROW_NUMBER() OVER (
               PARTITION BY site_id, date(timestamp, 'unixepoch')
               ORDER BY timestamp, id
           ) - 1 AS ordinal
    FROM events
)
INSERT INTO event_sampling (event_id, site_id, timestamp, bucket)
SELECT id, site_id, timestamp,
       (((ordinal + (ordinal >> 10) + (ordinal >> 20) + (ordinal >> 30) +
          (ordinal >> 40) + (ordinal >> 50)) * 405) +
        (site_id * 131) + ((day / 86400) * 17) + 29) & 1023
FROM ranked;

WITH ranked AS (
    SELECT id, site_id, started_at,
           CAST(strftime('%s', date(started_at, 'unixepoch')) AS INTEGER) AS day,
           ROW_NUMBER() OVER (
               PARTITION BY site_id, date(started_at, 'unixepoch')
               ORDER BY started_at, id
           ) - 1 AS ordinal
    FROM sessions
)
INSERT INTO session_sampling (session_id, site_id, started_at, bucket)
SELECT id, site_id, started_at,
       (((ordinal + (ordinal >> 10) + (ordinal >> 20) + (ordinal >> 30) +
          (ordinal >> 40) + (ordinal >> 50)) * 405) +
        (site_id * 131) + ((day / 86400) * 17) + 307) & 1023
FROM ranked;

INSERT INTO sampling_strata (fact_kind, site_id, day, next_ordinal)
SELECT 'event', site_id,
       CAST(strftime('%s', date(timestamp, 'unixepoch')) AS INTEGER), COUNT(*)
FROM events
GROUP BY site_id, date(timestamp, 'unixepoch');

INSERT INTO sampling_strata (fact_kind, site_id, day, next_ordinal)
SELECT 'session', site_id,
       CAST(strftime('%s', date(started_at, 'unixepoch')) AS INTEGER), COUNT(*)
FROM sessions
GROUP BY site_id, date(started_at, 'unixepoch');

UPDATE session_sampling
SET is_bot = EXISTS (
    SELECT 1 FROM events e INDEXED BY events_session_bot
    WHERE e.session_id = session_sampling.session_id AND e.bot_reason_id <> 0
);

WITH candidates AS (
    SELECT e.session_id, e.page_title_id, e.timestamp, e.id,
           ROW_NUMBER() OVER (PARTITION BY e.session_id ORDER BY e.timestamp, e.id) AS position
    FROM events e
    JOIN sessions s ON s.id = e.session_id AND s.entry_page_id = e.pathname_id
    WHERE e.name_id = (SELECT id FROM dim_event_name WHERE value = 'pageview')
)
UPDATE session_sampling
SET (entry_page_title_id, entry_title_at, entry_title_event) = (
    SELECT page_title_id, timestamp, id
    FROM candidates
    WHERE candidates.session_id = session_sampling.session_id AND position = 1
)
WHERE EXISTS (
    SELECT 1 FROM candidates
    WHERE candidates.session_id = session_sampling.session_id AND position = 1
);

-- Every event insert allocates one bucket and updates one exact daily counter in
-- the same transaction as the fact. This is intentionally trigger-owned so a
-- restore or import that supplies explicit ids cannot bypass sampling metadata.
CREATE TRIGGER events_sampling_insert
AFTER INSERT ON events
BEGIN
    INSERT INTO sampling_strata (fact_kind, site_id, day, next_ordinal)
    VALUES (
        'event', NEW.site_id,
        CAST(strftime('%s', date(NEW.timestamp, 'unixepoch')) AS INTEGER), 1
    )
    ON CONFLICT(fact_kind, site_id, day) DO UPDATE
    SET next_ordinal = next_ordinal + 1;

    INSERT INTO event_sampling (event_id, site_id, timestamp, bucket)
    SELECT NEW.id, NEW.site_id, NEW.timestamp,
           ((((next_ordinal - 1) + ((next_ordinal - 1) >> 10) +
              ((next_ordinal - 1) >> 20) + ((next_ordinal - 1) >> 30) +
              ((next_ordinal - 1) >> 40) + ((next_ordinal - 1) >> 50)) * 405) +
            (NEW.site_id * 131) + ((day / 86400) * 17) + 29) & 1023
    FROM sampling_strata
    WHERE fact_kind = 'event' AND site_id = NEW.site_id
      AND day = CAST(strftime('%s', date(NEW.timestamp, 'unixepoch')) AS INTEGER);

    INSERT INTO sampling_daily_counts (site_id, day, event_rows, session_rows)
    VALUES (
        NEW.site_id,
        CAST(strftime('%s', date(NEW.timestamp, 'unixepoch')) AS INTEGER), 1, 0
    )
    ON CONFLICT(site_id, day) DO UPDATE SET event_rows = event_rows + 1;

    UPDATE session_sampling
    SET is_bot = CASE WHEN NEW.bot_reason_id <> 0 THEN 1 ELSE is_bot END,
        entry_page_title_id = CASE
            WHEN NEW.name_id = (SELECT id FROM dim_event_name WHERE value = 'pageview')
             AND NEW.pathname_id = (SELECT entry_page_id FROM sessions WHERE id = NEW.session_id)
             AND (entry_title_at IS NULL OR NEW.timestamp < entry_title_at
                  OR (NEW.timestamp = entry_title_at AND NEW.id < entry_title_event))
            THEN NEW.page_title_id ELSE entry_page_title_id END,
        entry_title_at = CASE
            WHEN NEW.name_id = (SELECT id FROM dim_event_name WHERE value = 'pageview')
             AND NEW.pathname_id = (SELECT entry_page_id FROM sessions WHERE id = NEW.session_id)
             AND (entry_title_at IS NULL OR NEW.timestamp < entry_title_at
                  OR (NEW.timestamp = entry_title_at AND NEW.id < entry_title_event))
            THEN NEW.timestamp ELSE entry_title_at END,
        entry_title_event = CASE
            WHEN NEW.name_id = (SELECT id FROM dim_event_name WHERE value = 'pageview')
             AND NEW.pathname_id = (SELECT entry_page_id FROM sessions WHERE id = NEW.session_id)
             AND (entry_title_at IS NULL OR NEW.timestamp < entry_title_at
                  OR (NEW.timestamp = entry_title_at AND NEW.id < entry_title_event))
            THEN NEW.id ELSE entry_title_event END
    WHERE session_id = NEW.session_id;
END;

-- Deletion removes membership through the foreign key, decrements the exact
-- daily count, and repairs the one affected session fact through covering seeks.
CREATE TRIGGER events_sampling_delete
AFTER DELETE ON events
BEGIN
    UPDATE sampling_daily_counts
    SET event_rows = event_rows - 1
    WHERE site_id = OLD.site_id
      AND day = CAST(strftime('%s', date(OLD.timestamp, 'unixepoch')) AS INTEGER);

    DELETE FROM sampling_daily_counts
    WHERE site_id = OLD.site_id
      AND day = CAST(strftime('%s', date(OLD.timestamp, 'unixepoch')) AS INTEGER)
      AND event_rows = 0 AND session_rows = 0;

    UPDATE session_sampling
    SET is_bot = EXISTS (
            SELECT 1 FROM events e INDEXED BY events_session_bot
            WHERE e.session_id = OLD.session_id AND e.bot_reason_id <> 0
        ),
        entry_page_title_id = COALESCE((
            SELECT e.page_title_id FROM events e INDEXED BY events_session_entry
            WHERE e.session_id = OLD.session_id
              AND e.pathname_id = (SELECT entry_page_id FROM sessions WHERE id = OLD.session_id)
              AND e.name_id = (SELECT id FROM dim_event_name WHERE value = 'pageview')
            ORDER BY e.timestamp, e.id LIMIT 1
        ), 0),
        entry_title_at = (
            SELECT e.timestamp FROM events e INDEXED BY events_session_entry
            WHERE e.session_id = OLD.session_id
              AND e.pathname_id = (SELECT entry_page_id FROM sessions WHERE id = OLD.session_id)
              AND e.name_id = (SELECT id FROM dim_event_name WHERE value = 'pageview')
            ORDER BY e.timestamp, e.id LIMIT 1
        ),
        entry_title_event = (
            SELECT e.id FROM events e INDEXED BY events_session_entry
            WHERE e.session_id = OLD.session_id
              AND e.pathname_id = (SELECT entry_page_id FROM sessions WHERE id = OLD.session_id)
              AND e.name_id = (SELECT id FROM dim_event_name WHERE value = 'pageview')
            ORDER BY e.timestamp, e.id LIMIT 1
        )
    WHERE session_id = OLD.session_id;
END;

-- Session insertion is separate from update because the production UPSERT
-- fires this branch only for a genuinely new visit. That keeps daily session
-- counts exact without a read-before-write in the Go writer.
CREATE TRIGGER sessions_sampling_insert
AFTER INSERT ON sessions
BEGIN
    INSERT INTO sampling_strata (fact_kind, site_id, day, next_ordinal)
    VALUES (
        'session', NEW.site_id,
        CAST(strftime('%s', date(NEW.started_at, 'unixepoch')) AS INTEGER), 1
    )
    ON CONFLICT(fact_kind, site_id, day) DO UPDATE
    SET next_ordinal = next_ordinal + 1;

    INSERT INTO session_sampling (session_id, site_id, started_at, bucket)
    SELECT NEW.id, NEW.site_id, NEW.started_at,
           ((((next_ordinal - 1) + ((next_ordinal - 1) >> 10) +
              ((next_ordinal - 1) >> 20) + ((next_ordinal - 1) >> 30) +
              ((next_ordinal - 1) >> 40) + ((next_ordinal - 1) >> 50)) * 405) +
            (NEW.site_id * 131) + ((day / 86400) * 17) + 307) & 1023
    FROM sampling_strata
    WHERE fact_kind = 'session' AND site_id = NEW.site_id
      AND day = CAST(strftime('%s', date(NEW.started_at, 'unixepoch')) AS INTEGER);

    INSERT INTO sampling_daily_counts (site_id, day, event_rows, session_rows)
    VALUES (
        NEW.site_id,
        CAST(strftime('%s', date(NEW.started_at, 'unixepoch')) AS INTEGER), 0, 1
    )
    ON CONFLICT(site_id, day) DO UPDATE SET session_rows = session_rows + 1;
END;

CREATE TRIGGER sessions_sampling_delete
AFTER DELETE ON sessions
BEGIN
    UPDATE sampling_daily_counts
    SET session_rows = session_rows - 1
    WHERE site_id = OLD.site_id
      AND day = CAST(strftime('%s', date(OLD.started_at, 'unixepoch')) AS INTEGER);

    DELETE FROM sampling_daily_counts
    WHERE site_id = OLD.site_id
      AND day = CAST(strftime('%s', date(OLD.started_at, 'unixepoch')) AS INTEGER)
      AND event_rows = 0 AND session_rows = 0;
END;

-- A late event may move a session start across a UTC day. Preserve its stable
-- bucket, move the indexed time, and transfer the exact daily count atomically.
CREATE TRIGGER sessions_sampling_move
AFTER UPDATE OF site_id, started_at ON sessions
WHEN OLD.site_id <> NEW.site_id OR OLD.started_at <> NEW.started_at
BEGIN
    UPDATE session_sampling
    SET site_id = NEW.site_id, started_at = NEW.started_at
    WHERE session_id = NEW.id;

    UPDATE sampling_daily_counts
    SET session_rows = session_rows - 1
    WHERE site_id = OLD.site_id
      AND day = CAST(strftime('%s', date(OLD.started_at, 'unixepoch')) AS INTEGER);

    INSERT INTO sampling_daily_counts (site_id, day, event_rows, session_rows)
    VALUES (
        NEW.site_id,
        CAST(strftime('%s', date(NEW.started_at, 'unixepoch')) AS INTEGER), 0, 1
    )
    ON CONFLICT(site_id, day) DO UPDATE SET session_rows = session_rows + 1;

    DELETE FROM sampling_daily_counts
    WHERE site_id = OLD.site_id
      AND day = CAST(strftime('%s', date(OLD.started_at, 'unixepoch')) AS INTEGER)
      AND event_rows = 0 AND session_rows = 0;
END;

-- Session merges repoint events in bulk. Incrementally folding each moved row
-- into the survivor avoids a correlated rescan per row; the absorbed session is
-- deleted immediately afterwards by the writer.
CREATE TRIGGER events_sampling_repoint
AFTER UPDATE OF session_id ON events
WHEN OLD.session_id <> NEW.session_id
BEGIN
    UPDATE session_sampling
    SET is_bot = CASE WHEN NEW.bot_reason_id <> 0 THEN 1 ELSE is_bot END,
        entry_page_title_id = CASE
            WHEN NEW.name_id = (SELECT id FROM dim_event_name WHERE value = 'pageview')
             AND NEW.pathname_id = (SELECT entry_page_id FROM sessions WHERE id = NEW.session_id)
             AND (entry_title_at IS NULL OR NEW.timestamp < entry_title_at
                  OR (NEW.timestamp = entry_title_at AND NEW.id < entry_title_event))
            THEN NEW.page_title_id ELSE entry_page_title_id END,
        entry_title_at = CASE
            WHEN NEW.name_id = (SELECT id FROM dim_event_name WHERE value = 'pageview')
             AND NEW.pathname_id = (SELECT entry_page_id FROM sessions WHERE id = NEW.session_id)
             AND (entry_title_at IS NULL OR NEW.timestamp < entry_title_at
                  OR (NEW.timestamp = entry_title_at AND NEW.id < entry_title_event))
            THEN NEW.timestamp ELSE entry_title_at END,
        entry_title_event = CASE
            WHEN NEW.name_id = (SELECT id FROM dim_event_name WHERE value = 'pageview')
             AND NEW.pathname_id = (SELECT entry_page_id FROM sessions WHERE id = NEW.session_id)
             AND (entry_title_at IS NULL OR NEW.timestamp < entry_title_at
                  OR (NEW.timestamp = entry_title_at AND NEW.id < entry_title_event))
            THEN NEW.id ELSE entry_title_event END
    WHERE session_id = NEW.session_id;
END;
