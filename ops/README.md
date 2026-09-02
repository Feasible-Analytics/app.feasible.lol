<!--
README.md
Where an operator starts, and every signal the runbooks are allowed to name.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Operations

Everything in here is written against signals this build actually emits. A
runbook that names a metric we do not export is worse than no runbook: it sends
somebody looking for a number that will never appear, during the twenty minutes
that matter most.

| Document | When you need it |
|---|---|
| [litestream.md](litestream.md) | Continuous replication, and why the config is generated |
| [load-balancer.md](load-balancer.md) | Health-check settings, and degraded versus dead |
| [game-day.md](game-day.md) | The written exercise that breaks things on purpose |
| [runbooks/](runbooks/) | One document per failure this system actually has |

## The runbooks

| Runbook | The page that wakes you |
|---|---|
| [shard-down.md](runbooks/shard-down.md) | An app shard is unavailable while ingesters retain traffic |
| [rollup-behind.md](runbooks/rollup-behind.md) | Reports are slow and yesterday looks wrong |
| [restore-account.md](runbooks/restore-account.md) | One account's database is gone or corrupt |
| [write-buffer-growing.md](runbooks/write-buffer-growing.md) | The buffer only goes up |
| [orphaned-ingester-volume.md](runbooks/orphaned-ingester-volume.md) | A failed ingester left acknowledged events on its volume |
| [disk-filling.md](runbooks/disk-filling.md) | Free space is falling towards zero |

Every one of them has the same four sections, and the fourth is the one that
saves the outage:

- **Symptom** — what it looks like in the metrics, in the exact series names.
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

Both processes answer `/health/live`, `/health/ready`, and `/metrics` on that
listener.

```bash
curl -s http://127.0.0.1:19301/metrics | grep '^feasible_'
curl -s http://127.0.0.1:19301/health/ready | python3 -m json.tool
```

A hosted deployment puts app listeners on a protected network so ingesters can
reach the authenticated `/internal/domains` and `/internal/ingest` endpoints.
The public edge must never expose those paths or `/metrics`.

## The signal inventory

This is the complete set of series the binary exports. If a procedure needs a
number that is not on this list, the procedure is wrong or the metric has to be
added first.

**Ingest — is the front door working, and is anything lost**

| Series | Labels |
|---|---|
| `feasible_ingest_events_accepted_total` | — |
| `feasible_ingest_events_dropped_total` | `reason`: `hostname_not_allowed`, `unknown_site`, `account_dormant`, `site_deleted`, `shield_ip`, `shield_country`, `shield_page`, `no_session_for_engagement`, `rate_limited`, `invalid_payload`, `internal_error` |
| `feasible_ingest_events_classified_total` | `reason`: `bot`, `datacenter_ip`, `referrer_spam` |
| `feasible_ingest_fields_truncated_total` | `field`: `props_over_limit`, `prop_name_too_long`, `prop_value_too_long`, `prop_value_unsupported`, `url_too_long`, `engagement_time_clamped` |
| `feasible_ingest_events_written_total` | — |

A **dropped** event is gone. A **classified** event is stored with its reason
set and the customer has a toggle for it. Confusing the two turns a working bot
filter into a reported outage.

**The write buffer**

| Series | Labels |
|---|---|
| `feasible_ingest_buffer_events` | — |
| `feasible_ingest_buffer_oldest_seconds` | — |
| `feasible_ingest_buffer_parked_events` | — |
| `feasible_ingest_flushes_total` | `outcome`: `ok`, `error` |
| `feasible_ingest_flush_duration_seconds` | — |
| `feasible_ingest_flush_batch_events` | — |

**Roll-ups**

| Series | Labels |
|---|---|
| `feasible_rollup_runs_total` | `outcome`: `ok`, `error` |
| `feasible_rollup_duration_seconds` | — |
| `feasible_rollup_last_success_timestamp_seconds` | — |

**Reports and HTTP**

| Series | Labels |
|---|---|
| `feasible_query_duration_seconds` | `source`: `raw`, `rollup`, `mixed`, `none` |
| `feasible_query_failures_total` | `kind`: `caller`, `internal` |
| `feasible_http_requests_total` | `handler`: `event`, `stats`, `dashboard`, `tracker`, `app`, `api`; `status`: `2xx`–`5xx` |
| `feasible_http_request_duration_seconds` | `handler` |

**Storage and routing**

| Series | Labels |
|---|---|
| `feasible_database_bytes` | `database`: `system`, `accounts` |
| `feasible_database_wal_bytes` | `database`: `system`, `accounts` |
| `feasible_database_wal_bytes_max` | — |
| `feasible_database_files` | — |
| `feasible_database_directory_readable` | — |
| `feasible_database_open_handles` | — |
| `feasible_disk_total_bytes` | — |
| `feasible_disk_available_bytes` | — |
| `feasible_sites_routed` | — |
| `feasible_jobs` | `state`: `available`, `executing` |

Nothing on this endpoint carries a site, a domain, a path, a country or a
visitor. That is a rule, not an omission: those belong to customers, and a label
whose values come from the traffic grows a new time series for every URL a
crawler invents. Per-site drop counts live on the customer's own ingestion
health panel.

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
