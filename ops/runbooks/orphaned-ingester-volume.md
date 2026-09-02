<!--
orphaned-ingester-volume.md
Recovering accepted events from an ingester whose host cannot return.

Created: 2026-09-01
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# An ingester volume is orphaned

`buffer.db` is the ownership record for every event that ingester acknowledged
but an app shard has not committed. The host is disposable; the volume is not.

## Symptom

- An ingester disappears with a non-zero last reported
  `feasible_ingest_buffer_events` value.
- The public load balancer has healthy replacement ingesters, so new traffic is
  safe, but the missing host's accepted events have not appeared in dashboards.

## Diagnosis

Confirm the host will not return and locate the exact persistent volume mounted
at `FEASIBLE_INGEST_BUFFER_PATH`. Do not attach one writable volume to two live
ingesters. Preserve the failed instance's shard list and shared HMAC key;
the cached routing map in the database is useful, but its static denominator
comes from configuration.

## Fix

1. Fence or terminate the old host so it cannot write the volume again.
2. Attach the volume to one replacement ingester at the same buffer path.
3. Configure the complete `FEASIBLE_INGEST_SHARDS`, shared
   `FEASIBLE_INGEST_SALT`, and an accepted signing key.
4. Start the replacement off the public load balancer. Watch the queue drain.
5. When `feasible_ingest_buffer_events` and parked rows are zero, stop it and
   retire the volume, or register it as a normal ingester.

UUID receipts make replay safe even when the old host died after the app commit
but before its acknowledgment reached the ingester.

## What makes it worse

- Formatting or deleting the volume because the replacement has an empty new
  queue loses already acknowledged pageviews.
- Starting both old and replacement hosts against the same SQLite volume risks
  filesystem-level corruption.
- Editing rows by hand defeats exact acknowledgment and routing guarantees.
- Treating the browser as the recovery source is insufficient: the browser may
  have removed an event after the ingester's `202`.
