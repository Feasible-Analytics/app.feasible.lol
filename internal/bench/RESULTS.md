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

**Write numbers taken:** 6 September 2026. The read and storage sections below
are older and are dated where they are described.

**Driver:** `modernc.org/sqlite` (pure Go), with whatever pragmas
`internal/store/store.go` sets. At the time of measurement: WAL,
`synchronous=FULL`, `secure_delete(1)`, `busy_timeout` 5s, `foreign_keys(1)`,
`cache_size` 64 MB, `mmap_size` 256 MB, `temp_store=MEMORY`,
`wal_autocheckpoint` at 1000 pages.

---

## Writing

50,000 events per run through the real accept path: the same handler,
derivation, write buffer and shard writer a request takes, at the production
buffer bounds (250 events or 500 ms), 5,000 distinct visitors, one site per
account. Four runs of each; the rate is the median with the range beside it, and
the latency columns are the range across the four.

**Every number here is measured under saturation.** The driver keeps 250
requests in flight at once, matching the buffer bound, because a sequential
driver would produce one-event flushes and measure nothing about batching. Read
the latencies as "what a shard under sustained load does", not as what a quiet
site's visitors see. The quiet case is the section after the table.

**The request waits for a durable commit.** `/api/event` answers 202 only after
the batch carrying that event has been fsynced, so the accept latency is the
visitor's wait, not a hand-off. That is what a 202 means here, and it is the
reason `synchronous=FULL` is in force.

| Accounts | Events/s | Accept p50 | Accept p99 | Flush p50 | Flush p99 |
|---:|---:|---:|---:|---:|---:|
| 1 | 3,843 (3,215–5,550) | 31–69 ms | 86–183 ms | 29–66 ms | 83–154 ms |
| 4 | 2,662 (1,843–2,926) | 80–133 ms | 156–261 ms | 77–129 ms | 127–251 ms |
| 16 | 1,280 (1,203–1,404) | 126–149 ms | 338–487 ms | 81–107 ms | 186–374 ms |
| 64 | 605 (540–690) | 240–281 ms | 4.0–11.6 s | 89–110 ms | 303–389 ms |
| 256 | 413 (399–432) | 244–266 ms | 15.1–16.9 s | 78–94 ms | 293–358 ms |

**What this says.**

- **Throughput falls with the account count, and there is no plateau.** From one
  account to 256 the rate drops roughly nine-fold, and every step down the column
  costs something. This is the number that decides how many accounts belong on
  one shard, and it is the opposite of what the first measurement said.
- **The mechanism is batch fan-out, not the file count on its own.** One shared
  250-event buffer spread over N accounts flushes as min(N, 250) transactions,
  each separately fsynced: about 250 events per commit at one account and about
  one at 256. That points at batching per account rather than at fewer files.
- **Sixteen accounts cost about two thirds of the single-account rate**
  (60/65/67/77% across the four passes). The 1→4 step is not measurable at this
  noise level — the four passes disagree by 67%, 33%, 29% and 9% — so where
  exactly the curve turns is not something these numbers can say.
- **Accept p99 goes from under half a second to seconds.** Under 490 ms to
  sixteen accounts; 4–12 s at 64 and 15–17 s at 256. That is a saturated shard,
  not 256 ordinary accounts, and it is not a visitor's page load either: the
  tracker sends with `keepalive`, so the browser does not wait for the answer.
  What it does hold is a connection and a goroutine per event in flight, which
  is the resource that runs out first.
- **Flush latency no longer grows with the load.** 29–129 ms across every size,
  against seconds in the first measurement. The buffer is not growing past its
  bound while it waits, because the requests filling it are themselves blocked on
  the previous commit. The queue moved from the buffer to the request.

### How much of this is `synchronous=FULL`

**One** comparison pass on the same code with the pragma set to `NORMAL`, against
the four-pass FULL range above. One sample is enough to say which side of a
range it falls on and not enough to put a ratio on it, so that is all this claims:

| Accounts | NORMAL events/s | FULL range | Reading |
|---:|---:|---:|---|
| 1 | 7,629 | 3,215–5,550 | clear of the range: FULL costs something here |
| 4 | 2,627 | 1,843–2,926 | inside the range: no cost measurable |
| 16 | 1,488 | 1,203–1,404 | clear of the range: about 1.1× |
| 64 | 639 | 540–690 | inside the range: no cost measurable |

