<!--
game-day.md
Break it on purpose, on a schedule, and write down what actually happened.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Game day

A durability guarantee nobody has tested is a durability guess. This is the
exercise that turns it into a measurement: nine things go wrong deliberately, and
each one has an observation written down in advance so the person running it
knows whether it passed.

**Every exercise must end with zero lost events and zero double-counted events.**
That is the pass condition, and it is the same one every time.

Run it quarterly, and after any change to the ingest path, the transport, the
session fold or the fingerprint.

## Before you start

**Run this on a staging environment, never on production.** `feasible seed`
refuses to run with `FEASIBLE_ENV=production`, which is the harness this exercise
depends on. Build a staging shard and ingest tier with the same shape as
production — a load balancer, at least two ingestors, at least two shards, and
Litestream replicating to a bucket of its own.

Some exercises depend on the store-and-forward outbox and the polled routing map
in the ingest tier. Where they do, it is noted. An exercise you cannot run yet is
recorded as *not run*, never as passed.

### The measuring stick

Every exercise uses the same three commands. Learn them once.

**Send a known number of events:**

```bash
feasible seed -http -url http://<load-balancer> -http-events 5000
```

It prints what it sent, what was accepted, what was dropped and with which
reason, and the status codes it saw.

**Count what actually landed:**

```bash
DOMAIN=<the fixture's primary domain>
read ACCOUNT SITE <<<"$(sqlite3 -separator ' ' /var/lib/feasible/control.db \
  "SELECT account_id, id FROM sites WHERE domain='$DOMAIN';")"
sqlite3 "/var/lib/feasible/accounts/$(printf '%06d' "$ACCOUNT")/analytics.db" \
  "SELECT COUNT(*) FROM events WHERE site_id=$SITE;"
```

Take the count before and after. **The delta must equal the accepted count
exactly.** Lower is a loss. Higher is a double count. Both fail.

**Watch the pipeline:**

```bash
watch -n2 "curl -s http://127.0.0.1:19402/metrics |
  grep -E 'feasible_ingest_(buffer_events|events_accepted_total|flushes_total)|feasible_disk_available_bytes'"
```

### The record

For each exercise write down: the time it started, the observation at each step,
the before and after counts, the delta, and pass or fail. A game day whose result
is remembered rather than written is a game day you will run again from scratch.

---

## 1. Kill an ingestor mid-flight

**Break it.** With `feasible seed -http` running against the load balancer,
`kill -9` one ingestor. Not SIGTERM — the point is to skip the graceful path
entirely.

**Expected observation.**

- The load balancer removes it within about ten seconds: interval 5 s, unhealthy
  threshold 2.
- `feasible_ingest_events_accepted_total` on the surviving ingestors picks up the
  traffic. The seed run's status codes stay 202 apart from, at most, the requests
  in flight at the instant of the kill.
- Whatever was in that process's **in-memory** buffer is gone. Whatever had
  reached its **on-disk outbox** survives and is forwarded when it restarts.

**Pass.** Delta equals accepted, once the killed ingestor has restarted and
drained. If the delta is short by roughly one buffer's worth — up to 250 events —
that is the in-memory buffer, and it is the expected cost of `kill -9` rather
than a bug. Record the number.

**Then do it properly.** Repeat with `scripts/drain.sh` followed by SIGTERM. The
delta must now be exact, with no shortfall at all. **That difference is the whole
argument for drain-before-terminate**, and seeing it once is worth more than
reading it.

---

## 2. Take a shard down for ten minutes

**Break it.** `systemctl stop feasible` on one shard. Keep sending events for the
accounts that shard owns for ten minutes, then start it again.

*Depends on the store-and-forward outbox for a clean result.*

**Expected observation.**

- Visitors see nothing. `feasible_http_requests_total{handler="event",status="2xx"}`
  keeps climbing, and accept latency stays flat.
