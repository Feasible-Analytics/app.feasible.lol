<!--
orphaned-ingestor-volume.md
An ingestor is gone and its volume still holds events nobody will send again.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Drain an orphaned ingestor volume

An ingestor instance has gone — terminated by the platform, a hardware failure,
a deploy that skipped the drain. Its network-attached volume still holds an
outbox with events in it. **Every one of those events has already been answered
with a 202.** No browser will send them again, and nothing else in the system
knows they exist.

Ingestors run on network-attached volumes rather than instance storage precisely
so this is recoverable. That is only a mitigation if somebody actually does it,
which is what this document is for.

## Symptom

This one does not page you. A terminated instance stops being scraped, and a
metrics target that vanishes looks exactly like a scale-down.

| Signal | What it means |
|---|---|
| The ingestor's `/metrics` target disappears | The instance is gone. Whether that was planned is not something the metric can tell you |
| Its last scraped `feasible_ingest_buffer_events` was **not zero** | It was holding events when it went |
| `feasible_ingest_events_accepted_total` across the tier drops by one instance's share | The load balancer has already routed around it |

**Make target absence an alert.** An ingestor whose last known buffer depth was
non-zero and which is no longer scraped is an orphaned volume until somebody
proves otherwise. Without that alert this incident is discovered by a customer,
weeks later, as a hole in a chart.

## The 24-hour clock

Read this before anything else, because it decides how urgent this is.

The shard remembers written event ids for **`DedupeRetention`, 24 hours**. That
is what makes redelivery safe: an event the shard already committed, whose
acknowledgement was lost on the way back to the ingestor, is recognised on the
retry and skipped.

**After 24 hours that memory is gone.** Draining an old outbox can then write
those events a second time, and a double-counted pageview is as wrong as a
missing one and much harder to explain.

So: **drain an orphaned volume within 24 hours of the instance's last successful
flush.** Past that, weigh a possible double count against a certain loss — and
whichever you choose, tell the affected accounts which one it was.

## Fix

**1. Stop the platform from destroying the volume.** On most platforms a volume
attached to a terminated instance is deleted with it by default. Detach it and
clear the delete-on-termination flag before anything else. This step is the whole
mitigation; the rest is procedure.

**2. Attach it to a maintenance box and mount it somewhere new.** Never at the
path a running ingestor already uses — two processes over one SQLite outbox is
the way to corrupt it, and mounting over a live one hides the outbox underneath.

```bash
mount /dev/<volume> /mnt/orphan
ls -l /mnt/orphan/ingest/
```

**3. Run a drainer against it, off the load balancer.**

It needs the same `FEASIBLE_INTERNAL_KEYS` and the same
`FEASIBLE_INGEST_SHARDS` as the instance that died — the first so the shards
accept its signatures, the second so it knows where the events go. Confirm both
before starting anything:

```bash
FEASIBLE_APP_DATA_DIR=/mnt/orphan \
FEASIBLE_INGEST_BUFFER_PATH=/mnt/orphan/ingest/buffer.db \
  feasible ingest -check
```

`-check` resolves and prints the configuration — the shard list, the buffer path,
the number of internal keys — and exits without listening. A wrong shard list
here silently drops everything you were trying to save, so read the output before
you take the flag off.

```bash
FEASIBLE_APP_DATA_DIR=/mnt/orphan \
FEASIBLE_INGEST_BUFFER_PATH=/mnt/orphan/ingest/buffer.db \
  feasible ingest \
    -listen 127.0.0.1:19399 \
    -internal-listen 127.0.0.1:19499
```

Both listeners are on loopback and this instance is registered with nothing. It
is here to empty a file, not to accept traffic.

If it refuses to start on a schema version, run `feasible db migrate` against
`/mnt/orphan` first — the volume may predate a deploy.

**4. Watch it empty, and prove it landed.**

```bash
scripts/drain.sh http://127.0.0.1:19499/metrics
```

The script exits zero only when `feasible_ingest_buffer_events` reaches zero, and
non-zero on a timeout or an unreadable endpoint. Confirm the other half on the
shard: `feasible_ingest_events_written_total` should climb by roughly what the
outbox was holding. Zero on the ingestor without movement on the shard is not a
drain, it is a loss.

**5. Stop the drainer, unmount, and only then destroy the volume.**

Keep it for a day if there is any doubt. A detached volume costs pennies; the
events on it cannot be recreated.

## What was lost, and what to say

If the drain succeeded within 24 hours: **nothing**, and no double counting
either. That is the answer this whole arrangement exists to produce, and it is
worth writing in the incident record.

If the volume was destroyed, or the drain failed: everything the outbox held. Its
last scraped `feasible_ingest_buffer_events` is the size of the hole, and the
scrape timestamp is roughly when it starts. Both numbers belong in what the
customer is told.

If the drain happened after 24 hours: some events may be counted twice, bounded
by the number that were committed but unacknowledged — normally very small, but
not provably zero. Say that, rather than describing the recovery as clean.

## What makes it worse

**Letting the platform delete the volume with the instance.** The default on most
platforms, and the single reason this incident becomes unrecoverable. Turn it off
when the instance is created, not when it dies.

**Mounting the orphan volume over a running ingestor's data directory.** Either
two processes share one SQLite outbox, or the live outbox disappears under the
mount. Both lose events, and the second one does it invisibly.

**Registering the drainer with the load balancer.** It then accepts new traffic
into the outbox you are trying to empty, and it never finishes.

**Copying outbox rows into another ingestor's outbox by hand.** Redelivery is
safe because of the shard's 24-hour dedupe, not because of anything the copy
does. Outside that window it double counts, and inside it you have gained
nothing over running a drainer.

**Deleting the outbox to reclaim the volume.** Those rows are events with 202s
against them. Nothing anywhere logs their deletion.

**Assuming a zero buffer depth on the shard means the volume is empty.** They are
different processes with different buffers. Read the metrics endpoint of the
drainer itself.
