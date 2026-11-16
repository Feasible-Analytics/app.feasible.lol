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
	"time"

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

	return VerifyInstallation(context.Background(), client, InstallCheck{BaseURL: "https://feasible.lol"}, site)
}

// TestSnippetUsesThePerSiteToken checks the default snippet carries the opaque
// path rather than a shared filename. Filter lists name files individually, so
// a customer proxying one well-known name loses their traffic the day it is
// listed.
func TestSnippetUsesThePerSiteToken(t *testing.T) {
	site := &Site{Domain: "example.com"}

	keyer := tracker.NewKeyer(make([]byte, tracker.SecretSize), nil)

	snippet := Snippet("https://feasible.lol", "", keyer, site)

	if strings.Contains(snippet, tracker.PathLegacy) {
		t.Errorf("the default snippet should use the per-site path: %s", snippet)
	}

	if !strings.Contains(snippet, keyer.Path("example.com")) {
		t.Errorf("the snippet should carry this site's token: %s", snippet)
	}

	// The legacy variant is offered too, because it is the exact shape an
	// existing installation already has and what a tag manager needs.
	legacy := SnippetLegacy("https://feasible.lol", "", site)

	if !strings.Contains(legacy, `data-domain="example.com"`) {
		t.Errorf("the legacy snippet should carry data-domain: %s", legacy)
	}
}

// TestSnippetIsOneURLWithNothingElseToSet checks that pointing the script at a
// cache changes the host and nothing else.
//
// The snippet must never name an endpoint of its own. The script reports to
// wherever it was loaded from, which is what makes a customer's reverse proxy
// work untouched, and a second URL in the tag is a second thing to paste wrong.
func TestSnippetIsOneURLWithNothingElseToSet(t *testing.T) {
	site := &Site{Domain: "example.com"}
	keyer := tracker.NewKeyer(make([]byte, tracker.SecretSize), nil)

	snippet := Snippet("https://app.feasible.lol", "https://cdn.feasible.lol", keyer, site)

	if !strings.Contains(snippet, `src="https://cdn.feasible.lol/js/`) {
		t.Errorf("the script should load from the script origin: %s", snippet)
	}

	for _, unwanted := range []string{"api=", "data-api"} {
		if strings.Contains(snippet, unwanted) {
			t.Errorf("the snippet should carry no endpoint (%s): %s", unwanted, snippet)
		}
	}

	legacy := SnippetLegacy("https://app.feasible.lol", "https://cdn.feasible.lol", site)

	if !strings.Contains(legacy, `src="https://cdn.feasible.lol/js/`) || strings.Contains(legacy, "data-api") {
		t.Errorf("the legacy snippet should load from the script origin and name no endpoint: %s", legacy)
	}

	// With nothing configured the application serves its own script, which is
	// every self-hosted install.
	plain := Snippet("https://app.feasible.lol", "", keyer, site)

	if !strings.Contains(plain, `src="https://app.feasible.lol/js/`) {
		t.Errorf("an unconfigured install should serve its own script: %s", plain)
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
		result := VerifyInstallation(context.Background(), handler.Verifier, InstallCheck{BaseURL: "https://feasible.lol"}, &Site{Domain: domain})

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
		"the per-site snippet":      Snippet("https://feasible.lol", "", keyer, site),
		"the legacy snippet":        SnippetLegacy("https://feasible.lol", "", site),
		"the snippet with no keyer": Snippet("https://feasible.lol", "", nil, site),
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
		"the whole snippet": {SnippetLegacy("https://feasible.lol", "", site), VerifyFound},
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
			page := "<html><head>" + SnippetLegacy("https://feasible.lol", "", site) + "</head><body></body></html>"

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

// hostedCheck is the hosted service's shape: the application on one host and
// the script on another, with per-site paths.
func hostedCheck() InstallCheck {
	return InstallCheck{
		BaseURL:       "https://app.feasible.lol",
		ScriptBaseURL: "https://cdn.feasible.lol",
		Keyer:         tracker.NewKeyer(make([]byte, tracker.SecretSize), nil),
	}
}

// verifyPage runs the check for example.com against a page with the given
// body and Content-Security-Policy.
func verifyPage(t *testing.T, check InstallCheck, policy, body string) VerifyResult {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if policy != "" {
			w.Header().Set("Content-Security-Policy", policy)
		}

		writeTestResponse(t, w, body)
	}))
	t.Cleanup(server.Close)

	client := &http.Client{Transport: rewriteTo(server.URL)}

	return VerifyInstallation(context.Background(), client, check, &Site{Domain: "example.com"})
}

