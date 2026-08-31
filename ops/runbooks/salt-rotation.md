<!--
salt-rotation.md
A salt rotation went wrong. This one is a privacy incident as well as a correctness one.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# A salt rotation went wrong

The salt is the key a visitor's IP and user agent are hashed with. Two claims
rest on it, and both break here:

- **Visitor counts are right.** The same person on the same site on the same day
  is one visitor, because the same salt produced the same hash.
- **After 48 hours nobody can reconstruct a fingerprint, us included.** The salt
  that produced the hash has been deleted, so there is nothing left to test a
  guess against.

Three rules make those true. Rotation is at **00:00 UTC**, never a local
midnight. **Exactly two salts are live** — today's, which hashes, and
yesterday's, which is a session-lookup fallback and nothing else. **Rows older
than 48 hours are deleted, not archived.** Everything below is one of those three
having failed.

## Symptom

The correctness half often arrives as a customer report rather than a page, and
that is by design: no metric on `/metrics` carries a visitor, a site or a domain.

| Signal | What it means |
|---|---|
| `/health/ready` names `salts` as `failed` | No salt can be read or created — the process is 503 and out of the load balancer |
| `feasible_ingest_events_dropped_total{reason="internal_error"}` climbing | Without a salt there is no visitor id and therefore no event: accepted, counted as our failure, thrown away |
| `feasible_ingest_sessions_live` collapsing, then climbing from zero | Every visitor has become a new visitor |
| A customer reports visitors ≈ pageviews | The classic shape of a salt that changed under live traffic |
| A customer reports visitors doubling exactly | Two processes hashing with different salts |

## Diagnosis

**Count the salts.** On the shard's control database:

```bash
sqlite3 /var/lib/feasible/control.db \
  "SELECT COUNT(*),
          datetime(MIN(created_at), 'unixepoch') AS oldest,
          datetime(MAX(created_at), 'unixepoch') AS newest
   FROM salts;"
```

`created_at` is pinned to the start of the UTC day, so:

| Count | Meaning |
|---:|---|
| 2 | Healthy. Today and yesterday |
| 1 | The install's first day, or yesterday's row was lost |
| 3 or more | **The prune has stopped.** This is the privacy failure |
| 0 | Nothing can be hashed; readiness is already 503 |

`newest` should be today's date at `00:00:00`. Anything else means rotation did
not happen at 00:00 UTC.

**Ask two processes to hash the same request.** The debug endpoint returns the
derived event instead of writing one, and it names the salt day and the visitor
id it produced:

```bash
for host in ingest-01 ingest-02; do
  curl -s -X POST "http://$host:19302/api/event" \
    -H 'content-type: application/json' \
    -H 'X-Debug-Request: true' \
    -d '{"n":"pageview","d":"example.com","u":"https://example.com/"}' |
    python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["salt_day"], d["user_id"], d["site_domain"])'
done
```

**Every process that derives a fingerprint must return the same `user_id` for
the same request.** If two do not, they are not reading the same salts, and every
visitor the load balancer spreads between them is counted twice. `salt_day`
differing as well means one of them has not rotated.

## Fix

### The salts cannot be read at all

`salts` failed and `feasible_ingest_events_dropped_total{reason="internal_error"}`
is climbing. The rows are sealed with a key the process no longer has.

The key is `FEASIBLE_SALT_KEY` — 64 hex characters — or, when that is unset,
`$FEASIBLE_APP_DATA_DIR/salt.key`, generated on first run. If the file is
corrupt the process says so by name.

**Restore the key.** It is small, it does not change, and it should be backed up
somewhere the replica credentials cannot reach. Put it back and the process
recovers on its next refresh, within 90 seconds.

**If the key is genuinely gone**, the salt rows are permanently unreadable and
the only way forward is to delete them and let today's be created fresh:

```bash
systemctl stop feasible
sqlite3 /var/lib/feasible/control.db 'DELETE FROM salts;'
systemctl start feasible
```