- `feasible_ingest_buffer_events` climbs steadily on every ingestor.
- `feasible_ingest_flushes_total{outcome="error"}` climbs;
  `feasible_ingest_flush_duration_seconds` piles into the top buckets at the
  30-second flush timeout.
- `feasible_disk_available_bytes` on the ingestors falls at roughly 400 bytes per
  accepted event.
- The shard's `/health/ready` does not answer at all.

**On restart:** the buffer falls back to its normal sawtooth within a minute or
two, and `feasible_ingest_events_written_total` on the shard climbs by roughly
what the buffer was holding.

**Pass.** Delta equals accepted. Nothing is missing and nothing is doubled.

---

## 3. Restart every ingestor while a shard is down

**Break it.** With the shard still stopped, restart every ingestor at once.

*Depends on the routing map being cached on disk.*

**Expected observation.**

- Each ingestor comes back with a routing map it read from its own disk, because
  the shard it polls is down and cannot supply one.
- `feasible_sites_routed` is **non-zero** on every restarted ingestor.
- `/health/ready` returns 200. An empty map would be 503 with
  `routing_map: the routing map is empty — every event would be dropped as an
  unknown site`, and the tier would be out of the load balancer.
- `feasible_ingest_events_dropped_total{reason="unknown_site"}` does **not** move.

**Pass.** Events for the down shard's domains keep being accepted and buffered
throughout, and the delta after recovery is exact. **A restart during an outage
must not turn a delay into a drop.**

---

## 4. Delete a site during a shard outage

**Break it.** With the shard still down, delete one of its sites through the app.
Keep sending events for that domain.

*Depends on the ingest tier parking events it cannot resolve.*

**Expected observation.**

- The events are **parked, not deleted.** An ingestor whose routing map is
  incomplete — it cannot reach the shard that owns the domain — must not conclude
  the site does not exist.
- `feasible_ingest_events_dropped_total{reason="site_deleted"}` stays flat while
  the map is incomplete.
- When the shard returns and the map is complete again, the parked events resolve
  to a deleted site and are dropped, and *that* is when
  `reason="site_deleted"` moves.

**Pass.** The drop happens once, on a complete map, and is counted. A drop
recorded while the map was incomplete is a fail — it is the difference between
"we know this site is gone" and "we could not ask".

---

## 5. Rebalance an account between shards under live traffic

**Break it.** With traffic flowing, move one account from shard A to shard B: move
`accounts/<id>/` as a directory, and update both shards' account lists.

**Expected observation.**

- Ingestors still holding the old map forward to shard A. Shard A no longer owns
  the account and rejects the batch as `not_mine`.
- The ingestor re-routes rather than dropping, and the log line
  `shard returned not_mine` carries `map_complete` and the action taken. With a
  complete map, the action is a re-route; with an incomplete one it must be a
  park.
- Within one poll interval every ingestor has the new map and forwards to B.
- `feasible_ingest_events_written_total` on B starts climbing; on A it stops for
  that account.

**Pass.** Delta equals accepted. **Neither shard has a partial copy of the same
event.** Check both databases: the account's events must be in exactly one of
them.

---

## 6. Restore a shard from replication onto a fresh box

**Break it.** Terminate a shard entirely — instance and volume. Build a new box,
give it the same address, and restore.

```bash
litestream restore -config /etc/litestream.yml \
  -o /var/lib/feasible/control.db \
  s3://feasible-backups/shard-01/control

for account in $(aws s3 ls s3://feasible-backups/shard-01/ | awk '{print $2}' | grep account-); do
  id="${account#account-}"; id="${id%/}"
  mkdir -p "/var/lib/feasible/accounts/$id"
  litestream restore -config /etc/litestream.yml \
    -o "/var/lib/feasible/accounts/$id/analytics.db" \
    "s3://feasible-backups/shard-01/${account%/}"
done

feasible db migrate
feasible litestream check
systemctl start feasible
```

**Expected observation.**

- `feasible litestream check` passes: every restored database is in the
  configuration.
