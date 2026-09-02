//
// store_test.go
// Slugs, passwords and the rule that a protected link is never embeddable.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package sharing

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// fixture is a system database with one team and one site.
type fixture struct {
	db     *sql.DB
	store  *Store
	now    time.Time
	siteID int64
	domain string

	// teamID owns the site. Every mutation is fenced on it, so a test that
	// does not carry it cannot write anything.
	teamID int64
}

// TestPasswordAttemptsAreBoundedPerSourceAndLink checks the durable window,
// source/link isolation and atomic concurrency cap around PBKDF2 work.
func TestPasswordAttemptsAreBoundedPerSourceAndLink(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	first, err := f.store.CreateLinkForOwner(ctx, f.siteID, f.teamID, "first", "correct", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.store.CreateLinkForOwner(ctx, f.siteID, f.teamID, "second", "correct", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < PasswordAttemptLimit; attempt++ {
		if err := f.store.CheckPasswordForSource(ctx, first.ID, "source-a", "wrong"); !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("attempt %d = %v, want wrong password", attempt, err)
		}
	}
	if err := f.store.CheckPasswordForSource(ctx, first.ID, "source-a", "wrong"); !errors.Is(err, ErrPasswordThrottled) {
		t.Fatalf("attempt beyond cap = %v, want throttled", err)
	}
	if err := f.store.CheckPasswordForSource(ctx, first.ID, "source-b", "wrong"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("another source was globally blocked: %v", err)
	}
	if err := f.store.CheckPasswordForSource(ctx, second.ID, "source-a", "wrong"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("another link was globally blocked: %v", err)
	}

	f.now = f.now.Add(PasswordAttemptWindow + time.Second)
	if err := f.store.CheckPasswordForSource(ctx, first.ID, "source-a", "wrong"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("expired window did not reset: %v", err)
	}

	const workers = PasswordAttemptLimit * 3
	start := make(chan struct{})
	results := make(chan error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results <- f.store.CheckPasswordForSource(ctx, first.ID, "source-concurrent", "wrong")
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	wrong, throttled := 0, 0
	for err := range results {
		switch {
		case errors.Is(err, ErrWrongPassword):
			wrong++
		case errors.Is(err, ErrPasswordThrottled):
			throttled++
		default:
			t.Fatalf("concurrent attempt returned %v", err)
		}
	}
	if wrong != PasswordAttemptLimit || throttled != workers-PasswordAttemptLimit {
		t.Fatalf("concurrent wrong/throttled = %d/%d, want %d/%d", wrong, throttled,
			PasswordAttemptLimit, workers-PasswordAttemptLimit)
	}
}

// TestOneLinkHasACeilingAcrossEverySource checks the backstop a per-source
// budget cannot be: an attacker with a thousand addresses gets a thousand
// budgets, so the link needs a ceiling of its own that every source shares.
//
// The row is seeded with the unsalted digest rather than a PBKDF2 hash because
// the budget is spent before the derivation either way, and sixty guesses at
// the shipped iteration count would cost seconds for nothing.
func TestOneLinkHasACeilingAcrossEverySource(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	inserted, err := f.db.ExecContext(ctx, `
		INSERT INTO shared_links (site_id, name, slug, password_hash, password_salt, created_at)
		VALUES (?, 'ceiling', 'ceiling-link', ?, '', ?)
	`, f.siteID, legacyPasswordHash("correct"), f.now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	linkID, err := inserted.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	for spent, source := 0, 0; spent < PasswordLinkAttemptLimit; source++ {
		key := "source-" + strconv.Itoa(source)

		for attempt := 0; attempt < PasswordAttemptLimit && spent < PasswordLinkAttemptLimit; attempt++ {
			if err := f.store.CheckPasswordForSource(ctx, linkID, key, "wrong"); !errors.Is(err, ErrWrongPassword) {
				t.Fatalf("%s attempt %d = %v, want wrong password", key, attempt, err)
			}

			spent++
		}
	}

	if err := f.store.CheckPasswordForSource(ctx, linkID, "unspent-source", "wrong"); !errors.Is(err, ErrPasswordThrottled) {
		t.Fatalf("a source with its whole budget left = %v, want throttled", err)
	}

	// The ceiling counts every guess rather than only the wrong ones, so
	// holding the password is not a way to reopen it.
	if err := f.store.CheckPasswordForSource(ctx, linkID, "unspent-source", "correct"); !errors.Is(err, ErrPasswordThrottled) {
		t.Fatalf("the right password reopened a spent link ceiling: %v", err)
	}
}

// newFixture builds and seeds the database.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatalf("open control: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	f := &fixture{db: db, now: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC), domain: "acme.example"}
	f.store = NewStore(db)
	f.store.Now = func() time.Time { return f.now }

	team, err := db.Exec(`INSERT INTO teams (name, created_at, updated_at) VALUES ('Acme', ?, ?)`,
		f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatalf("insert team: %v", err)
	}

	f.teamID, _ = team.LastInsertId()

	site, err := db.Exec(`INSERT INTO sites (account_id, domain, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		f.teamID, f.domain, f.now.Unix(), f.now.Unix())
	if err != nil {
		t.Fatalf("insert site: %v", err)
	}

	f.siteID, _ = site.LastInsertId()

	return f
}

// TestAPasswordProtectedLinkIsNeverEmbeddable is the refusal this package
// exists to make.
//
// Making the password form work inside a third-party frame means serving it
// without X-Frame-Options, and a login form that any site may frame is one an
// attacker can place invisibly under a button on their own page. There is no
// configuration that makes it safe, so there is no configuration.
func TestAPasswordProtectedLinkIsNeverEmbeddable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	protected, err := f.store.CreateLinkForOwner(ctx, f.siteID, f.teamID, "client", "hunter2", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if protected.Embeddable() {
		t.Fatal("a password-protected link reports itself as embeddable")
	}

	open, err := f.store.CreateLinkForOwner(ctx, f.siteID, f.teamID, "public-ish", "", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if !open.Embeddable() {
		t.Fatal("a link with no password is not embeddable")
	}
}

// TestAPasswordIsCheckedAndNotStoredInTheClear checks the whole password path.
func TestAPasswordIsCheckedAndNotStoredInTheClear(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	link, err := f.store.CreateLinkForOwner(ctx, f.siteID, f.teamID, "client", "correct horse", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := f.store.CheckPasswordForSource(ctx, link.ID, "source-a", "correct horse"); err != nil {
		t.Fatalf("the right password was refused: %v", err)
	}

	if err := f.store.CheckPasswordForSource(ctx, link.ID, "source-a", "wrong"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("the wrong password was accepted: %v", err)
	}

	var hash, salt string
	if err := f.db.QueryRow(`SELECT password_hash, password_salt FROM shared_links WHERE id = ?`, link.ID).
		Scan(&hash, &salt); err != nil {
		t.Fatalf("read row: %v", err)
	}

	if hash == "correct horse" || hash == "" {
		t.Fatalf("the stored hash is %q", hash)
	}

	if salt == "" {
		t.Fatal("no salt was stored, so two links with the same password share a hash")
	}
}

// TestLegacyPasswordRowsUpgradeOnlyAfterSuccessfulVerification preserves links
// whose row still holds an unsalted digest. A wrong password leaves the
// empty-salt row untouched; the first correct password replaces it with
// PBKDF2, and later failures can never move that row back to the legacy
// representation.
func TestLegacyPasswordRowsUpgradeOnlyAfterSuccessfulVerification(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	const password = "legacy-secret"
	inserted, err := f.db.ExecContext(ctx, `
		INSERT INTO shared_links (site_id, name, slug, password_hash, password_salt, created_at)
		VALUES (?, 'legacy', 'legacy-link', ?, '', ?)
	`, f.siteID, legacyPasswordHash(password), f.now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	linkID, err := inserted.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	if err := f.store.CheckPasswordForSource(ctx, linkID, "source-a", "wrong"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong legacy password = %v, want ErrWrongPassword", err)
	}
	var beforeHash, beforeSalt string
	if err := f.db.QueryRowContext(ctx, `
		SELECT password_hash, password_salt FROM shared_links WHERE slug = 'legacy-link'
	`).Scan(&beforeHash, &beforeSalt); err != nil {
		t.Fatal(err)
	}
	if beforeHash != legacyPasswordHash(password) || beforeSalt != "" {
		t.Fatalf("failed check changed legacy row to hash/salt %q/%q", beforeHash, beforeSalt)
	}

	if err := f.store.CheckPasswordForSource(ctx, linkID, "source-a", password); err != nil {
		t.Fatalf("correct legacy password failed: %v", err)
	}
	var upgradedHash, upgradedSalt string
	if err := f.db.QueryRowContext(ctx, `
		SELECT password_hash, password_salt FROM shared_links WHERE slug = 'legacy-link'
	`).Scan(&upgradedHash, &upgradedSalt); err != nil {
		t.Fatal(err)
	}
	if upgradedSalt == "" || upgradedHash == beforeHash {
		t.Fatalf("legacy row was not upgraded: hash/salt %q/%q", upgradedHash, upgradedSalt)
	}
	if err := f.store.CheckPasswordForSource(ctx, linkID, "source-a", "wrong"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong upgraded password = %v, want ErrWrongPassword", err)
	}
	var finalSalt string
	if err := f.db.QueryRowContext(ctx, `SELECT password_salt FROM shared_links WHERE slug = 'legacy-link'`).Scan(&finalSalt); err != nil {
		t.Fatal(err)
	}
	if finalSalt != upgradedSalt {
		t.Fatalf("salt changed after failed verification: %q to %q", upgradedSalt, finalSalt)
	}
}

// TestTwoLinksWithTheSamePasswordHaveDifferentHashes checks that the salt is
// per link, which is what stops a stolen database revealing that two customers
// chose the same password.
func TestTwoLinksWithTheSamePasswordHaveDifferentHashes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first, err := f.store.CreateLinkForOwner(ctx, f.siteID, f.teamID, "a", "same", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	second, err := f.store.CreateLinkForOwner(ctx, f.siteID, f.teamID, "b", "same", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if hashOf(t, f, first.ID) == hashOf(t, f, second.ID) {
		t.Fatal("two links with the same password produced the same hash")
	}
}

// hashOf reads a link's stored hash.
func hashOf(t *testing.T, f *fixture, id int64) string {
	t.Helper()

	var hash string
	if err := f.db.QueryRow(`SELECT password_hash FROM shared_links WHERE id = ?`, id).Scan(&hash); err != nil {
		t.Fatalf("read hash: %v", err)
	}

	return hash
}

// TestDeriveKeyMatchesAnIndependentImplementation pins the exact bytes every
// stored hash was derived with, against a value produced outside this codebase.
//
// It is a known-answer test rather than a round trip because the salt is fed to
// PBKDF2 as its hex text rather than as decoded bytes. Anybody who "fixes" that
// gets a derivation that is still perfectly correct PBKDF2 and still passes a
// round-trip test, while silently locking every existing customer out of their
// own shared link. This is the check that catches it.
func TestDeriveKeyMatchesAnIndependentImplementation(t *testing.T) {
	// PBKDF2-HMAC-SHA256("hunter2", "abcdef0123456789", 200000, 32), computed
	// with OpenSSL through Python's hashlib.pbkdf2_hmac.
	const want = "cc84cb66506106e84515e9b9d8e2c70ea1df5f50c1ae3e0b015209a1f2d3706a"

	got, err := deriveKey("hunter2", "abcdef0123456789")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}

	if got != want {
		t.Fatalf("deriveKey produced\n%s\nwant\n%s", got, want)
	}
}

// TestDeriveKeyIsDeterministic checks that the shipped parameters produce the
// same answer twice, which is what makes a stored hash checkable at all.
func TestDeriveKeyIsDeterministic(t *testing.T) {
	first, err := deriveKey("hunter2", "abcdef0123456789")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}

	second, err := deriveKey("hunter2", "abcdef0123456789")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}

	if first != second {
		t.Fatal("the derivation is not deterministic")
	}

	other, err := deriveKey("hunter2", "0123456789abcdef")
	if err != nil {
		t.Fatalf("derive: %v", err)
	}

	if first == other {
		t.Fatal("the salt does not change the derived key")
	}
}

// TestALinkWithNoPasswordAcceptsAnything checks that the password check is a
// no-op for an open link rather than refusing every request.
func TestALinkWithNoPasswordAcceptsAnything(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	link, err := f.store.CreateLinkForOwner(ctx, f.siteID, f.teamID, "open", "", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := f.store.CheckPasswordForSource(ctx, link.ID, "source-a", ""); err != nil {
		t.Fatalf("an open link refused an empty password: %v", err)
	}
}

// TestSlugsAreUnguessable checks the token's shape and that two links never
// collide. The URL is the whole credential for whoever holds it.
func TestSlugsAreUnguessable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	seen := map[string]bool{}

	for i := 0; i < 200; i++ {
		link, err := f.store.CreateLinkForOwner(ctx, f.siteID, f.teamID, "", "", 0, 0)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}

		if len(link.Slug) < 20 {
			t.Fatalf("the slug %q is only %d characters", link.Slug, len(link.Slug))
		}

		if seen[link.Slug] {
			t.Fatalf("the slug %q was issued twice", link.Slug)
		}

		seen[link.Slug] = true
	}
}

// TestRevokingALinkRemovesItEntirely checks that there is no soft delete. A
// link is a live credential, and "revoked but still in the table" is one query
// bug away from still working.
func TestRevokingALinkRemovesItEntirely(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	link, err := f.store.CreateLinkForOwner(ctx, f.siteID, f.teamID, "temporary", "", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := f.store.RevokeLinkForOwner(ctx, f.siteID, f.teamID, link.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := f.store.Resolve(ctx, link.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked link still resolves: %v", err)
	}

	if err := f.store.RevokeLinkForOwner(ctx, f.siteID, f.teamID, link.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking twice = %v, want ErrNotFound", err)
	}
}

// TestAPublicSiteResolvesOnlyWhenItIsPublic checks that a private site answers
// the same as one that does not exist, so the endpoint cannot enumerate domains.
func TestAPublicSiteResolvesOnlyWhenItIsPublic(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.store.PublicSite(ctx, f.domain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a private site resolved: %v", err)
	}

	if err := f.store.SetPublicForOwner(ctx, f.siteID, f.teamID, true); err != nil {
		t.Fatalf("set public: %v", err)
	}

	link, err := f.store.PublicSite(ctx, f.domain)
	if err != nil {
		t.Fatalf("a public site did not resolve: %v", err)
	}

	if link.Domain != f.domain {
		t.Fatalf("resolved to %q", link.Domain)
	}

	if err := f.store.SetPublicForOwner(ctx, f.siteID, f.teamID, false); err != nil {
		t.Fatalf("set private: %v", err)
	}

	if _, err := f.store.PublicSite(ctx, f.domain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a site made private again still resolved: %v", err)
	}
}

// TestALinkPathKeepsTheSharePrefix pins the string every front-end URL has to
// keep. Dropping it is what produced an incumbent's redirect loop.
func TestALinkPathKeepsTheSharePrefix(t *testing.T) {
	link := Link{Slug: "abc123"}

	if got := link.Path(); got != "/share/abc123" {
		t.Fatalf("Path() = %q, want /share/abc123", got)
	}
}