The cost, stated exactly: **every visitor already active today gets a new id from
that moment on.** Today's visitor count for every site on this shard is inflated
by roughly the number of visitors who were mid-visit, and visits open across the
change are split in two. Yesterday and earlier are untouched — those events are
already stored with the ids they had. Tell the affected accounts that today's
visitor number is high and tomorrow's is correct.

### The salt did not rotate at 00:00 UTC

The store creates today's salt on demand as well as on its 90-second ticker, so
a stale `newest` means every process failed to write. Nearly always the control
database was read-only or full — check `feasible_disk_available_bytes` and the
`account_directory` component — and the repair is that failure, not the salt.
Once writes work, the next refresh creates the day's salt.

### There are three or more salt rows

The prune runs on every refresh, after the load, so a failure to prune never
leaves a process without salts — it leaves rows behind instead. **This is the
privacy failure.** A salt older than 48 hours, together with the stored hashes,
makes a visitor's IP and user agent brute-forceable, which is precisely the thing
retention exists to prevent.

```bash
sqlite3 /var/lib/feasible/control.db \
  "DELETE FROM salts WHERE created_at < strftime('%s','now') - 172800;"
```

Then find out why the refresh was failing — the same write failure as above, or a
process that has not refreshed since it started. Record how long the extra rows
survived. It is a retention breach with a duration, and the duration is the thing
anybody assessing it will ask for.

### Two processes disagree

Fix the disagreement, do not paper over it. The two debug responses tell you
which is wrong. Bring the odd one back onto the same salts and restart it after
draining:

```bash
scripts/drain.sh http://127.0.0.1:19402/metrics && systemctl restart feasible-ingest
```

Events already written with the wrong ids cannot be repaired — the salt is not
stored with them and there is nothing to re-derive from. Say so to the affected
accounts rather than quietly recomputing something that would be a guess.

## Backups, and the 48-hour promise

The live system deletes salts after 48 hours. **Replication does not** — the
default `FEASIBLE_LITESTREAM_RETENTION_HOURS` is 72, so the bucket holds
snapshots of `control.db` containing salt rows the live database has already
deleted.

That does not break the promise, and the reason it does not is the one rule that
must never be relaxed: **the salt rows in the replica are sealed, and the key
that unseals them is not in the bucket.** `salt.key` is a plain file that
Litestream does not replicate, because Litestream replicates only the SQLite
databases the configuration names.

So:

- **Never put `salt.key` in the replica bucket**, and never back it up with
  credentials that can read the replica. The two halves in one place unseal every
  historical snapshot at once, and that is a promise broken retroactively for
  every visitor in the window.
- **Never restore `control.db` to an old timestamp to recover something else.**
  It resurrects salts that retention deleted on purpose — and it rolls back
  sites, users and API keys created since, none of which is recoverable from an
  account database.
- If you want the replica window itself inside 48 hours, lower
  `FEASIBLE_LITESTREAM_RETENTION_HOURS` — and understand you are trading away how
  far back the control database can be restored.

## What makes it worse

**Copying `FEASIBLE_SALT_KEY` between environments.** Staging with production's
key can decrypt production's salts, which turns a test box into the one machine
that can reverse real visitors' fingerprints.

**Running `feasible seed` against a production data directory.** The seed
generator replaces the random source so a fake dataset hashes to the same ids
every run. A predictable salt is a reversible fingerprint. Nothing that serves
traffic may ever do this.

**Lengthening retention past 48 hours, or turning the prune off.** It is the
whole basis of "no cookies, no persistent identifiers". It is not a tunable.

**Rotating the salt manually to "reset" bad numbers.** It creates the exact
failure this runbook describes, deliberately, and the numbers before the reset do
not improve.

**Deleting the salts table to reclaim disk.** It is two rows. It is also the only
thing that can find yesterday's sessions, so every visit running over midnight
splits in two.

**Restoring an old `control.db` onto a running shard to get a salt back.**
Sessions were folded against the current salts; swapping them under a live
process produces a shard where some visits can be found and some cannot, with no
error anywhere.
