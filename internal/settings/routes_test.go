//
// routes_test.go
// Proving that no /settings/ pattern silently swallows another.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settings

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// route shadowing has taken a screen off the air three times in this codebase:
// a "/settings/" prefix mount that swallowed the account screens, the team
// screens landing on a segment the account screens already held, and the site
// configuration screens' {domain} wildcard reaching paths that were not
// domains. Every one of them was silent.
//
// Go's mux is silent by design here. It only panics when two patterns overlap
// and neither is more specific; when one *is* more specific it simply wins, and
// the other stops answering. Worse, the account screens are registered on the
// application's own inner mux behind "/", which the outer mux cannot see at
// all — so there is no overlap for it to notice in the first place.
//
// So the check cannot be "did registration complain". It has to be: build the
// real thing, ask it who answers, and compare that against the table.

// owners builds the mux the product builds, with a sentinel per owner in place
// of the real handler, so a probe answers with the name of whoever serves it.
func owners(t *testing.T, routes []Route) *http.ServeMux {
	t.Helper()

	mux := http.NewServeMux()

	// The signed-in application is mounted at the root and routes its own
	// screens internally. Standing in for it with a catch-all is exactly right:
	// it is what makes a pattern that steals one of its paths visible here,
	// which is the thing the outer mux cannot see for itself.
	mux.Handle("/", sentinel(OwnerAccount))

	for _, route := range routes {
		if route.Owner == OwnerAccount {
			continue
		}

		mux.Handle(route.Pattern, sentinel(route.Owner))
	}

	return mux
}

// sentinel answers with the name of the surface that owns it.
func sentinel(owner string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(owner))
	})
}

// answered reports which surface the mux routes a path to.
func answered(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

	return recorder.Body.String()
}

// swallowed returns every path in a route table that is answered by a surface
// other than the one that claims it.
//
// It is a function rather than inline assertions so the test below can point it
// at a deliberately broken table and prove it has teeth.
func swallowed(t *testing.T, routes []Route) []string {
	t.Helper()

	mux := owners(t, routes)

	var wrong []string

	for _, route := range routes {
		for _, path := range Probes(route) {
			if got := answered(t, mux, path); got != route.Owner {
				wrong = append(wrong, path+" is claimed by "+route.Owner+" and answered by "+got)
			}
		}
	}

	sort.Strings(wrong)

	return wrong
}

// TestNoSettingsRouteSwallowsAnother is the check itself. Every path the table
// claims has to be answered by the surface that claims it.
func TestNoSettingsRouteSwallowsAnother(t *testing.T) {
	if wrong := swallowed(t, Routes()); len(wrong) > 0 {
		t.Fatalf("%d /settings/ paths are served by the wrong surface:\n  %s",
			len(wrong), strings.Join(wrong, "\n  "))
	}
}

// TestTheCheckCatchesEveryCollisionWeHaveShipped points the same check at the
// three tables that were wrong, and requires it to fail on each.
//
// Without this the check above passes whether or not it can detect anything,
// which is the failure mode of every test written after the bug it describes.
func TestTheCheckCatchesEveryCollisionWeHaveShipped(t *testing.T) {
	cases := []struct {
		name  string
		table []Route
	}{
		{
			// The original: one prefix mount for the whole segment. It takes
			// the account screens with it and Go's mux says nothing.
			name: "a /settings/ prefix mount",
			table: append([]Route{{Pattern: PathPrefix, Owner: OwnerTeam}},
				accountRoutes()...),
		},
		{
			// The team screens on the segment the account screens already
			// hold. /settings/team is the team name and the two-factor policy;
			// the people belong at /settings/members.
			name: "the team screen on /settings/team",
			table: append([]Route{{Pattern: "/settings/team", Owner: OwnerTeam}},
				accountRoutes()...),
		},
		{
			// The site configuration wildcard reaching a path that is not a
			// domain. /settings/{domain}/sessions/revoke does not exist today,
			// but /settings/{domain}/{action} plus an action the account
			// screens also use is one rename away.
			name: "a {domain} wildcard over an account path",
			table: append([]Route{{Pattern: "/settings/{domain}/revoke", Owner: OwnerSite}},
				Route{Pattern: "/settings/sessions/revoke", Owner: OwnerAccount}),
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if wrong := swallowed(t, test.table); len(wrong) == 0 {
				t.Fatal("the check passed a table that takes a shipped screen off the air")
			}
		})
	}
}

