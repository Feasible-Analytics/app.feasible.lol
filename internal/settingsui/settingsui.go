//
// settingsui.go
// The one navigation every settings screen is drawn beside.
//
// Created: 2026-09-03
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package settingsui holds the chrome shared by every settings screen.
//
// The screens themselves are split across two packages for reasons that are
// about handlers, not about what a reader sees: to them it is one surface. So
// the header, the section list and the card styles live here, in a package both
// can import, rather than being written twice and drifting.
package settingsui

import (
	"embed"
	"fmt"
	"net/url"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/shields"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// Templates carries the shared header and section navigation. Both packages
// parse it alongside their own, so there is one copy of the markup.
//
//go:embed templates
var Templates embed.FS

// Tab names the screen being looked at. It is a string rather than a typed
// enumeration because it travels into templates, where a type buys nothing.
const (
	TabGeneral     = "general"
	TabTeam        = "team"
	TabSharing     = "sharing"
	TabConversions = "conversions"
	TabPaths       = "paths"
	TabImports     = "imports"
	TabHealth      = "health"
	TabShields     = "shields"
	TabReports     = "reports"
)

// Section is one entry in the settings navigation.
//
// The list is built here rather than in a template because which entries a
// person may see is a permission decision, and a permission decision spread
// through template conditionals is one nobody can test.
type Section struct {
	// LabelID is a catalogue id, so the navigation reads in the same language
	// as the screen beside it.
	LabelID string

	// URL names the site it is about. The locale is added where the link is
	// drawn, because these are built once and rendered per request.
	URL string

	// Current marks the screen being looked at. A navigation that does not say
	// where you are is a list of links.
	Current bool

	// Children are the sub-sections of a section that has them.
	Children []Section
}

// Expanded reports whether a parent is drawn open, which is whenever it or one
// of its children is the current screen.
func (s Section) Expanded() bool {
	if s.Current {
		return true
	}

	for _, child := range s.Children {
		if child.Current {
			return true
		}
	}

	return false
}

// shieldLabels names each rule kind in the navigation. The kinds themselves
// come from the shields package, so renaming one there is a compile error here
// rather than a Shields screen that quietly renders nothing.
var shieldLabels = map[string]string{
	shields.KindIP:       "settings.nav.shields.ip",
	shields.KindCountry:  "settings.nav.shields.country",
	shields.KindPage:     "settings.nav.shields.page",
	shields.KindHostname: "settings.nav.shields.hostname",
}

// ShieldKinds is every kind the navigation nests, in the order Shields shows
// them. It is exported because the screen filters on the same values.
func ShieldKinds() []string {
	return shields.Kinds
}

// Shell is everything the shared header and navigation need. Both packages'
// page structs carry one, which is what lets one template render both.
type Shell struct {
	Lang   string
	Domain string
	TeamID int64
	Role   teams.Role
	CSRF   string

	// Commerce is false on a self-hosted install, where there is no
	// subscription to manage and a billing link leads to a page about a
	// product this deployment does not sell.
	Commerce bool

	// Sections is the resolved navigation.
	Sections []Section
}

// GeneralPath is a site's General screen. It is the URL that already exists
// rather than a tidier one, because a settings page somebody bookmarked is a
// page that has to keep answering.
func GeneralPath(domain string) string {
	return "/sites/domain/" + url.PathEscape(domain) + "/settings"
}

// SitePath is any of the per-site screens owned by the settings package.
func SitePath(domain, action string) string {
	return "/settings/sites/" + url.PathEscape(domain) + "/" + action
}

// NewShell resolves the chrome for one screen.
//
// Every per-site screen gets the same list in the same order, and only Current
// moves. That sameness is the point: two screens that are one surface to the
// reader must not each arrive with a different set of places to go.
func NewShell(lang, domain string, teamID int64, role teams.Role, csrf, tab, shield string, commerce bool) Shell {
	shell := Shell{Lang: lang, Domain: domain, TeamID: teamID, Role: role, CSRF: csrf, Commerce: commerce}

	if domain == "" {
		shell.Sections = accountSections(teamID, tab)

		return shell
	}

	site := func(action string) string { return SitePath(domain, action) }

	shell.Sections = []Section{
		{LabelID: "settings.nav.general", URL: GeneralPath(domain), Current: tab == TabGeneral},
	}

	if teams.Can(role, teams.PermManageMembers) || teams.Can(role, teams.PermCreateAPIKey) {
		shell.Sections = append(shell.Sections, Section{
			LabelID: "settings.nav.team",
			URL:     fmt.Sprintf("/settings/members?site_context=%s", url.QueryEscape(domain)),
			Current: tab == TabTeam,
		})
	}

	shell.Sections = append(shell.Sections,
		Section{LabelID: "settings.nav.sharing", URL: site("sharing"), Current: tab == TabSharing},
		Section{LabelID: "settings.nav.goals", URL: site("conversions"), Current: tab == TabConversions},
		Section{LabelID: "settings.nav.funnels", URL: site("conversions") + "#funnels"},
		Section{LabelID: "settings.nav.properties", URL: site("conversions") + "#properties"},
		Section{LabelID: "settings.nav.paths", URL: site("paths"), Current: tab == TabPaths},
		Section{LabelID: "settings.nav.imports", URL: site("imports"), Current: tab == TabImports},
		Section{LabelID: "settings.nav.health", URL: site("health"), Current: tab == TabHealth},
		shieldsSection(domain, tab, shield),
		Section{LabelID: "settings.nav.reports", URL: site("reports"), Current: tab == TabReports},
		Section{LabelID: "settings.nav.danger", URL: GeneralPath(domain) + "#danger"},
	)

	return shell
}

// CurrentLabel is the label id of the section being looked at, for the phone's
// collapsed navigation. A closed box reading only "Settings sections" is one
// that will not say where you are without being opened.
func (s Shell) CurrentLabel() string {
	for _, section := range s.Sections {
		if section.Current {
			return section.LabelID
		}
		for _, child := range section.Children {
			if child.Current {
				return child.LabelID
			}
		}
	}

	return "settings.nav.label"
}

// shieldsSection is the one parent with children. Its four rule kinds are long
// enough that each deserves its own screen, and shallow enough that hiding them
// behind a page nobody bookmarked would be worse.
func shieldsSection(domain, tab, shield string) Section {
	base := SitePath(domain, "shields")

	parent := Section{LabelID: "settings.nav.shields", URL: base, Current: tab == TabShields && shield == ""}

	for _, kind := range shields.Kinds {
		parent.Children = append(parent.Children, Section{
			LabelID: shieldLabels[kind],
			URL:     base + "?kind=" + kind,
			Current: tab == TabShields && shield == kind,
		})
	}

	return parent
}

// accountSections is the same shell around the screens that belong to a team
// rather than to one site.
func accountSections(teamID int64, tab string) []Section {
	members := "/settings/members"
	if teamID > 0 {
		members = fmt.Sprintf("%s?team_id=%d", members, teamID)
	}

	return []Section{{LabelID: "settings.nav.team", URL: members, Current: tab == TabTeam}}
}
