//
// runtime_test.go
// Tests for the wiring both long-running commands share.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/httpserver"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// TestInternalListenerServesMetricsAndHealth checks the loopback listener every
// process runs beside its public one. It is assembled in one place for both
// process shapes, so a mistake here would silently cost the whole system its
// monitoring while everything else kept working.
func TestInternalListenerServesMetricsAndHealth(t *testing.T) {
	checks := &health.Set{}
	checks.Require("control_db", func(context.Context) error { return nil })

	// Port zero, because a test that hard-coded one would fail whenever
	// anything else on the machine happened to hold it.
	server := internalServer("test-internal", "127.0.0.1:0", checks)

	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}

	go func() { _ = server.Serve() }()

	t.Cleanup(func() {
		if err := server.Shutdown(context.Background(), 0); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	base := "http://" + server.Addr()

	body := fetch(t, base+"/metrics")
	if !strings.Contains(body, "feasible_") {
		t.Errorf("the metrics endpoint served no series of ours:\n%s", body)
	}

	// The health probes come with every listener, so a scrape target that is
	// answering is also a target that can be checked.
	if got := fetch(t, base+httpserver.PathReady); !strings.Contains(got, "control_db") {
		t.Errorf("readiness did not name its components: %s", got)
	}
}

// fetch reads one response body as a string.
func fetch(t *testing.T, url string) string {
	t.Helper()

	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

// TestJobCountsSplitsTheQueueByState checks the metrics endpoint's view of the
// background queue, against the real schema.
//
// It runs the statement rather than asserting on its text because the point of
// failure is the statement itself: a query that no longer matches the table
// would report an empty queue forever, which reads as a healthy one.
func TestJobCountsSplitsTheQueueByState(t *testing.T) {
	dir := migratedDataDir(t)

	db, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := []struct {
		state string
		count int
	}{
		{"available", 3},
		{"executing", 1},
		{"completed", 5},
	}

	for _, row := range rows {
		for i := 0; i < row.count; i++ {
			if _, err := db.Exec(
				"INSERT INTO jobs (queue, kind, args, state, scheduled_at) VALUES ('default', 'test', '{}', ?, 0)",
				row.state,
			); err != nil {
				t.Fatal(err)
			}
		}
	}

	counts, err := jobCounts(db)(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if counts.Available != 3 {
		t.Errorf("available = %d, want 3", counts.Available)
	}

	// Completed jobs must not be counted: they are history, and a depth that
	// only ever grows is a metric nobody can alert on.
	if counts.Executing != 1 {
		t.Errorf("executing = %d, want 1", counts.Executing)
	}
}
