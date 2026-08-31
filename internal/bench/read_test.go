//
// read_test.go
// The read benchmark: the same reports from raw rows and from summaries.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package bench

import (
	"context"
	"flag"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/seed"
)

// The dataset to read. The default is small enough to seed inside a benchmark
// run; the numbers that go in RESULTS.md come from pointing -bench.data-dir at
// a directory that `make seed-big` has already filled, because a claim about a
// million rows has to be measured on a million rows.
var (
	dataDir   = flag.String("bench.data-dir", "", "read benchmarks: an already-seeded data directory to measure instead of seeding one")
	pageviews = flag.Int64("bench.pageviews", 120_000, "read benchmarks: pageviews to seed when there is no -bench.data-dir")
	days      = flag.Int("bench.days", 400, "read benchmarks: days of history to seed, which has to cover the longest range being measured")
)

// seeded is the dataset, built once per process. Seeding is minutes of work at
// the sizes worth measuring, and doing it per benchmark iteration would time the
// generator instead of the queries.
var (
	seedOnce   sync.Once
	seededSet  Dataset
	seededErr  error
	seededTemp string
)

// TestMain removes a dataset this process seeded for itself. One that was
// pointed at with -bench.data-dir is somebody's, and is left alone.
func TestMain(m *testing.M) {
	code := m.Run()

	if seededTemp != "" {
		os.RemoveAll(seededTemp)
	}

	os.Exit(code)
}

// dataset returns the site to read, seeding one on first use.
func dataset(tb testing.TB) Dataset {
	tb.Helper()

	seedOnce.Do(func() {
		dir := *dataDir

		if dir == "" {
			seededTemp, seededErr = os.MkdirTemp("", "feasible-bench-")
			if seededErr != nil {
				return
			}

			dir = seededTemp

			// One site, so the whole of the generated history lands in one
			// database — which is the shape the estimates were written about.
			if _, seededErr = seed.Run(context.Background(), seed.Options{
				DataDir:   dir,
				Pageviews: *pageviews,
				Days:      *days,
				Sites:     1,
				Seed:      seed.DefaultSeed,
			}); seededErr != nil {
				return
			}
		}

		seededSet, seededErr = OpenDataset(context.Background(), dir)
	})

	if seededErr != nil {
		tb.Fatal(seededErr)
	}

	return seededSet
}

// BenchmarkRead times every report in the set against one seeded site, from raw
// rows and from the summary tables.
//
// The pair is the point. "Roll-ups make reports faster" is an architectural
// claim the whole storage design rests on, and until both halves are timed
// against the same data it is only a claim.
func BenchmarkRead(b *testing.B) {
	set := dataset(b)

	manager := accounts.NewManager(set.DataDir)
	b.Cleanup(func() { _ = manager.CloseAll() })

	account, err := manager.Open(context.Background(), set.AccountID)
	if err != nil {
		b.Fatal(err)
	}

	for _, c := range ReadCases(set) {
		b.Run(c.Name, func(b *testing.B) {
			var rows int

			for i := 0; i < b.N; i++ {
				var took time.Duration

				took, rows, err = RunRead(context.Background(), account.Reader(), c)
				if err != nil {
					b.Fatal(err)
				}

				b.ReportMetric(float64(took.Microseconds())/1000, "ms/report")
			}

			// A report that answered nothing is not a report that was fast,
			// and at a small seed size a 12-month range can genuinely be
			// empty — so this is reported rather than asserted.
			b.ReportMetric(float64(rows), "groups")
		})
	}
}