// accountRoutes is the account screens as table rows, for the cases above.
func accountRoutes() []Route {
	routes := make([]Route, 0, len(accountPaths))
	for _, path := range accountPaths {
		routes = append(routes, Route{Pattern: path, Owner: OwnerAccount})
	}

	return routes
}

// TestEveryAccountSettingsPathIsClaimed keeps the account half of the table
// honest.
//
// The paths are listed here rather than imported, so nothing stops somebody
// adding a screen over there and never touching this file — and the check above
// would then happily approve a pattern that swallows it. This reads them back
// out of the application's own registrations and fails if the two disagree.
func TestEveryAccountSettingsPathIsClaimed(t *testing.T) {
	source, err := os.ReadFile("../auth/web.go")
	if err != nil {
		t.Skipf("the signed-in application is not in this checkout: %v", err)
	}

	// The registrations are literal patterns, so reading them is a match on the
	// path rather than anything that has to understand Go.
	pattern := regexp.MustCompile(`"(?:GET|POST|PUT|DELETE|PATCH) (/settings/[^"]*)"`)

	registered := map[string]bool{}
	for _, match := range pattern.FindAllStringSubmatch(string(source), -1) {
		registered[match[1]] = true
	}

	if len(registered) == 0 {
		t.Fatal("no /settings/ registrations were found in the application — has the pattern changed?")
	}

	claimed := map[string]bool{}
	for _, path := range accountPaths {
		claimed[path] = true
	}

	var missing []string
	for path := range registered {
		if !claimed[path] {
			missing = append(missing, path)
		}
	}

	var stale []string
	for path := range claimed {
		if !registered[path] {
			stale = append(stale, path)
		}
	}

	sort.Strings(missing)
	sort.Strings(stale)

	if len(missing) > 0 {
		t.Fatalf("%d account settings paths are served and not claimed in routes.go, "+
			"so nothing would notice a pattern that swallowed them:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}

	if len(stale) > 0 {
		t.Fatalf("%d paths are claimed in routes.go and no longer served:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}

// TestEveryRouteIsNarrow pins the shape rather than the behaviour, because the
// behaviour above would also pass if somebody replaced the table with one
// prefix and moved every other screen out of its way. The point is that nothing
// on this surface claims more than the screen it is.
//
// A prefix pattern is what makes "which of these owns this URL" unanswerable:
// it claims every path below it, including ones nobody has thought of yet.
func TestEveryRouteIsNarrow(t *testing.T) {
	for _, route := range Routes() {
		if route.Pattern == PathPrefix || route.Pattern == SitePrefix {
			t.Fatalf("%q claims a whole segment rather than one screen", route.Pattern)
		}

		if strings.HasSuffix(route.Pattern, "/") {
			t.Fatalf("%q is a prefix mount and claims every path below it", route.Pattern)
		}

		if !strings.HasPrefix(route.Pattern, PathPrefix) {
			t.Fatalf("%q does not sit under %q", route.Pattern, PathPrefix)
		}
	}
}

// TestEveryPerSiteRouteSitsUnderTheSitePrefix is the rule that makes the
// collision impossible rather than merely absent today.
//
// A per-site pattern that claims the second segment — /settings/{domain}/… —
// overlaps every account and team screen, because a domain is customer data and
// `members` is a legal site name. Reserving one literal segment is what turns
// "we checked and it does not collide" into "it cannot".
func TestEveryPerSiteRouteSitsUnderTheSitePrefix(t *testing.T) {
	for _, route := range Routes() {
		if !strings.Contains(route.Pattern, "{domain}") {
			continue
		}

		if !strings.HasPrefix(route.Pattern, SitePrefix+"{domain}/") {
			t.Fatalf("%q takes a domain in a segment other than the one reserved for sites, "+
				"so a site named after another screen would collide with it", route.Pattern)
		}
	}
}
