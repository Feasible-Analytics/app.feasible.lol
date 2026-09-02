--
-- 0014_plausible_channel_parity.sql
-- Correct Plausible display labels that the first channel backfill classified as referrals.
--
-- Created: 2026-09-02
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--

INSERT OR IGNORE INTO dim_channel (value) VALUES ('AI Assistants');
INSERT OR IGNORE INTO dim_channel (value) VALUES ('Organic Search');
INSERT OR IGNORE INTO dim_channel (value) VALUES ('Organic Social');

UPDATE imported_rollups
SET channel_id = (SELECT id FROM dim_channel WHERE value = 'AI Assistants')
WHERE source_id IN (
    SELECT id FROM dim_source
    WHERE value IN ('ChatGPT', 'Google Gemini', 'Microsoft Copilot')
);

UPDATE imported_rollups
SET channel_id = (SELECT id FROM dim_channel WHERE value = 'Organic Search')
WHERE source_id IN (
    SELECT id FROM dim_source WHERE value = 'Brave'
);

UPDATE imported_rollups
SET channel_id = (SELECT id FROM dim_channel WHERE value = 'Organic Social')
WHERE source_id IN (
    SELECT id FROM dim_source WHERE value = 'X (Twitter)'
);
