//
// pathclean_test.go
// Ordered rules, the preview that comes before a save, and the map it builds.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package pathclean

import (
	"context"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// newAccount opens a migrated account database in a temporary directory.
func newAccount(t *testing.T) *accounts.Account {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	return account
}

// TestFirstMatchWins is the ordering rule. A specific rule above a general one
// has to keep its meaning, or writing a catch-all makes every rule below it
// unreachable with nothing saying so.
func TestFirstMatchWins(t *testing.T) {
	set, err := Compile([]Rule{
		{Position: 0, Pattern: `^/users/me$`, Replacement: "/users/me", Enabled: true},
		{Position: 1, Pattern: `^/users/[^/]+$`, Replacement: "/users/:id", Enabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := set.Clean("/users/me"); got != "/users/me" {
		t.Errorf("the specific rule was overtaken: got %q", got)
	}

	if got := set.Clean("/users/3f2a"); got != "/users/:id" {
		t.Errorf("/users/3f2a cleaned to %q, want /users/:id", got)
	}

	if got := set.Clean("/pricing"); got != "/pricing" {
		t.Errorf("an unmatched path was rewritten to %q", got)
	}
}

// TestDisabledRulesAreSkipped covers the trailing-slash switch, which ships off
// because a few sites genuinely serve different content at each spelling.
func TestDisabledRulesAreSkipped(t *testing.T) {
	off, err := Compile([]Rule{{Pattern: TrailingSlashPattern, Replacement: TrailingSlashReplacement}})
	if err != nil {
		t.Fatal(err)
	}

	if got := off.Clean("/about/"); got != "/about/" {
		t.Errorf("a disabled rule ran anyway: %q", got)
	}

	on, err := Compile([]Rule{{Pattern: TrailingSlashPattern, Replacement: TrailingSlashReplacement, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}

	if got := on.Clean("/about/"); got != "/about" {
		t.Errorf("/about/ cleaned to %q, want /about", got)
	}

	// The root must survive: rewriting "/" to "" would give every site a page
	// with no name at the top of its most-visited list.
	if got := on.Clean("/"); got != "/" {
		t.Errorf("the root cleaned to %q, want /", got)
	}
}

// TestPreviewShowsWhatWouldMerge covers the screen a customer sees before
// saving. A regular expression that eats half a site's URLs looks identical to
// a correct one until the list of merges is in front of you.
func TestPreviewShowsWhatWouldMerge(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)

	seedPages(t, account, map[string]int{
		"/users/aaaa": 5,
		"/users/bbbb": 3,
		"/pricing":    9,
	})

	merges, err := Preview(ctx, account.Reader(), 1, []Rule{
		{Pattern: `^/users/[^/]+$`, Replacement: "/users/:id", Enabled: true},
	}, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(merges) != 1 {
		t.Fatalf("%d merges previewed, want 1", len(merges))
	}

	if merges[0].Target != "/users/:id" {
		t.Fatalf("merge target = %q, want /users/:id", merges[0].Target)
	}

	if merges[0].Rows != 8 {
		t.Fatalf("merge covers %d rows, want 8 — the preview has to say how much data moves", merges[0].Rows)
	}

	if len(merges[0].Sources) != 2 {
		t.Fatalf("%d source paths listed, want the 2 that merge", len(merges[0].Sources))
	}
}

// TestMaterialiseIsReversible is what makes a rule safe to try. Nothing stored
// is rewritten: the map turns one interned id into another, so removing every
// rule empties the map and the original paths report as themselves again.
func TestMaterialiseIsReversible(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	now := time.Unix(1_800_000_000, 0)

	seedPages(t, account, map[string]int{"/users/aaaa": 1, "/users/bbbb": 1})

	rules := []Rule{{Pattern: `^/users/[^/]+$`, Replacement: "/users/:id", Enabled: true}}

	if err := Replace(ctx, account.Writer(), 1, rules, now); err != nil {
		t.Fatal(err)
	}

	moved, err := Materialise(ctx, account.Writer(), account.Intern, 1)
	if err != nil {
		t.Fatal(err)
	}

	if moved != 2 {
		t.Fatalf("%d paths mapped, want 2", moved)
	}

	if got := countMap(t, account); got != 2 {
		t.Fatalf("the map holds %d rows, want 2", got)
	}

	// The original paths are still in the dimension table, which is the whole
	// reason a rule can be taken back.
	if id, _ := account.Intern.ID(ctx, intern.Pathname, "/users/aaaa"); id == 0 {
		t.Fatal("the original path was destroyed by materialising the rules")
	}

	if err := Replace(ctx, account.Writer(), 1, nil, now); err != nil {
		t.Fatal(err)
	}

	if _, err := Materialise(ctx, account.Writer(), account.Intern, 1); err != nil {
		t.Fatal(err)
	}

	if got := countMap(t, account); got != 0 {
		t.Fatalf("removing every rule left %d mappings behind", got)
	}
}

// TestValidateRefusesABadPattern keeps an unparseable regular expression out of
// the store, so the error lands next to the field rather than inside a
// background job an hour later.
func TestValidateRefusesABadPattern(t *testing.T) {
	if err := Validate(Rule{Pattern: "([unclosed"}); err == nil {
		t.Error("an invalid regular expression was accepted")
	}

	if err := Validate(Rule{Pattern: "   "}); err == nil {
		t.Error("an empty pattern was accepted")
	}

	if err := Validate(Rule{Pattern: `^/users/[^/]+$`}); err != nil {
		t.Errorf("a valid pattern was refused: %v", err)
	}
}

// TestReplaceHoldsTheCap bounds a site's rule list, because every rule is run
// against every distinct path when the map is rebuilt.
func TestReplaceHoldsTheCap(t *testing.T) {
	account := newAccount(t)

	rules := make([]Rule, MaxRules+1)
	for i := range rules {
		rules[i] = Rule{Position: i, Pattern: "^/a$", Replacement: "/a", Enabled: true}
	}

	if err := Replace(context.Background(), account.Writer(), 1, rules, time.Unix(0, 0)); err == nil {
		t.Fatalf("%d rules were accepted, over the cap of %d", len(rules), MaxRules)
	}
}

// seedPages writes one event per pageview so the preview has rows to count.
func seedPages(t *testing.T, account *accounts.Account, pages map[string]int) {
	t.Helper()

	ctx := context.Background()

	name, err := account.Intern.ID(ctx, intern.EventName, "pageview")
	if err != nil {
		t.Fatal(err)
	}

	for path, count := range pages {
		id, err := account.Intern.ID(ctx, intern.Pathname, path)
		if err != nil {
			t.Fatal(err)
		}

		for i := 0; i < count; i++ {
			_, err := account.Writer().ExecContext(ctx, `
				INSERT INTO events (site_id, timestamp, name_id, user_id, session_id, pathname_id, scroll_depth)
				VALUES (1, 1000, ?, 1, 1, ?, 255)`, name, id)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

// countMap reads how many mappings a site has.
func countMap(t *testing.T, account *accounts.Account) int {
	t.Helper()

	var count int
	if err := account.Reader().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM path_clean_map WHERE site_id = 1").Scan(&count); err != nil {
		t.Fatal(err)
	}

	return count
}
