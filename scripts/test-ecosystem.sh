#!/usr/bin/env bash
#
# test-ecosystem.sh
# Run each ecosystem package's own tests, in whichever toolchains are installed.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

set -uo pipefail

# The packages under ecosystem/ are each destined for their own repository, so
# none of them is reachable from `go test ./...` and each one is tested by its
# own language's runner. This script is the one place that knows how to invoke
# all of them.
#
# A missing toolchain is reported and skipped rather than failing. Nobody should
# need PHP, Python, Ruby and Node installed to work on the Go binary, and a
# target that fails on a machine without them is a target people stop running —
# at which point it stops catching anything at all.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ECOSYSTEM="$ROOT/ecosystem"

FAILED=0
SKIPPED=0
PASSED=0

# say prints a section header, so the output of eight test runs stays readable
# as eight test runs rather than as one wall of text.
say() {
	printf '\n\033[1m── %s\033[0m\n' "$1"
}

# skip records a package that could not be tested here. It is counted rather
# than merely printed, because "everything passed" and "nothing ran" have to be
# distinguishable in the summary line.
skip() {
	printf '   skipped — %s\n' "$1"
	SKIPPED=$((SKIPPED + 1))
}

# run executes one package's test command in its own directory and records the
# verdict. The directory is entered in a subshell so a failing package cannot
# leave the rest of the script somewhere unexpected.
run() {
	local dir="$1"
	shift

	if [ ! -d "$dir" ]; then
		skip "$dir is not in this checkout"
		return
	fi

	if (cd "$dir" && "$@"); then
		PASSED=$((PASSED + 1))
	else
		printf '   FAILED: %s\n' "$dir"
		FAILED=$((FAILED + 1))
	fi
}

# have reports whether a toolchain is on the PATH.
have() {
	command -v "$1" >/dev/null 2>&1
}

say "Go SDK"
if have go; then
	run "$ECOSYSTEM/sdk-go" go test ./...
else
	skip "go is not installed"
fi

say "PHP SDK"
if have php; then
	run "$ECOSYSTEM/sdk-php" php tests/run.php
else
	skip "php is not installed"
fi

say "Python SDK"
if have python3; then
	run "$ECOSYSTEM/sdk-python" python3 -m unittest discover -s tests -t .
else
	skip "python3 is not installed"
fi

say "Ruby SDK"
if have ruby; then
	run "$ECOSYSTEM/sdk-ruby" ruby -Ilib -Itest test/feasible_test.rb
else
	skip "ruby is not installed"
fi

say "Node SDK"
if have node; then
	run "$ECOSYSTEM/sdk-node" node --test
else
	skip "node is not installed"
fi

say "Browser package"
if have node; then
	run "$ECOSYSTEM/npm-tracker" node --test
else
	skip "node is not installed"
fi

say "Looker Studio connector"
if have node; then
	run "$ECOSYSTEM/looker-studio-connector" node --test
else
	skip "node is not installed"
fi

say "WordPress plugin"
if have php; then
	# The syntax check comes first and covers every file, because a plugin with
	# a parse error in a file the tests never load is a plugin that white-screens
	# a customer's site on activation.
	if find "$ECOSYSTEM/wordpress-plugin" -name '*.php' -print0 | xargs -0 -n1 php -l >/dev/null; then
		printf '   php -l passed on every file\n'
		run "$ECOSYSTEM/wordpress-plugin" php tests/run.php
	else
		printf '   FAILED: php -l found a syntax error\n'
		FAILED=$((FAILED + 1))
	fi
else
	skip "php is not installed"
fi

printf '\n\033[1mecosystem: %d passed, %d skipped, %d failed\033[0m\n' "$PASSED" "$SKIPPED" "$FAILED"

exit $((FAILED > 0))
