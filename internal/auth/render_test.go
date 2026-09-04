//
// render_test.go
// Template parsing, the helpers the templates call, and the sparkline geometry.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/appui"
)

// TestEveryPageParses checks the whole template tree at once. A broken template
// is a start-up failure by design, and this is what catches it before a release
// rather than on the page nobody opens until Friday.
func TestEveryPageParses(t *testing.T) {
	views, err := newViews()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}

	// Every page a handler names has to exist, or that route is a 500 waiting
	// to happen.
	for _, name := range []string{
		"login", "register", "verify", "verify_failed", "forgot", "reset", "two_factor",
		"sites", "site_new", "site_settings", "onboarding",
		"settings_account", "settings_sessions", "settings_security", "settings_team",
		"recovery_codes", "deleted", "error",
	} {
		if _, ok := views.pages[name]; !ok {
			t.Errorf("no template named %q", name)
		}
	}
}

// TestSparklineNormalisesToItsOwnMaximum checks the geometry. The series is
// scaled to its own peak because the question the chart answers is "which way
// is this site going", not "how does it compare to the busy one next to it".
func TestSparklineNormalisesToItsOwnMaximum(t *testing.T) {
	points := string(sparklinePath([]int64{0, 5, 10}))

	if points == "" {
		t.Fatal("three points should produce a polyline")
	}

	pairs := strings.Fields(points)
	if len(pairs) != 3 {
		t.Fatalf("want 3 points, got %d: %s", len(pairs), points)
	}

	// The first point is the left edge and the last is the right edge, so the
	// line spans the whole box whatever the number of days.
	if !strings.HasPrefix(pairs[0], "0.0,") {
		t.Errorf("the first point should be at x=0, got %q", pairs[0])
	}

	if !strings.HasPrefix(pairs[2], "120.0,") {
		t.Errorf("the last point should be at the right edge, got %q", pairs[2])
	}

	// A site with no traffic gets a flat line rather than nothing: an empty box
	// reads as broken, a flat line reads as quiet.
	flat := string(sparklinePath([]int64{0, 0, 0}))
	if flat == "" {
		t.Error("an all-zero series should still draw a line")
	}

	// One point cannot make a line.
	if string(sparklinePath([]int64{5})) != "" {
		t.Error("a single point should draw nothing")
	}
}

// TestOverviewChartSharesOneScaleBetweenTheTwoSeries checks the geometry of a
// site card's chart. Both series are drawn against the same peak, which is what
// makes the gap between the lines readable as pageviews per visitor; scaling
// each to its own maximum would draw the two identically on every card.
func TestOverviewChartSharesOneScaleBetweenTheTwoSeries(t *testing.T) {
	// Visitors peak at 5 and pageviews at 10, so visitors must reach halfway up
	// the box and pageviews must reach the top.
	drawn := overviewChart([]int64{0, 5}, []int64{0, 10})

	visitors := strings.Fields(string(drawn.Visitors))
	if len(visitors) != 2 {
		t.Fatalf("want 2 visitor points, got %d: %s", len(visitors), drawn.Visitors)
	}

	if visitors[0] != "0.0,72.0" {
		t.Errorf("an empty first bucket should sit on the floor, got %q", visitors[0])
	}

	if visitors[1] != "240.0,36.5" {
		t.Errorf("half the peak should be drawn halfway up, got %q", visitors[1])
	}

	// The area is the same shape closed along the bottom, so it can be filled
	// behind the line.
	area := string(drawn.Pageviews)
	if !strings.HasPrefix(area, "0,72 ") || !strings.HasSuffix(area, " 240,72") {
		t.Errorf("the pageview area should close along the floor, got %q", area)
	}

	// A site with no traffic gets a flat line and no area, for the same reason
	// the sparkline does: an empty box reads as broken.
	quiet := overviewChart([]int64{0, 0}, []int64{0, 0})
	if quiet.Visitors == "" {
		t.Error("an all-zero series should still draw a line")
	}

	if quiet.Pageviews != "" {
		t.Error("an all-zero series should not fill an area")
	}

	// Two series of different lengths cannot share a scale, and drawing them
	// anyway would silently misalign the buckets.
	if mismatched := overviewChart([]int64{0, 1, 2}, []int64{0, 1}); mismatched.Visitors != "" {
		t.Error("series of different lengths should draw nothing")
	}
}

