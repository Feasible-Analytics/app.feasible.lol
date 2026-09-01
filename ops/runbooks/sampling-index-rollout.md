<!--
  sampling-index-rollout.md
  Deployment gate for account migration 0011.

  Created: 2026-08-31
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Sampling index rollout

Account migration `0011_sampling_indexes.sql` materializes deterministic
event/session sample membership, exact per-site UTC-day counts, and bounded
session bot/entry-title facts. It also adds the seek indexes and trigger
maintenance required to keep those facts current. Migration backfills every
existing event and session inside one transaction. Expect a permanent database
size increase, a WAL of similar size during migration, and additional work on
new fact inserts.

The checked-in WAL benchmark on an Apple M4 measured the following local
baseline with four events per session. These are capacity-planning evidence,
not production guarantees:

| Events | Sessions | DB growth | Peak WAL | Build/lock | Fact writes/s before -> after | Session UPSERTs/s before -> after |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 10,000 | 2,500 | 0.80 MiB | 0.83 MiB | 166 ms | 90,084 -> 30,878 | 113,844 -> 103,949 |
| 100,000 | 25,000 | 8.25 MiB | 8.32 MiB | 2.42 s | 73,452 -> 30,689 | 121,962 -> 114,751 |
| 500,000 | 125,000 | 42.91 MiB | 43.17 MiB | 10.68 s | 40,951 -> 20,191 | 76,317 -> 56,075 |

The largest fixture used about 90 bytes of permanent sampling/session-fact
storage per event at the measured event/session ratio, plus a migration WAL of
roughly the same size. The identical-copy probe measured 51-66% lower bulk fact
insert throughput. The production-shaped conflict path assigns `started_at` on
every session UPSERT; 0011's trigger guard avoided fact rewrites when it was
unchanged, and measured throughput was 6-27% lower. Production transactions and
data distributions differ, so deployment must use a representative copied
account rather than treating these ratios as a promise. Every measured
post-migration checkpoint returned zero busy/pending frames,
`PRAGMA integrity_check` returned `ok`, and the benchmark volume had about 70
GiB free.

The accepted design allocates a bucket from each site/day's chronological fact
ordinal, folds higher 1,024-row blocks, and stores membership in narrow indexed
tables. Query SQL never hashes or masks the fact id and uses no unindexed hash
expression. On the two-million-event giant-session fixture, a one-bucket event
aggregate used `event_sampling_seek` plus integer-primary-key lookups, reading
1,953 fact rows in 3.90 ms; the exact two-million-row scan took 302.51 ms.
Session bot exclusion and entry-title filters read one precomputed
`session_sampling` row and do not open the giant session's events.

## Integration gate

The assembled account migration order is topology `0008` and `0009`, M9
annotations/health `0010`, then sampling `0011`. The sampling SQL documents its
`Predecessor: 0010` and `Requires: 0008-0010` integration boundary, while the
migration runner independently validates every pending filename as a contiguous
set before it opens a migration transaction or writes `user_version`. Normal
account opens and the migration CLI use the complete `migrate.Account()` set.

Before deployment:

1. Run the fresh and populated version-7 upgrade tests to prove M9 `0010` precedes sampling `0011` without losing topology receipts, sessions, annotations, or health data.
2. Run the migration benchmark and EXPLAIN assertions on a recent copy of the largest account, then use the measured copy's storage and lock figures for the deploy gate.
3. Verify tracker `k` remains the permanent RFC 4122 identity topology parses into the `recent_event_ids` dedupe transaction, while legacy payloads without `k` retain a generated UUID.

## Measure a representative copy

1. Stop ingestion for the copied account and confirm SQLite is in WAL mode.
2. Record database and `-wal` sizes plus current event write throughput.
3. Run `go test ./internal/migrate -run '^$' -bench BenchmarkSamplingIndexCost -benchtime=1x` for baseline ratios.
4. Apply 0011 to a recent copy of the largest production account while timing the complete transaction. Treat that duration as the conservative write-lock window.
5. Checkpoint the WAL, record permanent database growth, run `PRAGMA integrity_check`, and compare write throughput before and after.

## Deploy gate

Proceed only when free disk covers the measured permanent growth plus twice the
peak migration WAL, and the measured lock window fits the account's ingestion
maintenance window. Pause account ingestion during the migration. Roll out
largest accounts individually, checkpoint after each account, and verify that
write-buffer depth returns to baseline before continuing.

If database growth, lock duration, or write-throughput loss exceeds the copied
account's operating margin, stop the rollout. Do not drop the indexes on a live
writer; restore the pre-migration copy or schedule a separate maintenance
window, then revisit bucket/index design with the query-plan and adversarial
row-bound benchmarks.