**The pragma is not what flattened the curve.** `NORMAL` is faster at one
account and by about a tenth at sixteen, and elsewhere the difference disappears
into the spread. It falls off with the account count in the same shape either
way. Past a handful of accounts the limit is the number of transactions per
flush, not the fsync inside each one.

The 256-account case is absent on purpose. The `NORMAL` sweep failed there, and
the harness's error text was lost by the filter that captured the run — so the
one thing worth knowing about it was not recorded. Re-running that size alone
afterwards passed at 495 events/s, which is not comparable to a FULL number
measured at the tail of a full sweep. Under `FULL`, which is what production
runs, 256 passed four times out of four.

### What a quiet site costs

The table above saturates the buffer. Most installs never do. One account, forty
events, nothing else running:

| Requests in flight | Accept p50 | Accept p99 |
|---:|---:|---:|
| 1 | 500 ms | 532 ms |
| 2 | 499 ms | 521 ms |
| 8 | 497 ms | 519 ms |

**A quiet site pays the flush interval, every time.** With too few events to fill
a 250-event batch, each one waits out the 500 ms timer before its commit and its
202. The busier the site, the *cheaper* an individual accept becomes, because the
batch fills before the timer fires — the opposite of the usual shape.

It costs the visitor nothing: the tracker sends with `keepalive` and never reads
the answer. It costs the server half a second of one connection per event, which
is only interesting if somebody is calling `/api/event` synchronously and waiting.

### Why these numbers are far below the ones this file used to hold

The previous table recorded about 6,400 events/s flat from one account to
sixteen, and an accept p50 of 13 µs. Both were measured before `4cbb157`, which
made two changes at once: `synchronous` went from `NORMAL` to `FULL`, and the
public request began waiting for a durable commit instead of answering as soon
as the event was buffered.

The second change is the one that moved these numbers. A 13 µs accept was an
event handed to a buffer; a 30 ms accept is an event on disk. Anybody comparing
against an older copy of this file is comparing two different promises.

This benchmark found a real bug the first time it was run at these sizes: with a
backed-up buffer the batch grows past SQLite's bind limit, and the dedupe lookup
was one bound parameter per id, so the write failed, the batch was requeued
unchanged and failed identically forever. The lookup is chunked now.

The seed generator reaches **13,987 events/s** on the same machine, but it is not
comparable: it writes in bulk with the indexes dropped and rebuilt afterwards.
The gap between the two is what indexing and per-batch transactions cost.

## Acquiring an account handle

Taken 6 September 2026, on the same machine. `go test ./internal/accounts -bench
BenchmarkAcquireCached`.

This runs once per account per write batch, once per dashboard request and once
per account per health flush, so it is the most frequently executed thing in the
process that is not the accept path itself.

| | Before | After |
|---|---:|---:|
| One goroutine | 38,786 ns | **82 ns** |
| Ten goroutines over sixteen accounts | 85,557 ns | **301 ns** |
| Allocations | 27 | 2 |
| Filesystem operations | 4 opens, 4 flocks, 2 stats | none |

A handle this process already holds is now a mutex and a map read. The file
locks are still taken when a handle is opened, which is where a file is actually
created; what went away is paying for them again on every subsequent use of a
handle that already holds the account's lifetime lease.

The benchmark is in the repository so that a change putting those locks back on
the cached path is visible as a number rather than as a slow afternoon.

## Reading

**Read numbers taken:** 31 August 2026, and not re-taken since — a change to the
write path does not move them. `make bench` seeds a tenth of this dataset, so its
read output is not comparable to the table below; the second command is the one
that produced it.

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

`modernc.org/sqlite` sustains a few thousand events a second on one account and a
few hundred across 256, which is far more than the traffic any single account
generates — a site doing a million pageviews a month averages under half an
event a second. What limits the shard is the account count, not the driver, so
there is no reason here to evaluate the cgo one: cross-architecture builds from a
single machine are worth more than a write path that is already ahead of the
load.
