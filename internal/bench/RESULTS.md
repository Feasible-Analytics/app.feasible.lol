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

**Machine:** Apple M4, 10 cores, macOS, SSD. A laptop rather than a server, so
read these as the shape of the curve rather than as a capacity plan for a box
you have not measured.

**Driver:** `modernc.org/sqlite` (pure Go), WAL, `synchronous=NORMAL`,
`wal_autocheckpoint` at 1000 pages.

---

## Writing

`make bench` — 50,000 events per run through the real accept path: the same
handler, derivation, write buffer and shard writer a request takes, at the
production buffer bounds (250 events or 500 ms), 5,000 distinct visitors, one
site per account.

| Accounts | Events/s | Accept p50 | Accept p99 | Flush p50 | Flush p99 |
|---:|---:|---:|---:|---:|---:|
| 1 | 5,068 | 11 µs | 28 µs | 449 ms | 5,405 ms |
| 4 | 8,369 | 13 µs | 79 µs | 1,017 ms | 3,577 ms |
| 16 | 6,022 | 13 µs | 80 µs | 2,700 ms | 5,102 ms |
| 64 | 3,626 | 13 µs | 79 µs | 1,770 ms | 10,396 ms |

**What this says.**

- **Sustained throughput is a few thousand events a second per process**, and it
  peaks at a handful of accounts rather than at one. One account is one write
  lock and one WAL, so a single database is a narrower pipe than four.
- **It degrades past about sixteen accounts.** At sixty-four the rate is down by
  more than half and the worst flush takes ten seconds. This is the number that
  decides how many accounts belong on one shard, and it is a real ceiling rather
  than an assumed one.
- **Accepting an event stays at tens of microseconds** whatever the writes are
  doing, which is the property that matters most: the visitor's page never waits
  on our disk. That is the buffer doing its job, and it is worth a test if it
  ever stops being true.
- **Flush latency is the thing to watch**, not accept latency. A flush that takes
  seconds means the buffer grew far past 250 events while it waited, which is
  exactly what `feasible_ingest_flush_batch_events` and
  `feasible_ingest_buffer_events` were added to make visible.

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
| Top pages, 28 days, raw | 2,468 ms | 2–5 s | as expected |
| Top pages, 12 months, raw | 7,579 ms | 30 s+ | four times better |
| Top pages, 28 days, roll-ups | 111 ms | under 10 ms | **ten times worse** |
| Top pages, 12 months, roll-ups | 401 ms | under 100 ms | **four times worse** |
| Top pages, 28 days, country filter, raw | 2,529 ms | 200–400 ms | **six times worse** |
| Today, raw | 23 ms | under 100 ms | as expected |

**What this says.**

- **Roll-ups earn their keep**: the same report is 22× faster over 28 days and
  19× faster over 12 months. The architecture is right.
- **They are an order of magnitude off the estimate.** Single-digit milliseconds
  was optimistic for a hundred-group breakdown across a year of daily buckets.
  A hundred milliseconds is still a dashboard that feels instant, but the
  estimate should not be quoted as if it were measured.
- **A filtered raw report costs the same as an unfiltered one.** Filtering by
  country does not narrow the scan — it still reads every session in the range —
  so the estimate of 200–400 ms was wrong by an order of magnitude. This is the
  clearest indexing opportunity the measurements found.
- **Today is genuinely cheap**, which is what makes the live dashboard viable
  without summarising the day that is still filling up.

## Storage

The same dataset on disk, with every index and both roll-up grains built:

| | |
|---|---:|
| Account database | 293.8 MB |
| Control database | 4.2 MB |
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
