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
	"os"
	"path/filepath"
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

// TestBothSnippetsCarryTheQueueStub ties the snippet to the bundle.
//
// The bundle replays a queue at install time, and the end-to-end suite proves
// the replay works — against a fixture that writes the stub itself. Only a Go
// assertion can say the page a customer pastes creates that queue at all.
func TestBothSnippetsCarryTheQueueStub(t *testing.T) {
	keyer := tracker.NewKeyer(make([]byte, tracker.SecretSize), nil)
	site := &Site{Domain: "example.com"}

	for name, snippet := range map[string]string{
		"the per-site snippet":      Snippet("https://feasible.lol", keyer, site),
		"the legacy snippet":        SnippetLegacy("https://feasible.lol", site),
		"the snippet with no keyer": Snippet("https://feasible.lol", nil, site),
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(snippet, tracker.QueueStub) {
				t.Fatalf("no queueing stub:\n%s", snippet)
			}

			// Before, or it is a stub for calls that have already thrown.
			if strings.Index(snippet, tracker.QueueStub) > strings.Index(snippet, "<script defer") {
				t.Errorf("the stub is after the script tag:\n%s", snippet)
			}

			// The queue the bundle reads by name.
			if !strings.Contains(snippet, "window.feasible.q") {
				t.Errorf("the stub does not fill the queue the bundle drains:\n%s", snippet)
			}
		})
	}
}

// TestTheQueueStubDoesNotSeizeTheGlobal is what makes the snippet safe to paste
// twice, and safe on a page where another tool already owns this name.
//
// An unguarded assignment would throw away a queue the first copy had already
// filled, and would stop a tool that keeps its configuration on its own global
// dead with no error anywhere.
func TestTheQueueStubDoesNotSeizeTheGlobal(t *testing.T) {
	if !strings.Contains(tracker.QueueStub, "window.feasible=window.feasible||") {
		t.Errorf("the stub assigns unconditionally: %s", tracker.QueueStub)
	}

	if !strings.Contains(tracker.QueueStub, "window.feasible.q=window.feasible.q||") {
		t.Errorf("the stub replaces the queue rather than appending to it: %s", tracker.QueueStub)
	}
}

// TestTheFixtureStubIsTheOneWeShip ties the end-to-end suite to the generator.
//
// The JavaScript tests prove the bundle drains a queue; this proves the queue
// they build is the one a customer's page actually creates. Whitespace is
// ignored because the fixture is formatted and the snippet is one line.
func TestTheFixtureStubIsTheOneWeShip(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "tracker", "tests", "fixtures", "queue.html"))
	if err != nil {
		t.Fatal(err)
	}

	squash := func(s string) string { return strings.Join(strings.Fields(s), "") }

	if !strings.Contains(squash(string(fixture)), squash(tracker.QueueStub)) {
		t.Error("the queue fixture writes a stub we do not ship, so the replay test proves nothing " +
			"about what a customer's page does")
	}
}

// TestVerifyReadsTheScriptTagAndNotTheStub checks the verifier still answers
// about the tag that loads us, now that the snippet is two tags.
//
// A page with only the stub is not installed, and a page carrying both is. The
// pair is what makes this an assertion rather than a hope: the "installed" case
// alone passes whether or not the inline tag is being read as the script tag.
func TestVerifyReadsTheScriptTagAndNotTheStub(t *testing.T) {
	site := &Site{Domain: "example.com"}

	for name, test := range map[string]struct {
		body string
		want VerifyOutcome
	}{
		"the whole snippet": {SnippetLegacy("https://feasible.lol", site), VerifyFound},
		"the stub alone":    {tracker.QueueStub, VerifyMissing},
	} {
		t.Run(name, func(t *testing.T) {
			page := "<html><head>" + test.body + "</head><body></body></html>"

			result := verifyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, page)
			}, site)

			if result.Outcome != test.want {
				t.Errorf("the verifier reported %q, want %q: %s", result.Outcome, test.want, result.Message)
			}
		})
	}
}

// TestAPolicyThatRefusesInlineScriptsIsReported is the failure this change
// would otherwise introduce quietly.
//
// The bundle still loads under a strict policy, so the install is not blocked —
// but the stub does not run, and every event fired before the bundle arrives is
// lost with the verifier saying nothing is wrong. The site most careful about
// its headers is the one that would never find out.
func TestAPolicyThatRefusesInlineScriptsIsReported(t *testing.T) {
	site := &Site{Domain: "example.com"}

	for name, test := range map[string]struct {
		policy string
		warned bool
	}{
		"a strict policy":            {"script-src 'self' https://feasible.lol", true},
		"a policy allowing inline":   {"script-src 'self' 'unsafe-inline' https://feasible.lol", false},
		"a policy carrying our hash": {"script-src 'self' " + tracker.QueueStubHash + " https://feasible.lol", false},

		// A browser ignores 'unsafe-inline' once any hash is present, so the
		// policy that looks permissive is the one that is not.
		"unsafe-inline beside another hash": {
			"script-src 'self' 'unsafe-inline' 'sha256-abc=' https://feasible.lol", true},

		// Whether the customer's template puts its nonce on our tag is not
		// something the HTML can answer, so it is not something to warn about.
		"a policy using a nonce": {"script-src 'self' 'nonce-r4nd0m' https://feasible.lol", false},

		"no policy at all": {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			page := "<html><head>" + SnippetLegacy("https://feasible.lol", site) + "</head><body></body></html>"

			result := verifyAgainst(t, func(w http.ResponseWriter, r *http.Request) {
				if test.policy != "" {
					w.Header().Set("Content-Security-Policy", test.policy)
				}

				_, _ = io.WriteString(w, page)
			}, site)

			if result.Outcome != VerifyFound {
				t.Fatalf("the verifier reported %q: %s", result.Outcome, result.Message)
			}

			if warned := strings.Contains(result.Message, tracker.QueueStubHash); warned != test.warned {
				t.Errorf("the message %q warns about inline scripts = %v, want %v",
					result.Message, warned, test.warned)
			}
		})
	}
}
