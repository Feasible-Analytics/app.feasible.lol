<!--
write-buffer-growing.md
Diagnosing direct account writes that are slow or failing.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# The write buffer is growing

The consolidated runtime has no acknowledged store-and-forward backlog. An
event request joins the in-memory batch, waits for its account transaction, and
receives `202` only after that transaction commits. A failed write receives
`503`; the tracker retains its UUID-tagged body and retries it later.

## Symptom

| Series or signal | Meaning |
|---|---|
| `feasible_ingest_buffer_events` | Requests are waiting for a direct account write |
| `feasible_ingest_flush_duration_seconds` | SQLite commit latency |
| `feasible_ingest_flushes_total{outcome="error"}` | Direct account writes are failing |
| Event endpoint 5xx rate | Browsers are being asked to retry |
| `feasible_disk_available_bytes` | A full shared volume may be blocking every account |
| `feasible_database_wal_bytes_max` | One account may have a stalled checkpoint |

A healthy buffer is a shallow sawtooth that drains at least every 500 ms. A
sustained ramp means request latency is rising; unlike the retired outbox
architecture, those events have not received a success response.

## Diagnosis

1. Check `/health/ready`. A failed `account_directory` or `control_db` names a
   shared-storage incident.
2. Compare flush errors with duration. Errors point to an unavailable, locked,
   corrupt, or full account database. Long successful flushes point to storage
   latency or a checkpoint held by a reader.
3. Check `feasible_database_wal_bytes_max` and free disk. Follow
   [disk-filling.md](disk-filling.md) before attempting database maintenance.
4. Identify whether one account fails while others continue. Readiness checks
   the directory, not every account file; one corrupt account remains isolated.

## Fix

Restore the direct write dependency: free or grow the volume, restore the
affected account database, end the reader holding a checkpoint, or repair the
shared mount. Keep healthy serving processes running so tracker retries have a
destination.

For a planned restart, remove the instance from the load balancer and drain it:

```bash
scripts/drain.sh http://127.0.0.1:19402/metrics
systemctl restart feasible-ingest
```

Confirm the buffer returns to its normal sawtooth and the event endpoint's 5xx
rate stops. Permanent UUID receipts make browser replay safe at any age.

## What makes it worse

- Restarting every process at once removes every retry destination.
- Deleting a WAL file loses committed transactions and can corrupt its database.
- Increasing the flush timeout hides slow writes while requests wait longer.
- Treating a 503 as an accepted drop destroys the browser's recovery path.
- Looking for an ingest outbox or internal shard sender wastes incident time;
  neither exists in the consolidated runtime.
