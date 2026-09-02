<!--
README.md
Where an operator starts, and every signal the runbooks are allowed to name.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Operations

Everything in here is written against health responses, application logs, and
the durable SQLite state the process actually owns. A runbook must not depend on
an HTTP metrics surface the product does not expose.

| Document | When you need it |
|---|---|
| [load-balancer.md](load-balancer.md) | Health-check settings, and degraded versus dead |
| [game-day.md](game-day.md) | The written exercise that breaks things on purpose |
| [runbooks/](runbooks/) | One document per failure this system actually has |

## The runbooks

| Runbook | The page that wakes you |
|---|---|
| [shard-down.md](runbooks/shard-down.md) | An app shard is unavailable while ingesters retain traffic |
| [rollup-behind.md](runbooks/rollup-behind.md) | Reports are slow and yesterday looks wrong |
| [write-buffer-growing.md](runbooks/write-buffer-growing.md) | The buffer only goes up |
| [orphaned-ingester-volume.md](runbooks/orphaned-ingester-volume.md) | A failed ingester left acknowledged events on its volume |
| [disk-filling.md](runbooks/disk-filling.md) | Free space is falling towards zero |

Every one of them has the same four sections, and the fourth is the one that
saves the outage:

- **Symptom** — what the customer, health checks, logs, or durable state show.
- **Diagnosis** — how to tell this from the three things that look like it.
- **Fix** — the commands, in order.
- **What makes it worse** — the plausible action that turns an incident into
  data loss.

## Where the signals are

Each process runs one listener. The protected network and edge proxy determine
which paths are externally reachable.

| | App (`feasible serve`) | Ingest (`feasible ingest`) |
|---|---|---|
| Listen | `FEASIBLE_APP_LISTEN`, default `127.0.0.1:19301` | `FEASIBLE_INGEST_LISTEN`, default `127.0.0.1:19302` |

Both processes answer `/health`, `/health/live`, and `/health/ready` on that
listener. `/health` is the compact serviceability answer for public uptime
monitoring; the two longer paths remain the internal liveness and readiness
probes.

```bash
curl -s http://127.0.0.1:19301/health/ready | python3 -m json.tool
```

A hosted deployment puts app listeners on a protected network so ingesters can
reach the authenticated `/internal/domains` and `/internal/ingest` endpoints.
The public edge must never expose those paths.

## The numbers these procedures are built on

From `internal/bench/RESULTS.md`, measured rather than estimated. Re-run with
`make bench` and update that file when they move.

| | |
|---|---|
| Sustained write throughput, per process | ~6,000 events/s, flat from 1 to 16 accounts |
| At 64 accounts on one shard | ~3,600 events/s |
| Accepting one hosted event | waits for its ingester-local outbox transaction |
| Worst flush at 64 accounts | 9–13 s |
| Roll-ups versus raw, 28 days | 20–30× faster |
| Storage, all in | ~210 bytes per event |

The degradation at 64 accounts is what decides how many accounts belong on one
shard. It is also why "just move more accounts onto the healthy box" is listed
under *what makes it worse* in more than one runbook.

## The two rules every procedure obeys

**The IP address never reaches disk.** Geolocation and fingerprinting happen at
the ingester before the derived event reaches `buffer.db`; the address is then
discarded. No backup, no snapshot and no debugging step may reintroduce one — if
a procedure would have you capture raw requests, it is the wrong procedure.

**Nothing fails silently.** If a step can lose data, the runbook says how much
and how to tell the customer. A recovery that quietly loses an hour is worse
than one that loses an hour and says so.
