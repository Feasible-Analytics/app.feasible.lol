#!/usr/bin/env bash
#
# drain.sh
# Wait until a process holds no unwritten events, so a deploy can terminate it.
#
# Created: 2026-08-31
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#
# Terminating a process with a full write buffer loses whatever is in it: the
# visitor already has a 202, so nothing retries and nothing reports it. This
# script is the step between "stop sending it traffic" and "kill it" — it exits
# zero only when the buffer has actually reached zero.
#
# It deliberately does not terminate anything. Deciding when to send SIGTERM is
# the deploy tool's job, and a script that both waited and killed would be one
# people run by hand on the wrong box.
#
# Order of operations in a deploy:
#
#   1. Deregister the instance from the load balancer, or let the readiness
#      probe do it (see ops/load-balancer.md for why explicit is better).
#   2. Run this script.
#   3. Send SIGTERM. Shutdown flushes the buffer again, so this is belt and
#      braces — but a buffer that will not drain is a shard that is down, and
#      that is worth knowing before the instance goes away rather than after.

set -euo pipefail

METRICS_URL="${1:-http://127.0.0.1:19302/metrics}"
TIMEOUT_SECONDS="${DRAIN_TIMEOUT_SECONDS:-120}"
POLL_SECONDS="${DRAIN_POLL_SECONDS:-2}"

# The gauge is read straight out of the Prometheus text body rather than through
# a query engine, because this runs on the box during a deploy and must not
# depend on the monitoring stack being up.
buffer_depth() {
  curl --silent --show-error --fail --max-time 5 "$METRICS_URL" |
    awk '$1 == "feasible_ingest_buffer_events" { print $2; found = 1 }
         END { if (!found) exit 1 }'
}

deadline=$(( $(date +%s) + TIMEOUT_SECONDS ))

echo "drain: waiting for feasible_ingest_buffer_events to reach 0 at $METRICS_URL"

while true; do
  if ! depth="$(buffer_depth)"; then
    # A metrics endpoint that cannot be read is not a drained process. Saying so
    # and failing is the whole point: the alternative is a deploy that treats an
    # unreachable box as an empty one.
    echo "drain: could not read $METRICS_URL — refusing to report this process as drained" >&2
    exit 1
  fi

  # The gauge is a float in the text format, so "0" arrives as "0".
  if [ "${depth%.*}" -eq 0 ] 2>/dev/null; then
    echo "drain: buffer is empty — safe to terminate"
    exit 0
  fi

  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "drain: ${depth} events are still buffered after ${TIMEOUT_SECONDS}s — do not terminate this process" >&2
    echo "drain: the shard it forwards to is almost certainly down; see ops/runbooks/shard-down.md" >&2
    exit 1
  fi

  echo "drain: ${depth} events still buffered"
  sleep "$POLL_SECONDS"
done
