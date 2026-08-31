<!--
shard-down.md
A shard is down, the ingestors are buffering, and nothing on the front door looks wrong.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# A shard is down and the ingestors are buffering

## Symptom

The front door looks perfect. That is the whole difficulty of this failure.

| Series | What it does |
|---|---|
| `feasible_ingest_buffer_events` | Climbs and never comes back down |
| `feasible_ingest_flushes_total{outcome="error"}` | Increasing |
| `feasible_ingest_flush_duration_seconds` | Piling into the top buckets, at the 30 s flush timeout |
| `feasible_ingest_events_accepted_total` | **Unchanged.** Visitors are fine |
| `feasible_http_requests_total{handler="event",status="2xx"}` | **Unchanged.** Every event still gets its 202 |
| `feasible_ingest_events_written_total` on the shard | Flat, or the shard is not being scraped at all |
| `feasible_disk_available_bytes` on the ingestor | Falling steadily |

`/health/ready` on the shard is 503 and names the component, or does not answer.

Alarm on the **worst** ingestor, never the average. One instance wedged against a
shard that owns half the accounts disappears completely into a mean.

## How long you have

Two limits, and they are not the same size.

**The in-memory write buffer is the first one.** A derived event costs roughly
the same order as its ~210-byte stored form, call it 400 bytes with its strings.
At the ~6,000 events/s one shard sustains, that is about 2.4 MB/s — **8.6 GB an
hour of resident memory across the tier.** The process is killed by the operating
system long before the disk is the problem, and everything in the buffer goes
with it, unannounced. Treat the in-memory buffer as minutes, not hours.

**The on-disk outbox is the second.** At the same 2.4 MB/s a **100 GB volume is
full in about twelve hours**. A tier sized with headroom runs at perhaps a tenth
of the shard ceiling, and the same volume then lasts **about five days**. Work
out your own number from the box rather than from this paragraph:

```
hours_left = feasible_disk_available_bytes / (rate(feasible_ingest_events_accepted_total[5m]) * 400) / 3600
```

The number that matters is not how much is waiting but how long it has been
waiting. Depth tells you the size of the backlog; the oldest row tells you
whether you are still falling behind.

## Diagnosis

**Is it the shard, or is it the routing map?**

```bash
curl -s http://127.0.0.1:19402/metrics | grep feasible_sites_routed
```

Zero means the routing map is empty, which is a different incident: readiness is
already 503 and the load balancer has taken that ingestor out. Non-zero with a
growing buffer means the map is fine and the shard is unreachable.

**Which shard, and is it retired?**

The ingest process logs its resolved shard list at start-up, and `feasible ingest
-check` prints it without needing anything to be running:

```bash
feasible ingest -check
```

Compare that list against the shards that actually exist. **A shard that was
retired and left in `FEASIBLE_INGEST_SHARDS` is the silent version of this
incident**: the routing map is permanently incomplete, so the ingestor can never
be sure an account is not somebody's, never deletes anything, and fills its
volume over days while every live shard writes normally. If
`feasible_ingest_events_written_total` is climbing healthily on every shard you
still have and the outbox is still growing, this is what you are looking at.

**What is wrong with the shard?**

```bash
curl -s http://<shard>:19401/health/ready | python3 -m json.tool
```

The component named in the body is the fix:

| Failed component | What it means |
|---|---|
| `control_db` | The control database cannot be pinged — the file, the disk, or the process |
| `account_directory` | `accounts/` cannot be written — full disk, permissions, read-only remount |
| `salts` | No salt could be read or created; usually a missing or corrupt `salt.key` |
| `routing_map` | The map has never been built, so this shard has just started |

## Fix

**1. Leave the ingestors alone.** Do not restart them, do not redeploy, do not
scale them. Their buffers hold events nobody will send again.

**2. Bring the shard back.** The failed component names the problem. If it is
disk, [disk-filling.md](disk-filling.md). If it will not come back at all, restore
it onto a fresh box from replication —
[restore-account.md](restore-account.md) — and **give the replacement the same
address**, so the ingestors reconnect without a configuration change and without a
restart that would cost their buffers.

**3. Watch it drain.** On the ingestor, `feasible_ingest_buffer_events` should
fall to near zero. On the shard, `feasible_ingest_events_written_total` should
climb by roughly what the buffer was holding. Both, or the drain is not real.

**4. If the shard is being retired, remove it from `FEASIBLE_INGEST_SHARDS` on
every ingestor.** This is a mandatory decommissioning step, not tidying up.
Restart the ingestors one at a time, and drain each one first:

```bash
scripts/drain.sh http://127.0.0.1:19402/metrics && systemctl restart feasible-ingest
```

**5. Alarm on it for next time.** A shard that has not been scraped for minutes
should page. The failure this runbook describes is quiet by design — the visitor
got a 202 — so the alarm is the only thing that shortens it.

## What makes it worse

**Restarting or terminating an ingestor while its buffer is non-zero.** Every one
of those events has already been answered with a 202. Nothing retries, nothing
logs it, and the customer sees a hole. `scripts/drain.sh` exits non-zero rather
than let a deploy do this, and it fails on an unreadable metrics endpoint too —
an unreachable box is not a drained one.

**Scaling the ingest tier up.** More ingestors accept more events that still
cannot be written, and each new instance is another buffer to lose. The
bottleneck is the shard; adding front doors adds exposure.

**Moving accounts onto a healthy shard to "spread the load".** Write throughput
is flat from 1 to 16 accounts and about **45% worse at 64**, with the worst flush
roughly doubling to ten seconds and more. Piling a dead shard's accounts onto a
live one can take the live one below its own arrival rate and turn one outage
into two.

**Deleting the outbox to free space.** Those rows are events, not a cache. The
row is deleted only after the shard acknowledges the commit, which is exactly
what has not happened.

**Raising the flush timeout to give it more time.** A wedged shard then holds a
goroutine and its batch for longer. The batch returns to the buffer either way;
the only thing a longer timeout buys is a slower discovery.

**Leaving a decommissioned shard in the configuration.** The routing map stays
incomplete forever, the ingestor never deletes anything, and the volume fills
over days with no failing component anywhere to explain it.
