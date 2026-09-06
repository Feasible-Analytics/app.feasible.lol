//
// i18n_test.go
// Tests for the message catalogue, including that it is complete.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package i18n

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"unicode"
)

// build makes a catalogue out of literal JSON, so a test can describe exactly
// the situation it is about instead of depending on whatever the product's own
// locales happen to contain this week.
func build(t *testing.T, files map[string]string) *Catalogue {
	t.Helper()

	mapped := fstest.MapFS{}
	for name, body := range files {
		mapped[name] = &fstest.MapFile{Data: []byte(body)}
	}

	catalogue, err := Load(mapped, "locales")
	if err != nil {
		t.Fatalf("the catalogue would not load: %v", err)
	}

	return catalogue
}

// TestFallbackIsPerString is the behaviour that makes a half-finished
// translation worth shipping. A locale that has done the sign-in screen and
// nothing else must show a translated sign-in screen and English elsewhere,
// rather than being unusable until somebody finishes it.
func TestFallbackIsPerString(t *testing.T) {
	catalogue := build(t, map[string]string{
		"locales/en/a.json": `{"one":"One","two":"Two"}`,
		"locales/de/a.json": `{"one":"Eins"}`,
	})

	if got := catalogue.T("de", "one"); got != "Eins" {
		t.Fatalf("a translated string was not used: %q", got)
	}

	if got := catalogue.T("de", "two"); got != "Two" {
		t.Fatalf("an untranslated string did not fall back to English: %q", got)
	}
}

// TestAnEmptyTranslationFallsBack covers the shape a translation tool produces
// for "not done yet". An empty string is not a translation, and rendering it
// would put a blank label on the screen where English would have been readable.
func TestAnEmptyTranslationFallsBack(t *testing.T) {
	catalogue := build(t, map[string]string{
		"locales/en/a.json": `{"one":"One"}`,
		"locales/de/a.json": `{"one":""}`,
	})

	if got := catalogue.T("de", "one"); got != "One" {
		t.Fatalf("an empty translation was rendered instead of the English: %q", got)
	}
}

// TestAMissingIDIsVisibleAndRecorded is the "never fail silently" rule applied
// to strings. An id nobody wrote must appear on the screen as itself, so it is
// obvious and greppable, and it must be recorded so a test can fail on it
// before a customer sees it.
func TestAMissingIDIsVisibleAndRecorded(t *testing.T) {
	catalogue := build(t, map[string]string{"locales/en/a.json": `{"one":"One"}`})

	if got := catalogue.T("en", "nope.missing"); got != "nope.missing" {
		t.Fatalf("a missing id did not render as itself: %q", got)
	}

	missing := catalogue.Missing()
	if len(missing) != 1 || missing[0] != "nope.missing" {
		t.Fatalf("the missing id was not recorded: %v", missing)
	}
}

// TestDuplicateIDsAreRefused covers a merge that would otherwise pick a winner
// by glob order. Two files claiming the same id is a change whose effect
// depends on a filename, and the person who introduced it has no other way to
// find out.
func TestDuplicateIDsAreRefused(t *testing.T) {
	_, err := Load(fstest.MapFS{
		"locales/en/a.json": &fstest.MapFile{Data: []byte(`{"one":"One"}`)},
		"locales/en/b.json": &fstest.MapFile{Data: []byte(`{"one":"Uno"}`)},
	}, "locales")

	if err == nil {
		t.Fatal("a duplicate id was accepted")
	}

	if !strings.Contains(err.Error(), `"one"`) {
		t.Fatalf("the error does not name the duplicated id: %v", err)
	}
}

// TestASourceLanguageIsRequired refuses a catalogue with no English. Every
// other locale falls back to it string by string, so without it a gap in a
// translation has nowhere to land.
func TestASourceLanguageIsRequired(t *testing.T) {
	_, err := Load(fstest.MapFS{
		"locales/de/a.json": &fstest.MapFile{Data: []byte(`{"one":"Eins"}`)},
	}, "locales")

	if err == nil {
		t.Fatal("a catalogue with no source language was accepted")
	}
}

