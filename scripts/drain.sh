#!/usr/bin/env bash
#
# drain.sh
# Wait until an ingester holds no undelivered events before terminating it.
#
# Created: 2026-09-02
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#
# The outbox database is the durable ownership record for pageviews that have
# received a public 202 but have not yet been acknowledged by an app shard. This
# script reads that local database directly so draining does not require a
# network endpoint or a separate monitoring system.

set -euo pipefail

BUFFER_PATH="${1:-./data/ingest/buffer.db}"
TIMEOUT_SECONDS="${DRAIN_TIMEOUT_SECONDS:-120}"
POLL_SECONDS="${DRAIN_POLL_SECONDS:-2}"

# buffer_depth reads the authoritative number of rows still owed to app shards.
# A missing database or table is an error rather than an empty queue because a
# deploy must never interpret an unreadable ownership record as safe to discard.
buffer_depth() {
  sqlite3 -batch -bail -noheader "$BUFFER_PATH" 'SELECT COUNT(*) FROM outbox;'
}

deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))

echo "drain: waiting for the outbox to reach 0 in $BUFFER_PATH"

while true; do
  if ! depth="$(buffer_depth)"; then
    echo "drain: could not read $BUFFER_PATH — refusing to report this process as drained" >&2
    exit 1
  fi

  if [ "$depth" -eq 0 ] 2>/dev/null; then
    echo "drain: outbox is empty — safe to terminate"
    exit 0
  fi

  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "drain: $depth events are still buffered after ${TIMEOUT_SECONDS}s — do not terminate this process" >&2
    echo "drain: the destination app shard may be down; see ops/runbooks/shard-down.md" >&2
    exit 1
  fi

  echo "drain: $depth events still buffered"
  sleep "$POLL_SECONDS"
done
