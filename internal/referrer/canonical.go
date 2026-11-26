//
// canonical.go
// Turning a source label from imported history into the name live collection stores.
//
// Created: 2026-11-19
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package referrer

import (
	"net/url"
	"strings"
)

// directLabels are the spellings imported history uses for a visit with no
// source. Live collection stores that visit with an empty source.
var directLabels = map[string]bool{
	"(direct)":      true,
	"direct":        true,
	"direct / none": true,
}

// importedSourceNames maps the display names other analytics tools give a
// source onto the name live collection stores for the same referrer. Keys are
// lower case. A name live collection does not know maps to the registrable
// domain its referrer resolves to, because that is what live collection stores
// for an unrecognised referrer.
var importedSourceNames = map[string]string{
	"x (twitter)":   "X",
	"google gemini": "Gemini",
	"brave":         "Brave Search",
	"vkontakte":     "VK",
	"sina weibo":    "Weibo",
	"outlook.com":   "Outlook",
	"protonmail":    "Proton Mail",
	"wikipedia":     "wikipedia.org",
	"phind":         "phind.com",
}

// canonicalNames indexes every source name live collection can store by its
// lower-case spelling, so a label that differs only in case folds onto it.
var canonicalNames = buildCanonicalNames()

// buildCanonicalNames collects the names from every source table: hostnames,
// country-domain families, Android packages and UTM aliases.
func buildCanonicalNames() map[string]string {
	names := map[string]string{}

	add := func(name string) {
		names[strings.ToLower(name)] = name
	}

	for _, source := range hosts {
		add(source.Name)
	}
	for _, source := range secondLevel {
		add(source.Name)
	}
	for _, source := range androidPackages {
		add(source.Name)
	}
	for _, source := range utmSourceAliases {
		add(source.Name)
	}

	return names
}

// CanonicalSource returns the source name live collection would store for a
// source label taken from imported history.
//
// The label is resolved in order: a direct label becomes the empty source; a
// display name another tool uses becomes ours; a name that differs from ours
// only in case takes our spelling; a UTM alias or Android package resolves the
// way it does at ingest; and a hostname resolves through the host table, or to
// its registrable domain when the table has no entry. Anything else is returned
// trimmed and otherwise unchanged.
func CanonicalSource(label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}

	key := strings.ToLower(label)

	if directLabels[key] {
		return ""
	}

	if name, ok := importedSourceNames[key]; ok {
		return name
	}

	if name, ok := canonicalNames[key]; ok {
		return name
	}

	if source, ok := utmSourceAliases[key]; ok {
		return source.Name
	}

	if strings.HasPrefix(key, androidScheme) {
		return Parse(label, "").Source
	}

	if source, ok := androidPackages[key]; ok {
		return source.Name
	}

	if looksLikeHost(key) {
		host := normaliseHost(key)
		if source, ok := lookupHost(host); ok {
			return source.Name
		}

		return registrable(host)
	}

	return label
}

// CanonicalReferrer returns the referrer live collection would store for a
// referrer taken from imported history: host and path, lower-case host, no
// scheme, no leading "www.", no query string and no trailing slash. A value
// that does not parse as a URL is returned trimmed and otherwise unchanged.
func CanonicalReferrer(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if strings.HasPrefix(strings.ToLower(value), androidScheme) {
		return Parse(value, "").Referrer
	}

	raw := value
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return value
	}

	resolved := Parse(raw, "")
	if resolved.Referrer == "" {
		return value
	}

	return resolved.Referrer
}

// looksLikeHost reports whether a lower-case label is a bare hostname: dotted,
// with no spaces, slashes or other characters a hostname cannot carry.
func looksLikeHost(label string) bool {
	if !strings.Contains(label, ".") || strings.HasPrefix(label, ".") || strings.HasSuffix(label, ".") {
		return false
	}

	for _, c := range label {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-', c == ':':
		default:
			return false
		}
	}

	return true
}
