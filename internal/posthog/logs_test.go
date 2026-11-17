//
// logs_test.go
// Tests for copying log lines to PostHog Logs.
//
// Created: 2026-11-16
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package posthog

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// batch is one OTLP request the fake PostHog received, decoded.
type batch struct {
	Auth     string
	Resource map[string]string
	Records  []record
}

// record is one decoded log record.
type record struct {
	Time     string
	Severity int
	Level    string
	Body     string
	Attrs    map[string]string
}

// logsServer starts a fake PostHog logs endpoint that answers status and hands
// each decoded batch to the returned channel.
func logsServer(t *testing.T, status int) (string, chan batch) {
	t.Helper()

	received := make(chan batch, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/i/v1/logs" || r.Header.Get("Content-Encoding") != "gzip" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}

		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var payload otlpRequest
		if err := json.NewDecoder(zr).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		received <- decode(r.Header.Get("Authorization"), payload)
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)

	return server.URL, received
}

// decode flattens a request into something a test can compare.
func decode(auth string, payload otlpRequest) batch {
	out := batch{Auth: auth, Resource: map[string]string{}}

	for _, resourceLogs := range payload.ResourceLogs {
		for _, attr := range resourceLogs.Resource.Attributes {
			out.Resource[attr.Key] = attr.Value.StringValue
		}

		for _, scope := range resourceLogs.ScopeLogs {
			for _, r := range scope.LogRecords {
				attrs := map[string]string{}
				for _, attr := range r.Attributes {
					attrs[attr.Key] = attr.Value.StringValue
				}

				out.Records = append(out.Records, record{
					Time: r.TimeUnixNano, Severity: r.SeverityNumber, Level: r.SeverityText,
					Body: r.Body.StringValue, Attrs: attrs,
				})
			}
		}
	}

	return out
}

// exporter starts an exporter against host with a long flush interval, so a
// test decides when a batch goes by filling it or by closing.
func exporter(t *testing.T, host string, batchSize int) (*LogExporter, *bytes.Buffer) {
	t.Helper()

	var local bytes.Buffer
	e := NewLogExporter(LogOptions{
		Token:         "phc_test",
		Host:          host,
		Service:       "feasible-app",
		Env:           "production",
		Version:       "v1.2.3",
		Level:         slog.LevelInfo,
		Log:           logger.New(logger.Options{Level: "debug", Output: &local}),
		FlushInterval: time.Hour,
		BatchSize:     batchSize,
	})
	t.Cleanup(func() { _ = e.Close(context.Background()) })

	return e, &local
}

// expectBatch returns the next batch the fake PostHog received.
func expectBatch(t *testing.T, received chan batch) batch {
	t.Helper()

	select {
	case b := <-received:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("no batch was delivered")
		return batch{}
	}
}

