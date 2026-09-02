<!--
litestream.md
Continuous replication of system.db and every account database.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Replication

Every SQLite database on the shared data volume is replicated continuously to
object storage by Litestream. The recovery point is roughly one second of
database state. A 202 proves the local account commit, not replica upload, so a
volume lost before its latest WAL reaches object storage can lose that sync
window; browser retries reduce but do not erase that recovery-point bound.

Written against **Litestream 0.5.8 or newer**. Older releases do not support
disabling remote retention and are incompatible with the no-`DeleteObject`
replicator role below.

## What is replicated

| File | Replica name |
|---|---|
| `$FEASIBLE_APP_DATA_DIR/system.db` | `system` |
| `$FEASIBLE_APP_DATA_DIR/accounts/000001/analytics.db` | `account-000001` |
| …one per account directory | `account-<id>` |

The account list is read from the **disk**, not from `system.db`. It is the same
choice `feasible db migrate` and `feasible db backup` make, for the same reason:
a box whose system database is unreadable is exactly when somebody is trying to
work out what is on it.

Nothing else in the data directory is replicated, and that is deliberate — see
[what must never reach the bucket](#what-must-never-reach-the-bucket).

## Why the configuration is generated

Litestream reads its configuration **once, at start-up**. The set of databases is
not fixed: an account database is created by that account's first event. A file
somebody wrote by hand is correct until the next customer signs up and then
replicates everything except the newest account — the one most likely to be lost
and the one nobody would think to check.

So the file is generated:

```bash
feasible litestream config -print          # what would this replicate
feasible litestream config                 # write it to $FEASIBLE_LITESTREAM_CONFIG
feasible litestream check                  # is anything on disk not in it
```

`check` exits non-zero and names the files. Run it from monitoring. An
unreplicated account produces no error anywhere else in the system.

### A database that appears while replication is running

This is the interesting case, and it has three parts.

1. **`feasible litestream config -watch` re-reads the account directory** every
   `FEASIBLE_LITESTREAM_WATCH_SECONDS` (default 60). It renders the
   configuration and compares the bytes.
2. **On a real change it rewrites the file and runs
   `FEASIBLE_LITESTREAM_ON_CHANGE`** — normally `systemctl restart litestream`.
   The write is a temp-file-and-rename, so a daemon starting at that instant
   never reads half a file.
3. **On no change it does nothing.** Restarting the daemon interrupts
   replication for every database on the box, so it happens when an account
   appears, not once a minute.

```bash
feasible litestream config -watch \
  -interval 60s \
  -on-change 'systemctl restart litestream'
```

**The exposure window is one watch interval.** An account created at T has a live
database with no replication until the next tick, at most 60 seconds later. If
that box is lost inside the window, that account's data is gone — it is a
brand-new account, so "gone" is at most 60 seconds of its first minute, but the
runbook still says it rather than rounding it to nothing.

**Shortening the interval is not free.** Every change restarts the daemon, and a
restart re-verifies each database's position against its replica before resuming.
Sixty seconds is the balance: short enough that a new account is covered within a
minute, long enough that a busy signup hour does not restart replication
continuously.

**If `-on-change` is empty the watcher says so at `warn` and changes nothing
else.** A configuration file that names a new database and a daemon that has not
re-read it look identical from the outside, and only one of them is replicating.

## How many files this scales to

One stream per database, so **N accounts is N+1 streams**. Three things scale
with that number.

**Object storage requests.** With a one-second sync interval, each database with
new write-ahead log pages does one PUT per second. Accounts receiving traffic
commit about twice a second — the write buffer flushes every 500 ms, grouped by
account — so a busy account is one PUT per second, all day. A shard with 64
accounts all receiving traffic is up to 65 PUT/s, about 5.6 million requests a
day. At $0.005 per thousand that is roughly $28 a day in requests alone, before
storage. Quiet accounts cost nothing, because no new pages means no upload.

The lever is `FEASIBLE_LITESTREAM_SYNC_SECONDS`, and it trades **directly against
the recovery point**: ten seconds is a tenth of the request cost and ten seconds
of database state at risk instead of one. Do not change it without writing the
new number into the customer-facing durability claim.

**File descriptors.** Litestream holds each database, its write-ahead log and its
shadow log open — call it four descriptors per stream. Sixty-five databases is
fine at any default. A thousand databases is four thousand descriptors and the
usual 1024 limit stops replication partway through with an error only the daemon
sees. Set `LimitNOFILE` in the unit.

**Shard sizing bounds all of it.** Write throughput is flat from 1 to 16 accounts
and about 45% off the plateau at 64, so a shard holds tens of accounts rather
than thousands. That is a capacity decision made for other reasons, and it
happens to keep the replication fan-out in a range nothing struggles with.

## Intervals, and the one pairing that matters

| Variable | Default | What it decides |
|---|---|---|
| `FEASIBLE_LITESTREAM_SYNC_SECONDS` | 1 | The recovery point |
| `FEASIBLE_LITESTREAM_SNAPSHOT_HOURS` | 6 | How much log a restore replays |

Generated configuration requires Litestream v0.5.8 or newer and sets
`retention.enabled: false`. Provider lifecycle is the authoritative remote
retention mechanism, and the replicator therefore has no `DeleteObject`
permission. Litestream still performs its local cleanup behavior.

### Account deletion and replicas

Deleting an account removes its local directory. On the next watcher pass, at
most 60 seconds later with the default interval, the generated configuration no
longer names that database and the daemon is restarted without it. This stops
new replication; it does not synchronously erase existing object-store data and
Litestream can no longer clean a prefix after its database leaves the file.

The durable removal control is the bucket lifecycle rule
`feasible-replica-expiration-v1`, not Litestream's configuration. It filters the
entire shard prefix, so it continues to cover a deleted `account-*` prefix and
old `system` snapshots containing expired salts after either database stops
changing. Render the exact provider JSON with:

```bash
FEASIBLE_ENV=development feasible litestream policy \
  -replica-url s3://BUCKET/SHARD_PREFIX > /tmp/replica-lifecycle.json
```

The bucket owner applies that file with its S3-compatible lifecycle API. The
rule uses **2 days**, because Amazon S3 adds the configured days to object age
and rounds up to the next midnight UTC: eligibility is therefore 48 to 72 hours
after creation or supersession. It covers current objects, noncurrent versions
and incomplete multipart uploads. Production requires a bucket that has never
had versioning enabled and has no Object Lock; versioning or retention locks can
leave a recoverable historical version outside this bound and the check refuses
both states.

**Seventy-two hours is the eligibility bound, not a physical-removal bound.** S3
lifecycle queues eligible objects for asynchronous removal and publishes no
maximum completion time. Customer and legal copy says exactly that and notes a
replica may remain operationally restorable while provider removal is pending.

The replication credential needs `s3:ListBucket` limited to the shard prefix,
`s3:GetBucketLocation` on the bucket, and `s3:GetObject`/`s3:PutObject` on
objects below it. It needs no `s3:DeleteObject`,
`s3:DeleteObjectVersion` or lifecycle permission. The read-only checker needs
only `s3:GetLifecycleConfiguration`, `s3:GetBucketVersioning`, `s3:GetBucketLocation` and
`s3:GetBucketObjectLockConfiguration` on the bucket. The bucket-owner action that
installs or updates the policy needs `s3:PutLifecycleConfiguration`; it needs no
object-delete permission because the provider lifecycle service performs expiry.
The reviewable role split is versioned in
`ops/s3/replica-access-v1.jsonc`; keep those roles separate in production.

Fetch and validate actual provider state before starting production replication:

```bash
export FEASIBLE_LITESTREAM_ATTESTATION=/etc/feasible/replica-attestation.json
scripts/check-replica-lifecycle.sh
```

The script performs read-only provider calls, binds all responses to the exact
bucket and shard prefix, validates freshness, and publishes one evidence bundle
with one atomic rename. Every production process, including
`feasible litestream config` and `feasible litestream check`, fails closed if
that bundle is absent, stale, mismatched, or invalid.
Run the script in the deployment gate and from
scheduled monitoring; an old local export is not evidence that nobody changed
the bucket afterwards.

## What must never reach the bucket

**`salt.key`.** It sits at `$FEASIBLE_APP_DATA_DIR/salt.key` and it decrypts the
fingerprint salts in `system.db`. The live database deletes salts after 48
hours, but an encrypted system snapshot can retain the deleted row until the
provider removes it. The lifecycle rule makes each replica object eligible
within 72 hours of being written; asynchronous removal has no published maximum.
Litestream only replicates the SQLite
files the configuration names, so the key is not in the bucket by default —
**and it must never be put there.** Separate key storage reduces exposure, but
it does not erase the replica re-identification capability: an authorised
operator restoring both the system replica and its separately backed-up key,
together with matching analytics data, could test a fingerprint while a
provider-retained snapshot remains.

Back the key up separately, to somewhere the replica credentials cannot reach. A
restored `system.db` without its key is a shard that cannot read any salt, which
is a fixable outage. A restored `system.db` with the key stored next to it is
not fixable, because it collapses the separation that limits replica exposure.

Restore `system.db` only while the service is stopped. Before any app or ingest
process starts, prune expired salts older than 48 hours from the restored database;
otherwise the restore extends their live availability beyond the published
window. Then start the service and verify the normal two-row salt state.

**Raw addresses.** There are none to leak: the IP is discarded at the event
endpoint before anything is written, so no replicated file has ever held one.
Nothing in a backup procedure may reintroduce one — packet captures, request
logs and "just log the header for a minute" are all the wrong answer.

**Credentials.** The generated file carries none. Litestream reads
`LITESTREAM_ACCESS_KEY_ID` and `LITESTREAM_SECRET_ACCESS_KEY` from its own
environment, so a file that is rewritten every time somebody signs up never
contains a secret. Keep the replication, checker and bucket-owner policy roles
separate with the minimum permissions listed above.

## Installing it

Two services: the daemon, and the watcher that keeps its configuration current.

```ini
# /etc/systemd/system/litestream.service
[Unit]
Description=Litestream
After=network.target

[Service]
Restart=always
LimitNOFILE=65535
EnvironmentFile=/etc/feasible/litestream.env   # the two LITESTREAM_* credentials
ExecStart=/usr/local/bin/litestream replicate -config /etc/litestream.yml

[Install]
WantedBy=multi-user.target
```

```ini
# /etc/systemd/system/feasible-litestream-config.service
[Unit]
Description=Keep the Litestream configuration current as accounts are created
After=network.target

[Service]
Restart=always
EnvironmentFile=/etc/feasible/replica-check.env
EnvironmentFile=/etc/feasible/app.env
ExecStartPre=/usr/local/bin/check-replica-lifecycle.sh
ExecStart=/usr/local/bin/feasible litestream config -watch

[Install]
WantedBy=multi-user.target
```

The watcher needs permission to write `/etc/litestream.yml` and to run the
on-change command. Give it exactly that.

## Verifying it, before you need it

```bash
# Actual bucket retention, versioning and Object Lock still match policy.
scripts/check-replica-lifecycle.sh

# Everything on disk is in the configuration.
feasible litestream check

# The daemon agrees about what it is replicating.
litestream databases -config /etc/litestream.yml

# One database really has snapshots in the bucket, with timestamps.
litestream snapshots -config /etc/litestream.yml \
  /var/lib/feasible/accounts/000001/analytics.db
```

A replica with no snapshot newer than the snapshot interval is a replica that
has stopped, whatever the daemon's log says. Check it on a schedule, and check
it as the last step of every restore rehearsal.

Restoring is [runbooks/restore-account.md](runbooks/restore-account.md), and it
is rehearsed in [game-day.md](game-day.md) rather than read for the first time
during an incident.
