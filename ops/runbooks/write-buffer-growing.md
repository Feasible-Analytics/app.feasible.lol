<!--
write-buffer-growing.md
Diagnosing an ingester outbox that is not reaching app shards.

Created: 2026-09-01
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# The ingest outbox is growing

A standalone ingester returns `202` only after the derived event is committed
to its local `buffer.db`. It then keeps that row until the owning app shard
names the UUID in a successful commit response. A growing queue means accepted
pageviews are safe on the ingester volume but are not visible in the dashboard
yet.

## Symptom

| Series or signal | Meaning |
|---|---|
| `feasible_ingest_buffer_events` | Derived events awaiting an app acknowledgment |
| `feasible_ingest_buffer_oldest_seconds` | Customer-visible analytics delay |
| `feasible_ingest_buffer_parked_events` | Permanently rejected or malformed rows awaiting review |
| `feasible_ingest_flushes_total{outcome="error"}` | Delivery attempts are failing |
| `feasible_ingest_flush_duration_seconds` | App-shard delivery latency |
| `feasible_disk_available_bytes` | Remaining room for accepted events |

The total alone is not an outage: a burst naturally creates a shallow queue.
The oldest age rising continuously means delivery is behind. One sender runs
per configured app shard, so one failed shard must not stop healthy shards.

## Diagnosis

1. Query the ingester's private listener. Its readiness must remain healthy
   while an app is down as long as `buffer.db`, the cached routing snapshot,
   and the current salt remain usable.
2. Compare several ingesters. A queue rising everywhere for one destination is
   an app-shard incident. A queue rising on one ingester is its network, disk,
   signing key, clock, or cached route.
3. Check app private-listener logs for HMAC failures, `not_mine`, account SQLite
   errors, and partial commit responses. Verify clocks are within five minutes.
4. Check free space on the ingester volume. Unknown domains are held only while
   the static shard map is incomplete and are bounded at 100,000 rows or 50 MB;
   reaching that boundary returns retryable `503` rather than claiming data.

Inspect parked rows before replaying them. After correcting the permanent
request or configuration problem, restart that ingester with
`feasible ingest -replay-parked`; it moves the rows back atomically.

## Fix

Restore the destination app shard or the private network first. Do not restart
healthy ingesters merely because they are doing their job and retaining data.
Once the app returns, normal workers drain automatically and use larger catch-up
batches after the oldest event is five minutes behind.

Before a planned ingester restart, remove it from the public load balancer and
wait for its local queue to reach zero:

```bash
scripts/drain.sh http://127.0.0.1:19402/metrics
systemctl restart feasible-ingest
```

If the process will not restart but its volume survives, attach that volume to
a replacement with the same shard list and internal keys. Follow
[orphaned-ingester-volume.md](orphaned-ingester-volume.md).

## What makes it worse

- Deleting, truncating, or recreating `buffer.db` loses pageviews that already
  received `202`.
- Removing an app URL from `FEASIBLE_INGEST_SHARDS` while it still owns accounts
  makes the routing map falsely complete.
- Pointing a shard entry at a load-balanced group destroys deterministic account
  ownership; each entry must address that shard's private listener.
- Returning synthetic `202` responses at the edge acknowledges data no ingester
  owns.
- Terminating an ingester before its outbox is empty without preserving its
  volume strands accepted events.
