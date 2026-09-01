<!--
game-day.md
Break the consolidated runtime on purpose and record the durable boundary.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Game day

Run this in staging quarterly and after changes to ingestion, SQLite durability,
session folding, or salt erasure. The deployment should match production: a
load balancer, at least two event-serving processes over the same data volume,
and Litestream replication.

## Control migration coordination

This branch owns control `0010_minimise_account_deletions`. Do not reserve or
stub the missing numbers: rebase it after Stripe `0006`, the control-global
random salt authority `0007`, and M9 `0008`/`0009`, resolve column/index
conflicts against those real migrations, then run the full `0001` through
`0010` upgrade. Until that rebase, the migration runner intentionally refuses
to stamp a version-5 control database across the gap.

Managed Payments rebase conflicts should stay in `internal/cli/commerce.go`,
where its provider-backed `CustomerRemover` is attached to the shared purger.
The authenticated deletion handler deliberately has no separate Stripe client.

A durability guarantee nobody has tested is a durability guess. This is the
exercise that turns it into a measurement: eight things go wrong deliberately, and
each one has an observation written down in advance so the person running it
knows whether it passed.

The acceptance boundary is simple: every HTTP `202` has exactly one permanent
UUID receipt and its committed fact or committed policy rejection. A `503` is
not accepted data; the browser must retain and safely replay it.

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

## 1. Kill a serving process during a request

Use `kill -9`, not SIGTERM. Requests without a response retry through another
process with the same UUID. Requests that received 202 must already exist in
SQLite. Pass when every retried UUID has one receipt and at most one fact.

Repeat after load-balancer deregistration and `scripts/drain.sh`; the graceful
run must complete without connection resets.

## 2. Make the account volume read-only

Keep sending traffic while direct writes fail. Expected behavior:

- event requests return `503`, not `202`;
- flush error and HTTP 5xx counters rise;
- the browser outbox retains the same serialized UUID body;
- readiness names `account_directory` when the directory probe fails.

Restore writes and confirm the browser replay creates one fact.

## 3. Race two processes on one visitor

Send different UUIDs for one visitor through both processes at the same time,
including engagement-before-pageview ordering. Pass when the account contains
one session, every event links to it, and no orphan remains after adoption.

## 4. Lose a 202 response

Let the server commit, then reset the connection before the client receives the
response. The browser must replay the same UUID. Pass when the receipt and fact
counts remain one.

## 5. Refuse salt erasure

In an isolated staging copy, install a SQLite trigger that rejects deletion from
`salts`, advance the test clock past retention, and refresh. Readiness and event
derivation must fail closed; the in-memory cache must not continue hashing.
Remove the trigger and verify current plus pre-provisioned next-day authority
material recovers.

## 6. Fill the shared disk

Fill a staging volume close to zero, then send events. No failed account commit
may receive 202. Remove the ballast, confirm readiness, and verify retained
browser events drain once with their original UUIDs. Never delete database WAL
files during this exercise.

## 7. Restore one account from Litestream

Stop writers, preserve the damaged file, restore the account to its original
path, run `feasible db migrate`, and restart. Replay UUIDs from before and after
the recovery point. Permanent receipts prevent duplication whenever the receipt
was included in the restored transaction; report the replica sync window as the
possible committed-data loss.

## 8. Interrupt an account deletion and inspect replica expiry

**Break it.** On staging, put a fixture account at day 90, keep one stale ingest
route and buffered batch for it, and make the payment-provider delete call fail.
Run one lifecycle sweep, kill the app after local removal, restart it, restore the
provider stub, and run the next sweep.

**Expected observation.** The first sweep creates
`.account-deletions/account-<id>.deleted` before removing control or account data.
The stale writer drains without recreating `accounts/<id>/`; the failed provider
identifier remains only for retry, `completed_at` stays NULL, and restart resumes
the exact pending work. The second sweep removes any deliberately recreated test
file again, completes provider deletion, and only then marks completion and sends
confirmation.

Run `scripts/check-replica-lifecycle.sh`. Inspect representative keys below both
`account-<id>/` and `control/` with the provider's object metadata API. Their
lifecycle expiration date must make them eligible no later than 72 hours after
creation or supersession. Record the eligibility date. Do not record physical
removal as immediate: provider lifecycle is asynchronous and has no published
maximum completion time.

**Pass.** No process or restart recreates live account data, deletion completion
waits for every checkpoint and provider success, the lifecycle checker passes for
the exact shard prefix, and both the deleted account prefix and old control
snapshots are covered. Attempting the restore runbook for the deleted id must stop
at its tombstone check.

---

## After the exercise

## What is deliberately absent

Do not test shard polling, network forwarding, `not_mine`, internal HMAC
delivery, an ingestor SQLite outbox, or an unrouted holding database. Current
main removed those components; operational exercises must not teach responders
to depend on them.
