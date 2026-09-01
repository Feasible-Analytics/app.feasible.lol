//
// routes.go
// Every route on the /settings/ surface, in one table, with a way to prove none
// of them swallows another.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settings

import (
	"net/http"
	"strings"
)

// MembersPattern is the team administration screen.
//
// It is /settings/members rather than /settings/team because the signed-in
// application's team screen — the name and the two-factor policy — already
// holds that URL. These are the people; that is the policy.
const MembersPattern = PathPrefix + "members"

// memberActions are the team screen's forms, relative to MembersPattern.
//
// They are listed rather than mounted as one /settings/members/ prefix for the
// same reason the site screens are: a prefix claims everything under it, and
// "everything under it" is not a thing anybody can check against the rest of
// the surface.
var memberActions = []string{
	"role",
	"remove",
	"invite",
	"invite/revoke",
	"api-keys",
	"api-keys/revoke",
	"transfer",
}

// teamSiteActions are the per-site screens the team gates rather than site
// ownership: publishing a site, mailing reports about it, and diagnosing what
// its tracker is sending.
var teamSiteActions = []string{
	"sharing",
	"sharing/public",
	"sharing/links",
	"sharing/links/revoke",
	"reports",
	"reports/subscription",
	"reports/alert",
	"health",
	"health/allow",
	"health/test-event",
}

// membersPath and sitesPath are the values the team handler switches on, with
// PathPrefix already removed.
const (
	membersPath = "members"
	sitesPath   = "sites/"
)

// The owners of a /settings/ route. A route's owner decides two things a
// pattern cannot say for itself: which handler serves it, and which gate it
// stands behind.
const (
	// OwnerSite is the site configuration screens — shields, path cleaning,
	// imports and exports. They stand behind the site ownership check.
	OwnerSite = "site"

	// OwnerTeam is the team screen and the per-site screens whose permission
	// question is "what may this person do in this team" rather than "does this
	// person own this site": members, sharing, reports and the health panel.
	OwnerTeam = "team"

	// OwnerAccount is the signed-in application's own screens — profile,
	// password, sessions, security and the team policy. Nothing here serves
	// them. They are in this table because they are the reason it exists: they
	// live under the same segment, they are registered on a different mux, and
	// a pattern here that swallowed one of them would take it off the air with
	// nothing anywhere to say so.
	OwnerAccount = "account"
)

// Route is one pattern on the /settings/ surface.
type Route struct {
	// Pattern is a net/http ServeMux pattern, or — for the account screens — a
	// literal path claimed on another mux.
	Pattern string

	// Owner is which surface answers it.
	Owner string
}

// accountPaths are the paths the signed-in application claims under this
// segment. They are listed rather than imported because internal/auth would
// then have to import this package to register them, and this package already
// imports what auth builds on.
//
// A screen added there and not added here is the failure this table exists to
// catch, so TestEveryAccountSettingsPathIsClaimed reads them back out of the
// auth package's own source and fails if the two lists disagree.
var accountPaths = []string{
	"/settings/profile",
	"/settings/password",
	"/settings/delete",
	"/settings/sessions",
	"/settings/sessions/revoke",
	"/settings/security",
	"/settings/security/2fa/start",
	"/settings/security/2fa/qr.png",
	"/settings/security/2fa/enable",
	"/settings/security/2fa/disable",
	"/settings/security/2fa/recovery",
	"/settings/team",
}

// Routes is every route on the /settings/ surface and who owns it.
//
// It exists because route shadowing has taken a screen off the air three times
// in this codebase, and Go's mux says nothing when it happens. A pattern that
// is merely *more specific* than another wins silently, and the account screens
// are registered on an inner mux the outer one cannot see at all — so there is
// no registration-time conflict to catch, no log line, and no failing request:
// the swallowed screen simply stops existing.
//
// One table is what makes that checkable. TestNoSettingsRouteSwallowsAnother
// walks it.
func Routes() []Route {
	routes := make([]Route, 0,
		len(actions)+len(memberActions)+len(teamSiteActions)+len(accountPaths)+1)

	for _, pattern := range Patterns() {
		routes = append(routes, Route{Pattern: pattern, Owner: OwnerSite})
	}

	routes = append(routes, Route{Pattern: MembersPattern, Owner: OwnerTeam})

	for _, action := range memberActions {
		routes = append(routes, Route{Pattern: MembersPattern + "/" + action, Owner: OwnerTeam})
	}

	for _, action := range teamSiteActions {
		routes = append(routes, Route{Pattern: SitePrefix + "{domain}/" + action, Owner: OwnerTeam})
	}

	for _, path := range accountPaths {
		routes = append(routes, Route{Pattern: path, Owner: OwnerAccount})
	}

	return routes
}

// Mount registers every route this package serves.
//
// Both handlers arrive already wrapped in their own gate, because the two gates
// are genuinely different: the site screens ask whether this person owns this
// site, and the team screens ask what this person may do in this team. Mounting
// is still done here, from the one table, so that a new screen is a row rather
// than a call somebody has to remember to add beside the others.
func Mount(mux *http.ServeMux, site, team http.Handler) {
	for _, route := range Routes() {
		switch route.Owner {
		case OwnerSite:
			mux.Handle(route.Pattern, site)

		case OwnerTeam:
			mux.Handle(route.Pattern, team)

		// The account screens are the signed-in application's, registered on
		// its own mux under "/". Registering them here would be the shadowing
		// this table exists to prevent, pointed the other way.
		case OwnerAccount:
		}
	}
}

// Probes returns the paths a route has to answer, so a check can ask the real
// mux "who serves this" rather than reasoning about pattern precedence.
//
// A wildcard is filled with values that are deliberately awkward. A domain is
// customer data: nothing stops somebody registering a site called `members` or
// `security`, and a site named after another screen is exactly where a wildcard
// and a literal fight.
func Probes(route Route) []string {
	pattern := route.Pattern

	// A trailing slash is a prefix match, so the path that tests it is one with
	// something after the slash. Nothing in the table should have one any more,
	// but the check has to keep working if somebody adds one back.
	if strings.HasSuffix(pattern, "/") {
		return []string{pattern + "example.com/sharing", pattern + "anything"}
	}

	if !strings.Contains(pattern, "{") {
		return []string{pattern}
	}

	paths := []string{}
	for _, domain := range []string{"example.com", "members", "team", "security", "sessions", "sites"} {
		filled := strings.ReplaceAll(pattern, "{domain}", domain)
		filled = strings.ReplaceAll(filled, "{token}", "abc123")
		paths = append(paths, filled)
	}

	return paths
}
