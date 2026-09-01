<!--
restore-account.md
Restoring one account database from replication, what it costs, and what to tell the customer.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Restore an account database from replication

## Symptom

One account, not the whole shard.

| Series | What it does |
|---|---|
| `feasible_database_files` | Falls by one, while `feasible_sites_routed` does not |
| `feasible_ingest_events_dropped_total{reason="internal_error"}` | Increasing |
| `feasible_query_failures_total{kind="internal"}` | Increasing |
| `feasible_database_directory_readable` | Still 1 — the directory is fine, one file is not |

The account's dashboard returns errors or an empty history. `/health/ready` on
the shard is still **200**: one account's file is not a component of the shard's
readiness, and it should not be — a single corrupt file must not take a box
serving fifty other accounts out of the load balancer.

## Diagnosis

Establish which of three things happened, because the recovery differs.

```bash
# The file is missing.
ls -l /var/lib/feasible/accounts/000042/

# The file is there and unreadable.
sqlite3 /var/lib/feasible/accounts/000042/analytics.db 'PRAGMA integrity_check;'

# The file is fine and the schema is behind this build.
feasible db migrate
```

A database at an older schema version is **refused on open**, with a message
naming the file and both versions. That is not corruption; that is a migration
that has not been run, and restoring over it would be the wrong repair.

## Fix

**1. Stop the process that would hold the file open.**

```bash
scripts/drain.sh http://127.0.0.1:19402/metrics   # on each ingestor first
systemctl stop feasible                            # then the shard
```

Restoring over a database a running process has open is how one corrupt file
becomes two. Nothing accepts events for this shard while it is down — the
ingestors buffer, which is what they are for; see
[shard-down.md](shard-down.md) for how long that is safe.

**2. Restore to a new path, never over the live file.**

```bash
litestream restore -config /etc/litestream.yml \
  -o /var/lib/feasible/restore/000042.db \
  s3://feasible-backups/shard-01/account-000042
```

To go back to a specific moment rather than the newest state, add
`-timestamp 2026-08-30T14:00:00Z`. Restoring to the newest state is the default
and is what you want unless you are undoing a bad write.

**3. Verify it before it is anybody's data.**

```bash
sqlite3 /var/lib/feasible/restore/000042.db 'PRAGMA integrity_check;'
sqlite3 /var/lib/feasible/restore/000042.db 'SELECT COUNT(*), MAX(timestamp) FROM events;'
```

The newest timestamp is the honest recovery point. Write it down — it is the
number the customer gets told.

**4. Move the damaged file aside, put the restored one in place.**

```bash
mv /var/lib/feasible/accounts/000042/analytics.db \
   /var/lib/feasible/accounts/000042/analytics.db.damaged
mv /var/lib/feasible/restore/000042.db \
   /var/lib/feasible/accounts/000042/analytics.db
chown feasible:feasible /var/lib/feasible/accounts/000042/analytics.db
```

Keep the damaged file until the account is confirmed healthy. It costs disk and
it is the only copy of anything the replica did not have.

**5. Bring the schema up and the summaries back.**

```bash
feasible db migrate
systemctl start feasible
feasible rollup rebuild -site <the account's domain>
```

**6. Confirm replication picked the file back up.**

```bash
feasible litestream check
```

A restored file at the same path is already in the configuration, so this should
pass immediately. If it does not, the file is live and unreplicated, which is
worse than the incident you just fixed.

## What is lost

The replica sync window is the authoritative bound.

**Up to one second of committed database state.** `FEASIBLE_LITESTREAM_SYNC_SECONDS`
is the recovery point, and it is 1 by default. Everything the shard committed in
that last second and never shipped is gone.

**Requests whose account transaction did not commit were not answered with
202.** The official tracker retains those UUID-tagged bodies and retries them.
That recovery is best effort because a browser can close permanently; do not
subtract it from the replica recovery-point claim.

**There is no age-based dedupe expiry.** Browser UUID receipts are permanent and
commit with their facts or policy rejections. A replay found in the restored
receipt table is skipped at any age. Manual imports remain a separate data path;
do not invent UUIDs or write fact rows around the ingest transaction.

## Telling the customer

Say it plainly, with the number from step 3, and say it before they ask.

> Between *(the newest timestamp in the restored file)* and *(the moment the
> shard came back)*, some pageviews for *(domain)* were not recorded. We restored
> from continuous replication; the gap is the last second of data before the
> failure plus anything that was in flight. Everything before and after that
> window is complete, and nothing has been counted twice.

Two things this project does not do: round the gap down, and describe a restore
as "no data was lost" when a window exists. The whole reason the gap is one
second rather than one day is that we can afford to be exact about it.

If the account has an export, the customer may have their own copy of the missing
window. Use the supported import path and verify its own idempotency rules.

## What makes it worse

**Restoring over a live file.** The process has the database and its write-ahead
log open. Overwriting the file underneath it corrupts both, and now the damaged
copy you kept is the only good one.

**Skipping the verification and the timestamp.** Without the newest timestamp
there is no honest recovery point, and the customer gets "some data may be
missing", which is the sentence that costs the account.

**Deleting the damaged file immediately.** It may hold events the replica never
received. Nothing is cheaper than a file on disk for a week.

**Restoring the whole shard to fix one account.** Every other account on the box
loses its last second too, for a fault none of them had.

**Restoring `control.db` from an old timestamp to "match".** The account
databases and the control database are replicated independently and are not a
consistent set. Rolling the control database back deletes sites, keys and users
created since, and none of that is in an account database to recover from.

**Putting `salt.key` into the replica bucket while you are in there.** It
decrypts the fingerprint salts in `control.db`, and a copy beside them unseals
every historical snapshot at once. Back it up somewhere the replica credentials
cannot reach.
