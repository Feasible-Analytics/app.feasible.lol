//
// normalise_test.go
// Tests for rewriting imported dimension values into the names live collection stores.
//
// Created: 2026-11-19
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dataio

import (
	"reflect"
	"testing"
)

// liveRows are dimension values live collection stores. Importing our own
// export carries exactly these, so normalising them must change nothing.
var liveRows = []map[string]string{
	{
		"event:page": "/caf%C3%A9", "event:hostname": "example.com", "event:page_title": "Pricing",
		"visit:entry_page": "/", "visit:exit_page": "/#/about",
		"visit:source": "", "visit:channel": "Direct", "visit:referrer": "",
		"visit:utm_source": "", "visit:utm_medium": "", "visit:utm_campaign": "",
		"visit:country": "US", "visit:region": "Oregon", "visit:city": "Portland",
		"visit:device": "Desktop", "visit:screen": "Laptop",
		"visit:browser": "Microsoft Edge", "visit:browser_version": "120",
		"visit:os": "macOS", "visit:os_version": "10.15", "visit:language": "en-US",
	},
	{
		"event:name": "Outbound Link: Click", "event:hostname": "(none)",
		"visit:source": "Google", "visit:channel": "Organic Search", "visit:referrer": "google.com",
		"visit:utm_source": "newsletter", "visit:utm_medium": "email", "visit:utm_campaign": "launch",
		"visit:utm_content": "hero", "visit:utm_term": "analytics",
		"visit:country": "Anonymous VPN Service", "visit:region": "US-OR",
		"visit:device": "Mobile", "visit:screen": "Mobile",
		"visit:browser": "Mobile App", "visit:browser_version": "17",
		"visit:os": "iPadOS", "visit:os_version": "17.4",
	},
	{
		"visit:source": "X", "visit:channel": "Organic Social", "visit:referrer": "news.ycombinator.com/item",
		"visit:browser": "Samsung Internet", "visit:os": "Windows", "visit:os_version": "10",
		"visit:device": "TV", "visit:screen": "Desktop",
	},
	{
		"visit:source": "example.com", "visit:referrer": "com.reddit.frontpage",
		"visit:os": "Android", "visit:os_version": "14", "visit:browser": "Chrome",
	},
}

// TestNormaliseKeepsLiveValues checks values live collection already stores
// pass through untouched for every import source.
func TestNormaliseKeepsLiveValues(t *testing.T) {
	for _, origin := range []string{SourceCSV, SourcePlausible, SourceGA4} {
		for _, row := range liveRows {
			if got := Normalise(origin, row); !reflect.DeepEqual(got, row) {
				for name, want := range row {
					if got[name] != want {
						t.Errorf("%s: %s %q became %q", origin, name, want, got[name])
					}
				}
			}
		}
	}
}

// TestNormalisePlaceholders checks a "no value" placeholder becomes the empty
// value live collection stores, except where live collection has its own
// spelling for nothing (a hostname) or the placeholder is a real bucket (a
// source or channel).
func TestNormalisePlaceholders(t *testing.T) {
	cases := []struct {
		dimension string
		want      string
	}{
		{"event:page", ""},
		{"event:hostname", "(none)"},
		{"visit:entry_page", ""},
		{"visit:exit_page", ""},
		{"visit:referrer", ""},
		{"visit:utm_source", ""},
		{"visit:utm_medium", ""},
		{"visit:utm_campaign", ""},
		{"visit:utm_content", ""},
		{"visit:utm_term", ""},
		{"visit:country", ""},
		{"visit:region", ""},
		{"visit:city", ""},
		{"visit:device", ""},
		{"visit:screen", ""},
		{"visit:browser", ""},
		{"visit:browser_version", ""},
		{"visit:os", ""},
		{"visit:os_version", ""},
		{"visit:language", ""},
		{"visit:source", "(not set)"},
		{"visit:channel", "(not set)"},
		{"event:name", "(not set)"},
	}

	for _, tc := range cases {
		got := Normalise(SourceGA4, map[string]string{tc.dimension: "(not set)"})[tc.dimension]
		if got != tc.want {
			t.Errorf("%s (not set) = %q, want %q", tc.dimension, got, tc.want)
		}
	}

	for _, placeholder := range []string{"(none)", "(direct)", "(organic)", "(referral)", "(cross-network)", "(not provided)", "(data not available)", "(gclid)", "(msclkid)", "(NOT SET)"} {
		if got := Normalise(SourceGA4, map[string]string{"visit:utm_campaign": placeholder})["visit:utm_campaign"]; got != "" {
			t.Errorf("utm_campaign %q = %q, want empty", placeholder, got)
		}
	}
}

// TestNormaliseByOrigin checks the one origin-specific rule: the API import's
// stand-in mediums for untagged visits are cleared, while the same words from
// an export are tags somebody set and are kept.
func TestNormaliseByOrigin(t *testing.T) {
	cases := []struct {
		origin string
		medium string
		want   string
	}{
		{SourceGA4, "organic", ""},
		{SourceGA4, "Referral", ""},
		{SourceGA4, "cpc", "cpc"},
		{SourceGA4, "email", "email"},
		{SourcePlausible, "organic", "organic"},
		{SourcePlausible, "referral", "referral"},
		{SourceCSV, "organic", "organic"},
	}

	for _, tc := range cases {
		got := Normalise(tc.origin, map[string]string{"visit:utm_medium": tc.medium})["visit:utm_medium"]
		if got != tc.want {
			t.Errorf("%s medium %q = %q, want %q", tc.origin, tc.medium, got, tc.want)
		}
	}
}

