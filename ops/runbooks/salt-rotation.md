<!--
salt-rotation.md
A fingerprint salt rotation or erasure failed.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# A salt rotation went wrong

Salt material is random and stored encrypted in the shared `control.db`.
Authority source `0` pre-provisions tomorrow's value, so the table normally
contains previous, current, and next-day rows while only previous and current
are usable for fingerprinting. Rows at the 48-hour boundary are deleted and the
WAL is truncated. A process that cannot complete that erasure fails closed.

## Symptom

- `/health/ready` names `salts` as failed.
- Event derivation returns retryable `503` with `Retry-After`; it is not counted
  as an accepted event or an `internal_error` drop.
- Two serving processes return different `user_id` values for the same debug
  request, indicating they do not share the same authority rows or key.
- A row older than 48 hours is a privacy incident even if collection works.

## Diagnosis

```bash
sqlite3 /var/lib/feasible/control.db \
  "SELECT created_at, source_shard, length(salt)
   FROM salts ORDER BY created_at;"
```

Healthy rows are marked `source_shard = 0`. The newest two dates are today and
tomorrow at `00:00:00` UTC; after the install's first rollover, yesterday is
present as the third row. No row may be older than the 48-hour cutoff.

Compare two processes with the same request:

```bash
for host in ingest-01 ingest-02; do
  curl -s -X POST "http://$host:19302/api/event" \
    -H 'content-type: application/json' -H 'X-Debug-Request: true' \
    -d '{"n":"pageview","d":"example.com","u":"https://example.com/"}' |
    python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["salt_day"], d["user_id"])'
done
```

## Fix

Restore `FEASIBLE_SALT_KEY`, or the generated `salt.key`, if rows cannot be
decrypted. All event-serving processes must use the same shared control database
and key. There is no `/internal/salts` replication endpoint.

If expired rows remain, first repair the disk, permissions, or lock preventing
the delete and WAL checkpoint. Let the normal refresh prune them, then verify no
expired row remains. Record the exact retention overrun as a privacy incident.

If the key is irretrievably lost, stop event traffic before deleting unreadable
rows and restarting. New random current and next-day material will be created,
but every active visitor receives a new identity and open sessions may split.
State that impact explicitly.

## Backups

Litestream snapshots can retain encrypted historical salt rows longer than the
live 48-hour window. Never store `salt.key` with credentials that can read those
snapshots. The encrypted rows and their decryption key must remain separate.

## What makes it worse

- Deriving salt values from the permanent key makes deleted days recoverable.
- Copying a salt key between environments expands who can decrypt the rows.
- Continuing to hash after prune failure breaks the 48-hour promise.
- Manually rotating to repair visitor numbers creates another split.
- Deleting the table to reclaim disk saves almost nothing and breaks identity.
