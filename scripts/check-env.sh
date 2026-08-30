#!/usr/bin/env bash
#
# check-env.sh
# Fails the build if the Go source reads an environment variable that .env.sample
# does not document.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#
# Without enforcement the sample rots within a month and every new developer
# loses an afternoon to a variable nobody wrote down. This runs in CI.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SAMPLE="$ROOT/.env.sample"

if [ ! -f "$SAMPLE" ]; then
  echo "check-env: $SAMPLE is missing" >&2
  exit 1
fi

# Names read by the application. Two patterns, because a name can reach the
# loader either as one of our own FEASIBLE_* literals or through a direct
# os.Getenv for something outside our namespace, such as CONFIG_DIR. Test files
# are excluded: they invent variables on purpose.
used="$(
  {
    grep -rhoE '"FEASIBLE_[A-Z0-9_]+"' \
      --include='*.go' --exclude='*_test.go' "$ROOT" || true
    grep -rhoE 'os\.(Getenv|LookupEnv)\("[A-Z][A-Z0-9_]*"' \
      --include='*.go' --exclude='*_test.go' "$ROOT" |
      grep -oE '"[A-Z][A-Z0-9_]*"' || true
  } | tr -d '"' | sort -u
)"

# Names declared in the sample, and whether each has a comment directly above it.
# A commented-out default such as "# CONFIG_DIR=..." still counts as declared —
# some variables have no sensible value to ship enabled.
declared="$(awk '
  /^[[:space:]]*#?[[:space:]]*[A-Za-z_][A-Za-z0-9_]*=/ {
    name = $0
    sub(/^[[:space:]]*#?[[:space:]]*/, "", name)
    sub(/=.*/, "", name)
    print name, (commented ? "documented" : "bare")
    commented = 0
    next
  }
  /^[[:space:]]*#/ { commented = 1; next }
  { commented = 0 }
' "$SAMPLE")"

declared_names="$(echo "$declared" | awk '{print $1}' | sort -u)"
status=0

for name in $used; do
  if ! echo "$declared_names" | grep -qx "$name"; then
    echo "check-env: $name is read by the Go source but is missing from .env.sample" >&2
    status=1
  fi
done

while read -r name state; do
  [ -z "$name" ] && continue
  if [ "$state" = "bare" ]; then
    echo "check-env: $name has no comment above it in .env.sample" >&2
    status=1
  fi
done <<< "$declared"

# The other direction is a warning, not a failure: a variable may be documented
# ahead of the code that reads it, and blocking on that would just teach people
# to skip the documentation.
for name in $declared_names; do
  case "$name" in
    FEASIBLE_*) ;;
    *) continue ;;
  esac

  if ! echo "$used" | grep -qx "$name"; then
    echo "check-env: warning: $name is documented but nothing reads it"
  fi
done

if [ "$status" -eq 0 ]; then
  count="$(echo "$used" | grep -c . || true)"
  echo "check-env: ok — $count environment variables, all documented"
fi

exit "$status"
