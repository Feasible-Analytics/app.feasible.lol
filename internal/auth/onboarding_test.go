//
// onboarding_test.go
// The snippet, and the installation check that fetches a real page.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/tracker"
)

// verifyAgainst runs the installation check against a test server, rewriting
// the request so the check can be exercised without a real domain.
func verifyAgainst(t *testing.T, handler http.HandlerFunc, site *Site) VerifyResult {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := &http.Client{Transport: rewriteTo(server.URL)}

	return VerifyInstallation(context.Background(), client, "https://feasible.lol", site)
}

// TestSnippetUsesThePerSiteToken checks the default snippet carries the opaque
// path rather than a shared filename. Filter lists name files individually, so
// a customer proxying one well-known name loses their traffic the day it is
// listed.
func TestSnippetUsesThePerSiteToken(t *testing.T) {
	site := &Site{Domain: "example.com"}

	keyer := tracker.NewKeyer(make([]byte, tracker.SecretSize), nil)

	snippet := Snippet("https://feasible.lol", keyer, site)

	if strings.Contains(snippet, tracker.PathLegacy) {
		t.Errorf("the default snippet should use the per-site path: %s", snippet)
	}

	if !strings.Contains(snippet, keyer.Path("example.com")) {
		t.Errorf("the snippet should carry this site's token: %s", snippet)
	}

	// The legacy variant is offered too, because it is the exact shape an
	// existing installation already has and what a tag manager needs.
	legacy := SnippetLegacy("https://feasible.lol", site)

	if !strings.Contains(legacy, `data-domain="example.com"`) {
		t.Errorf("the legacy snippet should carry data-domain: %s", legacy)
	}
}

// TestVerifyFindsTheSnippet checks the happy path — the answer the waiting
// screen is hoping for.
func TestVerifyFindsTheSnippet(t *testing.T) {
	result := verifyAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		writeTestResponse(t, w, `<html><head><script defer src="https://feasible.lol/js/fs-abc.js"></script></head></html>`)
	}, &Site{Domain: "example.com"})

	if result.Outcome != VerifyFound {
		t.Errorf("want %q, got %q: %s", VerifyFound, result.Outcome, result.Message)
	}

	if !result.OK() {
		t.Error("a found snippet should report OK")
	}
}

// TestVerifyReportsAMissingSnippet checks the most common real answer: the page
// loads fine and the change was never deployed.
func TestVerifyReportsAMissingSnippet(t *testing.T) {
	result := verifyAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		writeTestResponse(t, w, `<html><head><title>Hello</title></head><body>nothing here</body></html>`)
	}, &Site{Domain: "example.com"})

	if result.Outcome != VerifyMissing {
		t.Errorf("want %q, got %q: %s", VerifyMissing, result.Outcome, result.Message)
	}
}

// TestVerifyReportsTheWrongDomain checks the copy-paste mistake. The page looks
// instrumented and every pageview is being filed under somebody else's site,
// which is why this is its own outcome rather than "missing".
func TestVerifyReportsTheWrongDomain(t *testing.T) {
	result := verifyAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		writeTestResponse(t, w, `<script defer data-domain="other.example.com" src="https://feasible.lol/js/script.js"></script>`)
	}, &Site{Domain: "example.com"})

	if result.Outcome != VerifyWrongDomain {
		t.Errorf("want %q, got %q: %s", VerifyWrongDomain, result.Outcome, result.Message)
	}

	if !strings.Contains(result.Message, "other.example.com") {
		t.Errorf("the message should name the domain we found: %s", result.Message)
	}

	// A snippet listing several domains is legitimate — that is how one script
	// serves a site and its www variant.
	multi := verifyAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		writeTestResponse(t, w, `<script defer data-domain="example.com,www.example.com" src="https://feasible.lol/js/script.js"></script>`)
	}, &Site{Domain: "example.com"})

	if multi.Outcome != VerifyFound {
		t.Errorf("a multi-domain snippet should be accepted, got %q", multi.Outcome)
	}
}

// TestVerifyReportsACSPBlock checks the outcome nobody can diagnose from the
// HTML alone: the snippet is present and the browser will refuse to load it.
func TestVerifyReportsACSPBlock(t *testing.T) {
	result := verifyAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' cdn.example.com")
		writeTestResponse(t, w, `<script defer src="https://feasible.lol/js/fs-abc.js"></script>`)
	}, &Site{Domain: "example.com"})

	if result.Outcome != VerifyBlockedByCSP {
		t.Errorf("want %q, got %q: %s", VerifyBlockedByCSP, result.Outcome, result.Message)
	}

	// A policy that does allow us must not raise a false alarm, because it
	// would send somebody to edit a security header for no reason.
	allowed := verifyAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", "script-src 'self' feasible.lol")
		writeTestResponse(t, w, `<script defer src="https://feasible.lol/js/fs-abc.js"></script>`)
	}, &Site{Domain: "example.com"})

	if allowed.Outcome != VerifyFound {
		t.Errorf("an allowing policy should not be reported as a block, got %q", allowed.Outcome)
	}
}

