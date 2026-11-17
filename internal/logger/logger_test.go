//
// logger_test.go
// Tests for the structured logger and its named events.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestParseLevel covers the strings the configuration accepts, plus the
// deliberate fallback: an unknown level must not stop a process booting.
func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":    slog.LevelDebug,
		"INFO":     slog.LevelInfo,
		" warn ":   slog.LevelWarn,
		"warning":  slog.LevelWarn,
		"error":    slog.LevelError,
		"nonsense": slog.LevelInfo,
	}

	for input, want := range cases {
		if got := ParseLevel(input); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", input, got, want)
		}
	}
}

// TestJSONFormat checks that the json format really produces one parseable
// object per line. Production logs are searched by machine, and a handler that
// silently stayed on text would only be noticed during an incident.
func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer

	log := New(Options{Level: "debug", Format: "json", Output: &buf})
	log.EmailSent("someone@example.com", "verify", "/tmp/mail/1.html")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, buf.String())
	}

	if line["msg"] != "email sent" || line["template"] != "verify" || line["recipient"] != "someone@example.com" {
		t.Fatalf("missing fields: %v", line)
	}
}

// TestEventReceivedDropIsLoud is the "never fail silently" rule in test form. A
// dropped event has to reach a level people actually watch, and it has to carry
// the reason — an event that vanishes with no explanation is the top complaint
// about the products we compete with.
func TestEventReceivedDropIsLoud(t *testing.T) {
	var buf bytes.Buffer

	log := New(Options{Level: "warn", Format: "text", Output: &buf})
	log.EventReceived("example.com", "", "", "unknown_domain")

	out := buf.String()
	if !strings.Contains(out, "event dropped") || !strings.Contains(out, "unknown_domain") {
		t.Fatalf("drop was not logged at warn with a reason: %q", out)
	}
}

// TestEventReceivedAcceptedIsDebug confirms the happy path stays at debug.
// Logging every accepted event at info would bury the drops this system exists
// to make visible.
func TestEventReceivedAcceptedIsDebug(t *testing.T) {
	var buf bytes.Buffer

	log := New(Options{Level: "info", Format: "text", Output: &buf})
	log.EventReceived("example.com", "site-1", "shard-1", "")

	if buf.Len() != 0 {
		t.Fatalf("accepted events should not appear above debug: %q", buf.String())
	}
}

// TestAuthFailureCarriesSkew protects the one detail from the logging issue
// that saves a real hour: a signature rejected because two clocks drifted has
// to say so, with the observed skew.
func TestAuthFailureCarriesSkew(t *testing.T) {
	var buf bytes.Buffer

	log := New(Options{Level: "info", Format: "text", Output: &buf})
	log.AuthFailure("timestamp outside window", 42*time.Second)

	out := buf.String()
	if !strings.Contains(out, "clock_skew") || !strings.Contains(out, "42s") {
		t.Fatalf("skew missing from auth failure: %q", out)
	}
}

// TestTraceIdentityGating checks the switch that puts an IP address on disk:
// silent when off, and never turned on by --trace-events.
func TestTraceIdentityGating(t *testing.T) {
	var off bytes.Buffer

	New(Options{Level: "debug", Format: "text", Output: &off}).
		TraceIdentity("client_ip", "203.0.113.5")

	if off.Len() != 0 {
		t.Fatalf("identity trace logged while disabled: %q", off.String())
	}

	// The two switches are independent on purpose. A deployment reading its
	// channel attribution must not start writing addresses as a side effect.
	var events bytes.Buffer

	eventsOnly := New(Options{Level: "debug", Format: "text", TraceEvents: true, Output: &events})
	if eventsOnly.TraceIdentityEnabled() {
		t.Fatal("--trace-events must not enable the identity trace")
	}

	eventsOnly.TraceIdentity("client_ip", "203.0.113.5")

	if strings.Contains(events.String(), "203.0.113.5") {
		t.Fatalf("address logged with only --trace-events on: %q", events.String())
	}

	var on bytes.Buffer

	log := New(Options{Level: "info", Format: "text", TraceIdentity: true, Output: &on})
	if !log.TraceIdentityEnabled() {
		t.Fatal("TraceIdentityEnabled should report the flag")
	}

	log.TraceIdentity("client_ip", "203.0.113.5")

	if !strings.Contains(on.String(), "identity trace") || !strings.Contains(on.String(), "203.0.113.5") {
		t.Fatalf("identity trace not logged while enabled: %q", on.String())
	}
}

// TestTraceEventGating checks both halves of --trace-events: silent when off,
// and visible at info when on so the flag alone is enough without also raising
// the log level.
func TestTraceEventGating(t *testing.T) {
	var off bytes.Buffer

	New(Options{Level: "debug", Format: "text", Output: &off}).TraceEvent("domain", "example.com")

	if off.Len() != 0 {
		t.Fatalf("trace logged while disabled: %q", off.String())
	}

	var on bytes.Buffer

	log := New(Options{Level: "info", Format: "text", TraceEvents: true, Output: &on})
	if !log.TraceEventsEnabled() {
		t.Fatal("TraceEventsEnabled should report the flag")
	}

	log.TraceEvent("domain", "example.com")

	if !strings.Contains(on.String(), "event trace") {
		t.Fatalf("trace not logged while enabled: %q", on.String())
	}
}

// shipped is a slog handler that keeps every record it is given.
type shipped struct {
	level   slog.Level
	records *[]slog.Record
}

// Enabled reports whether level reaches this handler.
func (s shipped) Enabled(_ context.Context, level slog.Level) bool { return level >= s.level }

// Handle keeps the record.
func (s shipped) Handle(_ context.Context, r slog.Record) error {
	*s.records = append(*s.records, r)
	return nil
}

// WithAttrs is not needed by these tests.
func (s shipped) WithAttrs([]slog.Attr) slog.Handler { return s }

// WithGroup is not needed by these tests.
func (s shipped) WithGroup(string) slog.Handler { return s }

// TestShipReceivesEveryLineAtItsOwnLevel checks a shipped copy of each record
// goes to Ship as well as to the local output, filtered by Ship's own level.
func TestShipReceivesEveryLineAtItsOwnLevel(t *testing.T) {
	var out bytes.Buffer
	var records []slog.Record

	log := New(Options{Level: "debug", Output: &out, Ship: shipped{level: slog.LevelInfo, records: &records}})
	log.Debug("local only")
	log.EventReceived("example.com", "1", "", "no such site")

	if !strings.Contains(out.String(), "local only") || !strings.Contains(out.String(), "event dropped") {
		t.Fatalf("the local log is missing a line: %q", out.String())
	}

	if len(records) != 1 || records[0].Message != "event dropped" {
		t.Fatalf("shipped %d records, want only the drop", len(records))
	}
}

// TestIdentityTraceIsNeverShipped keeps visitor addresses on this machine.
func TestIdentityTraceIsNeverShipped(t *testing.T) {
	var out bytes.Buffer
	var records []slog.Record

	log := New(Options{Level: "debug", Output: &out, TraceIdentity: true, Ship: shipped{records: &records}})
	log.TraceIdentity("client_ip", "203.0.113.5")

	if !strings.Contains(out.String(), "203.0.113.5") {
		t.Fatalf("the identity trace did not reach the local log: %q", out.String())
	}
	if len(records) != 0 {
		t.Fatalf("the identity trace was shipped: %v", records[0].Message)
	}
}
