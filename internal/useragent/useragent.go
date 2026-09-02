//
// useragent.go
// Turning a User-Agent header into browser, operating system and device class.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package useragent parses the User-Agent header into the four dimensions a
// dashboard reports on: browser, browser version, operating system and device
// class. It is written from the public shape of the header rather than derived
// from anyone's data files — the widely-used device-detector regexes are
// LGPL-3.0, which would have to ship as a separate unmodified runtime file with
// its licence text, and folding them into our source is exactly what that
// licence forbids.
//
// The matching is ordered rather than clever, because every user agent lies.
// Edge claims to be Chrome, Chrome claims to be Safari, and Safari claims to be
// Mozilla; the only way through is to check the most specific claim first and
// stop at the first hit.
package useragent

import (
	"strconv"
	"strings"
)

// Device classes. They are constants because they are also dimension values
// stored in every event row, and a typo in one call site would create a second
// "Moblie" bucket that no report would ever reconcile.
const (
	DeviceDesktop = "Desktop"
	DeviceMobile  = "Mobile"
	DeviceTablet  = "Tablet"
	DeviceTV      = "TV"
)

// Result is one parsed user agent. Empty strings mean "we could not tell",
// which the schema already models as dimension id 0, so nothing downstream has
// to distinguish absent from unknown.
type Result struct {
	Browser        string
	BrowserVersion string
	OS             string
	OSVersion      string
	Device         string
}

// rule matches one token in a user agent and names what it means. Version
// extraction is a token to search for and read a version after, which covers
// every real header shape without a regex engine on the hot path.
type rule struct {
	// match is the substring that identifies this product.
	match string

	// name is the canonical product name we store.
	name string

	// versionAfter is the token the version number follows. When empty, the
	// version is read straight after `match` and a slash.
	versionAfter string
}

// browserRules are checked in order, most specific first. Order is the entire
// algorithm: every Chromium browser carries "Chrome" in its header and every
// WebKit browser carries "Safari", so a list sorted by specificity is what
// stops Edge being reported as Chrome and Chrome as Safari.
var browserRules = []rule{
	{match: "Edg/", name: "Edge", versionAfter: "Edg/"},
	{match: "EdgA/", name: "Edge", versionAfter: "EdgA/"},
	{match: "EdgiOS/", name: "Edge", versionAfter: "EdgiOS/"},
	{match: "OPR/", name: "Opera", versionAfter: "OPR/"},
	{match: "Opera", name: "Opera", versionAfter: "Version/"},
	{match: "SamsungBrowser/", name: "Samsung Internet", versionAfter: "SamsungBrowser/"},
	{match: "YaBrowser/", name: "Yandex Browser", versionAfter: "YaBrowser/"},
	{match: "Vivaldi/", name: "Vivaldi", versionAfter: "Vivaldi/"},
	{match: "Brave/", name: "Brave", versionAfter: "Brave/"},
	{match: "DuckDuckGo/", name: "DuckDuckGo", versionAfter: "DuckDuckGo/"},
	{match: "CriOS/", name: "Chrome", versionAfter: "CriOS/"},
	{match: "FxiOS/", name: "Firefox", versionAfter: "FxiOS/"},
	{match: "Firefox/", name: "Firefox", versionAfter: "Firefox/"},
	{match: "Chrome/", name: "Chrome", versionAfter: "Chrome/"},
	{match: "Chromium/", name: "Chromium", versionAfter: "Chromium/"},
	{match: "Safari/", name: "Safari", versionAfter: "Version/"},
	{match: "MSIE ", name: "Internet Explorer", versionAfter: "MSIE "},
	{match: "Trident/", name: "Internet Explorer", versionAfter: "rv:"},
}

// osRules are also ordered by specificity. Android must beat Linux because
// every Android header says Linux, and iPadOS must beat iOS because an iPad
// reports "CPU OS" where an iPhone reports "CPU iPhone OS".
var osRules = []rule{
	{match: "Windows NT", name: "Windows", versionAfter: "Windows NT "},
	{match: "Android", name: "Android", versionAfter: "Android "},
	{match: "CrOS", name: "Chrome OS", versionAfter: ""},
	{match: "iPhone OS", name: "iOS", versionAfter: "iPhone OS "},
	{match: "iPad", name: "iPadOS", versionAfter: "CPU OS "},
	{match: "iPod", name: "iOS", versionAfter: "CPU OS "},
	{match: "Mac OS X", name: "macOS", versionAfter: "Mac OS X "},
	{match: "Ubuntu", name: "Ubuntu", versionAfter: ""},
	{match: "Fedora", name: "Fedora", versionAfter: ""},
	{match: "Linux", name: "GNU/Linux", versionAfter: ""},
	{match: "FreeBSD", name: "FreeBSD", versionAfter: ""},
}

// windowsNames maps the NT version everyone's browser reports onto the name
// everyone actually uses. Reporting "Windows 10.0" is technically accurate and
// useless on a dashboard, and Windows 11 reports the same 10.0 as Windows 10 —
// which is why 11 is absent here and cannot be detected from this header at all.
var windowsNames = map[string]string{
	"5.1":  "XP",
	"5.2":  "XP",
	"6.0":  "Vista",
	"6.1":  "7",
	"6.2":  "8",
	"6.3":  "8.1",
	"10.0": "10",
}

