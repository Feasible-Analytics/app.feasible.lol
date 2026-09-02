<!--
shard-down.md
An app shard or its account storage is unavailable.

Created: 2026-09-01
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# An app shard is unavailable

App downtime must make dashboards unavailable, not lose pageviews. Public event
traffic continues to any healthy ingester, which commits each event to its
local outbox and retries the owning app shard until it returns.

## Symptom

- Dashboard and API requests routed to the failed app shard return errors.
- Every ingester's queue for that destination grows and
  `feasible_ingest_buffer_oldest_seconds` rises.
- Other app shards keep receiving batches and their account dashboards remain
  current.
- Public `/api/event` continues returning `202` while ingester disks and routing
  snapshots are healthy.

## Diagnosis

1. Query the failed app's private `/health/ready` endpoint and inspect
   `system_db`, `account_directory`, and `routing_map`.
2. Check its local `system.db`, account volume, permissions, and free space.
3. Call `/internal/domains` with an authenticated operator client and confirm
   the shard still publishes its owned domains. A failed poll must leave each
   ingester's previous contribution intact but makes the combined map
   incomplete after 60 seconds.
4. If only one account database is damaged, verify other accounts on that shard
   still commit and follow [restore-account.md](restore-account.md).

## Fix

Restore the app process or its attached account volume. Run maintenance while
the shard is out of the load balancer, then return it to service:

```bash
feasible db migrate
feasible litestream check
```

The ingesters drain automatically. Confirm buffer oldest age falls to zero and
that repeated UUIDs create one permanent receipt and at most one fact.

## What makes it worse

- Repointing the shard URL to a different app that does not own those accounts
  creates `not_mine` churn and can strand the queue.
- Deleting account databases or WAL files to make readiness green loses data.
- Removing the failed shard from every ingester's static list makes an
  incomplete routing map appear complete and allows unknown-domain drops.
- Copying account SQLite files while either process has them open is not a
  failover procedure; use the replica and restore runbook.
