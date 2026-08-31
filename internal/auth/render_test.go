//
// render_test.go
// Template parsing, the helpers the templates call, and the sparkline geometry.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"strings"
	"testing"
	"time"
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

// TestTemplateHelpers checks the functions the templates are allowed to call,
// since a wrong one shows up as wrong text on a page rather than a failure
// anywhere.
//
// Every one of them takes the locale first, because the templates are parsed
// once at start-up and a helper that closed over a language would answer in
// whichever one the process happened to be built with.
func TestTemplateHelpers(t *testing.T) {
	funcs := templateFuncs()

	ago, ok := funcs["ago"].(func(string, int64) string)
	if !ok {
		t.Fatal("ago is missing")
	}

	if got := ago("en", 0); got != "never" {
		t.Errorf("ago(0) = %q, want %q", got, "never")
	}

	// The counted branches go through the catalogue's plural forms, which is
	// why they moved: the hand-written version said "1 minutes ago".
	if got := ago("en", time.Now().Add(-90*time.Second).Unix()); got != "1 minute ago" {
		t.Errorf("ago(90 seconds) = %q, want %q", got, "1 minute ago")
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
