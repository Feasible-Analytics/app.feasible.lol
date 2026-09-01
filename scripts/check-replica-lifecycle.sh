#!/usr/bin/env bash
#
# check-replica-lifecycle.sh
# Fetch and validate the provider-side replica retention controls.
#
# Created: 2026-08-31
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

set -euo pipefail
umask 027

: "${FEASIBLE_LITESTREAM_REPLICA_URL:?set FEASIBLE_LITESTREAM_REPLICA_URL}"
: "${FEASIBLE_LITESTREAM_ATTESTATION:?set FEASIBLE_LITESTREAM_ATTESTATION}"

case "$FEASIBLE_LITESTREAM_REPLICA_URL" in
  s3://*/*) ;;
  *) echo "replica URL must be s3://bucket/shard-prefix" >&2; exit 1 ;;
esac

location=${FEASIBLE_LITESTREAM_REPLICA_URL#s3://}
bucket=${location%%/*}
prefix=${location#*/}
prefix=${prefix#/}
prefix=${prefix%/}/

# Every response and the final bundle stay in one temporary directory beside
# the destination. Validation sees one generation and publication is one
# same-filesystem atomic rename.
attestation_dir=$(dirname "$FEASIBLE_LITESTREAM_ATTESTATION")
install -d -m 0750 "$attestation_dir"
work_dir=$(mktemp -d "$attestation_dir/.replica-attestation.XXXXXX")
policy_tmp="$work_dir/lifecycle.json"
versioning_tmp="$work_dir/versioning.json"
object_lock_tmp="$work_dir/object-lock.json"
location_tmp="$work_dir/location.json"
object_lock_error="$work_dir/object-lock.error"
bundle_tmp="$attestation_dir/.replica-attestation.$$.json"

# cleanup removes only unpublished exports. Successfully validated files are
# atomically renamed below and therefore no longer have these temporary names.
cleanup() {
  rm -rf "$work_dir"
  rm -f "$bundle_tmp"
}
trap cleanup EXIT

aws_args=()
if [[ -n "${S3_ENDPOINT_URL:-}" ]]; then
  aws_args+=(--endpoint-url "$S3_ENDPOINT_URL")
fi

aws "${aws_args[@]}" s3api get-bucket-lifecycle-configuration \
  --bucket "$bucket" > "$policy_tmp"
aws "${aws_args[@]}" s3api get-bucket-versioning \
  --bucket "$bucket" > "$versioning_tmp"
aws "${aws_args[@]}" s3api get-bucket-location \
  --bucket "$bucket" > "$location_tmp"

if ! aws "${aws_args[@]}" s3api get-object-lock-configuration \
  --bucket "$bucket" > "$object_lock_tmp" 2> "$object_lock_error"; then
  if grep -Eq 'ObjectLockConfigurationNotFoundError|NoSuchObjectLockConfiguration' "$object_lock_error"; then
    printf '{}\n' > "$object_lock_tmp"
  else
    cat "$object_lock_error" >&2
    exit 1
  fi
fi

jq -n \
  --arg version "feasible-replica-attestation-v1" \
  --arg fetched_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg replica_url "$FEASIBLE_LITESTREAM_REPLICA_URL" \
  --arg bucket "$bucket" \
  --arg prefix "$prefix" \
  --slurpfile bucket_location "$location_tmp" \
  --slurpfile lifecycle "$policy_tmp" \
  --slurpfile versioning "$versioning_tmp" \
  --slurpfile object_lock "$object_lock_tmp" \
  '{version:$version,fetched_at:$fetched_at,replica_url:$replica_url,bucket:$bucket,prefix:$prefix,bucket_location:$bucket_location[0],lifecycle:$lifecycle[0],versioning:$versioning[0],object_lock:$object_lock[0]}' \
  > "$bundle_tmp"

FEASIBLE_LITESTREAM_ATTESTATION="$bundle_tmp" \
feasible litestream lifecycle-check \
  -replica-url "$FEASIBLE_LITESTREAM_REPLICA_URL" \
  -attestation "$bundle_tmp"

mv "$bundle_tmp" "$FEASIBLE_LITESTREAM_ATTESTATION"