// Parse turns a raw User-Agent header into its dimensions. An empty or
// unrecognised header returns zero values rather than an error: a visitor with
// a stripped user agent is still a visitor, and refusing the event would lose
// real traffic to fix a cosmetic gap.
func Parse(ua string) Result {
	if ua == "" {
		return Result{}
	}

	result := Result{Device: device(ua)}

	if match, ok := first(ua, browserRules); ok {
		result.Browser = match.name
		result.BrowserVersion = majorMinor(version(ua, match))
	}

	if match, ok := first(ua, osRules); ok {
		result.OS = match.name
		result.OSVersion = osVersion(match.name, version(ua, match))
	}

	return result
}

// first returns the earliest rule in the list that matches. It walks the list
// rather than the string because the list is ordered by specificity, and that
// order is the only thing separating Edge from Chrome.
func first(ua string, rules []rule) (rule, bool) {
	for _, r := range rules {
		if strings.Contains(ua, r.match) {
			return r, true
		}
	}

	return rule{}, false
}

// version reads the number that follows a rule's marker token. It falls back to
// the match token itself so a rule with no explicit marker still finds a version
// where one exists, which is what makes "Chrome/" work with no extra data.
func version(ua string, r rule) string {
	token := r.versionAfter
	if token == "" {
		token = r.match
	}

	index := strings.Index(ua, token)
	if index < 0 {
		return ""
	}

	rest := ua[index+len(token):]

	// A version runs until the first character that cannot be part of one.
	// Underscores are included because Apple writes macOS versions as 10_15_7.
	end := 0
	for end < len(rest) {
		c := rest[end]
		if (c >= '0' && c <= '9') || c == '.' || c == '_' {
			end++
			continue
		}
		break
	}

	return strings.ReplaceAll(strings.Trim(rest[:end], "._"), "_", ".")
}

// majorMinor trims a four-part browser version down to the two parts anyone
// groups by. Chrome ships a new build number every few days, and storing them
// all turns the browser-version report into a list of thousands of rows nobody
// can read — and grows the dimension table without bound.
func majorMinor(v string) string {
	if v == "" {
		return ""
	}

	parts := strings.Split(v, ".")

	// A browser's minor version has been zero for a decade, so the major on its
	// own is what people mean by "Chrome 120".
	return parts[0]
}

// osVersion normalises the operating system version. Windows is the special
// case that earns this function: its header reports an NT kernel version that
// nobody outside a driver team recognises.
func osVersion(name, raw string) string {
	if raw == "" {
		return ""
	}

	if name == "Windows" {
		if friendly, ok := windowsNames[raw]; ok {
			return friendly
		}
		return ""
	}

	parts := strings.Split(raw, ".")

	// Two components is the level people compare at: "iOS 17.4" is meaningful,
	// "iOS 17.4.1" splits one release across three rows.
	if len(parts) > 2 {
		parts = parts[:2]
	}

	// A trailing ".0" is noise on every platform that reports one.
	if len(parts) == 2 && parts[1] == "0" {
		parts = parts[:1]
	}

	for _, part := range parts {
		if _, err := strconv.Atoi(part); err != nil {
			return ""
		}
	}

	return strings.Join(parts, ".")
}

// device classifies the hardware. It is checked before the browser because the
// signals are independent — a tablet is a tablet whichever browser it runs —
// and because the mobile-versus-tablet distinction lives in tokens that no
// browser rule looks at.
func device(ua string) string {
	switch {
	case strings.Contains(ua, "iPad"),
		strings.Contains(ua, "Tablet"),
		strings.Contains(ua, "Silk/"),
		strings.Contains(ua, "PlayBook"),
		strings.Contains(ua, "Kindle"):
		return DeviceTablet

	// Android's own rule for telling a phone from a tablet is the presence of
	// the word "Mobile"; an Android header without it is a tablet.
	case strings.Contains(ua, "Android"):
		if strings.Contains(ua, "Mobile") {
			return DeviceMobile
		}
		return DeviceTablet

	case strings.Contains(ua, "iPhone"),
		strings.Contains(ua, "iPod"),
		strings.Contains(ua, "Windows Phone"),
		strings.Contains(ua, "Mobile Safari"),
		strings.Contains(ua, "Opera Mini"):
		return DeviceMobile

	case strings.Contains(ua, "SMART-TV"),
		strings.Contains(ua, "SmartTV"),
		strings.Contains(ua, "AppleTV"),
		strings.Contains(ua, "GoogleTV"),
		strings.Contains(ua, "Roku"):
		return DeviceTV

	case strings.Contains(ua, "Windows NT"),
		strings.Contains(ua, "Macintosh"),
		strings.Contains(ua, "CrOS"),
		strings.Contains(ua, "X11"):
		return DeviceDesktop
	}

	return ""
}
