//
// security_test.go
// HTTPS redirects, framing and the parameters we refuse to trust.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package sharing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPIsRedirectedToHTTPS is the acceptance criterion.
//
// A shared link's URL is the whole credential for whoever holds it, so putting
// it on the wire in clear text hands it to anything between the two ends.
func TestHTTPIsRedirectedToHTTPS(t *testing.T) {
	security := NewSecurity("https://stats.example.com")

	if !security.RequireHTTPS {
		t.Fatal("an https base URL did not turn on the redirect")
	}

	plain := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/share/abc?period=7d", nil)

	if !security.Apply(plain, request) {
		t.Fatal("a plain HTTP request was not redirected")
	}

	if plain.Code != http.StatusPermanentRedirect {
		t.Fatalf("the redirect is %d, want 308 — a 301 would turn the password POST into a GET", plain.Code)
	}

	if location := plain.Header().Get("Location"); location != "https://stats.example.com/share/abc?period=7d" {
		t.Fatalf("redirected to %q — the query string or the host was lost", location)
	}

	secure := httptest.NewRecorder()
	forwarded := httptest.NewRequest(http.MethodGet, "/share/abc", nil)
	forwarded.Header.Set("X-Forwarded-Proto", "https")

	if security.Apply(secure, forwarded) {
		t.Fatal("an HTTPS request was redirected")
	}
}

// TestAPlainInstallDoesNotRedirectItselfIntoALoop checks the local case.
// Redirecting to a listener that does not exist is a loop rather than a
// protection.
func TestAPlainInstallDoesNotRedirectItselfIntoALoop(t *testing.T) {
	security := NewSecurity("http://localhost:19300")

	recorder := httptest.NewRecorder()

	if security.Apply(recorder, httptest.NewRequest(http.MethodGet, "/share/abc", nil)) {
		t.Fatal("an http install redirected")
	}
}

// TestTheRedirectHostComesFromTheConfiguration checks that a request with an
// attacker-chosen Host header cannot make us redirect to their domain.
func TestTheRedirectHostComesFromTheConfiguration(t *testing.T) {
	security := NewSecurity("https://stats.example.com")

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/share/abc", nil)
	request.Host = "evil.example"

	security.Apply(recorder, request)

	if location := recorder.Header().Get("Location"); !strings.HasPrefix(location, "https://stats.example.com/") {
		t.Fatalf("redirected to %q — the request's own Host was believed", location)
	}
}

// TestFramingIsDeniedByDefaultAndOpenedDeliberately checks both directions of
// the framing policy.
func TestFramingIsDeniedByDefaultAndOpenedDeliberately(t *testing.T) {
	denied := httptest.NewRecorder()
	DenyFraming(denied)

	if denied.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("DenyFraming did not set X-Frame-Options")
	}

	if !strings.Contains(denied.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("DenyFraming did not set frame-ancestors")
	}

	allowed := httptest.NewRecorder()
	DenyFraming(allowed)
	AllowFraming(allowed)

	if allowed.Header().Get("X-Frame-Options") != "" {
		t.Error("AllowFraming left X-Frame-Options in place, so the embed will not render")
	}

	if !strings.Contains(allowed.Header().Get("Content-Security-Policy"), "frame-ancestors *") {
		t.Error("AllowFraming did not open frame-ancestors")
	}
}

// TestOnlyAProvableColourIsAccepted checks the background parameter.
//
// The value ends up as a colour the page paints itself with, so anything that
// is not provably a colour is a way to put attacker-chosen text into the
// document.
func TestOnlyAProvableColourIsAccepted(t *testing.T) {
	accepted := map[string]string{
		"#ffffff":       "#ffffff",
		"ffffff":        "#ffffff",
		"#FFF":          "#fff",
		"transparent":   "transparent",
		" TRANSPARENT ": "transparent",
	}

	for input, want := range accepted {
		if got := NormaliseBackground(input); got != want {
			t.Errorf("NormaliseBackground(%q) = %q, want %q", input, got, want)
		}
	}

	rejected := []string{
		"",
		"red; background-image: url(https://evil.example/x)",
		"url(javascript:alert(1))",
		"#12345",
		"</style><script>alert(1)</script>",
		"rgb(255,0,0)",
	}

	for _, input := range rejected {
		if got := NormaliseBackground(input); got != "" {
			t.Errorf("NormaliseBackground(%q) = %q, want it rejected", input, got)
		}
	}
}

// TestOnlyThreeThemesAreAccepted checks the theme parameter.
func TestOnlyThreeThemesAreAccepted(t *testing.T) {
	for _, input := range []string{"light", "DARK", " system "} {
		if NormaliseTheme(input) == "" {
			t.Errorf("NormaliseTheme(%q) was rejected", input)
		}
	}

	for _, input := range []string{"", "solarized", "dark; --x: y"} {
		if got := NormaliseTheme(input); got != "" {
			t.Errorf("NormaliseTheme(%q) = %q, want it rejected", input, got)
		}
	}
}

// TestTheCookieSignatureIsPerLinkAndPerSecret checks that a cookie solved for
// one link cannot be replayed against another, and that rotating the secret
// invalidates every outstanding cookie.
func TestTheCookieSignatureIsPerLinkAndPerSecret(t *testing.T) {
	secret := DeriveSecret([]byte("root secret"))
	other := DeriveSecret([]byte("rotated secret"))

	signature := SignSlug(secret, "abc")

	if !ValidSignature(secret, "abc", signature) {
		t.Fatal("a signature did not verify against its own slug")
	}

	if ValidSignature(secret, "def", signature) {
		t.Fatal("a cookie for one link verified against another")
	}

	if ValidSignature(other, "abc", signature) {
		t.Fatal("a cookie survived a secret rotation")
	}
}

// TestDeriveSecretIsDomainSeparated checks that the share cookie key is not the
// same value as the script secret it comes from, so the two uses of one root
// secret are not interchangeable.
func TestDeriveSecretIsDomainSeparated(t *testing.T) {
	root := []byte("the script secret")

	if string(DeriveSecret(root)) == string(root) {
		t.Fatal("the derived secret is the root secret")
	}
}

// TestIsHTTPSBelievesTheProxyHeader checks the one place a forwarded header is
// trusted, and why: the app listens on loopback and never terminates TLS, so
// r.TLS is nil on requests that were HTTPS the whole way to the proxy.
func TestIsHTTPSBelievesTheProxyHeader(t *testing.T) {
	cases := map[string]bool{
		"https":       true,
		"HTTPS":       true,
		"https, http": true,
		"http":        false,
		"":            false,
	}

	for header, want := range cases {
		request := httptest.NewRequest(http.MethodGet, "/share/abc", nil)

		if header != "" {
			request.Header.Set("X-Forwarded-Proto", header)
		}

		if got := IsHTTPS(request); got != want {
			t.Errorf("IsHTTPS with X-Forwarded-Proto %q = %v, want %v", header, got, want)
		}
	}
}
