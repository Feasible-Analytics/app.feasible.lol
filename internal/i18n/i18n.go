//
// i18n.go
// The message catalogue: one set of JSON files, read by both front ends.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package i18n holds every user-facing string in the product and hands them out
// in whichever language the visitor asked for.
//
// There is one catalogue, not two. The server-rendered screens read it directly
// through a template function, and the React dashboard reads the same merged map
// written into its bootstrap blob by the server. Two catalogues would drift the
// moment somebody translated a button on one screen and not the identical button
// on the other, and nothing would ever tell us.
//
// The files are plain JSON maps of dotted id to string, under
// locales/<tag>/<domain>.json. Flat rather than nested because both consumers
// want a flat map anyway, and because "which ids has this locale not
// translated" is then one set subtraction rather than a tree walk. Split into
// domain files rather than one file per locale because a translator takes on a
// screen at a time, and because two people working on different screens should
// not be editing the same file.
//
// Placeholders are named — {email}, {count} — rather than positional. A
// positional verb cannot be reordered by a translator whose grammar puts the
// object first, and the failure mode is a sentence that reads backwards in
// exactly the languages we cannot check.
//
// Nothing here fails silently. An id with no string in any locale comes back as
// the id itself, which is visible on screen and greppable, and it is recorded so
// a test can assert the catalogue is complete.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// catalogueFS holds every locale. The strings are embedded rather than read
// from disk for the same reason everything else in this binary is: a release is
// one file, and a locales directory that has to be copied alongside it is a
// directory that will be missing on somebody's server.
//
//go:embed locales
var catalogueFS embed.FS

// DefaultLocale is the language every other one falls back to, string by
// string. English is the source language: it is the one the strings are
// authored in, so it is the only locale guaranteed to be complete.
const DefaultLocale = "en"

// maxRecordedMissing caps the missing-id set. It is a diagnostic, not a log:
// a deployment serving a locale nobody finished translating would otherwise
// grow this map once per untranslated id and then stop, which is fine, but a
// bug that generated ids dynamically would grow it without bound.
const maxRecordedMissing = 500

// Locale is one language we can serve.
type Locale struct {
	// Tag is the BCP 47 tag, and the directory name under locales/.
	Tag string `json:"tag"`

	// Name is the language in English, for our own screens and documentation.
	Name string `json:"name"`

	// Native is the language in itself. A language picker that lists "German"
	// to somebody who only reads German is a picker they cannot use.
	Native string `json:"native"`

	// RTL is whether the script runs right to left, which the page shell turns
	// into dir="rtl". No locale we ship yet sets it; carrying the flag from the
	// start means adding one is a catalogue change rather than a layout change.
	RTL bool `json:"rtl"`
}

// names holds what each tag is called. It is a table in code rather than a
// field in the JSON because it is the same in every catalogue — a locale's own
// name does not vary by which locale is asking — and because an unlisted
// directory should not be able to become a shipping language by accident.
var names = map[string]Locale{
	"en": {Tag: "en", Name: "English", Native: "English"},
	"de": {Tag: "de", Name: "German", Native: "Deutsch"},
	"fr": {Tag: "fr", Name: "French", Native: "Français"},
	"es": {Tag: "es", Name: "Spanish", Native: "Español"},
}

// Catalogue is every locale's strings, loaded once.
type Catalogue struct {
	// messages is tag to id to string. It is read-only after load, so it needs
	// no lock; only the missing-id diagnostic below is written at runtime.
	messages map[string]map[string]string

	locales []Locale

	mu      sync.Mutex
	missing map[string]bool
}

// Default is the catalogue the whole product reads.
//
// It is built at package initialisation and panics on a malformed file, which
// is where a broken catalogue belongs. The alternative is a process that starts
// happily and then renders a screen of raw message ids to whoever opens it
// first, which is a bug report from a customer rather than a failed deploy.
var Default = mustLoad()

// mustLoad builds the embedded catalogue or stops the process.
func mustLoad() *Catalogue {
	catalogue, err := Load(catalogueFS, "locales")
	if err != nil {
		panic("i18n: " + err.Error())
	}

	return catalogue
}

// Load reads a catalogue out of any filesystem.
//
// It takes an fs.FS rather than reading the embedded one directly so that a
// test can build a catalogue from a handful of strings without adding a locale
// to the product, and so a future on-disk override directory needs no second
// loader.
func Load(files fs.FS, root string) (*Catalogue, error) {
	entries, err := fs.Glob(files, path.Join(root, "*", "*.json"))
	if err != nil {
		return nil, fmt.Errorf("find catalogue files: %w", err)
	}

	catalogue := &Catalogue{
		messages: map[string]map[string]string{},
		missing:  map[string]bool{},
	}

	for _, entry := range entries {
		tag := path.Base(path.Dir(entry))

		raw, err := fs.ReadFile(files, entry)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry, err)
		}

		var domain map[string]string
		if err := json.Unmarshal(raw, &domain); err != nil {
			return nil, fmt.Errorf("parse %s: %w", entry, err)
		}

		if catalogue.messages[tag] == nil {
			catalogue.messages[tag] = map[string]string{}
		}

		for id, text := range domain {
			// Two files claiming the same id is a merge that silently picks
			// one, and which one it picks depends on the order the glob
			// returned. Refusing is the only way the person who introduced the
			// duplicate finds out.
			if _, clash := catalogue.messages[tag][id]; clash {
				return nil, fmt.Errorf("%s defines %q, which another file in %s already defines", entry, id, tag)
			}

			catalogue.messages[tag][id] = text
		}
	}

	if len(catalogue.messages[DefaultLocale]) == 0 {
		return nil, fmt.Errorf("no %s catalogue was found under %s — it is the source language", DefaultLocale, root)
	}

	catalogue.locales = describe(catalogue.messages)

	return catalogue, nil
}

