<!--
game-day.md
Break the store-and-forward topology on purpose and record each durable boundary.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Game day

Run this in staging quarterly and after changes to ingestion, SQLite durability,
session folding, or daily fingerprint derivation. The deployment should match production: a
load balancer, at least two ingesters with separate persistent volumes, at
least two app shards with separate account ownership.

A durability guarantee nobody has tested is a durability guess. This is the
exercise that turns it into a measurement: six things go wrong deliberately, and
each one has an observation written down in advance so the person running it
knows whether it passed.

The acceptance boundary is simple: every HTTP `202` has a durable row in one
ingester's `buffer.db`. That row remains until one app shard names its UUID as
committed. Eventually it has exactly one permanent UUID receipt and its fact or
committed policy rejection. A `503` is not accepted data; the browser retains
and safely replays it.

## Measuring stick

Send a fixed UUID-tagged stream, record status counts, and compare fact and
receipt deltas in the account database. Repeat selected UUIDs after each fault;
the fact count must not change.

```bash
feasible seed -http -url http://<load-balancer> -http-events 5000
sqlite3 /var/lib/feasible/accounts/000001/analytics.db \
  'SELECT COUNT(*) FROM events; SELECT COUNT(*) FROM recent_event_ids;'
```

For each exercise record start/end time, statuses, receipt delta, fact delta,
rejection delta, and pass/fail.

## 1. Kill an ingester during a request

Use `kill -9`, not SIGTERM. Requests without a response retry through another
ingester with the same UUID. Requests that received 202 must exist in that
ingester's SQLite outbox even if the app has not seen them. Reattach the failed
volume to a replacement and pass when every retried UUID has one receipt and at
most one fact.

Repeat after load-balancer deregistration and `scripts/drain.sh`; the graceful
run must complete without connection resets.

## 2. Stop one app shard

Keep sending traffic through both ingesters. Expected behavior:

- event requests continue returning `202` after local outbox commits;
- each ingester's buffer depth and oldest age rise for that shard;
- healthy app shards keep committing;
- the combined routing map becomes incomplete but retains its last routes.

Restore the app shard and confirm both ingesters drain to one fact per UUID.

## 3. Race two ingesters on one visitor

Send different UUIDs for one visitor through both processes at the same time,
including engagement-before-pageview ordering. Pass when the account contains
one session, every event links to it, and no orphan remains after adoption.

## 4. Lose a 202 response

Let the server commit, then reset the connection before the client receives the
response. The browser must replay the same UUID. Pass when the receipt and fact
counts remain one.

## 5. Verify the shared daily salt

Send the same debug request through two ingesters configured with the same
`FEASIBLE_INGEST_SALT`; both must report the same day and visitor identifier.
Repeat across 00:00 UTC and verify the identifier changes while a session that
started before midnight still folds through the previous-day lookup. A mismatch
means an ingester has the wrong shared configuration.

## 6. Fill an ingester disk, then an app disk

Fill one ingester volume close to zero, then send events. No failed outbox
commit may receive 202; the load balancer should use the other ingester. Next
fill one app volume: ingesters continue accepting and retain deliveries. Remove
the ballast, confirm readiness, and verify all original UUIDs drain once. Never
delete database WAL files during this exercise.

## After the exercise

Record routing completeness, per-ingester queue depth and oldest age, app
receipt/fact deltas, and parked rows. The exercise is incomplete until shard polling, `not_mine`
rerouting, HMAC rotation, app downtime, and an orphaned ingester volume have all
been tested.
