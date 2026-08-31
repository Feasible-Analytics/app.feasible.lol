//
// ingest_test.go
// Tests for the `ingest` subcommand.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"strings"
	"testing"
)

// TestIngestReportsShards checks the shard list is parsed and reported. An
// ingestor pointed at the wrong shards drops every event it receives, so the
// list has to be visible at boot rather than discovered from missing data.
func TestIngestReportsShards(t *testing.T) {
	t.Setenv("FEASIBLE_INGEST_SHARDS", "http://127.0.0.1:19401, http://127.0.0.1:19402")

	code, stdout, stderr := run(t, "ingest", "-check")

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "19401") || !strings.Contains(stdout, "19402") {
		t.Fatalf("both shards should be reported: %q", stdout)
	}
}

// TestIngestReportsItsInternalListener checks the loopback address serving
// /metrics is resolved and reported. It defaults to a different port from the
// app's so that both processes can run on one machine, and a collision would
// otherwise only show up as a process that refuses to start.
func TestIngestReportsItsInternalListener(t *testing.T) {
	code, stdout, stderr := run(t, "ingest", "-check")

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "internal_listen=127.0.0.1:19402") {
		t.Fatalf("the internal listener was not reported: %q", stdout)
	}
}

// TestIngestListenFlagOverrides covers the same override the app has, since
// running two ingestors side by side is the normal way to test forwarding.
func TestIngestListenFlagOverrides(t *testing.T) {
	code, stdout, _ := run(t, "ingest", "-check", "-listen", "127.0.0.1:29302")

	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stdout, "listen=127.0.0.1:29302") {
		t.Fatalf("flag did not override the environment: %q", stdout)
	}
}
