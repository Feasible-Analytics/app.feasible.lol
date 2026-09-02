--
-- 0010_minimise_account_deletions.sql
-- Make account deletion resumable and remove personal fields when work finishes.
--
-- Created: 2026-08-31
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--

-- Managed Payments migration 0006 already owns the durable account lease,
-- provider-customer retry set, irreversible cleanup checkpoints, and leased
-- lifecycle outbox. Re-declaring those columns here would both conflict with
-- the newer implementation and weaken its account-wide payment fencing.
--
-- Jobs and global files still need an explicit owner that survives
-- independently of their JSON arguments and account shard. New enqueues always
-- set this column; the backfill makes existing import/export work visible to
-- deletion. This migration intentionally follows M9 control migrations 0008
-- and 0009 so deployed team, sharing, report, and lease state is established
-- before M8 adds deletion ownership.
ALTER TABLE jobs ADD COLUMN owner_team_id INTEGER;
UPDATE jobs
SET owner_team_id = CAST(json_extract(args, '$.account_id') AS INTEGER)
WHERE kind IN ('csv_import', 'ga4_import', 'search_console_import', 'site_export')
  AND json_valid(args)
  AND json_type(args, '$.account_id') = 'integer';
CREATE INDEX jobs_owner_team ON jobs(owner_team_id);

-- Import uploads and prepared exports live outside the account shard. Snapshot
-- their canonical paths before shard deletion so a crash cannot make their
-- ownership undiscoverable, and checkpoint global removal independently.
ALTER TABLE account_deletions ADD COLUMN artifact_manifest TEXT NOT NULL DEFAULT '[]';
ALTER TABLE account_deletions ADD COLUMN artifacts_indexed_at INTEGER;
ALTER TABLE account_deletions ADD COLUMN global_removed_at INTEGER;

-- Version 0006 materialized known failed or interrupted Stripe deletions. A
-- completed legacy row with ambiguous notes also needs a retry identity before
-- its completion flag is reopened.
INSERT INTO account_deletion_customers (team_id, customer_id, created_at)
SELECT team_id, stripe_customer_id, started_at
FROM account_deletions
WHERE completed_at IS NOT NULL
  AND stripe_customer_id <> ''
  AND NOT (
      notes LIKE '%payment customer % removed%'
      AND notes NOT LIKE '%payment customer NOT removed%'
  )
ON CONFLICT (team_id, customer_id) DO NOTHING;

-- A completed pre-Managed-Payments row proves only that the legacy account
-- shard removal ran. Control cleanup must run again now that jobs and raw
-- payment events have durable team ownership, and global artifact discovery
-- must run because the old workflow did not own those directories as a step.
-- Leaving those checkpoints NULL makes the normal purger perform and attest
-- the newly introduced work instead of inheriting an unsafe legacy success.
UPDATE account_deletions
SET local_removed_at = completed_at,
    provider_removed_at = CASE
        WHEN stripe_customer_id = ''
          OR (notes LIKE '%payment customer % removed%' AND notes NOT LIKE '%payment customer NOT removed%')
            THEN completed_at
        ELSE NULL
    END
WHERE completed_at IS NOT NULL;

-- Older audit rows retained names, addresses, provider identifiers, filesystem
-- paths, and provider errors. Keep contact fields only while a confirmation is
-- pending, clear provider ids once removal is known, reopen completion for the
-- current resumable worker, and reduce notes to generic state.
UPDATE account_deletions
SET team_name = CASE
        WHEN notified_at IS NOT NULL THEN ''
        ELSE team_name
    END,
    contact_email = CASE
        WHEN notified_at IS NOT NULL THEN ''
        ELSE contact_email
    END,
    stripe_customer_id = CASE WHEN provider_removed_at IS NOT NULL THEN '' ELSE stripe_customer_id END,
    completed_at = NULL,
    notes = CASE
        WHEN notified_at IS NOT NULL
            THEN 'live account data removed; deletion confirmation sent'
        WHEN completed_at IS NOT NULL
         AND stripe_customer_id <> ''
         AND (
             notes = ''
             OR notes LIKE '%payment customer NOT removed%'
             OR notes NOT LIKE '%payment customer % removed%'
         )
            THEN 'live account data removed; payment customer removal pending'
        WHEN completed_at IS NOT NULL
            THEN 'live account data removed'
        ELSE ''
    END;

-- Per-customer retry rows have no audit purpose after successful provider
-- removal. Removing them prevents a provider identifier from becoming
-- permanent deletion history.
DELETE FROM account_deletion_customers
WHERE removed_at IS NOT NULL;