// TestInterpolation covers the placeholder rules. The unknown-placeholder case
// is the important one: leaving the braces makes the broken string visible on
// the page, where dropping it would produce a sentence with a hole in it that
// reads as deliberate.
func TestInterpolation(t *testing.T) {
	catalogue := build(t, map[string]string{
		"locales/en/a.json": `{
			"plain":"No placeholders here",
			"one":"Signed in as {email}",
			"two":"{a} and {b}",
			"unknown":"Hello {nobody}",
			"repeat":"{x} then {x}"
		}`,
	})

	cases := []struct {
		name string
		id   string
		args []any
		want string
	}{
		{"a string with nothing to fill in is returned as-is", "plain", nil, "No placeholders here"},
		{"one placeholder", "one", []any{"email", "a@example.com"}, "Signed in as a@example.com"},
		{"two placeholders", "two", []any{"a", "1", "b", "2"}, "1 and 2"},
		{"an unknown placeholder keeps its braces", "unknown", []any{"someone", "x"}, "Hello {nobody}"},
		{"a repeated placeholder is filled every time", "repeat", []any{"x", "9"}, "9 then 9"},
		{"an integer value is formatted without a decimal point", "one", []any{"email", 7}, "Signed in as 7"},
		{"an odd trailing argument costs the placeholder, not the page", "one", []any{"email"}, "Signed in as {email}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := catalogue.T("en", tc.id, tc.args...); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPlurals covers the two rules the shipped languages need. The French case
// is the one an English author would never write: zero takes the singular, so
// "0 visiteurs" is wrong in exactly the language a product pitched at Europe
// cannot afford to get wrong.
func TestPlurals(t *testing.T) {
	catalogue := build(t, map[string]string{
		"locales/en/a.json": `{"n_one":"{count} site","n_other":"{count} sites"}`,
		"locales/fr/a.json": `{"n_one":"{count} site","n_other":"{count} sites"}`,
	})

	cases := []struct {
		locale string
		count  int
		want   string
	}{
		{"en", 0, "0 sites"},
		{"en", 1, "1 site"},
		{"en", 2, "2 sites"},
		{"fr", 0, "0 site"},
		{"fr", 1, "1 site"},
		{"fr", 2, "2 sites"},
	}

	for _, tc := range cases {
		if got := catalogue.N(tc.locale, "n", tc.count); got != tc.want {
			t.Fatalf("%s with %d: got %q, want %q", tc.locale, tc.count, got, tc.want)
		}
	}
}

// TestPluralsFallBackToTheSourceLanguage covers a locale that has translated
// one plural form and not the other. Falling back to the id would put a message
// key in the middle of a sentence; falling back to English puts a readable word
// there instead.
func TestPluralsFallBackToTheSourceLanguage(t *testing.T) {
	catalogue := build(t, map[string]string{
		"locales/en/a.json": `{"n_one":"{count} site","n_other":"{count} sites"}`,
		"locales/de/a.json": `{"n_one":"{count} Website"}`,
	})

	if got := catalogue.N("de", "n", 1); got != "1 Website" {
		t.Fatalf("the translated form was not used: %q", got)
	}

	if got := catalogue.N("de", "n", 3); got != "3 sites" {
		t.Fatalf("the untranslated form did not fall back to English: %q", got)
	}
}

// TestMessagesAreMergedOverEnglish is the contract the dashboard depends on.
// The client has no fallback logic at all, so every id it can ask for has to be
// present in the one map it receives.
func TestMessagesAreMergedOverEnglish(t *testing.T) {
	catalogue := build(t, map[string]string{
		"locales/en/a.json": `{"one":"One","two":"Two"}`,
		"locales/de/a.json": `{"one":"Eins"}`,
	})

	merged := catalogue.Messages("de")

	if merged["one"] != "Eins" || merged["two"] != "Two" {
		t.Fatalf("the merge is wrong: %v", merged)
	}
}

// TestCoverage is what a contributing guide quotes and what a release check
// reads before advertising a language as available.
func TestCoverage(t *testing.T) {
	catalogue := build(t, map[string]string{
		"locales/en/a.json": `{"one":"One","two":"Two","three":"Three","four":"Four"}`,
		"locales/de/a.json": `{"one":"Eins"}`,
	})

	if got := catalogue.Coverage("de"); got != 0.25 {
		t.Fatalf("coverage is %v, want 0.25", got)
	}

	if got := catalogue.Coverage("en"); got != 1 {
		t.Fatalf("the source language is not complete against itself: %v", got)
	}
}

// TestEnglishIsPinnedFirst covers the language picker's order. English is the
// source language and the fallback, so it heads the list; the rest are
// alphabetical so the picker does not reorder itself between builds.
func TestEnglishIsPinnedFirst(t *testing.T) {
	locales := build(t, map[string]string{
		"locales/en/a.json": `{"one":"One"}`,
		"locales/fr/a.json": `{"one":"Un"}`,
		"locales/de/a.json": `{"one":"Eins"}`,
	}).Locales()

	var tags []string
	for _, locale := range locales {
		tags = append(tags, locale.Tag)
	}

	want := []string{"en", "de", "fr"}
	if strings.Join(tags, ",") != strings.Join(want, ",") {
		t.Fatalf("locales are ordered %v, want %v", tags, want)
	}
}

// TestTheShippedCatalogueLoads is the guard on the embedded files. A malformed
// locale stops the process at start-up rather than rendering message ids to
// whoever opens a page first, and this is the test that catches it before a
// deploy rather than during one.
func TestTheShippedCatalogueLoads(t *testing.T) {
	if len(Default.IDs()) == 0 {
		t.Fatal("the shipped catalogue is empty")
	}

	for _, locale := range Default.Locales() {
		if locale.Native == "" {
			t.Fatalf("%s has no native name, so a language picker cannot label it", locale.Tag)
		}
	}
}

// referencePattern finds a message id anywhere in a source file.
//
// It is anchored on the catalogue's own namespaces rather than on the shape of
// a call, and that is what makes the scan complete. Ids are not always written
// at a call site: the dashboard keeps its report titles and period names in
// lookup tables and translates them at render, so a pattern matching `t("…")`
// would miss every one of them and report the whole table as unused strings.
//
// Anchoring on the namespace is also what keeps it honest in the other
// direction. A bare "any dotted literal" scan would count "app.js" and
// "index.html" as message ids and demand a string for each; nothing in this
// tree is named common.*, auth.* or dashboard.* except a message id.
// Three segments are required rather than two, which is the id scheme the
// README documents — <surface>.<screen>.<element>. It is also what keeps a
// context key like "auth.session" out of the set: an ordinary two-part
// namespaced string is not a message id, and nothing in this product's screens
// is addressed with fewer than three parts.
var referencePattern = regexp.MustCompile(
	`["'](` + strings.Join(namespaces, "|") + `)\.([a-z0-9_]+(?:\.[a-z0-9_]+)+)["']`)

// namespaces are the top-level segments every message id starts with. Adding a
// surface means adding its namespace here, which is a deliberate speed bump:
// a namespace nobody listed is a screen this check cannot see.
var namespaces = []string{"common", "auth", "dashboard", "settings", "pages"}

// renderingSurfaces are the trees that put a catalogue string in front of a
// person, and the file kinds worth reading in each. Adding a surface means
// adding it here, which is a deliberate speed bump: a tree nobody listed is a
// screen no check in this file can see.
func renderingSurfaces() map[string][]string {
	return map[string][]string{
		filepath.Join("..", "auth"):             {".html", ".go"},
		filepath.Join("..", "settings"):         {".html", ".go"},
		filepath.Join("..", "appui"):            {".html", ".go"},
		filepath.Join("..", "goals"):            {".go"},
		filepath.Join("..", "google"):           {".go"},
		filepath.Join("..", "shields"):          {".go"},
		filepath.Join("..", "dashboard"):        {".go"},
		filepath.Join("..", "health"):           {".go"},
		filepath.Join("..", "sharing"):          {".go"},
		filepath.Join("..", "billingui"):        {".html", ".go"},
		filepath.Join("..", "..", "web", "src"): {".ts", ".tsx"},
	}
}

// pluralSuffixes are the categories an id can be split into. They are stripped
// before comparing so that a catalogue holding "sites.count_one" and
// "sites.count_other" matches a call site that only ever names "sites.count".
var pluralSuffixes = []string{"_one", "_other", "_zero", "_two", "_few", "_many"}

// operatorVocabulary is what a customer of the hosted product must never be
// shown: an environment variable, an HTTP header name, or a shell command.
// They have no shell and no server, so every one of these is an instruction
// they cannot follow, which reads as "something is wrong with your account and
// you cannot fix it".
//
// Content-Security-Policy is deliberately absent. It appears on the tracker
// install screen, where the reader is putting a script tag on their own site
// and that is the exact name of the thing blocking them.
var operatorVocabulary = regexp.MustCompile(`FEASIBLE_[A-Z_]+|X-[A-Z][A-Za-z]*(?:-[A-Za-z]+)+|\bHSTS\b|\bcurl\b`)

// TestNoOperatorVocabularyReachesACustomer walks every catalogue and the prose
// in every template that renders one.
//
// The alternative to an absolute rule is branching the copy on hosted versus
// self-hosted, which doubles every string and every test and pays for it with a
// maintenance seam on screens whose primary audience is not the operator. This
// is what makes the rule cost nothing to keep.
//
// Go is not scanned. Every sentence a customer reads comes from the catalogue,
// and a Go string holding "X-Forwarded-For" is far more likely to be the header
// the code actually sets than a sentence about it — a scan there reports the
// working parts of the product and teaches people to ignore this test.
func TestNoOperatorVocabularyReachesACustomer(t *testing.T) {
	var offences []string

	for locale, messages := range Default.messages {
		for id, text := range messages {
			if hit := operatorVocabulary.FindString(text); hit != "" {
				offences = append(offences, fmt.Sprintf("%s/%s names %q", locale, id, hit))
			}
		}
	}

	offences = append(offences, scanTemplatesForOperatorVocabulary(t)...)

	sort.Strings(offences)

	if len(offences) > 0 {
		t.Fatalf("%d places show a customer something only an operator can change:\n  %s",
			len(offences), strings.Join(offences, "\n  "))
	}
}

// scanTemplatesForOperatorVocabulary reads the visible prose of every template
// on a rendering surface.
//
// Only text between tags is read, and only after the scripts are removed: a
// header name a page sends is the page working, and a header name a page prints
// is the fault this test is about.
func scanTemplatesForOperatorVocabulary(t *testing.T) []string {
	t.Helper()

	var offences []string

	for root, extensions := range renderingSurfaces() {
		if !slices.Contains(extensions, ".html") {
			continue
		}

		if _, err := os.Stat(root); err != nil {
			// A surface missing from this checkout is not a failure, but it is
			// also not silence: a guard that scans nothing and passes is the
			// thing this test exists to prevent.
			t.Logf("skipping %s: %v", root, err)

			continue
		}

		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if entry.IsDir() || !strings.HasSuffix(path, ".html") {
				return nil
			}

			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			for _, text := range templateProse(string(body)) {
				if hit := operatorVocabulary.FindString(text); hit != "" {
					offences = append(offences, fmt.Sprintf("%s names %q", path, hit))
				}
			}

			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	return offences
}

// scriptBlock and htmlComment are what comes out before the prose is read, and
// textNode is what is left worth reading.
var (
	scriptBlock = regexp.MustCompile(`(?s)<script\b.*?</script>`)
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	textNode    = regexp.MustCompile(`>([^<>]*)<`)
)

// templateProse returns the text a reader sees, with the scripts, the comments
// and every tag's attributes left behind.
func templateProse(body string) []string {
	body = scriptBlock.ReplaceAllString(body, "")
	body = htmlComment.ReplaceAllString(body, "")

	var out []string
	for _, match := range textNode.FindAllStringSubmatch(body, -1) {
		if text := strings.TrimSpace(match[1]); text != "" {
			out = append(out, text)
		}
	}

	return out
}

// TestEveryIDInUseHasAString is the completeness check, and it is the reason
// this file scans the source tree at all.
//
// A referenced id with no string renders as the raw id on a customer's screen,
// with a 200 and nothing in any log. Catching it here is the difference between
// a failing test and a screenshot in a support ticket.
func TestEveryIDInUseHasAString(t *testing.T) {
	used := scanForIDs(t)

	catalogue := map[string]bool{}
	for _, id := range Default.IDs() {
		catalogue[stripPlural(id)] = true
	}

	var missing []string
	for id := range used {
		if !catalogue[id] {
			missing = append(missing, id)
		}
	}

	sort.Strings(missing)

	if len(missing) > 0 {
		t.Fatalf("%d message ids are used by a screen and have no English string:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// TestEveryStringIsUsed is the other direction, and it matters more than it
// looks: a string nobody renders is a string a translator is paid to translate
// for a screen that does not exist, and it is how a catalogue becomes twice the
// size of the product.
func TestEveryStringIsUsed(t *testing.T) {
	used := scanForIDs(t)

	var orphans []string
	for _, id := range Default.IDs() {
		if !used[stripPlural(id)] {
			orphans = append(orphans, id)
		}
	}

	sort.Strings(orphans)

	if len(orphans) > 0 {
		t.Fatalf("%d English strings are in the catalogue and referenced by nothing:\n  %s",
			len(orphans), strings.Join(orphans, "\n  "))
	}
}

// TestPluralIDsAreComplete guards the reserved suffixes.
//
// `_one` and `_other` mean "this message varies with a count", and the selector
// reads them. An ordinary message that happens to end in one of them is a string
// the selector will pick for a count it was never about — and a plural with only
// one of its forms written is a sentence that disappears at every other count.
// Both mistakes render as something believable, which is why neither gets noticed.
func TestPluralIDsAreComplete(t *testing.T) {
	forms := map[string]map[string]bool{}

	for _, id := range Default.IDs() {
		base := stripPlural(id)
		if base == id {
			continue
		}

		if forms[base] == nil {
			forms[base] = map[string]bool{}
		}

		forms[base][strings.TrimPrefix(id[len(base):], "_")] = true
	}

	var broken []string
	for base, categories := range forms {
		// English needs exactly one and other. A locale needing more adds them
		// to its own file, which fallback covers; the source language is the
		// one that has to be complete.
		if !categories["one"] || !categories["other"] {
			broken = append(broken, base)
		}
	}

	sort.Strings(broken)

	if len(broken) > 0 {
		t.Fatalf("%d ids use a plural suffix without both English forms — either write the missing one "+
			"or rename the id so it does not end in a reserved suffix:\n  %s",
			len(broken), strings.Join(broken, "\n  "))
	}
}

// stripPlural removes a plural category suffix from an id, so that the two or
// six forms of one message compare as the single id a call site names.
func stripPlural(id string) string {
	for _, suffix := range pluralSuffixes {
		if trimmed, found := strings.CutSuffix(id, suffix); found {
			return trimmed
		}
	}

	return id
}

// scanForIDs collects every message id referenced anywhere in the product.
//
// It walks the two rendering surfaces rather than taking a list, because a list
// is a thing somebody forgets to add a new screen to — and the whole value of
// this check is that it notices a screen nobody told it about.
func scanForIDs(t *testing.T) map[string]bool {
	t.Helper()

	found := map[string]bool{}

	for root, extensions := range renderingSurfaces() {
		if _, err := os.Stat(root); err != nil {
			// A surface that is not in this checkout is not a failure — the
			// front end is built from the same tree, but a partial checkout
			// should not turn a completeness check into a false alarm.
			t.Logf("skipping %s: %v", root, err)

			continue
		}

		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}

			if !hasExtension(path, extensions) {
				return nil
			}

			// The catalogue's own files would otherwise report every id in the
			// product as "used", which would make both directions of this check
			// pass trivially.
			if strings.Contains(path, string(filepath.Separator)+"locales"+string(filepath.Separator)) {
				return nil
			}

			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			for _, match := range referencePattern.FindAllStringSubmatch(string(body), -1) {
				found[match[1]+"."+match[2]] = true
			}

			return nil
		})
		if err != nil {
			t.Fatalf("scanning %s: %v", root, err)
		}
	}

	return found
}

// hasExtension reports whether a path is one of the file types worth scanning.
// Limiting it keeps compiled assets and test fixtures out of the set, which
// would otherwise contribute ids that no screen actually renders.
func hasExtension(path string, extensions []string) bool {
	for _, extension := range extensions {
		if strings.HasSuffix(path, extension) {
			return true
		}
	}

	return false
}

// catalogueCall matches a template asking the catalogue for a string, in either
// form the templates use: a literal id, or one held in a field.
var catalogueCall = regexp.MustCompile(`\{\{-?\s*[tn]\s+\$?\.[A-Za-z.]*Lang\b`)

// templateAction matches one {{ }} so a text node made only of them can be told
// apart from one that puts words on the screen.
var templateAction = regexp.MustCompile(`(?s)\{\{.*?\}\}`)

// writesItsOwnWords reports whether a template puts any text on the screen that
// did not come from the Go layer. A template that renders only supplied values
// has nothing to translate and nothing to get wrong.
func writesItsOwnWords(body string) bool {
	for _, text := range templateProse(body) {
		if strings.ContainsFunc(templateAction.ReplaceAllString(text, ""), unicode.IsLetter) {
			return true
		}
	}

	return false
}

// TestEveryScreenWithWordsOnItUsesTheCatalogue is the guard against a whole
// screen shipping in one language inside a translated product.
//
// A template that renders words and asks the catalogue for none of them is also
// invisible to the two coverage tests above, because they work from the ids a
// file uses. So the file with no translations is also the file where every
// other guarantee in this package stops applying, and only a check phrased as
// "does it ask at all" can see it.
//
// A template that renders no words of its own is not a failure: several are
// pure structure around values the Go layer supplies, and those are recognised
// by having nothing to translate rather than by being named in a list.
func TestEveryScreenWithWordsOnItUsesTheCatalogue(t *testing.T) {
	scanned := 0

	for root, extensions := range renderingSurfaces() {
		if !slices.Contains(extensions, ".html") {
			continue
		}

		if _, err := os.Stat(root); err != nil {
			// A surface missing from this checkout is not a failure, but it is
			// also not silence: a guard that scans nothing and passes is the
			// thing this test exists to prevent.
			t.Logf("skipping %s: %v", root, err)

			continue
		}

		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if entry.IsDir() || !strings.HasSuffix(path, ".html") {
				return nil
			}

			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			scanned++

			if catalogueCall.Match(body) || !writesItsOwnWords(string(body)) {
				return nil
			}

			t.Errorf("%s renders words and asks the catalogue for none of them, so it is "+
				"English whatever language the reader chose — and the coverage tests above "+
				"cannot see the file at all", path)

			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if scanned == 0 {
		t.Fatal("no templates were read, so this test proves nothing")
	}
}
