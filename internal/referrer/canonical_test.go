//
// canonical_test.go
// Tests for turning imported source labels and referrers into the values live collection stores.
//
// Created: 2026-11-19
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package referrer

import (
	"strings"
	"testing"
)

// TestCanonicalSource covers every route a label can take to the name live
// collection stores: direct labels, other tools' display names, case folds,
// UTM aliases, Android packages, hostnames, and labels left alone.
func TestCanonicalSource(t *testing.T) {
	cases := []struct {
		label string
		want  string
	}{
		// Direct traffic is stored with no source.
		{"", ""},
		{"  ", ""},
		{"(direct)", ""},
		{"Direct", ""},
		{"Direct / None", ""},

		// Display names other tools use.
		{"X (Twitter)", "X"},
		{"Google Gemini", "Gemini"},
		{"Brave", "Brave Search"},
		{"Vkontakte", "VK"},
		{"Sina Weibo", "Weibo"},
		{"Outlook.com", "Outlook"},
		{"ProtonMail", "Proton Mail"},
		{"Wikipedia", "wikipedia.org"},
		{"Grok", "Grok"},
		{"DeepSeek", "DeepSeek"},
		{"Phind", "phind.com"},

		// Our own names, in any case.
		{"Youtube", "YouTube"},
		{"Github", "GitHub"},
		{"duckduckgo", "DuckDuckGo"},
		{"baidu", "Baidu"},
		{"YAHOO!", "Yahoo!"},
		{"Hacker News", "Hacker News"},
		{"You.com", "You.com"},

		// UTM aliases resolve the way they do at ingest.
		{"google", "Google"},
		{"fb", "Facebook"},
		{"twitter", "X"},
		{"newsletter", "Newsletter"},

		// Android packages.
		{"android-app://com.google.android.gm", "Gmail"},
		{"com.reddit.frontpage", "Reddit"},

		// Hostnames resolve through the host table, or to their registrable domain.
		{"t.co", "X"},
		{"m.facebook.com", "Facebook"},
		{"news.ycombinator.com", "Hacker News"},
		{"www.Google.co.uk", "Google"},
		{"search.brave.com", "Brave Search"},
		{"chatgpt.com", "ChatGPT"},
		{"en.wikipedia.org", "wikipedia.org"},
		{"blog.example.com", "example.com"},
		{"indiehackers.com", "indiehackers.com"},

		// Anything else is kept.
		{"producthunt", "producthunt"},
		{"(not set)", "(not set)"},
		{"  Spring Launch  ", "Spring Launch"},
	}

	for _, tc := range cases {
		if got := CanonicalSource(tc.label); got != tc.want {
			t.Errorf("CanonicalSource(%q) = %q, want %q", tc.label, got, tc.want)
		}
	}
}

// TestCanonicalSourceAgreesWithParse checks a hostname label lands on the same
// source live collection derives from a referrer on that host.
func TestCanonicalSourceAgreesWithParse(t *testing.T) {
	for _, host := range []string{
		"google.com", "l.facebook.com", "old.reddit.com", "lnkd.in", "gemini.google.com",
		"copilot.microsoft.com", "duck.ai", "l.meta.ai", "grok.com", "chat.deepseek.com",
		"en.wikipedia.org", "docs.example.org",
	} {
		want := Parse("https://"+host+"/", "").Source
		if got := CanonicalSource(host); got != want {
			t.Errorf("CanonicalSource(%q) = %q, live collection stores %q", host, got, want)
		}
	}
}

// TestImportedSourceNamesAreLiveNames checks every display-name mapping lands
// on a value live collection can store: one of our source names, or a
// registrable domain.
func TestImportedSourceNamesAreLiveNames(t *testing.T) {
	for label, name := range importedSourceNames {
		if canonicalNames[strings.ToLower(name)] == name || registrable(name) == name {
			continue
		}

		t.Errorf("%q maps to %q, which live collection never stores", label, name)
	}
}

// TestCanonicalReferrer checks imported referrers take the stored shape: host
// and path, lower-case host, no scheme, "www.", query string or trailing slash.
func TestCanonicalReferrer(t *testing.T) {
	cases := []struct {
		value string
		want  string
	}{
		{"", ""},
		{"google.com", "google.com"},
		{"www.Google.com/", "google.com"},
		{"news.ycombinator.com/item", "news.ycombinator.com/item"},
		{"https://www.example.com/blog/post/?utm_source=x", "example.com/blog/post"},
		{"Example.com/Path/", "example.com/Path"},
		{"android-app://com.reddit.frontpage", "com.reddit.frontpage"},
		{"com.reddit.frontpage", "com.reddit.frontpage"},
		{"not a url at all", "not a url at all"},
	}

	for _, tc := range cases {
		if got := CanonicalReferrer(tc.value); got != tc.want {
			t.Errorf("CanonicalReferrer(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
}