// TestVerifyAcceptsTheSnippetWeHandOut checks the exact text the install
// screen shows passes its own check, for both variants and on every host a
// hosted or self-hosted server gives out.
func TestVerifyAcceptsTheSnippetWeHandOut(t *testing.T) {
	site := &Site{Domain: "example.com"}
	hosted := hostedCheck()
	selfHosted := InstallCheck{BaseURL: "https://stats.example.org", Keyer: hosted.Keyer}

	cases := map[string]struct {
		check InstallCheck
		body  string
	}{
		"hosted, per-site":      {hosted, Snippet(hosted.BaseURL, hosted.ScriptBaseURL, hosted.Keyer, site)},
		"hosted, data-domain":   {hosted, SnippetLegacy(hosted.BaseURL, hosted.ScriptBaseURL, site)},
		"hosted, older install": {hosted, Snippet(hosted.BaseURL, "", hosted.Keyer, site)},
		"hosted, older legacy":  {hosted, SnippetLegacy(hosted.BaseURL, "", site)},
		"self-hosted":           {selfHosted, Snippet(selfHosted.BaseURL, "", selfHosted.Keyer, site)},
		"self-hosted, legacy":   {selfHosted, SnippetLegacy(selfHosted.BaseURL, "", site)},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			result := verifyPage(t, c.check, "", "<html><head>"+c.body+"</head></html>")

			if result.Outcome != VerifyFound {
				t.Errorf("want %q, got %q: %s", VerifyFound, result.Outcome, result.Message)
			}
		})
	}
}

// TestVerifyReadsTagsHoweverTheyAreWritten checks the forms a real page serves
// the tag in after a minifier, a CMS or a person has been at it.
func TestVerifyReadsTagsHoweverTheyAreWritten(t *testing.T) {
	check := hostedCheck()
	path := check.Keyer.Path("example.com")
	src := "https://cdn.feasible.lol" + path

	cases := map[string]string{
		"unquoted":              `<script defer src=` + src + `></script>`,
		"single-quoted":         `<script defer src='` + src + `'></script>`,
		"src first":             `<script src="` + src + `" defer></script>`,
		"async":                 `<script async src="` + src + `"></script>`,
		"uppercase":             `<SCRIPT DEFER SRC="` + src + `"></SCRIPT>`,
		"spaces and newlines":   "<script\n  defer\n  src = \"" + src + "\"\n></script>",
		"protocol-relative":     `<script defer src="//cdn.feasible.lol` + path + `"></script>`,
		"query string":          `<script defer src="` + src + `?hash=1"></script>`,
		"a > in another value":  `<script data-note="a > b" defer src="` + src + `"></script>`,
		"extra attributes":      `<script type="text/javascript" id="feasible-js" defer data-wp-strategy="defer" src="` + src + `"></script>`,
		"unquoted data-domain":  `<script defer data-domain=example.com src=https://cdn.feasible.lol/js/script.js></script>`,
		"data-domain after src": `<script src='https://app.feasible.lol/js/script.js' data-domain='www.example.com' async></script>`,
		"after other scripts":   `<script src="/app.js"></script><script>var a = "<script>";</script><script defer src=` + src + `></script>`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			result := verifyPage(t, check, "", body)

			if result.Outcome != VerifyFound {
				t.Errorf("want %q, got %q: %s", VerifyFound, result.Outcome, result.Message)
			}
		})
	}
}

// TestVerifyRecognisesAProxiedInstall checks a script served from the
// customer's own domain, where no hostname of ours appears on the page.
func TestVerifyRecognisesAProxiedInstall(t *testing.T) {
	check := hostedCheck()
	file := strings.TrimPrefix(check.Keyer.Path("example.com"), tracker.PathPrefix)

	cases := map[string]string{
		"per-site path, relative":   `<script defer data-api="/stats/api/event" src="/stats/js/` + file + `"></script>`,
		"per-site path, own host":   `<script defer data-api=/stats/api/event src=https://example.com/stats/js/` + file + `></script>`,
		"shared path, data-domain":  `<script defer data-domain="example.com" data-api="/stats/api/event" src="/stats/js/script.js"></script>`,
		"renamed path, data-domain": `<script defer data-domain="example.com" data-api="https://example.com/wp-json/k3/e" src="https://example.com/k3/m9.js"></script>`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			// A policy that only allows the site itself still loads a script
			// the site serves.
			result := verifyPage(t, check, "script-src 'self' 'unsafe-inline'", body)

			if result.Outcome != VerifyFound {
				t.Errorf("want %q, got %q: %s", VerifyFound, result.Outcome, result.Message)
			}
		})
	}
}