// closeExporter closes e and fails the test if that does not finish.
func closeExporter(t *testing.T, e *LogExporter) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestNoTokenShipsNothing is the self-hosted install: no exporter, and a nil
// one that is safe to close.
func TestNoTokenShipsNothing(t *testing.T) {
	e := NewLogExporter(LogOptions{Host: "https://us.i.posthog.com"})
	if e != nil {
		t.Fatal("an exporter was built with no token")
	}

	if e.Handler() != nil {
		t.Fatal("a nil exporter returned a handler")
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestRecordsArriveAsOTLP checks one record end to end: the token, the
// resource that names this process, the severity on OpenTelemetry's scale, and
// attributes from the call, from With, and from a group, flattened to dotted
// keys.
func TestRecordsArriveAsOTLP(t *testing.T) {
	host, received := logsServer(t, http.StatusOK)
	e, _ := exporter(t, host, 100)

	log := slog.New(e.Handler()).With("shard", 2).WithGroup("req")
	log.Warn("slow report", "duration", 1500*time.Millisecond, slog.Group("site", "id", 9), "error", errors.New("boom"))

	closeExporter(t, e)
	b := expectBatch(t, received)

	if b.Auth != "Bearer phc_test" {
		t.Fatalf("Authorization = %q", b.Auth)
	}

	for key, want := range map[string]string{
		"service.name": "feasible-app", "deployment.environment": "production", "service.version": "v1.2.3",
	} {
		if b.Resource[key] != want {
			t.Errorf("resource %s = %q, want %q", key, b.Resource[key], want)
		}
	}

	if len(b.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(b.Records))
	}

	r := b.Records[0]
	if r.Body != "slow report" || r.Severity != 13 || r.Level != "WARN" || r.Time == "" {
		t.Fatalf("record = %+v", r)
	}

	for key, want := range map[string]string{
		"shard": "2", "req.duration": "1.5s", "req.site.id": "9", "req.error": "boom",
	} {
		if r.Attrs[key] != want {
			t.Errorf("attribute %s = %q, want %q (all: %v)", key, r.Attrs[key], want, r.Attrs)
		}
	}
}

// TestSeverityFollowsOpenTelemetry pins the mapping PostHog filters levels by.
func TestSeverityFollowsOpenTelemetry(t *testing.T) {
	for level, want := range map[slog.Level]int{
		slog.LevelDebug: 5, slog.LevelInfo: 9, slog.LevelWarn: 13, slog.LevelError: 17,
		slog.Level(-20): 1, slog.Level(40): 24,
	} {
		if got := severityNumber(level); got != want {
			t.Errorf("severityNumber(%v) = %d, want %d", level, got, want)
		}
	}
}

// TestLevelIsRespected checks a record below the configured level is not
// shipped.
func TestLevelIsRespected(t *testing.T) {
	host, received := logsServer(t, http.StatusOK)
	e, _ := exporter(t, host, 100)

	log := slog.New(e.Handler())
	log.Debug("too quiet")
	log.Info("loud enough")

	closeExporter(t, e)
	b := expectBatch(t, received)

	if len(b.Records) != 1 || b.Records[0].Body != "loud enough" {
		t.Fatalf("records = %+v", b.Records)
	}
}

// TestAFullBatchIsSentWithoutWaiting checks the batch size triggers a send
// before the interval does.
func TestAFullBatchIsSentWithoutWaiting(t *testing.T) {
	host, received := logsServer(t, http.StatusOK)
	e, _ := exporter(t, host, 2)

	log := slog.New(e.Handler())
	log.Info("one")
	log.Info("two")

	if b := expectBatch(t, received); len(b.Records) != 2 {
		t.Fatalf("got %d records, want 2", len(b.Records))
	}
}

// TestCloseDrainsTheQueue checks nothing queued is left behind at exit.
func TestCloseDrainsTheQueue(t *testing.T) {
	host, received := logsServer(t, http.StatusOK)
	e, _ := exporter(t, host, 3)

	log := slog.New(e.Handler())
	for range 7 {
		log.Info("line")
	}

	closeExporter(t, e)

	total := 0
	for total < 7 {
		total += len(expectBatch(t, received).Records)
	}
	if total != 7 {
		t.Fatalf("shipped %d records, want 7", total)
	}
}

// TestARefusedBatchIsReportedLocally checks a failure is written to the local
// log, which still holds every line PostHog did not take.
func TestARefusedBatchIsReportedLocally(t *testing.T) {
	host, received := logsServer(t, http.StatusUnauthorized)
	e, local := exporter(t, host, 100)

	slog.New(e.Handler()).Error("something broke")

	closeExporter(t, e)
	expectBatch(t, received)

	out := local.String()
	if !strings.Contains(out, "log records not shipped to posthog") || !strings.Contains(out, "send_failed=1") ||
		!strings.Contains(out, "401") {
		t.Fatalf("the failure was not reported:\n%s", out)
	}
}

// TestAFullQueueDropsAndReports checks the handler never blocks the caller and
// that what it drops is counted.
func TestAFullQueueDropsAndReports(t *testing.T) {
	var local bytes.Buffer
	e := &LogExporter{
		level: slog.LevelInfo,
		log:   logger.New(logger.Options{Output: &local}),
		queue: make(chan otlpRecord, 1),
	}

	log := slog.New(e.Handler())
	log.Info("kept")
	log.Info("dropped")
	log.Info("dropped")

	e.reportLosses(true)

	if !strings.Contains(local.String(), "queue_full=2") {
		t.Fatalf("the drops were not reported:\n%s", local.String())
	}
}

// TestShippingThroughTheLogger checks the wiring the command uses: a logger
// with Ship writes locally and to PostHog, and the identity trace stays local.
func TestShippingThroughTheLogger(t *testing.T) {
	host, received := logsServer(t, http.StatusOK)
	e, _ := exporter(t, host, 100)

	var out bytes.Buffer
	log := logger.New(logger.Options{Level: "info", Output: &out, TraceIdentity: true, Ship: e.Handler()})

	log.Info("account created", "team", 4)
	log.TraceIdentity("client_ip", "203.0.113.9")

	closeExporter(t, e)
	b := expectBatch(t, received)

	if len(b.Records) != 1 || b.Records[0].Body != "account created" || b.Records[0].Attrs["team"] != "4" {
		t.Fatalf("shipped %+v, want only the account line", b.Records)
	}

	if !strings.Contains(out.String(), "account created") || !strings.Contains(out.String(), "203.0.113.9") {
		t.Fatalf("the local log is missing a line:\n%s", out.String())
	}
}

// TestAddressesStayLocal checks no IP address reaches PostHog, whatever the
// line, the key, the group or the spelling, while the local log keeps every
// one of them and the attributes around them still ship.
func TestAddressesStayLocal(t *testing.T) {
	host, received := logsServer(t, http.StatusOK)
	e, _ := exporter(t, host, 100)

	var out bytes.Buffer
	log := logger.New(logger.Options{Level: "info", Output: &out, Ship: e.Handler()})

	log.Warn("registration refused by the human check", "ip", "203.0.113.7", "email", "person@example.com")
	log.With("remote", "198.51.100.2:443").Warn("stripe webhook rejected", "error", "bad signature")
	log.WithGroup("request").Info("grouped",
		"remote_addr", "[2001:db8::1]:8080", slog.Group("client", "IP", "192.0.2.4"), "path", "/register")
	log.WithGroup("ip").Info("under an address group", "v4", "192.0.2.9")
	log.Info("spellings", "client.ip", "192.0.2.5", "Remote-Addr", "192.0.2.6", "clientIp", "192.0.2.10",
		"addr", "127.0.0.1:19301")
	log.Info("unnamed", "forwarded", "192.0.2.8, 10.0.0.1", "seen_from", "2001:db8::2",
		"version", "v1.2.3", "count", 1234)

	closeExporter(t, e)
	b := expectBatch(t, received)

	if len(b.Records) != 6 {
		t.Fatalf("got %d records, want 6", len(b.Records))
	}

	shipped, err := json.Marshal(b.Records)
	if err != nil {
		t.Fatal(err)
	}

	addresses := []string{
		"203.0.113.7", "198.51.100.2", "2001:db8::1", "192.0.2.4", "192.0.2.9", "192.0.2.5", "192.0.2.6",
		"192.0.2.10", "127.0.0.1", "192.0.2.8", "2001:db8::2",
	}
	for _, address := range addresses {
		if strings.Contains(string(shipped), address) {
			t.Errorf("%s was shipped: %s", address, shipped)
		}
		if !strings.Contains(out.String(), address) {
			t.Errorf("%s is missing from the local log:\n%s", address, out.String())
		}
	}

	kept := map[string]map[string]string{
		"registration refused by the human check": {"email": "person@example.com"},
		"stripe webhook rejected":                 {"error": "bad signature"},
		"grouped":                                 {"request.path": "/register"},
		"unnamed":                                 {"version": "v1.2.3", "count": "1234"},
	}
	for _, r := range b.Records {
		for key, want := range kept[r.Body] {
			if r.Attrs[key] != want {
				t.Errorf("%q: attribute %s = %q, want %q (all: %v)", r.Body, key, r.Attrs[key], want, r.Attrs)
			}
		}
		if len(kept[r.Body]) == 0 && len(r.Attrs) != 0 {
			t.Errorf("%q shipped attributes it should not have: %v", r.Body, r.Attrs)
		}
	}
}

// TestCarriesAddress pins which keys and values count as an address, and that
// values which only look a little like one still ship.
func TestCarriesAddress(t *testing.T) {
	for _, tc := range []struct {
		key, value string
		want       bool
	}{
		{"ip", "", true},
		{"remote", "anything", true},
		{"req.remote_ip", "x", true},
		{"X-Forwarded-For", "x", true},
		{"ip.v4", "x", true},
		{"from", "203.0.113.7", true},
		{"from", "[2001:db8::1]:443", true},
		{"from", "[2001:db8::1]", true},
		{"from", "fe80::1%eth0", true},
		{"chain", "unknown, 203.0.113.7", true},
		{"client_ip_source", "X-Forwarded-For", false},
		{"email", "person@example.com", false},
		{"version", "0.0.61", false},
		{"count", "1234", false},
		{"duration", "1.5s", false},
		{"at", "2026-11-16T21:38:00Z", false},
		{"hash", "ab:cd", false},
		{"description", "ship it", false},
	} {
		if got := carriesAddress(tc.key, tc.value); got != tc.want {
			t.Errorf("carriesAddress(%q, %q) = %v, want %v", tc.key, tc.value, got, tc.want)
		}
	}
}
