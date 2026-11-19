//
// normalise.go
// Imported dimension values, rewritten into the names live collection stores.
//
// Created: 2026-11-19
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dataio

import (
	"net/url"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/referrer"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/useragent"
)

// placeholders are the values imported history writes where it has no value.
// Live collection stores an empty value for the same thing. Keys are lower case.
var placeholders = map[string]bool{
	"(not set)":            true,
	"(none)":               true,
	"(not provided)":       true,
	"(data not available)": true,
	"(direct)":             true,
	"(organic)":            true,
	"(referral)":           true,
	"(cross-network)":      true,
	"(gclid)":              true,
	"(msclkid)":            true,
}

// syntheticMediums are the mediums the reporting-API import fills in for a
// visit that carried no utm_medium tag. Live collection stores an empty medium
// for those visits. Keys are lower case.
var syntheticMediums = map[string]bool{
	"organic":  true,
	"referral": true,
}

// screenSizes and channels index the values live collection stores for those
// two dimensions by lower case, so an imported value that differs only in case
// folds onto ours.
var (
	screenSizes = foldIndex(ingest.ScreenMobile, ingest.ScreenTablet, ingest.ScreenLaptop, ingest.ScreenDesktop)
	channels    = foldIndex(referrer.Channels...)
)

// foldIndex keys a list of names by their lower-case spelling.
func foldIndex(names ...string) map[string]string {
	index := make(map[string]string, len(names))
	for _, name := range names {
		index[strings.ToLower(name)] = name
	}

	return index
}

// Normalise returns an imported row's dimension values rewritten into the
// spelling live collection stores for the same thing, so a report over imported
// and collected days shows one row per browser, system, source and page rather
// than one per spelling. The origin is the import's source, one of the Source*
// constants. Values with no known equivalent come back trimmed and otherwise
// unchanged, and the input map is not modified.
func Normalise(origin string, dimensions map[string]string) map[string]string {
	out := make(map[string]string, len(dimensions))

	system := useragent.CanonicalOS(dimensions["visit:os"])

	for name, value := range dimensions {
		out[name] = normaliseValue(origin, name, value, system)
	}

	return out
}

// normaliseValue rewrites one value. The operating system is the row's own,
// already canonical, because a system version means different things on
// different systems.
func normaliseValue(origin, dimension, value, system string) string {
	value = strings.TrimSpace(value)

	switch dimension {
	case "visit:source":
		return referrer.CanonicalSource(value)

	case "visit:channel":
		return fold(value, channels)

	case "event:name", "event:page_title":
		return value
	}

	if placeholders[strings.ToLower(value)] {
		if dimension == "event:hostname" {
			return ingest.NoneHostname
		}

		return ""
	}

	switch dimension {
	case "visit:referrer":
		return referrer.CanonicalReferrer(value)

	case "visit:utm_medium":
		if origin == SourceGA4 && syntheticMediums[strings.ToLower(value)] {
			return ""
		}

		return value

	case "event:hostname":
		return canonicalHostname(value)

	case "event:page", "visit:entry_page", "visit:exit_page":
		return canonicalPath(value)

	case "visit:country":
		return canonicalCountry(value)

	case "visit:device":
		return useragent.CanonicalDevice(value)

	case "visit:screen":
		return fold(value, screenSizes)

	case "visit:browser":
		return useragent.CanonicalBrowser(value)

	case "visit:browser_version":
		return useragent.CanonicalBrowserVersion(value)

	case "visit:os":
		return system

	case "visit:os_version":
		return useragent.CanonicalOSVersion(system, value)
	}

	return value
}

// fold returns the indexed spelling of a value that matches one ignoring case,
// and the value itself otherwise.
func fold(value string, index map[string]string) string {
	if canonical, ok := index[strings.ToLower(value)]; ok {
		return canonical
	}

	return value
}

// canonicalHostname returns the hostname live collection stores for an
// imported one. A value that does not parse as a host is returned unchanged.
func canonicalHostname(value string) string {
	if value == "" || value == ingest.NoneHostname {
		return value
	}

	raw := value
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return value
	}

	return ingest.StoredHostname(parsed)
}

// canonicalPath returns the path live collection stores for an imported one:
// percent-escaped, with any query string removed and any fragment kept. A
// value that is not an absolute path is returned unchanged.
func canonicalPath(value string) string {
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return value
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return value
	}

	return ingest.StoredPath(parsed)
}

// canonicalCountry upper-cases a two-letter country code, which is the form
// live collection stores. Any other value is returned unchanged.
func canonicalCountry(value string) string {
	if len(value) != 2 {
		return value
	}

	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return value
		}
	}

	return strings.ToUpper(value)
}