// TestGroupDigitsAndDurationReadAsNumbersDo checks the two formatters the
// all-sites cards run every figure through.
func TestGroupDigitsAndDurationReadAsNumbersDo(t *testing.T) {
	for _, tc := range []struct {
		value int64
		want  string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1234567, "1,234,567"},
		{-4321, "-4,321"},
	} {
		if got := groupDigits(tc.value); got != tc.want {
			t.Errorf("groupDigits(%d) = %q, want %q", tc.value, got, tc.want)
		}
	}

	for _, tc := range []struct {
		seconds int64
		want    string
	}{
		{0, "0s"},
		{45, "45s"},
		{143, "2m 23s"},
		{3660, "1h 01m"},
	} {
		if got := humanDuration("en", tc.seconds); got != tc.want {
			t.Errorf("humanDuration(%d) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

// TestTemplateHelpers checks the functions the templates are allowed to call,
// since a wrong one shows up as wrong text on a page rather than a failure
// anywhere.
//
// Every one of them takes the locale before its value, because the templates
// are parsed once at start-up and a helper that closed over a language would
// answer in whichever one the process happened to be built with. The two that
// measure time take the clock ahead of that, for the same reason: a helper that
// read time.Now would ignore the clock a test installed.
func TestTemplateHelpers(t *testing.T) {
	funcs := templateFuncs()

	ago, ok := funcs["ago"].(func(time.Time, string, int64) string)
	if !ok {
		t.Fatal("ago is missing")
	}

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	if got := ago(now, "en", 0); got != "never" {
		t.Errorf("ago(0) = %q, want %q", got, "never")
	}

	// The counted branches go through the catalogue's plural forms, which is
	// why they moved: the hand-written version said "1 minutes ago".
	if got := ago(now, "en", now.Add(-90*time.Second).Unix()); got != "1 minute ago" {
		t.Errorf("ago(90 seconds) = %q, want %q", got, "1 minute ago")
	}

	// The clock is the page's, not the process's. A helper still reading
	// time.Now would answer from the wall clock and ignore this one entirely.
	if got := ago(now.Add(time.Hour), "en", now.Unix()); got != "1 hour ago" {
		t.Errorf("ago measured against the wrong clock: %q", got)
	}

	until, ok := funcs["until"].(func(time.Time, string, int64) string)
	if !ok {
		t.Fatal("until is missing")
	}

	if got := until(now, "en", now.Add(-time.Second).Unix()); got != "expired" {
		t.Errorf("until(past) = %q, want %q", got, "expired")
	}

	date, ok := funcs["date"].(func(string, int64) string)
	if !ok {
		t.Fatal("date is missing")
	}

	if got := date("en", 0); got != "—" {
		t.Errorf("date(0) = %q, want an em dash", got)
	}

	translate, ok := funcs["t"].(func(string, string, ...any) string)
	if !ok {
		t.Fatal("t is missing")
	}

	if got := translate("en", "auth.login.title"); got != "Sign in" {
		t.Errorf("t(auth.login.title) = %q", got)
	}

	count, ok := funcs["n"].(func(string, string, int, ...any) string)
	if !ok {
		t.Fatal("n is missing")
	}

	if got := count("en", "auth.sites.count", 1); got != "1 site in this account." {
		t.Errorf("n(auth.sites.count, 1) = %q", got)
	}

	dict, ok := funcs["dict"].(func(...any) map[string]any)
	if !ok {
		t.Fatal("dict is missing")
	}

	built := dict("a", 1, "b", 2)
	if built["a"] != 1 || built["b"] != 2 {
		t.Errorf("dict built %v", built)
	}

	// An odd argument count must not panic — a template typo should render a
	// slightly wrong page, not take the process down.
	if len(dict("a")) != 0 {
		t.Error("an unpaired key should be dropped")
	}
}

// TestTheAccountNavigationFollowsTheScreenBeingRendered pins where the section
// list says the reader is.
//
// The two-factor forms all post to paths under /settings/security and re-render
// that screen, so an exact-match on the path would show Security with
// Preferences marked current — a navigation that lies about where you are.
func TestTheAccountNavigationFollowsTheScreenBeingRendered(t *testing.T) {
	for path, want := range map[string]string{
		"/settings":                        appui.TabAccount,
		"/settings/":                       appui.TabAccount,
		"/settings/security":               appui.TabSecurity,
		"/settings/security/2fa/start":     appui.TabSecurity,
		"/settings/security/2fa/enable":    appui.TabSecurity,
		"/settings/security/2fa/recovery":  appui.TabSecurity,
		"/settings/security/2fa/disable":   appui.TabSecurity,
		"/settings/sessions":               appui.TabDevices,
		"/settings/sessions/revoke":        appui.TabDevices,
		"/settings/team":                   appui.TabTeamPolicy,
		"/settings/somewhere-nobody-built": appui.TabAccount,
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)

		if got := accountTab(request); got != want {
			t.Errorf("%s marked %q current, want %q", path, got, want)
		}
	}
}

// TestAStandaloneScreenShedsTheBar keeps the dead ends quiet.
//
// The verification gate advertises destinations that all bounce straight back
// to it, and the deleted screen belongs to somebody whose account is gone — a
// bar carrying their team and a sign-out button describes a thing that no
// longer exists.
func TestAStandaloneScreenShedsTheBar(t *testing.T) {
	for _, name := range []string{"verify", "verify_failed", "deleted", "error"} {
		if !standalone[name] {
			t.Errorf("%s renders the account bar", name)
		}
	}

	for _, name := range []string{"sites", "settings_account", "settings_security", "site_settings"} {
		if standalone[name] {
			t.Errorf("%s was made standalone and lost its bar", name)
		}
	}
}
