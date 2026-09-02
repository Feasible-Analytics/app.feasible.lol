<!--
rollup-behind.md
The roll-up worker has stopped keeping up, and the dashboard is slow rather than wrong.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# The roll-up worker is behind

## Symptom

`feasible rollup status` shows stale covered windows. The application log emits
`roll-up build failed` at error with the reason, and `slow report` at warn for
every report over a second with the domain, source, metrics, dimensions, and
date range.

## What the numbers look like meanwhile

**The dashboard is slow, not wrong.** A range the summaries do not cover is
answered from raw events, and raw events are the truth. Nothing a customer sees
is incorrect.

From `internal/bench/RESULTS.md`, on a dataset of 1.4 million events for one
site:

| Report | From roll-ups | From raw |
|---|---:|---:|
| Top pages, 28 days | 81–111 ms | 2.1–2.5 s |
| Top pages, 12 months | 0.4–0.7 s | 7.6–13.1 s |
| Today | — | under 25 ms |

So the shape of the incident is: today and realtime are unaffected — they are
always raw — the 28-day dashboard becomes a two-second wait, and the annual view
becomes a ten-second one. Multiply that by every open dashboard, and the second
symptom is a shard whose read pool is saturated.

**The real damage is later, not now.** Each pass reworks the last two days,
because an event can arrive late and the session fold can merge a visit into an
earlier one — either changes a bucket that was already sealed. **If the worker
is down for longer than two days, the rework window no longer covers the gap**,
and a sealed bucket keeps a number it should have lost. Restarting the worker
does not fix that. Rebuilding does.

## Diagnosis

```bash
feasible rollup status
```

It prints the covered window per site and grain, and the dimensions each summary
is keyed by. Two different answers, two different problems:

- **One site's window is stale, the rest are current** — that site's build is
  failing. The log line names it.
- **Every window is stale** — the worker is not running, or every pass is
  failing. Check that the app process is up and read `roll-up build failed`.

A third possibility worth ruling out before touching anything: a report can be
slow with perfectly current roll-ups, because a **filtered** raw report costs
about what an unfiltered one costs — filtering by country does not narrow the
scan. The `slow report` log's source says whether the query used raw rows or a
roll-up.

## Fix

**1. Read the error.** `roll-up build failed` carries the reason. A locked
database, a missing account file and a schema mismatch are all different repairs.

**2. One site:**

```bash
feasible rollup rebuild -site example.com
```

It deletes that site's summary rows and builds them again from raw. Safe, because
a roll-up is a cache and never the truth.

**3. Everything behind, gap under two days:**

```bash
feasible rollup build
```

One pass of exactly what the worker does hourly. Run it off-peak, then run
`feasible rollup status` again and confirm the covered windows advance.

**4. Gap longer than two days:** rebuild the affected sites rather than trusting
the rework window. `feasible rollup rebuild` with no `-site` rebuilds every site,
which is correct and expensive — do it site by site on a busy shard.

## What makes it worse

**Restarting the app process to kick the worker while a pass is running.** A pass
is not one transaction across every site. A restart part-way through leaves some
sites rebuilt and some not, and the next pass only reworks two days — so the
sites it did not reach keep their stale buckets, and now you cannot tell which
ones they were.

**Running `feasible rollup rebuild` across every site on a live shard at once.**
It reads the site's entire history and holds the account's write lock while it
does. On a shard that is already behind, that competes directly with ingestion,
and the buffer starts growing on top of everything else.

**Deleting the summary tables by hand to force a rebuild.** `rollup rebuild`
deletes and rebuilds inside one command. Deleting the rows yourself leaves every
report reading raw until a pass finishes, on a shard you have just made slower.

**Telling the customer their numbers are wrong.** They are not. They are being
computed the slow way from the same events. Saying otherwise costs trust you then
have to earn back with numbers that never changed.

**Turning the worker off to make the shard faster.** It is the reason reports are
20–30× faster over 28 days. Off, the shard is not busy, and every dashboard is.
