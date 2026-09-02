//
// server_test.go
// Tests for the health probes and the graceful stop.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package httpserver

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
)

// newTestServer binds a server on a kernel-chosen port and serves it, returning
// its base URL. The port is zero because a test that hard-coded one would fail
// whenever anything else on the machine happened to hold it.
func newTestServer(t testing.TB, handler http.Handler) (*Server, string) {
	t.Helper()

	server := New("test", "127.0.0.1:0", handler)

	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}

	go server.Serve() //nolint:errcheck // the error surfaces through the request that fails

	t.Cleanup(func() {
		if err := server.Shutdown(context.Background(), 0); err != nil {
			t.Error(err)
		}
	})

	return server, "http://" + server.Addr()
}

// get fetches a path and returns the status and body.
func get(t testing.TB, url string) (int, string) {
	t.Helper()

	resp, err := http.Get(url) //nolint:noctx // a test against a loopback listener
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(body)
}

// TestHealthProbes checks all three paths answer. The public monitor is a small
// serviceability answer, while the two internal probes distinguish a running
// process from one that is safe to receive traffic.
func TestHealthProbes(t *testing.T) {
	_, base := newTestServer(t, http.NotFoundHandler())

	if code, body := get(t, base+PathHealth); code != http.StatusOK || body != "ok\n" {
		t.Errorf("monitoring health = %d %q, want 200 ok", code, body)
	}
	if code, _ := get(t, base+PathLive); code != http.StatusOK {
		t.Errorf("liveness = %d, want 200", code)
	}
	if code, _ := get(t, base+PathReady); code != http.StatusOK {
		t.Errorf("readiness = %d, want 200", code)
	}
}

// TestReadinessRespectsTheProcessDependencies checks the components a process
// registers — an empty routing map, say. A failed one keeps traffic away
// without the process appearing dead and being restarted, and it names itself
// in the body so that whoever was woken up knows which one it was.
func TestReadinessRespectsTheProcessDependencies(t *testing.T) {
	server, base := newTestServer(t, http.NotFoundHandler())

	ready := false

	checks := &health.Set{}
	checks.Require("routing_map", health.Condition(func() bool { return ready }, "the routing map is empty"))
	server.Health = checks

	code, body := get(t, base+PathReady)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readiness = %d, want 503 while a dependency is failing", code)
	}

	if !strings.Contains(body, "routing_map") || !strings.Contains(body, "the routing map is empty") {
		t.Errorf("the body does not say which dependency failed or why: %s", body)
	}
	if code, body := get(t, base+PathHealth); code != http.StatusServiceUnavailable || body != "unhealthy\n" {
		t.Errorf("monitoring health = %d %q, want 503 unhealthy while a dependency is failing", code, body)
	}

	// Liveness must stay true regardless: a liveness probe that failed on a
	// downstream dependency would turn one slow database into a restart loop
	// across every process at once.
	if code, _ := get(t, base+PathLive); code != http.StatusOK {
		t.Fatal("liveness followed readiness down")
	}

	ready = true

	code, body = get(t, base+PathReady)
	if code != http.StatusOK {
		t.Fatalf("readiness = %d, want 200 once every dependency is up: %s", code, body)
	}
	if code, body := get(t, base+PathHealth); code != http.StatusOK || body != "ok\n" {
		t.Errorf("monitoring health = %d %q, want 200 ok once every dependency is up", code, body)
	}
}

// TestReadinessReportsDegradedWithoutRefusingTraffic checks that an optional
// dependency is visible and harmless. A missing geolocation database is the
// case this exists for: it makes the dashboard worse and must never make the
// process refuse traffic.
func TestReadinessReportsDegradedWithoutRefusingTraffic(t *testing.T) {
	server, base := newTestServer(t, http.NotFoundHandler())

	checks := &health.Set{}
	checks.Optional("geolocation", health.Condition(func() bool { return false }, "no database is loaded"))
	server.Health = checks

	code, body := get(t, base+PathReady)
	if code != http.StatusOK {
		t.Fatalf("readiness = %d, want 200 — an optional dependency must not refuse traffic", code)
	}

	if !strings.Contains(body, health.StatusDegraded) {
		t.Errorf("the body does not report the degraded component: %s", body)
	}
}

// TestHandlerIsMounted checks the wrapped handler still serves everything that
// is not a health path.
func TestHandlerIsMounted(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	})

	_, base := newTestServer(t, handler)

	code, body := get(t, base+"/anything")
	if code != http.StatusTeapot || body != "hello" {
		t.Fatalf("got %d %q, want 418 hello", code, body)
	}
}

// TestShutdownDrainsBeforeClosing checks readiness flips before the listener
// closes. Without that pause a rolling deploy drops the requests that were
// already in flight towards this replica.
func TestShutdownDrainsBeforeClosing(t *testing.T) {
	server := New("drain", "127.0.0.1:0", http.NotFoundHandler())

	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	go server.Serve() //nolint:errcheck // the shutdown below is the assertion

	base := "http://" + server.Addr()

	// The drain window is long enough to observe readiness go false while the
	// listener is still accepting.
	done := make(chan error, 1)
	go func() { done <- server.Shutdown(context.Background(), 500*time.Millisecond) }()

	time.Sleep(100 * time.Millisecond)

	if code, _ := get(t, base+PathReady); code != http.StatusServiceUnavailable {
		t.Fatalf("readiness = %d during the drain, want 503", code)
	}
	if code, _ := get(t, base+PathLive); code != http.StatusOK {
		t.Fatal("the listener stopped accepting before the drain finished")
	}
	if code, _ := get(t, base+PathHealth); code != http.StatusServiceUnavailable {
		t.Fatalf("monitoring health = %d during the drain, want 503", code)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the shutdown never finished")
	}

	// After the shutdown the listener is closed, so a request must fail.
	if _, err := http.Get(base + PathLive); err == nil { //nolint:noctx // a test against a loopback listener
		t.Fatal("the server was still accepting after shutdown")
	}
}

// TestPortInUseIsReportedAtStartUp checks the bind happens before serving, so a
// busy port is an error next to the log line naming it rather than a goroutine
// failing quietly a moment later.
func TestPortInUseIsReportedAtStartUp(t *testing.T) {
	first, base := newTestServer(t, http.NotFoundHandler())
	_ = base

	second := New("second", first.Addr(), http.NotFoundHandler())

	if err := second.Listen(); err == nil {
		t.Fatal("binding an address already in use succeeded")
	}
}

// TestTimeoutsAreSet checks none of them are left at Go's zero value, which
// means "no timeout" and is how a process ends up holding thousands of
// connections that will never send anything.
func TestTimeoutsAreSet(t *testing.T) {
	server := New("timeouts", "127.0.0.1:0", http.NotFoundHandler())

	if server.server.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is unset")
	}
	if server.server.ReadTimeout == 0 {
		t.Error("ReadTimeout is unset")
	}
	if server.server.WriteTimeout == 0 {
		t.Error("WriteTimeout is unset")
	}
	if server.server.IdleTimeout == 0 {
		t.Error("IdleTimeout is unset")
	}
}
