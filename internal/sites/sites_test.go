//
// sites_test.go
// Tests for the routing map: lookups, normalisation and the whole-snapshot swap.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package sites

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newControlDB builds a migrated control database with one team.
func newControlDB(t testing.TB) *sql.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	if _, err := db.Exec("INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Team', ?, ?)", now, now); err != nil {
		t.Fatal(err)
	}

	return db
}

// addSite inserts a site row.
func addSite(t testing.TB, db *sql.DB, id int64, domain string) {
	t.Helper()

	now := time.Now().Unix()
	if _, err := db.Exec(
		"INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (?, 1, ?, ?, ?)",
		id, domain, now, now,
	); err != nil {
		t.Fatal(err)
	}
}

// TestRefreshLoadsSites checks the snapshot is built from the database, which is
// the only place routing comes from.
func TestRefreshLoadsSites(t *testing.T) {
	ctx := context.Background()
	db := newControlDB(t)

	addSite(t, db, 1, "example.com")
	addSite(t, db, 2, "another.example")

	cache := New(db)
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	if cache.Len() != 2 {
		t.Fatalf("snapshot holds %d sites, want 2", cache.Len())
	}

	site, ok := cache.Lookup("example.com")
	if !ok {
		t.Fatal("example.com is not in the map")
	}
	if site.ID != 1 || site.AccountID != 1 {
		t.Fatalf("site = %+v, want id 1 in account 1", site)
	}

	if _, ok := cache.Lookup("nobody.example"); ok {
		t.Fatal("an unregistered domain resolved")
	}
}

// TestLookupNormalisesTheDomain is the difference between a working install and
// total silent data loss. A snippet that says WWW.Example.com and a site
// registered as example.com are the same site.
func TestLookupNormalisesTheDomain(t *testing.T) {
	ctx := context.Background()
	db := newControlDB(t)

	addSite(t, db, 1, "example.com")

	cache := New(db)
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	for _, domain := range []string{"example.com", "EXAMPLE.COM", "www.example.com", "  Example.com  ", "example.com."} {
		if _, ok := cache.Lookup(domain); !ok {
			t.Errorf("%q did not resolve", domain)
		}
	}
}

// TestSiteRegisteredWithWWW checks the normalisation applies to what is stored
// as well as what is looked up, so it does not matter which form was typed into
// the dashboard.
func TestSiteRegisteredWithWWW(t *testing.T) {
	ctx := context.Background()
	db := newControlDB(t)

	addSite(t, db, 1, "www.example.com")

	cache := New(db)
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	if _, ok := cache.Lookup("example.com"); !ok {
		t.Fatal("a site registered with www. did not match the bare domain")
	}
}

// TestSetAddsWithoutARefresh covers the single-process path, where a site that
// was just created should not have to wait out a refresh interval for its first
// event.
func TestSetAddsWithoutARefresh(t *testing.T) {
	cache := New(newControlDB(t))

	cache.Set(Site{ID: 9, AccountID: 1, Domain: "fresh.example"})

	if _, ok := cache.Lookup("fresh.example"); !ok {
		t.Fatal("the site was not added")
	}
}

// TestSnapshotIsSwappedWhole checks a reader sees either the old map or the new
// one and never a half-updated one, which is what makes a lookup lock-free.
func TestSnapshotIsSwappedWhole(t *testing.T) {
	ctx := context.Background()
	db := newControlDB(t)

	addSite(t, db, 1, "example.com")

	cache := New(db)
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers hammering the map while it is rebuilt underneath them. Run under
	// -race this is the whole assertion.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, ok := cache.Lookup("example.com"); !ok {
						panic("example.com vanished mid-refresh")
					}
				}
			}
		}()
	}

	for i := 0; i < 20; i++ {
		if err := cache.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}

	close(stop)
	wg.Wait()
}

// TestBuiltAtMovesOnRefresh checks a stalled refresh is visible rather than
// merely quiet. A routing map that quietly stopped updating looks exactly like
// every new customer failing to send events.
func TestBuiltAtMovesOnRefresh(t *testing.T) {
	ctx := context.Background()
	cache := New(newControlDB(t))

	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	first := cache.BuiltAt()

	if first.IsZero() {
		t.Fatal("the snapshot has no build time")
	}

	time.Sleep(2 * time.Millisecond)

	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !cache.BuiltAt().After(first) {
		t.Fatal("the build time did not move on a refresh")
	}
}

// TestAcceptTrafficUntilIsCarried checks the grace period reaches the ingest
// path. Dropping a paying customer's traffic the instant a card fails loses
// data they can never get back.
func TestAcceptTrafficUntilIsCarried(t *testing.T) {
	ctx := context.Background()
	db := newControlDB(t)

	addSite(t, db, 1, "example.com")

	if _, err := db.Exec("UPDATE teams SET accept_traffic_until = 1234567890 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}

	cache := New(db)
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	site, ok := cache.Lookup("example.com")
	if !ok {
		t.Fatal("example.com is not in the map")
	}
	if site.AcceptTrafficUntil != 1234567890 {
		t.Fatalf("accept_traffic_until = %d, want 1234567890", site.AcceptTrafficUntil)
	}
}

// BenchmarkLookup keeps the hottest read in the system honest. It runs once per
// event and must be an atomic load and a map read, with no lock and no I/O.
func BenchmarkLookup(b *testing.B) {
	db := newControlDB(b)
	addSite(b, db, 1, "example.com")

	cache := New(db)
	if err := cache.Refresh(context.Background()); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = cache.Lookup("example.com")
	}
}
