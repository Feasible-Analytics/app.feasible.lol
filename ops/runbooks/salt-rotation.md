<!--
salt-rotation.md
A fingerprint salt rotation or erasure failed.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# A salt rotation went wrong

Salt material is random and stored encrypted in the shared `system.db`.
Authority source `0` pre-provisions tomorrow's value, so the table normally
contains previous, current, and next-day rows while only previous and current
are usable for fingerprinting. Rows at the 48-hour boundary are deleted and the
WAL is truncated. A process that cannot complete that erasure fails closed.

The salt is the key a visitor's IP and user agent are hashed with. Two claims
rest on it, and both break here:

- **Visitor counts are right.** The same person on the same site on the same day
  is one visitor, because the same salt produced the same hash.
- **After 48 hours the live system cannot reconstruct a fingerprint.** The salt
  that produced the hash has been deleted from the live system database.
  Each encrypted replica object becomes eligible for provider removal within
  72 hours of being written, as described below. Provider removal is
  asynchronous with no published maximum, and an authorised restore with the
  separately stored key can re-identify while a snapshot remains.

Three rules make those true. Rotation is at **00:00 UTC**, never a local
midnight. Today's salt hashes, yesterday's is a session-lookup fallback, and
tomorrow's is pre-provisioned but not yet used. **Rows older than 48 hours are
deleted, not archived.** Everything below is one of those rules having failed.

## Symptom

- `/health/ready` names `salts` as failed.
- Event derivation returns retryable `503` with `Retry-After`; it is not counted
  as an accepted event or an `internal_error` drop.
- Two serving processes return different `user_id` values for the same debug
  request, indicating they do not share the same authority rows or key.
- A row older than 48 hours is a privacy incident even if collection works.

## Diagnosis

```bash
sqlite3 /var/lib/feasible/system.db \
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
decrypted. App shards own the system database; ingesters fetch only the current
and previous salts over the authenticated `/internal/salts` endpoint and keep
them in memory. Every app shard must serve the same salt authority, and every
ingester must use an accepted HMAC key.

If expired rows remain, first repair the disk, permissions, or lock preventing
the delete and WAL checkpoint. Let the normal refresh prune them, then verify no
expired row remains. Record the exact retention overrun as a privacy incident.

If the key is irretrievably lost, stop event traffic before deleting unreadable
rows and restarting. New random current and next-day material will be created,
but every active visitor receives a new identity and open sessions may split.
State that impact explicitly.

## Backups

The live system deletes salts after 48 hours. **Replication does not** —
snapshots of `system.db` can contain salt rows the live database has already
deleted. Generated Litestream configuration disables remote retention deletion,
and the mandatory provider lifecycle is the sole remote removal authority. The
mandatory bucket lifecycle makes each snapshot eligible for provider removal no
later than 72 hours after creation; physical removal is asynchronous with no
published maximum.

That means the 48-hour guarantee applies to the live system, not every retained
copy. The salt rows in the replica are sealed and the key that unseals them is
not in the bucket. That separation reduces the chance of disclosure, but an
authorised restore using the separately backed-up key and matching analytics
data can re-identify a fingerprint while the provider still retains a snapshot;
re-identification remains possible for that whole interval.
The 72-hour statement is an eligibility bound, not a physical-erasure bound.

So:

- **Never put `salt.key` in the replica bucket**, and never back it up with
  credentials that can read the replica. The two halves in one place unseal every
  historical snapshot at once, and that is a promise broken retroactively for
  every visitor in the window.
- **Restore `system.db` only while the service is stopped.** Before either app
  or ingest starts, prune expired salts by running `DELETE FROM salts WHERE created_at <
  strftime('%s','now') - 172800;` against the restored file. Starting first
  resurrects salts that live retention deleted on purpose. A system restore
  also rolls back sites, users and API keys created since, none of which is
  recoverable from an account database.
- Changing the replica window means versioning and revalidating the provider
  lifecycle policy and every public retention statement. Do not add
  `DeleteObject` to the replicator as an undocumented second authority.

## What makes it worse

- Deriving salt values from the permanent key makes deleted days recoverable.
- Copying a salt key between environments expands who can decrypt the rows.
- Continuing to hash after prune failure breaks the 48-hour promise.
- Manually rotating to repair visitor numbers creates another split.
- Deleting the table to reclaim disk saves almost nothing and breaks identity.
