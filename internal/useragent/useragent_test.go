//
// useragent_test.go
// Tests for the parser, and for the ordering that keeps Edge from being Chrome.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package useragent

import (
	"testing"
	"time"
)

// TestParse walks the headers a real dashboard sees. The ordering of the rules
// is the whole algorithm: every Chromium browser carries "Chrome" in its header
// and every WebKit browser carries "Safari", so a rule checked in the wrong
// order reports Edge as Chrome and Chrome as Safari.
func TestParse(t *testing.T) {
	cases := []struct {
		name string
		ua   string
		want Result
	}{
		{
			name: "Chrome on macOS",
			ua:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			want: Result{Browser: "Chrome", BrowserVersion: "120", OS: "macOS", OSVersion: "10.15", Device: DeviceDesktop},
		},
		{
			name: "Edge, which claims to be Chrome",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0",
			want: Result{Browser: "Edge", BrowserVersion: "120", OS: "Windows", OSVersion: "10", Device: DeviceDesktop},
		},
		{
			name: "Opera, which claims to be Chrome too",
			ua:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36 OPR/105.0.0.0",
			want: Result{Browser: "Opera", BrowserVersion: "105", OS: "Windows", OSVersion: "10", Device: DeviceDesktop},
		},
		{
			name: "Firefox on Linux",
			ua:   "Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0",
			want: Result{Browser: "Firefox", BrowserVersion: "121", OS: "GNU/Linux", Device: DeviceDesktop},
		},
		{
			name: "Safari on macOS, which claims to be Mozilla",
			ua:   "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Safari/605.1.15",
			want: Result{Browser: "Safari", BrowserVersion: "17", OS: "macOS", OSVersion: "10.15", Device: DeviceDesktop},
		},
		{
			name: "Safari on iPhone",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1",
			want: Result{Browser: "Safari", BrowserVersion: "17", OS: "iOS", OSVersion: "17.4", Device: DeviceMobile},
		},
		{
			name: "Chrome on iOS, which is Safari underneath",
			ua:   "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/120.0.0.0 Mobile/15E148 Safari/604.1",
			want: Result{Browser: "Chrome", BrowserVersion: "120", OS: "iOS", OSVersion: "17.4", Device: DeviceMobile},
		},
		{
			name: "an iPad, which reports a different OS token",
			ua:   "Mozilla/5.0 (iPad; CPU OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/604.1",
			want: Result{Browser: "Safari", BrowserVersion: "17", OS: "iPadOS", OSVersion: "17.4", Device: DeviceTablet},
		},
		{
			name: "Chrome on an Android phone",
			ua:   "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
			want: Result{Browser: "Chrome", BrowserVersion: "120", OS: "Android", OSVersion: "14", Device: DeviceMobile},
		},
		{
			// Android's own rule for telling a phone from a tablet is the
			// presence of the word "Mobile"; without it, it is a tablet.
			name: "Chrome on an Android tablet",
			ua:   "Mozilla/5.0 (Linux; Android 13; SM-X710) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			want: Result{Browser: "Chrome", BrowserVersion: "120", OS: "Android", OSVersion: "13", Device: DeviceTablet},
		},
		{
			name: "Samsung Internet",
			ua:   "Mozilla/5.0 (Linux; Android 13; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/23.0 Chrome/115.0.0.0 Mobile Safari/537.36",
			want: Result{Browser: "Samsung Internet", BrowserVersion: "23", OS: "Android", OSVersion: "13", Device: DeviceMobile},
		},
		{
			name: "Internet Explorer 11, which does not say Explorer anywhere",
			ua:   "Mozilla/5.0 (Windows NT 6.1; Trident/7.0; rv:11.0) like Gecko",
			want: Result{Browser: "Internet Explorer", BrowserVersion: "11", OS: "Windows", OSVersion: "7", Device: DeviceDesktop},
		},
		{
			name: "an empty header is not an error",
			ua:   "",
			want: Result{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.ua)

			if got != tc.want {
				t.Fatalf("Parse = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestWindowsElevenIsNotDetectable documents a real limit rather than a bug.
// Windows 11 reports the same NT 10.0 as Windows 10, so the header cannot tell
// them apart and pretending otherwise would be a made-up number.
func TestWindowsElevenIsNotDetectable(t *testing.T) {
	ten := Parse("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	if ten.OSVersion != "10" {
		t.Fatalf("OS version = %q, want 10", ten.OSVersion)
	}
}

// TestBrowserVersionIsTrimmed checks the build number is dropped. Chrome ships
// one every few days, and storing them all turns the browser-version report
// into thousands of unreadable rows and grows the dimension table without bound.
func TestBrowserVersionIsTrimmed(t *testing.T) {
	for _, ua := range []string{
		"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/120.0.6099.109 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/120 Safari/537.36",
	} {
		if got := Parse(ua).BrowserVersion; got != "120" {
			t.Errorf("version = %q, want 120 for %q", got, ua)
		}
	}
}

// TestCacheReturnsTheSameAnswer checks the LRU is transparent. Parsing is the
// most expensive and most repetitive thing the ingest path does per event, so
// the cache is on the hot path from the first version.
func TestCacheReturnsTheSameAnswer(t *testing.T) {
	cache := NewCache(10, DefaultTTL)

	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	want := Parse(ua)

	for i := 0; i < 5; i++ {
		if got := cache.Parse(ua); got != want {
			t.Fatalf("call %d = %+v, want %+v", i, got, want)
		}
	}

	hits, misses, size := cache.Stats()
	if hits != 4 || misses != 1 {
		t.Fatalf("hits/misses = %d/%d, want 4/1", hits, misses)
	}
	if size != 1 {
		t.Fatalf("cache holds %d entries, want 1", size)
	}
}

// TestCacheEvictsTheLeastRecentlyUsed checks the bound holds. Without it a
// header-randomising bot could make the process hold an unbounded number of
// strings.
func TestCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	cache := NewCache(2, DefaultTTL)

	cache.Parse("first")
	cache.Parse("second")
	cache.Parse("first")
	cache.Parse("third")

	if _, _, size := cache.Stats(); size != 2 {
		t.Fatalf("cache holds %d entries, want 2", size)
	}

	// "first" was used more recently than "second", so it is the one that
	// survived and asking for it again must be a hit.
	before, _, _ := cache.Stats()
	cache.Parse("first")
	after, _, _ := cache.Stats()

	if after != before+1 {
		t.Fatal("the least recently used entry was evicted instead of the oldest")
	}
}

// TestCacheExpiresEntries checks the TTL. Nothing about a parse goes stale, so
// the TTL is about not holding a hundred thousand strings that stopped being
// asked for weeks ago.
func TestCacheExpiresEntries(t *testing.T) {
	cache := NewCache(10, DefaultTTL)

	now := cache.now()
	cache.now = func() time.Time { return now }

	cache.Parse("something")

	now = now.Add(DefaultTTL + time.Minute)

	_, missesBefore, _ := cache.Stats()
	cache.Parse("something")
	_, missesAfter, _ := cache.Stats()

	if missesAfter != missesBefore+1 {
		t.Fatal("an expired entry was served from the cache")
	}
	if _, _, size := cache.Stats(); size != 1 {
		t.Fatalf("cache holds %d entries after re-parsing, want 1", size)
	}
}

// TestEmptyHeaderIsNotCached checks a stripped user agent does not take a slot
// that every such request would then contend on.
func TestEmptyHeaderIsNotCached(t *testing.T) {
	cache := NewCache(10, DefaultTTL)

	cache.Parse("")

	if _, _, size := cache.Stats(); size != 0 {
		t.Fatalf("cache holds %d entries for the empty header, want 0", size)
	}
}

// BenchmarkParse and BenchmarkCachedParse show what the cache buys, which is the
// reason it exists on a path budgeted at well under a millisecond.
func BenchmarkParse(b *testing.B) {
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	for i := 0; i < b.N; i++ {
		_ = Parse(ua)
	}
}

// BenchmarkCachedParse measures the hit path.
func BenchmarkCachedParse(b *testing.B) {
	cache := NewCache(DefaultCapacity, DefaultTTL)
	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	cache.Parse(ua)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cache.Parse(ua)
	}
}
