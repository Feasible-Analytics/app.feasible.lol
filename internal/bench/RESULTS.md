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

**Write numbers taken:** 6 September 2026.

**Driver:** `modernc.org/sqlite` (pure Go), with the pragmas in
`internal/store/store.go`: WAL, `synchronous=FULL`, `busy_timeout` 5s,
`cache_size` 64 MB, `mmap_size` 256 MB, `wal_autocheckpoint` at 1000 pages.

---

## Writing

50,000 events per run through the real accept path: the same handler,
derivation, write buffer and shard writer a request takes, at the production
buffer bounds (250 events or 500 ms), 5,000 distinct visitors, one site per
account. Three runs of each, reported as median (range).

**The request waits for a durable commit.** `/api/event` answers 202 only after
the batch carrying that event has been fsynced, so the accept latency below is
the visitor's wait, not a hand-off. That is what a 202 means here and it is the
reason `synchronous=FULL` is in force.

| Accounts | Events/s | Accept p50 | Accept p99 | Flush p50 | Flush p99 |
|---:|---:|---:|---:|---:|---:|
| 1 | 3,701 (3,215–5,550) | 30–69 ms | 110–183 ms | 29–66 ms | 100–154 ms |
| 4 | 2,638 (1,843–2,926) | 83–133 ms | 156–256 ms | 80–129 ms | 127–243 ms |
| 16 | 1,279 (1,203–1,280) | 142–149 ms | 338–487 ms | 81–107 ms | 252–374 ms |
| 64 | 584 (540–626) | 247–281 ms | 4.0–11.6 s | 89–110 ms | 303–389 ms |
| 256 | 424 (399–432) | 244–259 ms | 15.1–16.4 s | 78–94 ms | 293–358 ms |

**What this says.**

- **Throughput falls with the account count, and there is no plateau.** From one
  account to 256 the rate drops nine-fold, and every step down the column costs
  something. The bottleneck is the number of database files, which is the
  opposite of what the first measurement said, and it is the number that decides
  how many accounts belong on one shard.
- **The knee is between 1 and 16, not at 64.** Four accounts already cost 30% of
  the single-account rate and sixteen cost two thirds. The 64-to-256 stretch is
  comparatively flat — by then the cost is paid.
- **Accept p99 is the number to watch.** It stays under half a second to sixteen
  accounts and then goes to seconds: 4–12 s at 64, 15–16 s at 256. A visitor's
  page really is waiting that long for the beacon at those sizes. p50 stays a
  quarter of a second, so this is a tail, but it is a tail attached to somebody's
  page load.
- **Flush latency is now flat and small**, 78–110 ms at every size. The buffer is
  no longer growing past its bound while it waits, because the requests filling
  it are themselves blocked on the previous commit. The queue moved from the
  buffer to the request.

### How much of this is `synchronous=FULL`

One comparison pass on the same code with the pragma set to `NORMAL`:

| Accounts | NORMAL events/s | FULL events/s | Cost of FULL |
|---:|---:|---:|---:|
| 1 | 7,629 | 3,701 | 2.1× |
| 4 | 2,627 | 2,638 | none measurable |
| 16 | 1,488 | 1,279 | 1.2× |
| 64 | 639 | 584 | 1.1× |
| 256 | 495 | 424 | 1.2× |

**The pragma is not what flattened the curve.** It costs about half the rate on
one account and roughly a tenth beyond four; past that the limit is the number
of write locks and the per-request wait, not the fsync.

### Why these numbers are far below the ones this file used to hold

The previous table recorded about 6,400 events/s flat from one account to
sixteen, and an accept p50 of 13 µs. Both were measured before `4cbb157`, which
made two changes at once: `synchronous` went from `NORMAL` to `FULL`, and the
public request began waiting for a durable commit instead of answering as soon
as the event was buffered.

The second change is the one that moved these numbers. A 13 µs accept was an
event handed to a buffer; a 30 ms accept is an event on disk. Anybody comparing
against an older copy of this file is comparing two different promises.

One further observation, not recorded above because it did not reproduce: under
`NORMAL`, the 256-account case failed once inside a full sweep and passed on its
own immediately afterwards. Production runs `FULL`, where 256 passed three times
out of three.

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
