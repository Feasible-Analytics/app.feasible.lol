<!--
shard-down.md
An account database or shared account volume is unavailable.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Account storage is unavailable

The filename remains for existing operational links. There is no network shard
delivery service in the consolidated runtime: every event-serving process opens
the shared account database and commits directly.

## Symptom

- `/health/ready` is `503` when `control_db` or `account_directory` fails.
- Event requests for an unavailable account return `503`, never a durability
  `202`.
- `feasible_ingest_flushes_total{outcome="error"}` and HTTP 5xx increase.
- The tracker keeps failed bodies in its browser outbox with the same UUID.
- If only one account file is corrupt, other accounts continue and process
  readiness may remain healthy.

## Diagnosis

Check the shared mount, directory write probe, free space, permissions, and the
specific account database:

```bash
curl -s http://127.0.0.1:19402/health/ready | python3 -m json.tool
curl -s http://127.0.0.1:19402/metrics | \
  grep -E 'feasible_ingest_flushes_total|feasible_disk_available_bytes|feasible_database_wal_bytes_max'
sqlite3 /var/lib/feasible/accounts/000042/analytics.db 'PRAGMA integrity_check;'
```

An empty or stale site snapshot is different: it produces a named
`unknown_site` policy drop. Storage failure produces a retryable 503.

## Fix

Restore the mount or the individual account database, then run migrations before
returning the process to traffic:

```bash
feasible db migrate
feasible litestream check
```

For one damaged account, follow [restore-account.md](restore-account.md). After
recovery, use a real browser check to confirm the retained body drains and the
server stores one fact for its UUID.

## What makes it worse

- Returning 202 from a proxy-generated fallback page falsely acknowledges data
  that never reached SQLite.
- Deleting account databases or WAL files to make readiness green loses data.
- Starting a second process against a different local data directory splits the
  authoritative site, salt, receipt, and session state.
- Searching for network outbox rows, polling maps, `not_mine` responses, or an
  internal delivery endpoint follows architecture that current main removed.