- `/health/ready` returns 200 with every component `ok`, or `geolocation:
  degraded` if the city database has not been downloaded on the new box — **which
  is still a 200 and still takes traffic.**
- The ingestors reconnect with no configuration change, because the address is
  the same.
- The buffers drain.

**Pass.** Delta equals accepted, minus at most one second of committed state —
the replication sync interval — and even that is normally zero, because those
events were never acknowledged and are still in the outbox. **Time the whole
restore and write the number down. That number is the recovery time objective,
and until it is measured it is a wish.**

---

## 7. Kill a shard mid-forward

**Break it.** `kill -9` a shard at the moment a large batch is being forwarded.
Do it during a burst so the batch is big.

**Expected observation.**

- The forward fails or times out at the 30-second flush timeout. The batch
  returns to the front of the ingestor's buffer rather than being lost.
- `feasible_ingest_flushes_total{outcome="error"}` increments once per attempt.
- On restart, the shard replays the batch. **Any event it had already committed
  before dying is recognised by its id and skipped** — the dedupe table remembers
  written ids for 24 hours.

**Pass.** Delta equals accepted exactly. This exercise is specifically the
double-counting test: a partially committed batch redelivered in full must not
produce a single duplicate row.

---

## 8. Fill a disk

**Break it.** On a shard, fill the data volume to within a few megabytes:

```bash
fallocate -l <most of the free space> /var/lib/feasible/ballast
```

**Expected observation.**

- `feasible_disk_available_bytes` falls to near zero.
- `/health/ready` returns **503** naming `account_directory` — the probe creates
  and removes a file, which is the only check that catches a directory that stats
  perfectly and cannot take a byte.
- The load balancer removes the shard.
- `feasible_ingest_events_dropped_total{reason="internal_error"}` climbs on the
  shard, and the ingestors start buffering.
- `feasible_database_wal_bytes` stops falling: a checkpoint needs room too.

**Recover.** `rm /var/lib/feasible/ballast`. Readiness returns to 200 within one
check interval, the load balancer brings the shard back, and the buffers drain.

**Pass.** Delta equals accepted after recovery. Also confirm the reverse: while
the disk was full, **nothing reported success** — every failure is visible in the
metrics and in `/health/ready`, and none of it is silent.

---

## 9. Corrupt a database file

**Break it.** Stop a shard, corrupt one account database, start it again:

```bash
systemctl stop feasible
dd if=/dev/urandom of=/var/lib/feasible/accounts/000042/analytics.db \
   bs=4096 seek=3 count=2 conv=notrunc
systemctl start feasible
```

**Expected observation.**

- **`/health/ready` is still 200.** One account's file is not a component of the
  shard's readiness, and it must not be: fifty healthy accounts do not leave the
  load balancer because one file is broken.
- That account's queries fail —
  `feasible_query_failures_total{kind="internal"}` climbs — and its writes fail
  with `feasible_ingest_events_dropped_total{reason="internal_error"}`.
- `sqlite3 … 'PRAGMA integrity_check;'` reports the damage.

**Recover.** Follow [runbooks/restore-account.md](runbooks/restore-account.md)
end to end, including the customer-facing sentence. Do not shortcut it — the
point of this exercise is the runbook, not the corruption.

**Pass.** The account is restored, `feasible litestream check` passes, the
recovery point from the restored file is written down, and the delta for **every
other account on the shard is exact** — the blast radius was one account.

---

## After the exercise

Three things, in this order.

**Write down every number** — the recovery time in exercise 6, the buffer loss in
exercise 1, every delta. They are the only evidence the guarantee holds, and next
quarter's run needs something to compare against.

**Fix the runbooks that were wrong.** A step that did not work, a metric that did
not exist, a command that needed a flag nobody wrote down: fix it the same day,
while you still remember. That is what this exercise produces that nothing else
does.

**Tear the staging environment down, including the bucket.** It holds fixture
data, not customer data, but a forgotten replica bucket with real credentials is
a standing risk for no benefit.
