<!--
write-buffer-growing.md
The write buffer only goes up, and where the actual limit is.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# The write buffer is growing

## Symptom

| Series | What it does |
|---|---|
| `feasible_ingest_buffer_events` | Trending up. Sawtooth is healthy; a ramp is not |
| `feasible_ingest_flush_batch_events` | Far above 250 — batches are arriving much larger than the buffer's own size |
| `feasible_ingest_flush_duration_seconds` | p99 in seconds, or in the top buckets |
| `feasible_ingest_flushes_total{outcome="error"}` | Increasing, if the transport is failing rather than merely slow |
| `feasible_ingest_events_accepted_total` minus `feasible_ingest_events_written_total` | The gap the buffer still owes |
| `feasible_ingest_sessions_live` | Growing alongside, on the shard |

**Accept latency stays at 13 µs throughout.** That is the property the design
protects, and it is also why nothing on the front door tells you this is
happening: the visitor's page never waits on our disk, so a buffer filling up is
invisible from outside.

The healthy shape is a sawtooth between roughly zero and 250, the buffer's size
threshold, cycling at most every 500 ms. Anything that only goes up is this
runbook.

## Where the actual limit is

**There is no configured maximum.** `feasible_ingest_buffer_events` is bounded by
the memory of the process, and nothing refuses an event to protect it — refusing
would mean a visitor's beacon getting an error it cannot act on, which is the one
thing this pipeline will not do.

So the limit is arithmetic. A derived event held in memory costs roughly the
same order as its ~210-byte stored form; call it **400 bytes with its strings**.

| Buffer depth | Roughly |
|---:|---|
| 250 | 100 KB — normal |
| 100,000 | 40 MB — ten seconds behind at the shard ceiling |
| 1,000,000 | 400 MB — three minutes behind |
| 10,000,000 | 4 GB — half an hour behind, and the box is in trouble |

At the measured ceiling of ~6,000 events/s for one shard, an entirely stalled
writer accumulates **about 2.4 MB per second, 8.6 GB per hour**. On a 4 GB
instance the process is killed by the operating system in **under half an hour**,
and every buffered event dies with it, unlogged — each one has already been
answered with a 202, so nothing resends and nothing reports it.

**That is the real limit: the OOM killer.** Everything else about this incident
is a race against it.

There are two smaller bounds worth knowing because they shape what you see:

- **`FlushTimeout` is 30 seconds.** A flush that overruns is abandoned and its
  batch is put back at the front of the buffer, so a wedged shard produces a
  30-second cycle of large batches rather than one permanently parked goroutine.
- **A size-triggered flush is scheduled once at a time.** Appends past the
  threshold do not each start a goroutine; the ticker picks up whatever a
  skipped trigger left. So a slow shard produces a slow buffer, not a pile of
  goroutines.

## Diagnosis

Three causes, and they look different.

**The shard cannot be reached.** `feasible_ingest_flushes_total{outcome="error"}`
is climbing and flush durations are pinned at the 30-second timeout. This is
[shard-down.md](shard-down.md).

**The shard is reachable and slow.** Flushes succeed, `outcome="error"` is flat,
`feasible_ingest_flush_duration_seconds` sits in seconds. Look at the shard:

```bash
curl -s http://<shard>:19401/metrics | grep -E 'feasible_database_wal_bytes|feasible_disk_available_bytes|feasible_database_open_handles'
```

- `feasible_database_wal_bytes_max` growing without falling is a checkpoint that
  is not completing — one account is holding a read transaction open. SQLite
  exposes no last-checkpoint time, so a WAL that only grows is how this shows
  itself.
- `feasible_disk_available_bytes` falling towards zero is
  [disk-filling.md](disk-filling.md), and a full disk stalls every write at once.
- `feasible_database_open_handles` near the account count on a shard whose flush
  p99 has doubled is the 64-account degradation, below.

**The shard has too many accounts.** Throughput is flat from 1 to 16 accounts
and about **45% off the plateau at 64**, with the worst flush roughly doubling
to ten seconds and more. If the buffer started growing when accounts were added
rather than when anything broke, this is a capacity problem wearing an
incident's clothes.

## Fix

**1. Decide whether you are ahead of the OOM.** Depth times 400 bytes against the
instance's free memory. If the answer is minutes, everything else waits.

**2. Fix the shard, not the buffer.** The buffer is the symptom. Whichever of the
three causes above it is, the repair is at the shard: bring it back, finish the
checkpoint, free the disk, or move accounts off it.

**3. Drain deliberately when you have to move a process.**

```bash
scripts/drain.sh http://127.0.0.1:19402/metrics && systemctl restart feasible-ingest
```

Never the restart without the drain.

**4. If it is capacity, move accounts between shards while both are healthy** —
not while one is stalled. An account moves as a directory, and the routing map is
data rather than a hash range, so the operation is "move the file, update the
lists" rather than a migration. Do it as planned work.

## What makes it worse

**Restarting the process "to clear the buffer".** It clears it by losing it.
Every event in there has a 202 against it and no sender that will try again.

**Terminating the instance because it looks unhealthy.** Same loss, and now the
load balancer sends that traffic to another instance that will buffer it against
the same broken shard.

**Raising the buffer size or the flush interval to smooth the graph.** Both make
each batch larger and each loss bigger. The graph looks calmer because the
sawtooth is wider, not because anything improved.

**Raising `FlushTimeout` so flushes stop failing.** They stop being *reported* as
failing. The batch returns to the buffer either way; all a longer timeout buys is
a later discovery and a deeper buffer at the moment you make it.

**Adding ingest capacity.** More front doors accept more events against the same
stalled shard, and each new process is another buffer to lose.

**Moving the failing shard's accounts onto the healthy one during the incident.**
Past sixteen accounts, throughput falls; past sixty-four it is 45% down. The most
likely outcome is two shards behind instead of one.
