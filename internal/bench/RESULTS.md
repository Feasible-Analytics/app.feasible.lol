<!--
RESULTS.md
The last measured numbers, so a change that halves them is visible.

Created: 2026-08-30
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Measured, not estimated

Every capacity claim in this project used to be an estimate. These are the
numbers the estimates are now checked against. Re-run them with `make bench` and
update this file when they move — a benchmark nobody records is a benchmark
nobody can regress against.

**Machine:** Apple M4, 10 cores, macOS, SSD. A laptop rather than a server, and
one running everything else a laptop runs, so **read the shape of the curve
rather than the third significant figure.** Repeat runs of the same benchmark
vary by a third either way, and the ranges below say by how much.

**Driver:** `modernc.org/sqlite` (pure Go), WAL, `synchronous=NORMAL`,
`wal_autocheckpoint` at 1000 pages.

---

## Writing

50,000 events per run through the real accept path: the same handler,
derivation, write buffer and shard writer a request takes, at the production
buffer bounds (250 events or 500 ms), 5,000 distinct visitors, one site per
account. Three runs of each, reported as median (range).

| Accounts | Events/s | Accept p50 | Accept p99 | Flush p50 | Flush p99 |
|---:|---:|---:|---:|---:|---:|
| 1 | 6,481 (4,153–7,199) | 13 µs | 30–127 µs | 364–787 ms | 3.5–5.6 s |
| 4 | 6,578 (4,891–8,064) | 13 µs | 59–116 µs | 1.2–2.9 s | 4.1–7.0 s |
| 16 | 6,389 (5,959–6,413) | 12 µs | 44–87 µs | 2.4–2.8 s | 4.7–5.1 s |
| 64 | 3,614 (2,932–4,249) | 13 µs | 109–287 µs | 1.7–3.4 s | 9.2–12.6 s |

**What this says.**

- **Sustained throughput is around six thousand events a second per process**,
  and it is flat from one account to sixteen. Whatever the bottleneck is, it is
  not the number of database files.
- **It degrades at sixty-four accounts** — about 45% off the plateau, with the
  worst flush roughly doubling to ten seconds and more. That is the number that
  decides how many accounts belong on one shard, and it is now measured rather
  than assumed.
- **Accepting an event stays at about 13 µs whatever the writes are doing.**
  That is the property that matters most: the visitor's page never waits on our
  disk. If that number ever tracks the flush numbers, the write has leaked onto
  the request path.
- **Flush latency is the thing to investigate**, not accept latency. Flushes
  measured in seconds mean the buffer grew far past 250 events while it waited;
  the durable outbox depth and oldest row show that backlog directly.

This benchmark found a real bug the first time it was run at these sizes: with a
backed-up buffer the batch grows past SQLite's bind limit, and the dedupe lookup
was one bound parameter per id, so the write failed, the batch was requeued
unchanged and failed identically forever. The lookup is chunked now.

The seed generator reaches **13,987 events/s** on the same machine, but it is not
comparable: it writes in bulk with the indexes dropped and rebuilt afterwards.
The gap between the two is what indexing and per-batch transactions cost.

## Reading

One site, 365 days, **1,394,408 events** (999,963 pageviews, 432,845 visits,
431,536 visitors), roll-ups built. Reproduce with:

```bash
make bench   # seeds its own smaller dataset
go test ./internal/bench -run '^$' -bench BenchmarkRead -benchtime 3x \
    -bench.pageviews 1000000 -bench.days 365 -timeout 40m
```

Top pages is the report used throughout because it is the busiest table in the
product: a grouped breakdown with four metrics, ordered and limited to 100.

| Report | Measured | The plan's estimate | |
|---|---:|---:|---|
| Top pages, 28 days, raw | 2.1–2.5 s | 2–5 s | as expected |
| Top pages, 12 months, raw | 7.6–13.1 s | 30 s+ | better |
| Top pages, 28 days, roll-ups | 81–111 ms | under 10 ms | **an order of magnitude worse** |
| Top pages, 12 months, roll-ups | 0.4–0.7 s | under 100 ms | **an order of magnitude worse** |
| Top pages, 28 days, country filter, raw | 1.5–2.5 s | 200–400 ms | **an order of magnitude worse** |
| Today, raw | under 25 ms | under 100 ms | as expected |

The ranges are two runs of the same dataset. Today's figure depends on how far
into the day the generated history reaches, so it moves between a fraction of a
millisecond and about 25 ms; either way it is cheap.

**What this says.**

- **Roll-ups earn their keep**: the same report is 20–30× faster over 28 days
  and about 19× faster over 12 months. The architecture is right.
- **They are an order of magnitude off the estimate.** Single-digit milliseconds
  was optimistic for a hundred-group breakdown across a year of daily buckets.
  Under a tenth of a second still feels instant, but the estimate should stop
  being quoted as though it had been measured.
- **A filtered raw report costs about what an unfiltered one costs.** Filtering
  by country does not narrow the scan — it still reads every session in the
  range — so the 200–400 ms estimate was wrong by an order of magnitude. This is
  the clearest indexing opportunity the measurements found.

## Storage

The same dataset on disk, with every index and both roll-up grains built:

| | |
|---|---:|
| Account database | 293.8 MB |
| System database | 4.2 MB |
| Per event, all in | ~210 bytes |

A million pageviews is about 300 MB once it is indexed and summarised. Raw rows
age out and roll-ups do not, so the long-run figure per year is lower than
multiplying that by twelve suggests.

## The driver

`modernc.org/sqlite` sustains several thousand events a second through the full
accept path, which is far more than the traffic any single account generates —
a site doing a million pageviews a month averages under half an event a second.
There is no reason here to evaluate the cgo driver, and cross-architecture builds
from one machine are worth more than a write path that is already ahead of the
load.
