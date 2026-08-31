//
// negotiate.go
// Deciding which language a request gets, and letting the reader override it.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package i18n

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// CookieName is where a chosen language is remembered.
//
// A cookie rather than a column on the user: the choice has to work on the
// signed-out screens too — sign-in, registration, password reset — where there
// is no user to hang a preference on, and a reader who cannot read the sign-in
// page in their own language never gets far enough to have a preference stored.
const CookieName = "feasible_lang"

// QueryParam is the override in a URL. It exists so a language can be linked to
// directly: a support answer, a screenshot, or a translator checking their own
// work needs one URL, not a set of instructions for changing a browser setting.
const QueryParam = "lang"

// CookieMaxAge is how long a chosen language is remembered, in seconds. A year,
// because a language preference does not go stale and asking somebody to pick
// it again every month is the kind of small insult that makes a product feel
// foreign.
const CookieMaxAge = 365 * 24 * 60 * 60

// maxAcceptLanguage bounds how much of the header we will parse. The header is
// attacker-controlled and unbounded, and sorting a hundred thousand fragments
// per request is a free denial of service.
const maxAcceptLanguage = 512

// Negotiate decides which language one request is answered in.
//
// The precedence is explicit because every ordering has a failure somebody
// notices: a query parameter, then the cookie, then the browser's own
// Accept-Language, then English.
//
// The query parameter beats the cookie so a link into a specific language works
// even for a reader who has already chosen a different one. The cookie beats
// Accept-Language because a deliberate choice must outrank a browser default —
// the commonest complaint about language switching is a site that keeps
// resetting to the operating system's language.
func (c *Catalogue) Negotiate(r *http.Request) string {
	if requested := strings.TrimSpace(r.URL.Query().Get(QueryParam)); requested != "" {
		if tag, ok := c.match(requested); ok {
			return tag
		}
	}

	if cookie, err := r.Cookie(CookieName); err == nil {
		if tag, ok := c.match(strings.TrimSpace(cookie.Value)); ok {
			return tag
		}
	}

	if tag, ok := c.matchAccept(r.Header.Get("Accept-Language")); ok {
		return tag
	}

	return DefaultLocale
}

// match resolves one tag to a locale we actually have.
//
// An exact match wins, then the base language: a browser asking for "de-AT"
// gets the German catalogue, because Austrian German is far closer to German
// than to English and refusing it would serve a German speaker an English page
// over a region subtag.
func (c *Catalogue) match(tag string) (string, bool) {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tag == "" {
		return "", false
	}

	if c.Supported(tag) {
		return tag, true
	}

	if base := baseLanguage(tag); base != tag && c.Supported(base) {
		return base, true
	}

	return "", false
}

// acceptEntry is one language from an Accept-Language header, with its weight.
type acceptEntry struct {
	tag string

	// quality is the q-value. It is a float because the header's own grammar
	// says so, and rounding it to compare would collapse the deliberate
	// distinctions a browser makes between its second and third choices.
	quality float64

	// order is the position in the header, used to break ties. The
	// specification says equal weights keep their listed order, and sorting
	// without it makes the chosen language depend on Go's sort being stable.
	order int
}

// matchAccept reads the browser's own preference list.
//
// The header is parsed rather than string-matched because the order in it is
// not the preference order: `en;q=0.2, de;q=0.9` asks for German, and a naive
// "first tag wins" reads it as English. That is the bug that makes a European
// visitor's browser setting look ignored.
func (c *Catalogue) matchAccept(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", false
	}

	if len(header) > maxAcceptLanguage {
		header = header[:maxAcceptLanguage]
	}

	entries := make([]acceptEntry, 0, 8)

	for index, part := range strings.Split(header, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")

		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" {
			continue
		}

		entry := acceptEntry{tag: tag, quality: 1, order: index}

		if key, value, found := strings.Cut(params, "="); found && strings.TrimSpace(key) == "q" {
			quality, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || quality < 0 || quality > 1 {
				// A malformed q-value is treated as "no preference expressed"
				// rather than as a reason to drop the language: the reader
				// still asked for it.
				quality = 1
			}

			entry.quality = quality
		}

		// q=0 is the header's way of saying "not this one", and honouring it is
		// the difference between a preference list and a list of every language
		// the browser has ever heard of.
		if entry.quality == 0 {
			continue
		}

		entries = append(entries, entry)
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].quality != entries[j].quality {
			return entries[i].quality > entries[j].quality
		}

		return entries[i].order < entries[j].order
	})

	// The whole list is walked in preference order rather than only the first
	// entry, so a browser whose first choice we do not carry still gets its
	// second rather than falling straight to English.
	for _, entry := range entries {
		if entry.tag == "*" {
			continue
		}

		if tag, ok := c.match(entry.tag); ok {
			return tag, true
		}
	}

	return "", false
}

// RememberCookie is the cookie that stores a chosen language.
//
// It is deliberately not HttpOnly: the dashboard's own language switcher is
// JavaScript, and a cookie it cannot read is a switcher that has to round-trip
// through the server to change a label. Nothing in it is a credential — it is
// the name of a language — so the usual reason for HttpOnly does not apply.
// SameSite=Lax is kept, so a cross-site request cannot silently change the
// language of a page somebody is reading.
func RememberCookie(tag string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    tag,
		Path:     "/",
		MaxAge:   CookieMaxAge,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// Negotiate decides the language for a request using the product's catalogue.
func Negotiate(r *http.Request) string { return Default.Negotiate(r) }

// Apply resolves the language for a request and, when the reader asked for one
// explicitly, writes the cookie that remembers it.
//
// The two are done together because doing them apart is how a language switcher
// works once and then reverts on the next page: the query parameter is gone from
// the URL, and nothing wrote down what it said.
func Apply(w http.ResponseWriter, r *http.Request) string {
	tag := Default.Negotiate(r)

	if requested := strings.TrimSpace(r.URL.Query().Get(QueryParam)); requested != "" && Default.Supported(tag) {
		http.SetCookie(w, RememberCookie(tag, r.TLS != nil))
	}

	return tag
}
