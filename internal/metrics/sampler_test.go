//
// sampler_test.go
// Tests for the gauges read at scrape time.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package metrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// gather collects one sampler's series into a map keyed by name and its first
// label value, which is enough to assert on and short enough to read.
func gather(t *testing.T, s *sampler) map[string]float64 {
	t.Helper()

	registry := prometheus.NewRegistry()
	registry.MustRegister(s)

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]float64{}

	for _, family := range families {
		for _, metric := range family.GetMetric() {
			key := family.GetName()
			for _, label := range metric.GetLabel() {
				key += "/" + label.GetValue()
			}

			out[key] = metric.GetGauge().GetValue()
		}
	}

	return out
}

// TestSamplerReadsTheProcess checks the accessors are read on the scrape rather
// than captured when Watch was called. A gauge that reported the value a
// process had at start-up is worse than no gauge.
func TestSamplerReadsTheProcess(t *testing.T) {
	depth := 7

	s := newSampler(Sources{
		BufferDepth:  func() int { return depth },
		Sessions:     func() int { return 3 },
		Sites:        func() int { return 2 },
		OpenAccounts: func() int { return 1 },
	})

	if got := gather(t, s)["feasible_ingest_buffer_events"]; got != 7 {
		t.Errorf("buffer depth = %v, want 7", got)
	}

	depth = 99

	values := gather(t, s)

	if got := values["feasible_ingest_buffer_events"]; got != 99 {
		t.Errorf("buffer depth = %v after the buffer grew, want 99", got)
	}
	if got := values["feasible_ingest_sessions_live"]; got != 3 {
		t.Errorf("sessions = %v, want 3", got)
	}
	if got := values["feasible_sites_routed"]; got != 2 {
		t.Errorf("routed sites = %v, want 2", got)
	}
}

// TestSamplerSkipsWhatAProcessDoesNotHave checks a nil accessor exports no
// series. The two process shapes know different things, and an ingestor
// reporting zero open account databases would look like an outage rather than
// like a process that has none by design.
func TestSamplerSkipsWhatAProcessDoesNotHave(t *testing.T) {
	values := gather(t, newSampler(Sources{Sites: func() int { return 1 }}))

	if _, exists := values["feasible_ingest_buffer_events"]; exists {
		t.Error("a process with no buffer still reported a buffer depth")
	}
	if _, exists := values["feasible_sites_routed"]; !exists {
		t.Error("the one accessor that was supplied was not reported")
	}
}

// TestSamplerReportsTheJobQueue checks the two states are reported separately.
// A queue that is filling up and one that is stuck on a single job look
// identical in a total.
func TestSamplerReportsTheJobQueue(t *testing.T) {
	values := gather(t, newSampler(Sources{
		Jobs: func(context.Context) (JobCounts, error) {
			return JobCounts{Available: 12, Executing: 1}, nil
		},
	}))

	if got := values["feasible_jobs/available"]; got != 12 {
		t.Errorf("available = %v, want 12", got)
	}
	if got := values["feasible_jobs/executing"]; got != 1 {
		t.Errorf("executing = %v, want 1", got)
	}
}

// TestSamplerSkipsAQueueItCannotRead checks a failed read exports nothing. Zero
// available jobs is what a healthy queue looks like, and reporting it for a
// database we could not read would be an all-clear we have not earned.
func TestSamplerSkipsAQueueItCannotRead(t *testing.T) {
	values := gather(t, newSampler(Sources{
		Jobs: func(context.Context) (JobCounts, error) {
			return JobCounts{}, errors.New("database is locked")
		},
	}))

	if _, exists := values["feasible_jobs/available"]; exists {
		t.Error("a queue that could not be read still reported a depth")
	}
}

// TestSamplerSizesTheDatabases checks the storage gauges, including that the
// per-account sizes are summed rather than labelled. An account id is a
// customer, and naming customers is not what this endpoint is for.
func TestSamplerSizesTheDatabases(t *testing.T) {
	dir := t.TempDir()

	write(t, filepath.Join(dir, "control.db"), 100)
	write(t, filepath.Join(dir, "control.db-wal"), 10)

	for _, account := range []int64{1, 2} {
		path := accounts.Path(dir, account)

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		write(t, path, 1000)
		write(t, path+"-wal", int(account)*50)
	}

	values := gather(t, newSampler(Sources{DataDir: dir}))

	if got := values["feasible_database_bytes/control"]; got != 100 {
		t.Errorf("control size = %v, want 100", got)
	}
	if got := values["feasible_database_bytes/accounts"]; got != 2000 {
		t.Errorf("account size = %v, want 2000", got)
	}
	if got := values["feasible_database_wal_bytes/accounts"]; got != 150 {
		t.Errorf("account WAL = %v, want 150", got)
	}
	if got := values["feasible_database_wal_bytes_max"]; got != 100 {
		t.Errorf("largest WAL = %v, want 100 — one stuck account is invisible in a sum", got)
	}
	if got := values["feasible_database_files"]; got != 2 {
		t.Errorf("database count = %v, want 2", got)
	}
	if got := values["feasible_database_directory_readable"]; got != 1 {
		t.Errorf("readable = %v, want 1", got)
	}
}

// TestSamplerReportsFreeSpace covers the one number here that predicts a
// failure rather than describing one: every size gauge looks healthy right up
// to the moment a database cannot grow.
func TestSamplerReportsFreeSpace(t *testing.T) {
	dir := t.TempDir()

	if _, _, ok := diskSpace(dir); !ok {
		t.Skip("this platform does not report free space")
	}

	values := gather(t, newSampler(Sources{DataDir: dir}))

	total, available := values["feasible_disk_total_bytes"], values["feasible_disk_available_bytes"]

	if total <= 0 {
		t.Fatalf("total = %v, want a real filesystem size", total)
	}
	if available <= 0 || available > total {
		t.Fatalf("available = %v with a total of %v", available, total)
	}
}

// write makes a file of an exact size.
func write(t *testing.T, path string, size int) {
	t.Helper()

	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
}
