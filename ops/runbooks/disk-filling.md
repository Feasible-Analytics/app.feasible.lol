<!--
disk-filling.md
The disk is filling: which files, in what order, and what is actually safe to remove.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# The disk is filling

## Symptom

| Series | What it does |
|---|---|
| `feasible_disk_available_bytes` | Falling towards zero |
| `feasible_database_bytes{database="accounts"}` | Growing — the normal cause |
| `feasible_database_wal_bytes{database="accounts"}` | Growing and never falling — the abnormal one |
| `feasible_database_wal_bytes_max` | One account far above the rest |
| `feasible_ingest_buffer_events` | Climbing, once writes start failing |
| Event endpoint 5xx | Increasing because failed commits are retryable |
| `feasible_database_directory_readable` | Drops to 0 if the directory itself becomes unusable |

`/health/ready` fails on `account_directory` once the disk is genuinely full:
the probe creates and removes a file, which is the only check that answers the
question being asked. A directory that stats perfectly well and cannot take a
byte is exactly this failure.

Alert well before zero. **Ten percent free on a shard is already an incident**,
because the write-ahead log needs room to grow before a checkpoint can shrink
the database, and a database that cannot grow cannot checkpoint either.

## What is in the data directory

Everything lives under `FEASIBLE_APP_DATA_DIR`. This is the whole inventory,
ordered by how safe it is to remove.

| Path | What it is | Safe to remove |
|---|---|---|
| `backups/` | Snapshots from `feasible db backup` | **Yes** — oldest first |
| `favicons/` | Cached source icons for report rows | **Yes** — refetched on demand |
| `exports/` | Prepared archives from the export screen | **Yes, once collected** — see below |
| `imports/` | Uploaded files waiting to be imported | **Only after the import has run** |
| `geoip/dbip-city-lite.mmdb` | City geolocation, ~60 MB | **Yes** — countries still work, cities go unknown |
| `geoip/dbip-country-lite.mmdb` | Country geolocation | Yes, but it degrades every event's country |
| `lists/` | Refreshed bot and spam lists | Yes — an embedded baseline is compiled in |
| `salt.key`, `app.key`, `script.key` | Encryption keys | **Never** |
| `system.db`, `system.db-wal` | Sites, users, keys, salts, jobs | **Never** |
| `accounts/*/analytics.db`, `-wal` | Customer data | **Never** |

**Never delete a `-wal` file.** It is not a log in the "old and disposable"
sense; it holds committed transactions that are not yet in the database file.
Deleting one loses them, and on a file the process has open it corrupts the
database.

## Fix, in order

**1. The reclaims that cost nothing.**

```bash
du -sh /var/lib/feasible/*
ls -lt /var/lib/feasible/backups | tail -20
rm /var/lib/feasible/backups/<the oldest snapshots>
rm -rf /var/lib/feasible/favicons
```

`feasible db backup` writes a dated file per database per run and never prunes.
On a shard with fifty accounts that is fifty files a run, and it is nearly always
the answer. Favicons are a cache and refill themselves.

**2. Exports and imports.**

`exports/` holds a prepared archive per export job. Once the customer has
downloaded it, it is a file nobody will ask for again. `imports/` holds uploads;
removing one that has not been imported loses the customer's file, so check the
job first — `feasible_jobs{state="available"}` above zero means work is still
queued.

**3. The city database, if you need 60 MB right now.**

```bash
rm /var/lib/feasible/geoip/dbip-city-lite.mmdb
```

Geolocation reports `degraded` and keeps taking traffic; cities become unknown
and countries keep working. It is a download, not data.

**4. A write-ahead log that will not shrink.**

`feasible_database_wal_bytes_max` far above the others is one account whose
checkpoint is not completing, usually because a long-running read is holding the
file. The log is truncated when the process closes the handle cleanly:

```bash
scripts/drain.sh http://127.0.0.1:19402/metrics   # on each ingestor
systemctl restart feasible                         # closes every handle, checkpointing each WAL
```

Shutdown closes every account handle so each write-ahead log is checkpointed on
the way out rather than left for the next start-up to recover.

**5. Reclaim space inside the databases.**

```bash
feasible db backup -out /mnt/roomier/snapshots
```

`db backup` uses `VACUUM INTO`, which takes a read transaction and compacts as
it copies. It needs room for the copy, so point `-out` at a different filesystem
when the problem is space. It refuses to overwrite an existing file rather than
replacing yesterday's good snapshot with today's broken one.

**6. Grow the volume.** For a shard, this is usually the real answer. A million
pageviews is about 300 MB indexed and summarised — roughly 210 bytes per event,
all in — and raw rows age out while roll-ups do not, so the long-run figure per
year is lower than multiplying by twelve suggests.

## What makes it worse

**Deleting a `-wal` file.** It holds committed transactions. On an open database
it corrupts the file; on a closed one it silently loses whatever had not been
checkpointed.

**Deleting an ingest outbox.** A hosted ingester's `buffer.db` contains events
that may already have received `202` and are waiting for an app acknowledgment.
Move or drain its persistent volume; never remove the file to reclaim space.

**Deleting an account database that "looks stale".** There is no such thing here:
a quiet account is a customer with a quiet website.

**Running `VACUUM` in place on a full disk.** It needs roughly as much free space
as the database itself, and it takes the write lock for the whole operation.
`db backup -out` on another filesystem is the version that works.

**Truncating the salts table to reclaim space.** It is a few rows and it is the
only thing that can find yesterday's sessions. See
[salt-rotation.md](salt-rotation.md).

**Turning off `feasible db backup` because it fills the disk.** Point `-out`
somewhere else and prune it. Replication is the recovery mechanism, but the
snapshots are what you fall back to when the replica is unusable, and discovering
you have neither is a bad day.
