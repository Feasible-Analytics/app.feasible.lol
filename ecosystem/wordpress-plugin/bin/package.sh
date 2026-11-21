#!/usr/bin/env bash
#
# package.sh
# Builds the tracker into the plugin, checks the plugin, and zips it for the plugin directory.
#
# Created: 2026-11-21
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

set -euo pipefail

PLUGIN="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TRACKER="$PLUGIN/../../tracker"
SLUG="feasible-analytics"
SCRIPT="$PLUGIN/assets/js/feasible.js"

# The plugin serves the tracker from its own folder, so the zip has to carry a
# copy built from the tracker source in this checkout. The tracker build writes
# that copy itself, beside the one the Go binary embeds.
if [ -d "$TRACKER" ]; then
	echo "building the tracker"
	if [ ! -d "$TRACKER/node_modules" ]; then
		npm --prefix "$TRACKER" ci --silent
	fi
	npm --prefix "$TRACKER" run build --silent
else
	echo "no tracker folder beside the plugin; packaging the committed assets/js/feasible.js"
fi

if [ ! -s "$SCRIPT" ]; then
	echo "FAILED: $SCRIPT is missing" >&2
	exit 1
fi

# The three places a release names its version have to agree, or the directory
# serves one version while the plugin reports another.
header_version="$(sed -n 's/^ \* Version: *\([0-9.]*\).*/\1/p' "$PLUGIN/$SLUG.php")"
constant_version="$(sed -n "s/^define( 'FEASIBLE_VERSION', '\([0-9.]*\)' );/\1/p" "$PLUGIN/$SLUG.php")"
stable_tag="$(sed -n 's/^Stable tag: *\([0-9.]*\).*/\1/p' "$PLUGIN/readme.txt")"

if [ -z "$header_version" ] || [ "$header_version" != "$constant_version" ] || [ "$header_version" != "$stable_tag" ]; then
	echo "FAILED: version mismatch (header $header_version, constant $constant_version, stable tag $stable_tag)" >&2
	exit 1
fi

echo "checking syntax and running the tests"
find "$PLUGIN" -name '*.php' -not -path "$PLUGIN/build/*" -print0 | xargs -0 -n1 php -l >/dev/null
(cd "$PLUGIN" && php tests/run.php)

# Only what a site needs goes in the zip. The tests, this script and the
# developer README stay in the repository.
OUT="$PLUGIN/build"
STAGE="$OUT/stage/$SLUG"
ZIP="$OUT/$SLUG-$header_version.zip"

rm -rf "$OUT/stage" "$ZIP"
mkdir -p "$STAGE"

cp "$PLUGIN/$SLUG.php" "$PLUGIN/uninstall.php" "$PLUGIN/readme.txt" "$PLUGIN/LICENSE" "$STAGE/"
cp -R "$PLUGIN/includes" "$PLUGIN/assets" "$PLUGIN/languages" "$STAGE/"
find "$STAGE" -name '.DS_Store' -delete

(cd "$OUT/stage" && zip -qr -X "$ZIP" "$SLUG")
rm -rf "$OUT/stage"

echo "built $ZIP"
