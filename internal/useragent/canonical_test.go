//
// canonical_test.go
// Tests for turning imported browser, system and device names into the names Parse stores.
//
// Created: 2026-11-19
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package useragent

import "testing"

// TestCanonicalBrowser covers every browser alias and the case fold onto our
// own names.
func TestCanonicalBrowser(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"Edge", "Microsoft Edge"},
		{"Microsoft Edge", "Microsoft Edge"},
		{"Samsung Browser", "Samsung Internet"},
		{"YaBrowser", "Yandex Browser"},
		{"Opera Mini", "Opera"},
		{"Opera GX", "Opera"},
		{"DuckDuckGo Privacy Browser", "DuckDuckGo"},
		{"Android Webview", "Mobile App"},
		{"Safari (in-app)", "Mobile App"},
		{"chrome", "Chrome"},
		{" Firefox ", "Firefox"},
		{"Samsung Internet", "Samsung Internet"},
		{"UC Browser", "UC Browser"},
		{"", ""},
	}

	for _, tc := range cases {
		if got := CanonicalBrowser(tc.name); got != tc.want {
			t.Errorf("CanonicalBrowser(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCanonicalOS covers every system alias and the case fold.
func TestCanonicalOS(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"Mac", "macOS"},
		{"Macintosh", "macOS"},
		{"Mac OS X", "macOS"},
		{"OS X", "macOS"},
		{"macos", "macOS"},
		{"Linux", "GNU/Linux"},
		{"ChromeOS", "Chrome OS"},
		{"Chrome OS", "Chrome OS"},
		{"Windows", "Windows"},
		{"ios", "iOS"},
		{"iPadOS", "iPadOS"},
		{"Tizen", "Tizen"},
		{"", ""},
	}

	for _, tc := range cases {
		if got := CanonicalOS(tc.name); got != tc.want {
			t.Errorf("CanonicalOS(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCanonicalDevice covers every device alias and the case fold.
func TestCanonicalDevice(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"desktop", "Desktop"},
		{"mobile", "Mobile"},
		{"tablet", "Tablet"},
		{"smart tv", "TV"},
		{"Laptop", "Desktop"},
		{"Desktop", "Desktop"},
		{"Wearable", "Wearable"},
		{"", ""},
	}

	for _, tc := range cases {
		if got := CanonicalDevice(tc.name); got != tc.want {
			t.Errorf("CanonicalDevice(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCanonicalVersions checks imported versions are cut the way Parse cuts
// them: browsers to the major number, systems to two components without a
// trailing ".0", and Windows NT numbers to release names.
func TestCanonicalVersions(t *testing.T) {
	browsers := []struct {
		version string
		want    string
	}{
		{"140.0", "140"},
		{"140", "140"},
		{"17.4.1", "17"},
		{"beta", "beta"},
		{"", ""},
	}

	for _, tc := range browsers {
		if got := CanonicalBrowserVersion(tc.version); got != tc.want {
			t.Errorf("CanonicalBrowserVersion(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}

	systems := []struct {
		system  string
		version string
		want    string
	}{
		{"macOS", "10.15", "10.15"},
		{"iOS", "17.4.1", "17.4"},
		{"Android", "14.0", "14"},
		{"iOS", "6.1", "6.1"},
		{"Windows", "10.0", "10"},
		{"Windows", "6.1", "7"},
		{"Windows", "10", "10"},
		{"Windows", "11", "11"},
		{"Windows", "XP", "XP"},
		{"Android", "1..2", "1..2"},
		{"Android", "", ""},
	}

	for _, tc := range systems {
		if got := CanonicalOSVersion(tc.system, tc.version); got != tc.want {
			t.Errorf("CanonicalOSVersion(%q, %q) = %q, want %q", tc.system, tc.version, got, tc.want)
		}
	}
}

// TestAliasesNameWhatParseStores checks every alias lands on a name Parse can
// return, so an alias cannot invent a spelling live collection never writes.
func TestAliasesNameWhatParseStores(t *testing.T) {
	for _, check := range []struct {
		aliases map[string]string
		ours    map[string]string
	}{
		{importedBrowsers, browserNames},
		{importedSystems, systemNames},
		{importedDevices, deviceNames},
	} {
		known := map[string]bool{}
		for _, name := range check.ours {
			known[name] = true
		}

		for alias, name := range check.aliases {
			if !known[name] {
				t.Errorf("%q maps to %q, which Parse never returns", alias, name)
			}
		}
	}
}

// TestCanonicalNamesMatchParse checks a real header and the imported spelling
// of the same platform meet on one name.
func TestCanonicalNamesMatchParse(t *testing.T) {
	parsed := Parse("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.2210.91")

	if got := CanonicalOS("Macintosh"); got != parsed.OS {
		t.Errorf("CanonicalOS(Macintosh) = %q, Parse stores %q", got, parsed.OS)
	}
	if got := CanonicalBrowser("Edge"); got != parsed.Browser {
		t.Errorf("CanonicalBrowser(Edge) = %q, Parse stores %q", got, parsed.Browser)
	}
	if got := CanonicalDevice("desktop"); got != parsed.Device {
		t.Errorf("CanonicalDevice(desktop) = %q, Parse stores %q", got, parsed.Device)
	}
	if got := CanonicalOSVersion(parsed.OS, "10.15.7"); got != parsed.OSVersion {
		t.Errorf("CanonicalOSVersion(10.15.7) = %q, Parse stores %q", got, parsed.OSVersion)
	}
	if got := CanonicalBrowserVersion("120.0.2210.91"); got != parsed.BrowserVersion {
		t.Errorf("CanonicalBrowserVersion(120.0.2210.91) = %q, Parse stores %q", got, parsed.BrowserVersion)
	}
}