// describe turns the loaded tags into the ordered locale list a picker renders.
// English is pinned first because it is the source language and the fallback;
// the rest are alphabetical so the list does not reorder itself between builds.
func describe(messages map[string]map[string]string) []Locale {
	locales := make([]Locale, 0, len(messages))

	for tag := range messages {
		locale, known := names[tag]
		if !known {
			// A directory with no entry in the table still serves — the strings
			// are there and refusing them would help nobody — but it is
			// labelled with its tag so the omission is visible in the picker.
			locale = Locale{Tag: tag, Name: tag, Native: tag}
		}

		locales = append(locales, locale)
	}

	sort.Slice(locales, func(i, j int) bool {
		if locales[i].Tag == DefaultLocale {
			return true
		}
		if locales[j].Tag == DefaultLocale {
			return false
		}

		return locales[i].Tag < locales[j].Tag
	})

	return locales
}

// Locales lists what can be served, for a language picker.
func (c *Catalogue) Locales() []Locale {
	return c.locales
}

// Supported reports whether a tag has a catalogue of its own. It is an exact
// match on the tag: matching "de-AT" to "de" is negotiation, and it belongs in
// the negotiator where the precedence rules are written down.
func (c *Catalogue) Supported(tag string) bool {
	_, ok := c.messages[tag]

	return ok
}

// lookup finds one string, falling back to English and then to the id.
//
// The fallback is per string rather than per locale, which is what makes a
// half-finished translation useful: a locale that has translated the sign-in
// screen and nothing else shows a translated sign-in screen and English
// elsewhere, instead of being unusable until somebody finishes it.
func (c *Catalogue) lookup(locale, id string) string {
	if text, ok := c.messages[locale][id]; ok && text != "" {
		return text
	}

	if text, ok := c.messages[DefaultLocale][id]; ok && text != "" {
		return text
	}

	c.record(id)

	return id
}

// record notes an id with no string anywhere. Returning the id is what makes
// the gap visible on screen; recording it is what lets a test fail on it before
// anybody has to see it.
func (c *Catalogue) record(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.missing) >= maxRecordedMissing {
		return
	}

	c.missing[id] = true
}

// Missing lists the ids that were asked for and not found, sorted. It is the
// evidence behind "the catalogue is complete", and it is what a health check
// would read if we ever put one on this.
func (c *Catalogue) Missing() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	ids := make([]string, 0, len(c.missing))
	for id := range c.missing {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	return ids
}

// Has reports whether a locale has its own translation of an id, without
// falling back. It answers "what is left to translate", which is the one
// question fallback deliberately hides.
func (c *Catalogue) Has(locale, id string) bool {
	text, ok := c.messages[locale][id]

	return ok && text != ""
}

