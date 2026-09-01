<!--
litestream.md
Continuous replication of control.db and every account database.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Replication

Every SQLite database on the shared data volume is replicated continuously to
object storage by Litestream. The recovery point is roughly one second of
database state. A 202 proves the local account commit, not replica upload, so a
volume lost before its latest WAL reaches object storage can lose that sync
window; browser retries reduce but do not erase that recovery-point bound.

Written against **Litestream 0.3.x**.

## What is replicated

| File | Replica name |
|---|---|
| `$FEASIBLE_APP_DATA_DIR/control.db` | `control` |
| `$FEASIBLE_APP_DATA_DIR/accounts/000001/analytics.db` | `account-000001` |
| …one per account directory | `account-<id>` |

The account list is read from the **disk**, not from `control.db`. It is the same
choice `feasible db migrate` and `feasible db backup` make, for the same reason:
a box whose control database is unreadable is exactly when somebody is trying to
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
| `FEASIBLE_LITESTREAM_RETENTION_HOURS` | 72 | How far back a restore can go |

**Retention must stay longer than the snapshot interval.** A restore replays log
segments on top of the newest snapshot still within retention; retention shorter
than the snapshot interval deletes that snapshot before its replacement exists
and leaves a replica nobody can restore from. `feasible litestream config`
refuses to write a configuration that does this, naming both numbers.

## What must never reach the bucket

**`salt.key`.** It sits at `$FEASIBLE_APP_DATA_DIR/salt.key` and it decrypts the
fingerprint salts in `control.db`. The salts are what turn a visitor's IP and
user agent into a stored hash, and after 48 hours they are deleted so that the
hash cannot be reconstructed by anyone, us included. Litestream only replicates
the SQLite files the configuration names, so the key is not in the bucket by
default — **and it must never be put there.** The salts in the replicated
`control.db` are sealed; the key beside them in the same bucket would unseal
every historical copy at once.

Back the key up separately, to somewhere the replica credentials cannot reach. A
restored `control.db` without its key is a shard that cannot read any salt, which
is a fixable outage. A restored `control.db` with the key stored next to it is
not fixable, because the property it breaks is one we told customers about.

**Raw addresses.** There are none to leak: the IP is discarded at the event
endpoint before anything is written, so no replicated file has ever held one.
Nothing in a backup procedure may reintroduce one — packet captures, request
logs and "just log the header for a minute" are all the wrong answer.

**Credentials.** The generated file carries none. Litestream reads
`LITESTREAM_ACCESS_KEY_ID` and `LITESTREAM_SECRET_ACCESS_KEY` from its own
environment, so a file that is rewritten every time somebody signs up never
contains a secret. Give the credentials write and list permission on the prefix
and nothing else — replication needs no delete, and a key that cannot delete is a
key that cannot be used to destroy the backups.

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
EnvironmentFile=/etc/feasible/app.env
ExecStart=/usr/local/bin/feasible litestream config -watch

[Install]
WantedBy=multi-user.target
```

The watcher needs permission to write `/etc/litestream.yml` and to run the
on-change command. Give it exactly that.

## Verifying it, before you need it

```bash
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
