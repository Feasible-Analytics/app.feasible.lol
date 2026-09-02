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

	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// configureStandaloneIngest supplies the private signing identity required by
// every standalone ingester, while leaving individual tests free to vary URLs.
func configureStandaloneIngest(t *testing.T) {
	t.Helper()
	t.Setenv("FEASIBLE_INTERNAL_KEY", "test-secret")
}

// TestIngestReportsShards checks the shard list is parsed and reported. An
// ingestor pointed at the wrong shards drops every event it receives, so the
// list has to be visible at boot rather than discovered from missing data.
func TestIngestReportsShards(t *testing.T) {
	configureStandaloneIngest(t)
	t.Setenv("FEASIBLE_INGEST_SHARDS", `["http://127.0.0.1:19301","http://127.0.0.1:29301"]`)

	code, stdout, stderr := run(t, "ingest", "-check")

	if code != ExitOK {
		t.Fatalf("exit code %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "19301") || !strings.Contains(stdout, "29301") {
		t.Fatalf("both shards should be reported: %q", stdout)
	}
}

// TestIngestListenFlagOverrides covers the same override the app has, since
// running two ingestors side by side is the normal way to test forwarding.
func TestIngestListenFlagOverrides(t *testing.T) {
	configureStandaloneIngest(t)
	code, stdout, _ := run(t, "ingest", "-check", "-listen", "127.0.0.1:29302")

	if code != ExitOK {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(stdout, "listen=127.0.0.1:29302") {
		t.Fatalf("flag did not override the environment: %q", stdout)
	}
}

// TestStandaloneRecorderCoversRequestAndWriterSides pins the split-topology
// wiring. The standalone command must attach the recorder to the public
// pipeline and to final shard outcomes, just as the direct app process does.
func TestStandaloneRecorderCoversRequestAndWriterSides(t *testing.T) {
	service := &ingest.Service{
		Handler: &ingest.Handler{},
		Writer:  &ingest.Writer{},
	}
	recorder := &health.Recorder{}

	attachIngestRecorder(service, recorder)

	if service.Handler.Observer != recorder {
		t.Fatal("the standalone request pipeline does not use the health recorder")
	}
	if service.Writer.Observer != recorder {
		t.Fatal("the standalone writer does not use the health recorder")
	}
}