// TestVerifyIgnoresScriptsThatAreNotOurs checks a data-domain alone is not
// taken for our snippet, because other analytics scripts carry one too.
func TestVerifyIgnoresScriptsThatAreNotOurs(t *testing.T) {
	cases := map[string]string{
		"another tool with data-domain": `<script defer data-domain="example.com" src="https://stats.example.net/js/tracker.js"></script>`,
		"an unrelated script":           `<script src="https://example.com/static/app.js" defer></script>`,
		"our name in the text only":     `<p>We moved to cdn.feasible.lol last week.</p><script src="/app.js"></script>`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			result := verifyPage(t, hostedCheck(), "", body)

			if result.Outcome != VerifyMissing {
				t.Errorf("want %q, got %q: %s", VerifyMissing, result.Outcome, result.Message)
			}
		})
	}
}

// TestVerifyReportsAnotherSitesPath checks the copy-paste mistake in its
// per-site form, where there is no data-domain to read.
func TestVerifyReportsAnotherSitesPath(t *testing.T) {
	check := hostedCheck()
	other := `<script defer src="https://cdn.feasible.lol` + check.Keyer.Path("other.example.com") + `"></script>`

	result := verifyPage(t, check, "", other)

	if result.Outcome != VerifyWrongDomain {
		t.Errorf("want %q, got %q: %s", VerifyWrongDomain, result.Outcome, result.Message)
	}

	// A page carrying both is installed: the right one is what counts.
	both := other + `<script defer src="https://cdn.feasible.lol` + check.Keyer.Path("example.com") + `"></script>`

	if result := verifyPage(t, check, "", both); result.Outcome != VerifyFound {
		t.Errorf("want %q, got %q: %s", VerifyFound, result.Outcome, result.Message)
	}
}

// TestVerifyChecksThePolicyAgainstTheScriptHost checks the policy is read for
// the host the tag loads from, which is not the application's.
func TestVerifyChecksThePolicyAgainstTheScriptHost(t *testing.T) {
	check := hostedCheck()
	body := `<script defer src=https://cdn.feasible.lol` + check.Keyer.Path("example.com") + `></script>`

	cases := map[string]VerifyOutcome{
		"script-src 'self' https://cdn.feasible.lol": VerifyFound,
		"script-src 'self' https:":                   VerifyFound,
		"script-src 'nonce-abc123' 'strict-dynamic'": VerifyFound,
		"script-src 'self' https://app.feasible.lol": VerifyBlockedByCSP,
		"default-src 'self'":                         VerifyBlockedByCSP,
	}

	for policy, want := range cases {
		result := verifyPage(t, check, policy, body)

		if result.Outcome != want {
			t.Errorf("%q: want %q, got %q: %s", policy, want, result.Outcome, result.Message)
		}

		if want == VerifyBlockedByCSP && !strings.Contains(result.Message, "cdn.feasible.lol") {
			t.Errorf("%q: the message should name the host to allow: %s", policy, result.Message)
		}
	}
}

// TestMissingSnippetSaysWhatATagManagerUserNeeds checks the message for a page
// with nothing in its HTML is true for a tag manager install as well as for a
// page that was never deployed.
func TestMissingSnippetSaysWhatATagManagerUserNeeds(t *testing.T) {
	result := verifyPage(t, hostedCheck(), "", `<script src="https://www.googletagmanager.com/gtm.js?id=GTM-XXXX" async></script>`)

	if result.Outcome != VerifyMissing || result.OK() {
		t.Fatalf("want %q, got %q", VerifyMissing, result.Outcome)
	}

	for _, want := range []string{"tag manager", "first visit"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("the message should mention %q: %s", want, result.Message)
		}
	}
}

// TestRecentVisitStandsInForAnUnseenSnippet checks that visits arriving turn
// "not in the HTML" into a pass, and that nothing else is changed by them.
func TestRecentVisitStandsInForAnUnseenSnippet(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	missing := VerifyResult{Outcome: VerifyMissing, URL: "https://example.com/", Message: "missing"}

	recent := missing.WithRecentVisit(now.Add(-time.Hour), now)
	if recent.Outcome != VerifyCollecting || !recent.OK() {
		t.Errorf("a visit an hour ago should pass, got %q", recent.Outcome)
	}

	if !strings.Contains(recent.Message, "Visits are arriving") {
		t.Errorf("the message should say why it passed: %s", recent.Message)
	}

	if stale := missing.WithRecentVisit(now.Add(-48*time.Hour), now); stale.Outcome != VerifyMissing {
		t.Errorf("a visit two days ago proves nothing about today, got %q", stale.Outcome)
	}

	if never := missing.WithRecentVisit(time.Time{}, now); never.Outcome != VerifyMissing {
		t.Errorf("no visit at all should stay missing, got %q", never.Outcome)
	}

	wrong := VerifyResult{Outcome: VerifyWrongDomain}
	if got := wrong.WithRecentVisit(now, now); got.Outcome != VerifyWrongDomain {
		t.Errorf("only a missing snippet is overridden, got %q", got.Outcome)
	}
}
