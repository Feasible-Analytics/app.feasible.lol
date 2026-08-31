//
// negotiate_test.go
// Tests for choosing a language, in the order a reader expects.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package i18n

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// threeLanguages is a catalogue with English, German and French in it. The
// negotiation tests need a fixed set to match against; using the product's own
// locales would make them fail the day somebody adds a language.
func threeLanguages(t *testing.T) *Catalogue {
	t.Helper()

	catalogue, err := Load(fstest.MapFS{
		"locales/en/a.json": &fstest.MapFile{Data: []byte(`{"one":"One"}`)},
		"locales/de/a.json": &fstest.MapFile{Data: []byte(`{"one":"Eins"}`)},
		"locales/fr/a.json": &fstest.MapFile{Data: []byte(`{"one":"Un"}`)},
	}, "locales")
	if err != nil {
		t.Fatalf("the catalogue would not load: %v", err)
	}

	return catalogue
}

// request builds one request with the header, cookie and query a case is about.
func request(target, accept, cookie string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)

	if accept != "" {
		r.Header.Set("Accept-Language", accept)
	}

	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
	}

	return r
}

// TestNegotiationPrecedence covers the whole order in one table, because the
// order is the thing being tested: any two of these rules in the wrong sequence
// produce a page in a language somebody did not ask for, and the complaint that
// follows is always "it keeps resetting".
func TestNegotiationPrecedence(t *testing.T) {
	catalogue := threeLanguages(t)

	cases := []struct {
		name   string
		target string
		accept string
		cookie string
		want   string
	}{
		{"nothing asked for is English", "/", "", "", "en"},
		{"the query parameter wins", "/?lang=fr", "de", "de", "fr"},
		{"the cookie beats the browser default", "/", "de", "fr", "fr"},
		{"the browser default is used when nothing else was chosen", "/", "de", "", "de"},
		{"a language we do not have falls through to the next rule", "/?lang=ja", "de", "", "de"},
		{"a region subtag matches its base language", "/", "de-AT", "", "de"},
		{"an unknown cookie is ignored rather than served", "/", "fr", "zz", "fr"},
		{"case does not matter", "/?lang=DE", "", "", "de"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := catalogue.Negotiate(request(tc.target, tc.accept, tc.cookie)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestQualityValuesAreHonoured is the bug a naive implementation ships: the
// order of the tags in Accept-Language is not the preference order, so
// "en;q=0.2, de;q=0.9" asks for German and reading it left to right serves
// English. That is what makes a European visitor's browser setting look
// ignored.
func TestQualityValuesAreHonoured(t *testing.T) {
	catalogue := threeLanguages(t)

	cases := []struct {
		accept string
		want   string
	}{
		{"en;q=0.2, de;q=0.9", "de"},
		{"de;q=0.9, fr;q=0.95", "fr"},
		{"ja, de;q=0.8", "de"},
		{"de;q=0, fr", "fr"},
		{"*", "en"},
		{"de;q=nonsense", "de"},
		{"fr, de", "fr"},
	}

	for _, tc := range cases {
		if got := catalogue.Negotiate(request("/", tc.accept, "")); got != tc.want {
			t.Fatalf("%q resolved to %q, want %q", tc.accept, got, tc.want)
		}
	}
}

// TestAnEnormousHeaderIsBounded covers the fact that Accept-Language is
// attacker-controlled and unbounded. Sorting a hundred thousand fragments once
// per request is a denial of service that costs the sender one header.
func TestAnEnormousHeaderIsBounded(t *testing.T) {
	catalogue := threeLanguages(t)

	header := ""
	for i := 0; i < 20000; i++ {
		header += "xx,"
	}
	header += "de"

	// The German at the end is past the bound and is not reached, which is the
	// point: the answer is the fallback rather than a request that took a
	// measurable amount of time.
	if got := catalogue.Negotiate(request("/", header, "")); got != DefaultLocale {
		t.Fatalf("an oversized header resolved to %q", got)
	}
}

// TestApplyRemembersAnExplicitChoice is what stops a language switcher working
// once and then reverting. The parameter is gone from the next URL, so if
// nothing wrote it down the next page is in the browser's language again.
func TestApplyRemembersAnExplicitChoice(t *testing.T) {
	w := httptest.NewRecorder()

	if got := Apply(w, request("/?lang=en", "", "")); got != "en" {
		t.Fatalf("Apply resolved to %q", got)
	}

	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != CookieName || cookies[0].Value != "en" {
		t.Fatalf("Apply did not write the language cookie: %v", cookies)
	}
}

// TestApplyWritesNothingWithoutAChoice covers the ordinary page load. Setting a
// cookie on every request would turn a language nobody chose into one they are
// stuck with, which is the opposite of what the cookie is for.
func TestApplyWritesNothingWithoutAChoice(t *testing.T) {
	w := httptest.NewRecorder()

	Apply(w, request("/", "en", ""))

	if cookies := w.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("a request with no explicit choice set a cookie: %v", cookies)
	}
}

// TestTheLanguageCookieIsReadableByTheDashboard documents a deliberate
// exception. The switcher in the dashboard is JavaScript, and a cookie it
// cannot read is a switcher that has to round-trip through the server to change
// a label. Nothing in it is a credential — it is the name of a language.
func TestTheLanguageCookieIsReadableByTheDashboard(t *testing.T) {
	cookie := RememberCookie("de", true)

	if cookie.HttpOnly {
		t.Fatal("the language cookie is HttpOnly, so the dashboard cannot read it")
	}

	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("the language cookie is not SameSite=Lax, so another site could change it")
	}

	if !cookie.Secure {
		t.Fatal("the language cookie was not marked Secure over TLS")
	}
}