// TestNormaliseReadsTheRowsOwnSystem checks a system version is cut by the
// rules of the system on the same row, after that system's name is resolved.
func TestNormaliseReadsTheRowsOwnSystem(t *testing.T) {
	cases := []struct {
		system  string
		version string
		want    string
	}{
		{"Windows", "6.1", "7"},
		{"iOS", "6.1", "6.1"},
		{"Mac", "10.15.7", "10.15"},
		{"", "14.0", "14"},
	}

	for _, tc := range cases {
		got := Normalise(SourceCSV, map[string]string{"visit:os": tc.system, "visit:os_version": tc.version})
		if got["visit:os_version"] != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.system, tc.version, got["visit:os_version"], tc.want)
		}
	}
}

// TestNormaliseDoesNotModifyItsInput checks the caller's row is left as it was.
func TestNormaliseDoesNotModifyItsInput(t *testing.T) {
	row := map[string]string{"visit:os": "Macintosh", "visit:source": "(direct)"}

	got := Normalise(SourceGA4, row)

	if row["visit:os"] != "Macintosh" || row["visit:source"] != "(direct)" {
		t.Fatalf("the input row changed: %v", row)
	}
	if got["visit:os"] != "macOS" || got["visit:source"] != "" {
		t.Fatalf("Normalise = %v", got)
	}
}

// TestNormaliseHelpers covers the path, hostname and country rules on the
// values they leave alone as well as the ones they rewrite.
func TestNormaliseHelpers(t *testing.T) {
	paths := map[string]string{
		"/café":          "/caf%C3%A9",
		"/café?x=1":      "/caf%C3%A9",
		"/a b":           "/a%20b",
		"/%7Euser":       "/%7Euser",
		"/#/about":       "/#/about",
		"/about/":        "/about/",
		"about":          "about",
		"//example.com/": "//example.com/",
		"/100%":          "/100%",
	}
	for value, want := range paths {
		if got := canonicalPath(value); got != want {
			t.Errorf("canonicalPath(%q) = %q, want %q", value, got, want)
		}
	}

	hosts := map[string]string{
		"www.Example.com":          "example.com",
		"Shop.Example.com":         "shop.example.com",
		"https://www.example.com/": "example.com",
		"example.com:8443":         "example.com",
		"(none)":                   "(none)",
		"":                         "",
	}
	for value, want := range hosts {
		if got := canonicalHostname(value); got != want {
			t.Errorf("canonicalHostname(%q) = %q, want %q", value, got, want)
		}
	}

	countries := map[string]string{
		"us":                    "US",
		"Gb":                    "GB",
		"US":                    "US",
		"USA":                   "USA",
		"Anonymous VPN Service": "Anonymous VPN Service",
		"1A":                    "1A",
	}
	for value, want := range countries {
		if got := canonicalCountry(value); got != want {
			t.Errorf("canonicalCountry(%q) = %q, want %q", value, got, want)
		}
	}
}

// TestNormaliseIsIdempotent checks normalising a value twice gives what
// normalising it once did. The rename command matches rows by their old values
// and relies on this, so a renamed row is never caught by a second rename.
func TestNormaliseIsIdempotent(t *testing.T) {
	raw := map[string][]string{
		"visit:source": {"(direct)", "Direct", "X (Twitter)", "Google Gemini", "Brave", "Vkontakte", "Sina Weibo",
			"Outlook.com", "ProtonMail", "Wikipedia", "Grok", "Youtube", "google", "m.facebook.com",
			"en.wikipedia.org", "blog.example.com", "android-app://com.google.android.gm", "producthunt", "(not set)"},
		"visit:referrer":        {"www.News.ycombinator.com/item/", "https://Example.com/a/?q=1", "(not set)"},
		"visit:channel":         {"organic search", "Unassigned"},
		"visit:utm_medium":      {"organic", "referral", "(none)", "(gclid)", "cpc"},
		"visit:utm_campaign":    {"(organic)", "(cross-network)", "Spring"},
		"visit:browser":         {"Edge", "Samsung Browser", "Android Webview", "Safari (in-app)", "YaBrowser", "Opera GX", "chrome"},
		"visit:browser_version": {"140.0", "17.4.1", "beta"},
		"visit:device":          {"desktop", "smart tv", "Laptop"},
		"visit:screen":          {"laptop", "MOBILE"},
		"visit:country":         {"us", "(not set)"},
		"event:hostname":        {"www.Example.com", "(not set)", "https://www.example.com/"},
		"event:page":            {"/café?x=1", "/a b", "/#/about", "(not set)"},
		"visit:entry_page":      {"/über/"},
	}

	systems := []struct{ os, version string }{
		{"Macintosh", "10.15.7"}, {"Mac", "14.0"}, {"Windows", "6.1"}, {"Windows", "10.0"},
		{"Linux", ""}, {"iOS", "17.4.1"}, {"Android", "14.0"},
	}

	for _, origin := range []string{SourceCSV, SourcePlausible, SourceGA4} {
		for dimension, values := range raw {
			for _, value := range values {
				once := Normalise(origin, map[string]string{dimension: value})
				twice := Normalise(origin, once)
				if !reflect.DeepEqual(once, twice) {
					t.Errorf("%s %s %q: once %q, twice %q", origin, dimension, value, once[dimension], twice[dimension])
				}
			}
		}

		for _, system := range systems {
			once := Normalise(origin, map[string]string{"visit:os": system.os, "visit:os_version": system.version})
			twice := Normalise(origin, once)
			if !reflect.DeepEqual(once, twice) {
				t.Errorf("%s %v: once %v, twice %v", origin, system, once, twice)
			}
		}
	}
}