// IDs lists every id in the source language, sorted. It is the denominator for
// a translation-completeness figure and the input to the test that checks no
// screen references a string nobody wrote.
func (c *Catalogue) IDs() []string {
	ids := make([]string, 0, len(c.messages[DefaultLocale]))
	for id := range c.messages[DefaultLocale] {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	return ids
}

// T renders one string with its placeholders filled in.
//
// Arguments are alternating name and value — T("de", "sites.owner", "email",
// user.Email) — which is the same shape as this codebase's log calls, so there
// is one convention to remember rather than two. An odd trailing argument is
// ignored rather than fatal: a missing value should cost one placeholder, not
// the page it appears on.
func (c *Catalogue) T(locale, id string, args ...any) string {
	return interpolate(c.lookup(locale, id), args)
}

// N renders a string that changes with a count.
//
// The id names the base, and the plural category is a suffix on it:
// "sites.count_one" and "sites.count_other". A count is always available to the
// string as {count}, so the caller never passes it twice.
//
// This exists because the alternative — an English author writing "%d sites"
// and trusting every translator to notice — is how "1 sites" ships. Making the
// call site say "this varies with a number" is the only reliable moment to
// catch it.
func (c *Catalogue) N(locale, id string, count int, args ...any) string {
	category := pluralCategory(locale, count)

	text, ok := c.messages[locale][id+"_"+category]
	if !ok || text == "" {
		// A locale with no rule for this category falls back to the source
		// language's own categories rather than to the id, so an untranslated
		// plural still reads as a sentence.
		if text, ok = c.messages[DefaultLocale][id+"_"+pluralCategory(DefaultLocale, count)]; !ok || text == "" {
			c.record(id + "_" + category)

			return id
		}
	}

	return interpolate(text, append([]any{"count", count}, args...))
}

// Messages returns one locale's strings merged over English, which is exactly
// what the dashboard's bootstrap blob carries.
//
// It is merged here rather than in the browser so the client needs no fallback
// logic at all: it receives one flat map in which every id it can ask for is
// present. That is also what keeps the two front ends honest — they are reading
// the same resolved strings, not two implementations of the same fallback rule.
func (c *Catalogue) Messages(locale string) map[string]string {
	base := c.messages[DefaultLocale]
	merged := make(map[string]string, len(base))

	for id, text := range base {
		merged[id] = text
	}

	for id, text := range c.messages[locale] {
		if text != "" {
			merged[id] = text
		}
	}

	return merged
}

// Coverage is how much of the source language a locale has translated, as a
// fraction from zero to one. It is what a contributing guide quotes and what a
// release check looks at before advertising a language.
func (c *Catalogue) Coverage(locale string) float64 {
	base := c.messages[DefaultLocale]
	if len(base) == 0 {
		return 0
	}

	translated := 0
	for id := range base {
		if c.Has(locale, id) {
			translated++
		}
	}

	return float64(translated) / float64(len(base))
}

// interpolate substitutes {name} placeholders.
//
// It is a hand-written scan rather than a regular expression or text/template
// because it runs on every string on every page, and because the failure mode
// has to be gentle: an unknown placeholder is left exactly as written so the
// broken string is visible and searchable, rather than being replaced with
// "%!name(MISSING)" or eaten entirely.
func interpolate(text string, args []any) string {
	if len(args) < 2 || !strings.Contains(text, "{") {
		return text
	}

	var builder strings.Builder
	builder.Grow(len(text))

	for {
		open := strings.IndexByte(text, '{')
		if open < 0 {
			break
		}

		end := strings.IndexByte(text[open:], '}')
		if end < 0 {
			break
		}
		end += open

		name := text[open+1 : end]

		value, found := argument(args, name)
		if !found {
			// The braces are kept so the untranslatable gap shows up on the
			// page rather than becoming an invisible empty string.
			builder.WriteString(text[:end+1])
			text = text[end+1:]

			continue
		}

		builder.WriteString(text[:open])
		builder.WriteString(value)

		text = text[end+1:]
	}

	builder.WriteString(text)

	return builder.String()
}

// argument finds one named value in the alternating argument list and formats
// it. Only the types a message can usefully carry are given a fast path; the
// rest go through fmt, because refusing an unexpected type here would turn a
// caller's mistake into a blank sentence.
func argument(args []any, name string) (string, bool) {
	for i := 0; i+1 < len(args); i += 2 {
		key, ok := args[i].(string)
		if !ok || key != name {
			continue
		}

		switch value := args[i+1].(type) {
		case string:
			return value, true
		case int:
			return strconv.Itoa(value), true
		case int64:
			return strconv.FormatInt(value, 10), true
		case float64:
			return strconv.FormatFloat(value, 'f', -1, 64), true
		default:
			return fmt.Sprint(value), true
		}
	}

	return "", false
}

// pluralCategory picks the plural form a count takes in a language.
//
// This is the small subset of the CLDR rules that the languages we ship
// actually use, written out rather than pulled in as a dependency: a full rule
// engine is several hundred kilobytes of tables to answer a question that has
// two answers in every language currently in the catalogue. Adding a language
// with more categories — Polish, Russian, Arabic — means adding its rule here
// and the extra _few / _many strings to its files.
func pluralCategory(locale string, count int) string {
	if count < 0 {
		count = -count
	}

	switch baseLanguage(locale) {
	case "fr":
		// French treats zero as singular: "0 visiteur", not "0 visiteurs".
		if count <= 1 {
			return "one"
		}
	default:
		if count == 1 {
			return "one"
		}
	}

	return "other"
}

// baseLanguage strips a region or script subtag. "de-AT" and "de" share plural
// rules, and a rule table keyed on full tags would need a row per country for
// no difference in behaviour.
func baseLanguage(locale string) string {
	if index := strings.IndexAny(locale, "-_"); index > 0 {
		return locale[:index]
	}

	return locale
}

// T renders a string from the product's catalogue. The package-level spellings
// exist so a call site reads as i18n.T(locale, id) rather than threading the
// catalogue through every handler that renders a page.
func T(locale, id string, args ...any) string { return Default.T(locale, id, args...) }

// N renders a count-dependent string from the product's catalogue.
func N(locale, id string, count int, args ...any) string {
	return Default.N(locale, id, count, args...)
}

// Messages returns the merged strings for one locale from the product's
// catalogue.
func Messages(locale string) map[string]string { return Default.Messages(locale) }

// Locales lists the languages the product can serve.
func Locales() []Locale { return Default.Locales() }

// Supported reports whether the product has a catalogue for a tag.
func Supported(tag string) bool { return Default.Supported(tag) }
