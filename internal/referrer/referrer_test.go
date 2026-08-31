//
// referrer_test.go
// Tests for resolving a Referer header to a stored referrer and a canonical source.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package referrer

import "testing"

// TestParseKnownSources checks the host map and the registrable-domain fallback
// that makes one entry cover every regional and mobile subdomain.
func TestParseKnownSources(t *testing.T) {
	cases := []struct {
		referrer string
		source   string
		category Category
	}{
		{"https://www.google.com/search?q=analytics", "Google", CategorySearch},
		{"https://google.co.uk/", "Google", CategorySearch},
		{"https://www.google.de/search?q=x", "Google", CategorySearch},
		{"https://amazon.co.jp/dp/x", "Amazon", CategoryShopping},
		{"https://m.facebook.com/", "Facebook", CategorySocial},
		{"https://l.facebook.com/l.php?u=x", "Facebook", CategorySocial},
		{"https://t.co/abc123", "X", CategorySocial},
		{"https://old.reddit.com/r/analytics", "Reddit", CategorySocial},
		{"https://news.ycombinator.com/item?id=1", "Hacker News", CategorySocial},
		{"https://www.youtube.com/watch?v=x", "YouTube", CategoryVideo},
		{"https://mail.google.com/mail/u/0", "Gmail", CategoryEmail},
		{"https://chatgpt.com/", "ChatGPT", CategoryAI},
		{"https://duckduckgo.com/", "DuckDuckGo", CategorySearch},
	}

	for _, tc := range cases {
		got := Parse(tc.referrer, "example.com")

		if got.Source != tc.source {
			t.Errorf("%s: source = %q, want %q", tc.referrer, got.Source, tc.source)
		}
		if got.Category != tc.category {
			t.Errorf("%s: category = %d, want %d", tc.referrer, got.Category, tc.category)
		}
	}
}

// TestUnknownReferrerReportsItsOwnDomain checks the fallback. A blog nobody has
// heard of is still a useful row, and "other" would not be.
func TestUnknownReferrerReportsItsOwnDomain(t *testing.T) {
	got := Parse("https://blog.somebody.example/post/1", "example.com")

	if got.Source != "somebody.example" {
		t.Fatalf("source = %q, want somebody.example", got.Source)
	}
	// The stored referrer keeps the full host, because the Referrers report
	// shows the exact page while the Sources tab groups by the domain.
	if got.Referrer != "blog.somebody.example/post/1" {
		t.Fatalf("referrer = %q, want blog.somebody.example/post/1", got.Referrer)
	}
}

// TestSameSiteIsDirect checks an internal link is not an acquisition. Counting
// it would make every site its own biggest traffic source.
func TestSameSiteIsDirect(t *testing.T) {
	for _, referrer := range []string{
		"https://example.com/pricing",
		"https://www.example.com/",
		"https://blog.example.com/post",
	} {
		if got := Parse(referrer, "example.com"); got.Source != Direct {
			t.Errorf("%s: source = %q, want %q", referrer, got.Source, Direct)
		}
	}
}

// TestNoReferrerIsDirect checks "Direct / None" is a real value. It is usually
// the largest bucket, and leaving it out makes every total look wrong.
func TestNoReferrerIsDirect(t *testing.T) {
	if got := Parse("", "example.com"); got.Source != Direct {
		t.Fatalf("source = %q, want %q", got.Source, Direct)
	}
}

// TestQueryStringIsDroppedFromTheStoredReferrer checks we do not keep somebody
// else's session tokens. Referrer query strings routinely carry them, and they
// are neither ours nor useful.
func TestQueryStringIsDroppedFromTheStoredReferrer(t *testing.T) {
	got := Parse("https://somebody.example/post?session=secret&token=abc", "example.com")

	if got.Referrer != "somebody.example/post" {
		t.Fatalf("referrer = %q, want the path with no query string", got.Referrer)
	}
}

// TestTrailingSlashDoesNotSplitARow checks the same referring page is one row.
func TestTrailingSlashDoesNotSplitARow(t *testing.T) {
	with := Parse("https://somebody.example/post/", "example.com")
	without := Parse("https://somebody.example/post", "example.com")

	if with.Referrer != without.Referrer {
		t.Fatalf("a trailing slash split the referrer: %q vs %q", with.Referrer, without.Referrer)
	}
}

// TestAndroidInAppReferrer is the attribution that would otherwise be Direct.
// An in-app browser sends a package name, which no host lookup resolves.
func TestAndroidInAppReferrer(t *testing.T) {
	cases := map[string]string{
		"android-app://com.google.android.gm":      "Gmail",
		"android-app://com.facebook.katana":        "Facebook",
		"android-app://com.instagram.android":      "Instagram",
		"android-app://com.google.android.youtube": "YouTube",
	}

	for referrer, want := range cases {
		if got := Parse(referrer, "example.com"); got.Source != want {
			t.Errorf("%s: source = %q, want %q", referrer, got.Source, want)
		}
	}

	// An unrecognised package still reports itself rather than vanishing.
	got := Parse("android-app://com.unknown.app", "example.com")
	if got.Source != "com.unknown.app" {
		t.Fatalf("source = %q, want the package name", got.Source)
	}
}

// TestMalformedReferrerIsKept checks a referrer we cannot parse is still
// evidence of something. Turning it into Direct would leave a customer
// wondering why traffic they can see in their own logs is missing.
func TestMalformedReferrerIsKept(t *testing.T) {
	got := Parse("not a url at all", "example.com")

	if got.Source != "not a url at all" {
		t.Fatalf("source = %q, want the raw value", got.Source)
	}
}

// TestSourceFromUTM checks the alias table folds the naming variants people
// type. facebook, fb and facebook-ads are the same company and belong in one
// row on the Sources tab.
func TestSourceFromUTM(t *testing.T) {
	cases := map[string]string{
		"fb":           "Facebook",
		"facebook":     "Facebook",
		"facebook-ads": "Facebook",
		"FaceBook":     "Facebook",
		"adwords":      "Google",
		"google":       "Google",
		"newsletter":   "Newsletter",
		"google.com":   "Google",
	}

	for tag, want := range cases {
		got, ok := SourceFromUTM(tag)
		if !ok {
			t.Errorf("%q resolved to nothing", tag)
			continue
		}
		if got.Name != want {
			t.Errorf("%q: source = %q, want %q", tag, got.Name, want)
		}
	}

	// An unknown tag is kept exactly as sent, so the Campaigns report can
	// distinguish tags the Sources tab folds together.
	got, ok := SourceFromUTM("Our-Spring-Push")
	if !ok || got.Name != "Our-Spring-Push" {
		t.Fatalf("unknown tag = %q, want it kept verbatim", got.Name)
	}

	if _, ok := SourceFromUTM("   "); ok {
		t.Fatal("a blank tag resolved to a source")
	}
}
