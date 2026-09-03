//
// settingsui_test.go
// What the settings navigation offers, and to whom.
//
// Created: 2026-09-03
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settingsui

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
	want := labels(NewShell("en", "a.example", "", 1, teams.RoleOwner, "", TabGeneral, "").Sections)

	for _, tab := range []string{TabGeneral, TabSharing, TabConversions, TabPaths, TabImports, TabHealth, TabShields, TabReports} {
		got := labels(NewShell("en", "a.example", "", 1, teams.RoleOwner, "", tab, "").Sections)

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
		shell := NewShell("en", "a.example", "", 1, teams.RoleOwner, "", tab, "")

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
	all := NewShell("en", "a.example", "", 1, teams.RoleOwner, "", TabShields, "")
	shields := find(t, all.Sections, "settings.nav.shields")

	if len(shields.Children) != 4 {
		t.Fatalf("shields nests %d kinds, want 4", len(shields.Children))
	}
	if !shields.Expanded() {
		t.Error("shields is closed on its own screen")
	}

	one := find(t, NewShell("en", "a.example", "", 1, teams.RoleOwner, "", TabShields, "country").Sections, "settings.nav.shields")

	if one.Current {
		t.Error("the parent is marked current while a child is showing")
	}
	if !one.Expanded() {
		t.Error("shields is closed while one of its kinds is showing")
	}

	// Somewhere else entirely leaves it shut.
	elsewhere := find(t, NewShell("en", "a.example", "", 1, teams.RoleOwner, "", TabHealth, "").Sections, "settings.nav.shields")
	if elsewhere.Expanded() {
		t.Error("shields is open on a screen that is not shields")
	}
}

// TestASectionNobodyMayReachIsNotOffered keeps the permission rule in one
// testable place rather than in template conditionals.
func TestASectionNobodyMayReachIsNotOffered(t *testing.T) {
	owner := labels(NewShell("en", "a.example", "", 1, teams.RoleOwner, "", TabGeneral, "").Sections)
	viewer := labels(NewShell("en", "a.example", "", 1, teams.RoleViewer, "", TabGeneral, "").Sections)

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

// TestADomainWithNothingToConfigureFallsBackToTheAccount covers the screens
// that belong to a team rather than to one site. They render in the same shell,
// and a per-site section there would lead to /settings/sites//shields.
func TestADomainWithNothingToConfigureFallsBackToTheAccount(t *testing.T) {
	shell := NewShell("en", "", "", 4, teams.RoleOwner, "", TabTeam, "")

	if got := labels(shell.Sections); len(got) != 1 || got[0] != "settings.nav.team" {
		t.Fatalf("the account shell offers %v", got)
	}

	if url := shell.Sections[0].URL; !strings.Contains(url, "team_id=4") {
		t.Errorf("the account members link is %q, want the team on it", url)
	}
}

// TestADomainSurvivesEveryLinkItIsPutIn is what stops a site whose name carries
// a space or a slash from producing a link to somewhere else.
func TestADomainSurvivesEveryLinkItIsPutIn(t *testing.T) {
	const domain = "a b/c.example"

	shell := NewShell("en", domain, "", 1, teams.RoleOwner, "", TabGeneral, "")

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
