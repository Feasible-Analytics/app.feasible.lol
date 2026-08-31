//
// serve_test.go
// Tests for the `serve` subcommand.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"strings"
	"testing"
)

// TestServeReportsResolvedConfig checks that serve resolves and reports the
// values the rest of the system is built on. Until the HTTP server exists this
// is the only place a wrong listen address or base URL becomes visible.
func TestServeReportsResolvedConfig(t *testing.T) {
	t.Setenv("FEASIBLE_APP_BASE_URL", "http://rager.example.ts.net:19300")
	t.Setenv("FEASIBLE_APP_TRANSPORT", "http")

	code, stdout, stderr := run(t, "serve", "-check")

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}

	for _, want := range []string{
		"base_url=http://rager.example.ts.net:19300",
		"transport=http",
		"internal_listen=127.0.0.1:19401",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("serve did not report %q; got %q", want, stdout)
		}
	}
}

// TestServeListenFlagOverrides covers the flag overriding the environment, which
// is what makes a second instance on another port a one-word change.
func TestServeListenFlagOverrides(t *testing.T) {
	t.Setenv("FEASIBLE_APP_LISTEN", "127.0.0.1:19301")

	code, stdout, _ := run(t, "serve", "-check", "-listen", "127.0.0.1:29301")

	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stdout, "listen=127.0.0.1:29301") {
		t.Fatalf("flag did not override the environment: %q", stdout)
	}
}

// TestServeTraceEventsFlag is the plumbing check for --trace-events: the root
// flag has to reach the configuration the subcommand runs with, or turning it on
// would appear to do nothing.
func TestServeTraceEventsFlag(t *testing.T) {
	code, stdout, _ := run(t, "--trace-events", "serve", "-check")

	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stdout, "trace_events=true") {
		t.Fatalf("--trace-events did not reach the config: %q", stdout)
	}
}

// TestServeConfigErrorExitsOne separates a bad configuration from a bad command
// line. Both used to be exit 2 in a hundred other tools, and it makes a boot
// loop impossible to diagnose from a supervisor log.
func TestServeConfigErrorExitsOne(t *testing.T) {
	t.Setenv("FEASIBLE_APP_TRANSPORT", "carrier-pigeon")

	code, _, stderr := run(t, "serve", "-check")

	if code != ExitError {
		t.Fatalf("exit code %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "configuration error") {
		t.Fatalf("unhelpful message: %q", stderr)
	}
}
