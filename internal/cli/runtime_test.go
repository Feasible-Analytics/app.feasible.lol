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
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/httpserver"
)

// TestProcessListenerServesApplicationHealthAndInternalRoutes checks all three
// surfaces share one socket. A second listener must not quietly return through
// later refactoring.
func TestProcessListenerServesApplicationHealthAndInternalRoutes(t *testing.T) {
	checks := &health.Set{}
	checks.Require("system_db", func(context.Context) error { return nil })
	application := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("application"))
	})
	internal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("internal"))
	})

	// Port zero, because a test that hard-coded one would fail whenever
	// anything else on the machine happened to hold it.
	server := httpserver.New("test-process", "127.0.0.1:0", processRoutes(application, internal))
	server.Health = checks

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

	response, err := http.Get(base + "/metrics") //nolint:noctx // loopback test listener
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := response.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("metrics endpoint = %d, want 404", response.StatusCode)
	}

	// The health probes come with every listener, so a scrape target that is
	// answering is also a target that can be checked.
	if got := fetch(t, base+httpserver.PathReady); !strings.Contains(got, "system_db") {
		t.Errorf("readiness did not name its components: %s", got)
	}
	if got := fetch(t, base+"/internal/domains"); got != "internal" {
		t.Errorf("internal route returned %q", got)
	}
	if got := fetch(t, base+"/"); got != "application" {
		t.Errorf("application route returned %q", got)
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