// TestVerifyReportsAnUnreachablePage checks the fourth outcome, which is what
// somebody gets when the domain does not resolve or the server errors.
func TestVerifyReportsAnUnreachablePage(t *testing.T) {
	result := verifyAgainst(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, &Site{Domain: "example.com"})

	if result.Outcome != VerifyUnreachable {
		t.Errorf("want %q, got %q: %s", VerifyUnreachable, result.Outcome, result.Message)
	}

	if result.StatusCode != http.StatusInternalServerError {
		t.Errorf("the status should be reported, got %d", result.StatusCode)
	}
}

// TestCSPAllowsIsForgiving checks that a policy we cannot parse is treated as
// permissive. Telling somebody their CSP is blocking us when it is not is worse
// than saying nothing.
func TestCSPAllowsIsForgiving(t *testing.T) {
	cases := map[string]bool{
		"":                                       true,
		"frame-ancestors 'none'":                 true,
		"default-src *":                          true,
		"script-src 'self' https://feasible.lol": true,
		"script-src 'self'":                      false,
		"default-src 'self'":                     false,
	}

	for policy, want := range cases {
		if got := cspAllows(policy, "feasible.lol"); got != want {
			t.Errorf("cspAllows(%q) = %v, want %v", policy, got, want)
		}
	}
}

// TestInstallPlatformsCoverTheCommonOnes checks the list is complete and that
// every entry carries the platform-specific warning, which is the only reason a
// list like this beats one generic instruction.
func TestInstallPlatformsCoverTheCommonOnes(t *testing.T) {
	want := []string{"html", "wordpress", "nextjs", "nuxt", "astro", "shopify", "webflow", "squarespace", "ghost", "framer", "gtm"}

	got := map[string]InstallPlatform{}
	for _, platform := range InstallPlatforms() {
		got[platform.ID] = platform
	}

	for _, id := range want {
		platform, ok := got[id]
		if !ok {
			t.Errorf("no instructions for %q", id)
			continue
		}

		if len(platform.Steps) == 0 {
			t.Errorf("%q has no steps", id)
		}

		if platform.Note == "" {
			t.Errorf("%q has no platform-specific warning", id)
		}
	}
}

// TestNormaliseHostIgnoresWhatDoesNotMatter checks the comparison used on both
// sides of the domain and CSP checks.
func TestNormaliseHostIgnoresWhatDoesNotMatter(t *testing.T) {
	cases := map[string]string{
		"https://Feasible.LOL/": "feasible.lol",
		"'feasible.lol'":        "feasible.lol",
		"www.feasible.lol":      "feasible.lol",
		"feasible.lol:8443":     "feasible.lol",
	}

	for input, want := range cases {
		if got := normaliseHost(input); got != want {
			t.Errorf("normaliseHost(%q) = %q, want %q", input, got, want)
		}
	}

	if got := hostOf("https://feasible.lol/app"); got != "feasible.lol" {
		t.Errorf("hostOf: want %q, got %q", "feasible.lol", got)
	}
}

// rewriteTo sends every request to a test server instead of its real host, so a
// check with a hard-coded URL can be exercised without a network.
func rewriteTo(base string) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		target, err := http.NewRequestWithContext(r.Context(), r.Method, base+r.URL.Path, r.Body)
		if err != nil {
			return nil, err
		}

		target.Header = r.Header

		return http.DefaultTransport.RoundTrip(target)
	})
}

// roundTripFunc adapts a function to the RoundTripper interface.
type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls the function.
func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// TestInstallationCheckCannotReachAPrivateAddress covers the half a domain
// check cannot.
//
// The domain is a value the customer typed, and the result reports the status
// code, the CSP header and the dial error back to them. Without a guarded
// dialler, "Verify installation" is a way to ask the server what it can see on
// its own network and read the answer.
func TestInstallationCheckCannotReachAPrivateAddress(t *testing.T) {
	handler, err := NewHandler(Options{Log: logger.New(logger.Options{Level: "error", Output: io.Discard})})
	if err != nil {
		t.Fatalf("build handler: %v", err)
	}

	for _, domain := range []string{"127.0.0.1", "169.254.169.254", "10.0.0.1"} {
		result := VerifyInstallation(context.Background(), handler.Verifier, "https://feasible.lol", &Site{Domain: domain})

		if result.Outcome != VerifyUnreachable {
			t.Fatalf("%s was reachable: outcome = %q", domain, result.Outcome)
		}
		if result.StatusCode != 0 {
			t.Fatalf("%s answered with status %d, so the dial was not refused", domain, result.StatusCode)
		}
	}
}
