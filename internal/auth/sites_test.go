//
// sites_test.go
// Creating sites, the dual-write window, pinning, folders and account scoping.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"testing"
	"time"
)

// TestCleanDomainAcceptsAPastedURL checks the forgiving parsing. People paste a
// URL far more often than they type a hostname, and refusing one is a pointless
// argument with a user who gave us exactly what we needed.
func TestCleanDomainAcceptsAPastedURL(t *testing.T) {
	cases := map[string]string{
		"https://example.com/blog?utm=x": "example.com",
		"http://Example.COM":             "example.com",
		"example.com:8080":               "example.com",
		"  example.com/  ":               "example.com",
		"example.com.":                   "example.com",
		"":                               "",
	}

	for input, want := range cases {
		if got := CleanDomain(input); got != want {
			t.Errorf("CleanDomain(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestValidateDomainRejectsWhatCannotResolve checks the deliberately permissive
// rule: internal hostnames and new top-level domains have to work, so only what
// could not be a hostname at all is refused.
func TestValidateDomainRejectsWhatCannotResolve(t *testing.T) {
	if err := ValidateDomain("nodot"); err == nil {
		t.Error("a hostname with no dot should be rejected")
	}

	if err := ValidateDomain("under_score.com"); err == nil {
		t.Error("an underscore is not valid in a hostname")
	}

	if err := ValidateDomain(""); err == nil {
		t.Error("an empty domain should be rejected")
	}

	for _, domain := range []string{"example.com", "analytics.internal.corp", "a.museum", "xn--80ak6aa92e.com"} {
		if err := ValidateDomain(domain); err != nil {
			t.Errorf("%q should be accepted: %v", domain, err)
		}
	}
}

// TestCreateSiteNormalisesTheDomain checks the site is registered under the
// same spelling the ingest path derives from an event. Treating "www.x.com" and
// "x.com" as two sites is total, silent data loss for whichever one is not in
// the routing map.
func TestCreateSiteNormalisesTheDomain(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, team, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	site, err := s.CreateSite(ctx, team.ID, "https://WWW.Example.com/pricing", "Marketing", "Europe/London")
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	if site.Domain != "example.com" {
		t.Errorf("want %q, got %q", "example.com", site.Domain)
	}

	if site.Label() != "Marketing" {
		t.Errorf("the display name should be what the list shows, got %q", site.Label())
	}

	if _, err := s.CreateSite(ctx, team.ID, "example.com", "", "Etc/UTC"); err != ErrDomainTaken {
		t.Errorf("want ErrDomainTaken, got %v", err)
	}

	if _, err := s.CreateSite(ctx, team.ID, "other.example.com", "", "Not/AZone"); err == nil {
		t.Error("an unknown timezone should be refused")
	}
}

// TestSiteLabelFallsBackToTheDomain checks the list never shows an empty name.
func TestSiteLabelFallsBackToTheDomain(t *testing.T) {
	if got := (&Site{Domain: "example.com"}).Label(); got != "example.com" {
		t.Errorf("want %q, got %q", "example.com", got)
	}
}

// TestChangeDomainOpensTheDualWriteWindow checks the 72-hour overlap, and that
// nothing but the routing entry moves.
//
// The site keeps its id, which is what every goal, funnel and segment is keyed
// on. The incumbent keyed goals on the domain string instead, so their
// change-domain feature silently wiped every goal a customer had configured.
func TestChangeDomainOpensTheDualWriteWindow(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Now()
	s.SetClock(func() time.Time { return now })

	_, team, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	site, err := s.CreateSite(ctx, team.ID, "old.example.com", "", "Etc/UTC")
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	if err := s.ChangeDomain(ctx, team.ID, site.ID, "new.example.com"); err != nil {
		t.Fatalf("change domain: %v", err)
	}

	changed, err := s.SiteByID(ctx, team.ID, site.ID)
	if err != nil {
		t.Fatalf("read site: %v", err)
	}

	if changed.ID != site.ID {
		t.Error("the site id must not change — every goal and funnel is keyed on it")
	}

	if changed.Domain != "new.example.com" {
		t.Errorf("want the new domain, got %q", changed.Domain)
	}

	if changed.PreviousDomain != "old.example.com" {
		t.Errorf("the old domain should be remembered, got %q", changed.PreviousDomain)
	}

	if !changed.DualWriteActive(now) {
		t.Error("the dual-write window should be open immediately after the change")
	}

	if changed.DualWriteActive(now.Add(DualWriteWindow + time.Minute)) {
		t.Error("the dual-write window should close after 72 hours")
	}
}

// TestChangeDomainRefusesOneAlreadyTracked checks the uniqueness guard, since
// two sites answering to one domain would make routing ambiguous.
func TestChangeDomainRefusesOneAlreadyTracked(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, team, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	first, err := s.CreateSite(ctx, team.ID, "one.example.com", "", "Etc/UTC")
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	if _, err := s.CreateSite(ctx, team.ID, "two.example.com", "", "Etc/UTC"); err != nil {
		t.Fatalf("create site: %v", err)
	}

	if err := s.ChangeDomain(ctx, team.ID, first.ID, "two.example.com"); err != ErrDomainTaken {
		t.Errorf("want ErrDomainTaken, got %v", err)
	}
}

// TestPinnedSitesComeFirst checks that a pin survives every sort order, which
// is what makes a pin worth having at all.
func TestPinnedSitesComeFirst(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, team, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if _, err := s.CreateSite(ctx, team.ID, "aaa.example.com", "", "Etc/UTC"); err != nil {
		t.Fatalf("create site: %v", err)
	}

	last, err := s.CreateSite(ctx, team.ID, "zzz.example.com", "", "Etc/UTC")
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	if err := s.SetPinned(ctx, team.ID, last.ID, true); err != nil {
		t.Fatalf("pin site: %v", err)
	}

	for _, order := range []string{"", "name", "created"} {
		list, err := s.ListSites(ctx, team.ID, order)
		if err != nil {
			t.Fatalf("list sites: %v", err)
		}

		if list[0].ID != last.ID {
			t.Errorf("sort %q: the pinned site should be first, got %q", order, list[0].Domain)
		}
	}

	// The traffic sort happens in Go, after the counts are attached, and has to
	// respect the pin too.
	list, err := s.ListSites(ctx, team.ID, "traffic")
	if err != nil {
		t.Fatalf("list sites: %v", err)
	}

	list[0].Visitors, list[1].Visitors = 1000, 1

	SortByTraffic(list)

	if list[0].ID != last.ID {
		t.Errorf("the pinned site should stay first even when another has more traffic, got %d", list[0].ID)
	}
}

// TestFoldersReorderAndDeleteSafely checks the two things a folder has to get
// right: the order the user dragged it into, and that deleting one does not
// delete the sites inside it.
func TestFoldersReorderAndDeleteSafely(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, team, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	first, err := s.CreateFolder(ctx, team.ID, "Clients")
	if err != nil {
		t.Fatalf("create folder: %v", err)
	}

	second, err := s.CreateFolder(ctx, team.ID, "Internal")
	if err != nil {
		t.Fatalf("create folder: %v", err)
	}

	if err := s.ReorderFolders(ctx, team.ID, []int64{second.ID, first.ID}); err != nil {
		t.Fatalf("reorder folders: %v", err)
	}

	folders, err := s.ListFolders(ctx, team.ID)
	if err != nil {
		t.Fatalf("list folders: %v", err)
	}

	if folders[0].ID != second.ID {
		t.Errorf("the dragged order did not stick: got %q first", folders[0].Name)
	}

	site, err := s.CreateSite(ctx, team.ID, "a.example.com", "", "Etc/UTC")
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	if err := s.MoveSite(ctx, team.ID, site.ID, first.ID, 1000); err != nil {
		t.Fatalf("move site: %v", err)
	}

	if err := s.DeleteFolder(ctx, team.ID, first.ID); err != nil {
		t.Fatalf("delete folder: %v", err)
	}

	survivor, err := s.SiteByID(ctx, team.ID, site.ID)
	if err != nil {
		t.Fatalf("the site should have survived its folder: %v", err)
	}

	if survivor.FolderID != 0 {
		t.Errorf("the site should be back at the top level, got folder %d", survivor.FolderID)
	}
}

// TestSiteScopedReadsRefuseAnotherTeam checks the account scoping that stops a
// guessed id in a URL reaching somebody else's site.
func TestSiteScopedReadsRefuseAnotherTeam(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, teamA, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	_, teamB, err := s.CreateUser(ctx, "b@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	site, err := s.CreateSite(ctx, teamA.ID, "a.example.com", "", "Etc/UTC")
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	if _, err := s.SiteByID(ctx, teamB.ID, site.ID); err != ErrNotFound {
		t.Errorf("another team must not be able to read the site, got %v", err)
	}

	// A scoped write against the wrong team is a no-op rather than an error,
	// which is what makes a handler impossible to get wrong: the account id is
	// in the WHERE clause of every statement.
	if err := s.MoveSite(ctx, teamB.ID, site.ID, 0, 500); err != nil {
		t.Fatalf("move site: %v", err)
	}

	unchanged, err := s.SiteByID(ctx, teamA.ID, site.ID)
	if err != nil {
		t.Fatalf("read site: %v", err)
	}

	if unchanged.Position == 500 {
		t.Error("the scoped update should have touched nothing")
	}

	if err := s.DeleteSite(ctx, teamB.ID, site.ID); err != nil {
		t.Fatalf("delete site: %v", err)
	}

	if _, err := s.SiteByID(ctx, teamA.ID, site.ID); err != nil {
		t.Errorf("another team must not be able to delete the site: %v", err)
	}
}

// TestMarkOnboardedStopsTheWizardReappearing checks the flag both the finish and
// the skip paths set.
func TestMarkOnboardedStopsTheWizardReappearing(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, team, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	site, err := s.CreateSite(ctx, team.ID, "a.example.com", "", "Etc/UTC")
	if err != nil {
		t.Fatalf("create site: %v", err)
	}

	if site.OnboardedAt != 0 {
		t.Fatal("a new site should not be marked onboarded")
	}

	if err := s.MarkOnboarded(ctx, team.ID, site.ID); err != nil {
		t.Fatalf("mark onboarded: %v", err)
	}

	reloaded, err := s.SiteByID(ctx, team.ID, site.ID)
	if err != nil {
		t.Fatalf("read site: %v", err)
	}

	if reloaded.OnboardedAt == 0 {
		t.Error("the onboarded flag did not stick")
	}
}

// TestCommonTimezonesAreRealZones checks every suggestion loads, because a
// dropdown entry that fails validation is a dead end the user cannot get past.
func TestCommonTimezonesAreRealZones(t *testing.T) {
	for _, zone := range CommonTimezones() {
		if _, err := time.LoadLocation(zone); err != nil {
			t.Errorf("%q is not a loadable timezone: %v", zone, err)
		}
	}
}
