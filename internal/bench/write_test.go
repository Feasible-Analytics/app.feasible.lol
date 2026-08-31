//
// write_test.go
// The write benchmark: events per second, and where it stops scaling.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package bench

import (
	"context"
	"fmt"
	"testing"
)

// writeEvents is how many events one load run sends. It is large enough that
// the buffer fills hundreds of times and small enough that the whole benchmark
// finishes inside a coffee break.
const writeEvents = 50_000

// accountCounts is the axis the run sweeps. One account is the self-hoster; the
// rest are what a shard looks like as it fills up, and the point of the sweep is
// to find where the rate stops being flat. Every write is a separate database
// file, a separate write lock and a separate WAL, which is the whole reason the
// number cannot be assumed.
var accountCounts = []int{1, 4, 16, 64}

// BenchmarkWrite measures sustained events per second through the accept path.
//
// It reports a rate and both latency distributions rather than the ns/op the
// framework prints, because the question is not how long an average event takes
// — it is whether the endpoint stays fast while the writes behind it get slower,
// which is exactly what an average hides.
func BenchmarkWrite(b *testing.B) {
	for _, count := range accountCounts {
		b.Run(fmt.Sprintf("accounts-%d", count), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				result, err := RunWrite(context.Background(), WriteOptions{
					DataDir:  b.TempDir(),
					Accounts: count,
					Events:   writeEvents,
					Visitors: 5_000,
				})
				if err != nil {
					b.Fatal(err)
				}

				if result.Dropped != 0 {
					b.Fatalf("%d events were dropped — the load is not measuring what it thinks", result.Dropped)
				}

				b.ReportMetric(result.EventsPerSecond, "events/s")
				b.ReportMetric(float64(result.Accept.P50.Microseconds()), "accept-p50-µs")
				b.ReportMetric(float64(result.Accept.P99.Microseconds()), "accept-p99-µs")
				b.ReportMetric(float64(result.Flush.P50.Milliseconds()), "flush-p50-ms")
				b.ReportMetric(float64(result.Flush.P99.Milliseconds()), "flush-p99-ms")
			}
		})
	}
}

// TestWriteLoadIsHonest runs a tiny load and checks the run itself is sound
// before anybody reads a number off it: every event accepted, every event
// written, nothing dropped, and more than one flush.
//
// It runs in the normal suite because a load generator that has quietly stopped
// generating load is worse than no benchmark at all — it reports a very good
// number.
func TestWriteLoadIsHonest(t *testing.T) {
	result, err := RunWrite(context.Background(), WriteOptions{
		DataDir:  t.TempDir(),
		Accounts: 2,
		Events:   1_200,
		Visitors: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	if result.Dropped != 0 {
		t.Errorf("%d events were dropped", result.Dropped)
	}

	if result.Accept.Count != result.Events {
		t.Errorf("timed %d accepts of %d events", result.Accept.Count, result.Events)
	}

	if result.Written != int64(result.Events) {
		t.Errorf("%d of %d events reached a database", result.Written, result.Events)
	}

	// More than one batch, because a load written by a single flush at the end
	// is not measuring the batching path at all.
	if result.Batches < 2 {
		t.Errorf("the load produced %d flushes, which is too few to have exercised batching", result.Batches)
	}

	if result.EventsPerSecond <= 0 {
		t.Errorf("the run reported %v events a second", result.EventsPerSecond)
	}
}
