//
// intern_test.go
// Tests and benchmark for the dimension-string cache.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package intern

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newAccountDB builds a migrated account database. The cache is only meaningful
// against the real dimension tables, so these tests run the actual schema
// rather than a hand-written stand-in that could drift from it.
func newAccountDB(t testing.TB) *sql.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.Account()); err != nil {
		t.Fatal(err)
	}

	return db
}

// newWarmCache is the state the ingest path always runs in: a cache that was
// warmed when the account database was opened.
func newWarmCache(t testing.TB) *Cache {
	t.Helper()

	cache := New(newAccountDB(t))
	if err := cache.Warm(context.Background()); err != nil {
		t.Fatal(err)
	}

	return cache
}

// TestIDIsStableAcrossCalls is the property everything else rests on. An id
// that changed between calls would split one page into several rows in every
// report, and the damage would be permanent.
func TestIDIsStableAcrossCalls(t *testing.T) {
	ctx := context.Background()
	cache := newWarmCache(t)

	first, err := cache.ID(ctx, Pathname, "/pricing")
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 {
		t.Fatal("a real value was given id 0, which belongs to the empty string")
	}

	for i := 0; i < 3; i++ {
		again, err := cache.ID(ctx, Pathname, "/pricing")
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("call %d returned %d, want %d", i, again, first)
		}
	}

	// A different value must not collide with it, and a different dimension is
	// a different namespace entirely.
	other, err := cache.ID(ctx, Pathname, "/blog")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("two paths share an id")
	}
}

// TestIDSurvivesARestart checks the ids are the database's, not the map's. A
// process restart that renumbered anything would rewrite history.
func TestIDSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	db := newAccountDB(t)

	first := New(db)
	if err := first.Warm(ctx); err != nil {
		t.Fatal(err)
	}

	want, err := first.ID(ctx, Source, "Hacker News")
	if err != nil {
		t.Fatal(err)
	}

	second := New(db)
	if err := second.Warm(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := second.ID(ctx, Source, "Hacker News")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("after a restart the id is %d, want %d", got, want)
	}
}

// TestEmptyStringIsAlwaysZero pins the rule that removes NULL handling from the
// whole schema. Most events have no campaign, no region and no referrer, so
// this is also the most common call the ingest path makes.
func TestEmptyStringIsAlwaysZero(t *testing.T) {
	ctx := context.Background()
	cache := newWarmCache(t)

	for _, dimension := range Dimensions {
		id, err := cache.ID(ctx, dimension, "")
		if err != nil {
			t.Fatal(err)
		}
		if id != EmptyID {
			t.Fatalf("%s gave the empty string id %d", dimension, id)
		}
	}
}

// TestEveryDimensionHasATable is the check that keeps this package and the
// migration honest with each other. A dimension declared here but missing from
// the schema would fail at the first event of that kind, in production.
func TestEveryDimensionHasATable(t *testing.T) {
	ctx := context.Background()
	cache := newWarmCache(t)

	for _, dimension := range Dimensions {
		if _, err := cache.ID(ctx, dimension, "value-"+string(dimension)); err != nil {
			t.Errorf("%s: %v", dimension, err)
		}
	}
}

// TestUnknownDimensionIsRefused makes a typo an immediate error rather than a
// table name interpolated into SQL.
func TestUnknownDimensionIsRefused(t *testing.T) {
	cache := newWarmCache(t)

	if _, err := cache.ID(context.Background(), Dimension("nonsense"), "x"); err == nil {
		t.Fatal("an unknown dimension was accepted")
	}
}

// TestWarmPicksUpExistingRows covers the start-up path: a database that already
// holds a month of traffic must not re-insert every value it already has.
func TestWarmPicksUpExistingRows(t *testing.T) {
	ctx := context.Background()
	db := newAccountDB(t)

	if _, err := db.ExecContext(ctx, "INSERT INTO dim_country (value) VALUES ('US'), ('GB')"); err != nil {
		t.Fatal(err)
	}

	cache := New(db)
	if err := cache.Warm(ctx); err != nil {
		t.Fatal(err)
	}

	// Two seeded rows plus the empty string that ships with the schema.
	if got := cache.Size(Country); got != 3 {
		t.Fatalf("warmed %d values, want 3", got)
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dim_country").Scan(&count); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.ID(ctx, Country, "US"); err != nil {
		t.Fatal(err)
	}

	var after int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dim_country").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != count {
		t.Fatalf("a warmed value was inserted again: %d rows became %d", count, after)
	}
}

// TestConcurrentIDsAgree is the race this cache exists inside. Ingest is
// concurrent, and two goroutines meeting on a value nobody has sent before must
// come away with the same id rather than one id and one constraint error.
func TestConcurrentIDsAgree(t *testing.T) {
	ctx := context.Background()
	cache := newWarmCache(t)

	const goroutines = 16

	var (
		wait sync.WaitGroup
		mu   sync.Mutex
		ids  = map[int64]int{}
	)

	for i := 0; i < goroutines; i++ {
		wait.Add(1)

		go func() {
			defer wait.Done()

			id, err := cache.ID(ctx, Pathname, "/racy")
			if err != nil {
				t.Error(err)
				return
			}

			mu.Lock()
			ids[id]++
			mu.Unlock()
		}()
	}

	wait.Wait()

	if len(ids) != 1 {
		t.Fatalf("goroutines disagreed on the id: %v", ids)
	}
}

// BenchmarkID measures the hot path. Interning happens roughly twenty times per
// event, so this has to be a map read: if it ever shows a database round trip,
// ingest throughput is gone.
func BenchmarkID(b *testing.B) {
	ctx := context.Background()
	cache := newWarmCache(b)

	// A realistic working set: a site's popular paths, all already interned.
	paths := make([]string, 64)
	for i := range paths {
		paths[i] = fmt.Sprintf("/blog/post-%d", i)

		if _, err := cache.ID(ctx, Pathname, paths[i]); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := cache.ID(ctx, Pathname, paths[i%len(paths)]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIDParallel is the same lookup under the concurrency ingest actually
// runs at, which is what shows whether the shared lock is the bottleneck.
func BenchmarkIDParallel(b *testing.B) {
	ctx := context.Background()
	cache := newWarmCache(b)

	if _, err := cache.ID(ctx, Pathname, "/pricing"); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := cache.ID(ctx, Pathname, "/pricing"); err != nil {
				b.Fatal(err)
			}
		}
	})
}
