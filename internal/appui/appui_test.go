//
// appui_test.go
// What the settings navigation offers, and to whom.
//
// Created: 2026-09-03
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package appui

import (
	"net/url"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// labels is the section list as a reader sees it, for the ordering and presence
// assertions below.
func labels(sections []Section) []string {
	out := make([]string, 0, len(sections))
	for _, section := range sections {
		out = append(out, section.LabelID)
	}

	return out
}

// find returns one section by its label id.
func find(t *testing.T, sections []Section, labelID string) Section {
	t.Helper()

	for _, section := range sections {
		if section.LabelID == labelID {
			return section
		}
	}

	t.Fatalf("no %q section in %v", labelID, labels(sections))

	return Section{}
}

// TestEverySiteScreenGetsTheSameSections is the whole point of the shell. Two
// screens that are one surface to the reader must not each arrive with a
// different set of places to go.
func TestEverySiteScreenGetsTheSameSections(t *testing.T) {
	want := labels(NewShell("en", "a.example", 1, teams.RoleOwner, "", TabGeneral, "", true, Header{}).Sections)

	for _, tab := range []string{TabGeneral, TabSharing, TabConversions, TabPaths, TabImports, TabHealth, TabShields, TabReports} {
		got := labels(NewShell("en", "a.example", 1, teams.RoleOwner, "", tab, "", true, Header{}).Sections)

		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("the %s screen offers %v, want %v", tab, got, want)
		}
	}
}

// TestExactlyOneSectionIsCurrent keeps the navigation honest: a list that does
// not say where you are is a list of links, and one that says two places is
// worse than one that says none.
func TestExactlyOneSectionIsCurrent(t *testing.T) {
	for tab, labelID := range map[string]string{
		TabGeneral:     "settings.nav.general",
		TabTeam:        "settings.nav.team",
		TabSharing:     "settings.nav.sharing",
		TabConversions: "settings.nav.goals",
		TabPaths:       "settings.nav.paths",
		TabImports:     "settings.nav.imports",
		TabHealth:      "settings.nav.health",
		TabShields:     "settings.nav.shields",
		TabReports:     "settings.nav.reports",
	} {
		shell := NewShell("en", "a.example", 1, teams.RoleOwner, "", tab, "", true, Header{})

		var current []string
		for _, section := range shell.Sections {
			if section.Current {
				current = append(current, section.LabelID)
			}
			for _, child := range section.Children {
				if child.Current {
					current = append(current, child.LabelID)
				}
			}
		}

		if len(current) != 1 || current[0] != labelID {
			t.Errorf("the %s screen marked %v current, want just %s", tab, current, labelID)
		}
	}
}

// TestShieldsNestsItsFourKindsAndOpensOnOne checks the one section that has
// children. The parent opens when it or any child is showing, because a
// navigation that hides where you are is telling you nothing.
func TestShieldsNestsItsFourKindsAndOpensOnOne(t *testing.T) {
	all := NewShell("en", "a.example", 1, teams.RoleOwner, "", TabShields, "", true, Header{})
	shields := find(t, all.Sections, "settings.nav.shields")

	if len(shields.Children) != 4 {
		t.Fatalf("shields nests %d kinds, want 4", len(shields.Children))
	}
	if !shields.Expanded() {
		t.Error("shields is closed on its own screen")
	}

	one := find(t, NewShell("en", "a.example", 1, teams.RoleOwner, "", TabShields, "country", true, Header{}).Sections, "settings.nav.shields")

	if one.Current {
		t.Error("the parent is marked current while a child is showing")
	}
	if !one.Expanded() {
		t.Error("shields is closed while one of its kinds is showing")
	}

	// Somewhere else entirely leaves it shut.
	elsewhere := find(t, NewShell("en", "a.example", 1, teams.RoleOwner, "", TabHealth, "", true, Header{}).Sections, "settings.nav.shields")
	if elsewhere.Expanded() {
		t.Error("shields is open on a screen that is not shields")
	}
}

// TestASectionNobodyMayReachIsNotOffered keeps the permission rule in one
// testable place rather than in template conditionals.
func TestASectionNobodyMayReachIsNotOffered(t *testing.T) {
	owner := labels(NewShell("en", "a.example", 1, teams.RoleOwner, "", TabGeneral, "", true, Header{}).Sections)
	viewer := labels(NewShell("en", "a.example", 1, teams.RoleViewer, "", TabGeneral, "", true, Header{}).Sections)

	for _, want := range []string{"settings.nav.general", "settings.nav.shields"} {
		if !contains(owner, want) || !contains(viewer, want) {
			t.Errorf("%s is missing from a navigation", want)
		}
	}

	if !contains(owner, "settings.nav.team") {
		t.Error("an owner cannot reach the people screen")
	}
	if contains(viewer, "settings.nav.team") {
		t.Error("a viewer is offered the people screen")
	}
}

// TestTheAccountShellOffersTheScreensThatBelongToAPerson covers the screens
// that are about somebody rather than about one site. They wear the same shell,
// and a per-site section there would lead to /settings/sites//shields.
func TestTheAccountShellOffersTheScreensThatBelongToAPerson(t *testing.T) {
	owner := NewShell("en", "", 4, teams.RoleOwner, "", TabAccount, "", true, Header{})

	if got := labels(owner.Sections); len(got) == 0 || got[0] != "settings.nav.preferences" {
		t.Fatalf("the account shell opens with %v", got)
	}

	for _, want := range []string{"settings.nav.security", "settings.nav.devices", "settings.nav.danger"} {
		if !contains(labels(owner.Sections), want) {
			t.Errorf("%s is missing from the account shell", want)
		}
	}

	// The team screens are the ones a permission gates.
	viewer := labels(NewShell("en", "", 4, teams.RoleViewer, "", TabAccount, "", true, Header{}).Sections)

	for _, gated := range []string{"settings.nav.team_policy", "settings.nav.team"} {
		if !contains(labels(owner.Sections), gated) {
			t.Errorf("an owner cannot reach %s", gated)
		}
		if contains(viewer, gated) {
			t.Errorf("a viewer is offered %s", gated)
		}
	}

	// The team travels on the links that need it.
	for _, section := range owner.Sections {
		if section.LabelID == "settings.nav.team" && !strings.Contains(section.URL, "team_id=4") {
			t.Errorf("the members link is %q, want the team on it", section.URL)
		}
	}
}

// TestADomainSurvivesEveryLinkItIsPutIn is what stops a site whose name carries
// a space or a slash from producing a link to somewhere else.
func TestADomainSurvivesEveryLinkItIsPutIn(t *testing.T) {
	const domain = "a b/c.example"

	shell := NewShell("en", domain, 1, teams.RoleOwner, "", TabGeneral, "", true, Header{})

	for _, section := range shell.Sections {
		for _, link := range append([]string{section.URL}, childURLs(section)...) {
			parsed, err := url.Parse(link)
			if err != nil {
				t.Errorf("%s produced an unparseable link %q: %v", section.LabelID, link, err)
				continue
			}

			// The domain has to come back out of the path exactly as it went
			// in, or the link leads to a site that does not exist.
			if !strings.Contains(parsed.Path, domain) && !strings.Contains(parsed.RawQuery, "site_context") {
				t.Errorf("%s lost the domain in %q (path decoded to %q)", section.LabelID, link, parsed.Path)
			}

			if strings.Contains(link, " ") {
				t.Errorf("%s left a raw space in %q", section.LabelID, link)
			}
		}
	}
}

// childURLs flattens one section's children.
func childURLs(section Section) []string {
	out := make([]string, 0, len(section.Children))
	for _, child := range section.Children {
		out = append(out, child.URL)
	}

	return out
}

// contains reports membership without pulling in a dependency for it.
func contains(haystack []string, needle string) bool {
	for _, candidate := range haystack {
		if candidate == needle {
			return true
		}
	}

	return false
}

// TestTheBarOnlyOffersBillingWhereItMeansSomething keeps two conditions in one
// place. A self-hosted deployment sells no subscription, so the row leads to a
// page about a product it does not have; and a member who may not pay has no
// use for it either.
func TestTheBarOnlyOffersBillingWhereItMeansSomething(t *testing.T) {
	for name, header := range map[string]Header{
		"an owner on a hosted install": {Commerce: true, Role: teams.RoleOwner, TeamID: 3},
		"an admin on a hosted install": {Commerce: true, Role: teams.RoleAdmin, TeamID: 3},
	} {
		if url := header.BillingURL(); !strings.Contains(url, "team=3") {
			t.Errorf("%s was offered %q, want the team on it", name, url)
		}
	}

	for name, header := range map[string]Header{
		"an owner on a self-hosted install": {Commerce: false, Role: teams.RoleOwner, TeamID: 3},
		"a viewer on a hosted install":      {Commerce: true, Role: teams.RoleViewer, TeamID: 3},
	} {
		if url := header.BillingURL(); url != "" {
			t.Errorf("%s was offered %q", name, url)
		}
	}
}

// TestTheBarKeepsTheReaderOnTheSiteTheyCameFrom checks the two links in the bar
// itself. Somebody on one site's settings who presses Dashboard wants that
// site's stats, not a picker asking which one they meant.
func TestTheBarKeepsTheReaderOnTheSiteTheyCameFrom(t *testing.T) {
	with := Header{TeamID: 2, Site: "a b.example"}

	if got := with.DashboardURL(); got != "/dashboard/a%20b.example" {
		t.Errorf("dashboard link = %q", got)
	}
	if got := with.SitesURL(); !strings.Contains(got, "team_id=2") || !strings.Contains(got, "site_context=a+b.example") {
		t.Errorf("sites link = %q, want the team and the site on it", got)
	}

	// With no site in scope the picker chooses, and an unresolved team is not
	// guessed at with a zero.
	bare := Header{}

	if got := bare.DashboardURL(); got != "/dashboard/" {
		t.Errorf("bare dashboard link = %q", got)
	}
	if got := bare.SitesURL(); got != "/sites" {
		t.Errorf("bare sites link = %q", got)
	}
}

// TestTheAvatarFallsBackToALetter is what a person with no picture sees. It is
// the same fallback the dashboard's own account button uses, so the two
// surfaces never disagree about somebody's initial.
func TestTheAvatarFallsBackToALetter(t *testing.T) {
	for name, want := range map[string]string{"": "?", "spicer@cloudmanic.com": "S"} {
		if got := (Header{Email: name}).Initial(); got != want {
			t.Errorf("an account with email %q shows %q, want %q", name, got, want)
		}
	}

	// A name wins over the address, and a non-ASCII first letter still works.
	if got := (Header{Name: "Ötzi", Email: "a@example.com"}).Initial(); got != "Ö" {
		t.Errorf("initial = %q, want Ö", got)
	}
}
