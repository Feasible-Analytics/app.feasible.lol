//
// render_test.go
// What the shared layout puts on the page, and what it leaves off.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"html/template"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestAContentWithNoCodeRendersNoCodeBlock keeps the verification email's one
// block from appearing, empty, on the other twenty-two.
func TestAContentWithNoCodeRendersNoCodeBlock(t *testing.T) {
	plain, err := Content{Subject: "S", Heading: "H", Body: []string{"B"}}.HTML()
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(plain, "letter-spacing:6px") {
		t.Errorf("a content with no code rendered the code block:\n%s", plain)
	}

	coded, err := Content{Subject: "S", Heading: "H", Code: "12345678"}.HTML()
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(coded, "12345678") || !strings.Contains(coded, "letter-spacing:6px") {
		t.Errorf("the code was not rendered prominently:\n%s", coded)
	}

	if !strings.Contains(Content{Code: "12345678"}.Text(), "12345678") {
		t.Error("the plain-text part lost the code")
	}
}

// TestAMessageWithNoNewBlocksHasNoGapWhereTheyWouldBe covers the plain-text
// twin, which the golden file above does not.
func TestAMessageWithNoNewBlocksHasNoGapWhereTheyWouldBe(t *testing.T) {
	text := plainMessage().Text()

	if strings.Contains(text, "\n\n\n") {
		t.Errorf("a blank block was rendered where a kicker, note, figure or table would be:\n%q", text)
	}
}

// TestAFigureColoursItselfFromItsDirection keeps the three change colours in
// one place, since they are inlined on the element and cannot be a variable.
func TestAFigureColoursItselfFromItsDirection(t *testing.T) {
	// Literals, not constants. Comparing a constant against itself passes just
	// as happily with growth coloured red.
	for direction, want := range map[string][3]string{
		"up":       {"up", "#166534", "#86efac"},
		"down":     {"down", "#991b1b", "#fca5a5"},
		"flat":     {"flat", "#605d5d", "#bab6b6"},
		"":         {"flat", "#605d5d", "#bab6b6"},
		"sideways": {"flat", "#605d5d", "#bab6b6"},
	} {
		class, light, dark := change(direction)

		if class != want[0] {
			t.Errorf("direction %q has class %q, want %q", direction, class, want[0])
		}

		if string(light) != want[1] {
			t.Errorf("direction %q is %s by day, want %s", direction, light, want[1])
		}

		// The dark colour too: the class is a media-query hook, so a class and
		// a colour that disagree render a falling metric green at night with
		// nothing about the light rendering to say so.
		if string(dark) != want[2] {
			t.Errorf("direction %q is %s at night, want %s", direction, dark, want[2])
		}
	}
}

// TestTheKickerToneIsTheSameColourByDayAndNight pins the other pair.
func TestTheKickerToneIsTheSameColourByDayAndNight(t *testing.T) {
	for name, want := range map[Tone][3]string{
		ToneAlarm: {"alarm", "#991b1b", "#fca5a5"},
		"":        {"accent", "#ae1800", "#ff8f77"},
		"typo":    {"accent", "#ae1800", "#ff8f77"},
	} {
		class, light, dark := tone(name)

		if class != want[0] || string(light) != want[1] || string(dark) != want[2] {
			t.Errorf("tone %q is %q/%s/%s, want %q/%s/%s", name, class, light, dark, want[0], want[1], want[2])
		}
	}
}

// TestTheAlarmKickerIsNotTheAccent keeps a spike alert visually distinct from a
// weekly report at a glance.
func TestTheAlarmKickerIsNotTheAccent(t *testing.T) {
	alarm := Content{Kicker: "Spike alert", KickerTone: ToneAlarm}
	plain := Content{Kicker: "Weekly report"}

	if alarm.KickerColour() == plain.KickerColour() {
		t.Errorf("both kicker tones render %s", alarm.KickerColour())
	}
}

// plainMessage is a message using only the blocks the layout has always had.
// It is the fixture the golden file below is rendered from.
func plainMessage() Content {
	return Content{
		Subject: "Your site is close to its pageview limit",
		Heading: "Your site is close to its pageview limit",
		Body: []string{
			"harbor.my has used 87% of the pageviews included in your plan this month.",
			"Nothing stops when you reach the limit. We will get in touch about the next tier.",
		},
		Facts: []Fact{
			{Label: "Used this month", Value: "870,412"},
			{Label: "Included", Value: "1,000,000"},
			{Label: "Resets", Value: "Tue, 1 October 2026"},
		},
		Primary:   Button{Label: "Open the dashboard", URL: "https://app.feasible.lol/harbor.my"},
		Secondary: []Button{{Label: "See the plans", URL: "https://app.feasible.lol/billing"}},
		Closing:   "You are receiving this because you own this site.",
	}
}

// TestAPlainMessageMatchesItsGolden makes every change to the layout a
// deliberate one.
//
// A message using only the blocks most messages use is rendered and compared
// byte for byte. A block that leaks whitespace or markup into a message that
// does not carry it shows up here, and so does an accidental change to the
// shell every message shares.
func TestAPlainMessageMatchesItsGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "plain_message.html"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := plainMessage().HTML()
	if err != nil {
		t.Fatal(err)
	}

	if got != string(want) {
		t.Errorf("the layout changed. If that was the point, regenerate testdata/plain_message.html."+
			"\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// TestTheMetricRowWrapsEvenly keeps a fifth figure from squeezing the other
// four instead of starting a second line.
func TestTheMetricRowWrapsEvenly(t *testing.T) {
	figures := func(n int) []Figure {
		made := make([]Figure, n)
		for i := range made {
			made[i] = Figure{Label: "Metric", Value: "1"}
		}

		return made
	}

	for count, want := range map[int][]int{
		0: nil,
		1: {1},
		4: {4},
		5: {3, 3},
		6: {3, 3},
		8: {4, 4},
		9: {3, 3, 3},
	} {
		rows := Content{Figures: figures(count)}.FigureRows()

		if len(rows) != len(want) {
			t.Errorf("%d figures made %d rows, want %d", count, len(rows), len(want))

			continue
		}

		for i, row := range rows {
			// Padded, so every row is the same width and the columns line up.
			if len(row.Cells) != want[i] {
				t.Errorf("%d figures: row %d has %d cells, want %d", count, i, len(row.Cells), want[i])
			}
		}
	}
}

// TestAPaddingCellCarriesNoLabel keeps the empty cell that squares off a short
// row from rendering a blank figure.
func TestAPaddingCellCarriesNoLabel(t *testing.T) {
	content := Content{Figures: []Figure{
		{Label: "Visitors", Value: "1"},
		{Label: "Pageviews", Value: "2"},
		{Label: "Bounce rate", Value: "3"},
		{Label: "Visit duration", Value: "4"},
		{Label: "Visits", Value: "5"},
	}}

	rows := content.FigureRows()
	if len(rows) != 2 || rows[1].Cells[2].Label != "" {
		t.Fatalf("the short row is not padded with an empty cell: %+v", rows)
	}

	html, err := content.HTML()
	if err != nil {
		t.Fatal(err)
	}

	// Five labels rendered, not six.
	if got := strings.Count(html, `<div class="muted"`); got != 5 {
		t.Errorf("%d figure labels rendered, want 5:\n%s", got, html)
	}
}

// TestTheFooterSeparatorsSuitTheirBody keeps one definition rendering correctly
// in two places: a line break between the address lines in the HTML, a newline
// between them in the text.
func TestTheFooterSeparatorsSuitTheirBody(t *testing.T) {
	html, err := plainMessage().HTML()
	if err != nil {
		t.Fatal(err)
	}

	text := plainMessage().Text()

	previous := Company.Name

	for _, line := range Company.AddressLines {
		if !strings.Contains(html, previous+"<br>\n"+line) {
			t.Errorf("the HTML footer does not break between %q and %q:\n%s", previous, line, html)
		}

		if !strings.Contains(text, previous+"\n"+line) {
			t.Errorf("the text footer does not break between %q and %q:\n%s", previous, line, text)
		}

		previous = line
	}

	// And no <br> leaks into the plain-text part.
	if strings.Contains(text, "<br>") {
		t.Errorf("the text footer carries markup:\n%s", text)
	}
}

// TestThePreheaderIsRenderedAndHidden covers the ~90 characters a phone shows
// beside the subject, which with nothing set is whatever the client scrapes.
func TestThePreheaderIsRenderedAndHidden(t *testing.T) {
	content := plainMessage()
	content.Preheader = "870,412 of 1,000,000 pageviews used this month."

	html, err := content.HTML()
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(html, content.Preheader) {
		t.Fatalf("the preheader is not in the message:\n%s", html)
	}

	// Hidden, and before anything else in the body: a preheader after the
	// wordmark previews the wordmark.
	hidden := `<div style="display:none; max-height:0; overflow:hidden; mso-hide:all;">` + content.Preheader
	if !strings.Contains(html, hidden) {
		t.Errorf("the preheader is not hidden:\n%s", html)
	}

	if strings.Index(html, content.Preheader) > strings.Index(html, "Feasible<span") {
		t.Error("the preheader is rendered after the wordmark")
	}
}

// TestAnUnsetPreheaderFallsBackToTheFirstParagraph keeps the preview from being
// the heading repeated.
func TestAnUnsetPreheaderFallsBackToTheFirstParagraph(t *testing.T) {
	for name, expected := range map[string]struct {
		content Content
		want    string
	}{
		"an explicit preheader": {
			content: Content{Preheader: "Explicit.", Body: []string{"First."}, Heading: "Heading"},
			want:    "Explicit.",
		},
		"the first paragraph": {
			content: Content{Body: []string{"First.", "Second."}, Heading: "Heading"},
			want:    "First.",
		},
		"a blank first paragraph is skipped": {
			content: Content{Body: []string{"  ", "Second."}, Heading: "Heading"},
			want:    "Second.",
		},
		"a message with no body": {
			content: Content{Subheading: "31 August – 6 September", Heading: "harbor.my"},
			want:    "31 August – 6 September",
		},
		"nothing but a heading": {
			content: Content{Heading: "harbor.my"},
			want:    "harbor.my",
		},
	} {
		if got := expected.content.PreheaderText(); got != expected.want {
			t.Errorf("%s: preheader = %q, want %q", name, got, expected.want)
		}
	}
}

// TestTheLayoutHandlesDarkModeItself keeps Apple Mail and Outlook from
// inverting the light palette on their own, which turns the card and the page
// into two near-identical greys.
func TestTheLayoutHandlesDarkModeItself(t *testing.T) {
	html, err := plainMessage().HTML()
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{
		"the color-scheme meta":     `<meta name="color-scheme" content="light dark">`,
		"the supported meta":        `<meta name="supported-color-schemes" content="light dark">`,
		"a dark block":              "@media (prefers-color-scheme: dark)",
		"the dark page":             string(Dark.Page),
		"the dark card":             string(Dark.Card),
		"the dark ink":              string(Dark.Ink),
		"a phone breakpoint":        "@media only screen and (max-width: 480px)",
		"buttons that stack":        ".button { display: block !important;",
		"padding that steps down":   ".pad    { padding-left: 20px !important;",
		"the light palette as well": string(Colours.Page),
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the layout is missing %s (%q)", name, want)
		}
	}
}

// TestEveryDarkColourIsReachable keeps a palette entry that no class applies
// from looking like dark-mode support it is not.
func TestEveryDarkColourIsReachable(t *testing.T) {
	content := plainMessage()
	content.Kicker = "Weekly report"
	content.Subheading = "31 August – 6 September 2026"
	content.Note = "No traffic was recorded in this period."
	content.Figures = []Figure{
		{Label: "Up", Value: "1", Change: "+1%", Direction: "up"},
		{Label: "Down", Value: "2", Change: "−1%", Direction: "down"},
		{Label: "Flat", Value: "3", Change: "no change", Direction: "flat"},
	}
	content.Tables = []Table{{Title: "Top pages", Rows: []Row{{Label: "/", Value: "1"}}}}

	html, err := content.HTML()
	if err != nil {
		t.Fatal(err)
	}

	alarm := content
	alarm.KickerTone = ToneAlarm

	alarmHTML, err := alarm.HTML()
	if err != nil {
		t.Fatal(err)
	}

	for class := range map[string]bool{
		"page": true, "card": true, "ink": true, "text": true, "muted": true,
		"accent": true, "rule": true, "facts": true, "note": true, "outline": true, "fill": true,
		"up": true, "down": true, "flat": true,
	} {
		// The class list, not a substring of the copy: `class="ink rule"` is a
		// match and the word "rule" in a sentence is not.
		if !carries(html, class) {
			t.Errorf("no element carries the %q class, so its dark colour never applies", class)
		}
	}

	if !carries(alarmHTML, "alarm") {
		t.Error("an alarm kicker carries no alarm class, so its dark colour never applies")
	}
}

// carries reports whether any element's class list holds one class.
func carries(html, class string) bool {
	for _, attribute := range regexp.MustCompile(`class="([^"]*)"`).FindAllStringSubmatch(html, -1) {
		for _, name := range strings.Fields(attribute[1]) {
			if name == class {
				return true
			}
		}
	}

	return false
}

// readable is WCAG AA for text below 18.66px bold, which is every size in the
// layout. Nothing here is decoration: the smallest text is the postal address,
// and it is the piece with a legal reason to be legible.
const readable = 4.5

// TestEveryColourIsReadableOnItsGround measures instead of assuming.
//
// The dark palette is the reason this exists — a green that reads on a
// near-white card disappears on a near-black one — but the light palette is
// measured on the same rules, because nothing had ever checked it either.
func TestEveryColourIsReadableOnItsGround(t *testing.T) {
	for name, palette := range map[string]Palette{"light": Colours, "dark": Dark} {
		for what, pair := range map[string][2]template.CSS{
			"the heading on the card":     {palette.Ink, palette.Card},
			"body copy on the card":       {palette.Text, palette.Card},
			"a label on the card":         {palette.Muted, palette.Card},
			"the kicker on the card":      {palette.AccentText, palette.Card},
			"an alarm kicker on the card": {palette.Alarm, palette.Card},
			"a rising figure":             {palette.Up, palette.Card},
			"a falling figure":            {palette.Down, palette.Card},
			"an unchanged figure":         {palette.Flat, palette.Card},
			"a fact label":                {palette.Muted, palette.FactsBackground},
			"a fact value":                {palette.Ink, palette.FactsBackground},
			"the note":                    {palette.NoteText, palette.NoteBackground},
			"an outlined button":          {palette.Ink, palette.Card},
		} {
			if ratio := contrast(t, pair[0], pair[1]); ratio < readable {
				t.Errorf("%s: %s is %.2f:1 (%s on %s), want at least %.1f",
					name, what, ratio, pair[0], pair[1], readable)
			}
		}
	}
}

// TestTheDarkBlockOverridesEveryPairItIsMeasuredOn keeps the contrast test
// honest: a pair measured in the palette but never applied together is a number
// about nothing.
func TestTheDarkBlockOverridesEveryPairItIsMeasuredOn(t *testing.T) {
	html, err := richMessage().HTML()
	if err != nil {
		t.Fatal(err)
	}

	dark := html[strings.Index(html, "@media (prefers-color-scheme: dark)"):]
	dark = dark[:strings.Index(dark, "@media only screen")]

	for what, want := range map[string]string{
		"the page":            "background: " + string(Dark.Page),
		"the card":            "background: " + string(Dark.Card),
		"the card's edge":     "border-color: " + string(Dark.Border),
		"a section rule":      "border-color: " + string(Dark.Rule),
		"the button's ground": "background: " + string(Dark.Accent),
		"the button's text":   "color: " + string(Dark.OnAccent),
		"an outlined button":  "border-color: " + string(Dark.Ink),
		"the facts block":     "background: " + string(Dark.FactsBackground),
		"the note":            "background: " + string(Dark.NoteBackground),
	} {
		if !strings.Contains(dark, want) {
			t.Errorf("the dark block never sets %s (%q)", what, want)
		}
	}
}

// TestTheAccentButtonMatchesTheProduct pins the two numbers rather than hiding
// them.
//
// Paper on the light flag is 3.76:1, which ui/tokens.css says the marketing
// site accepts for the words on a button. The dark theme flips to near-black on
// a lightened flag and reaches 5.9:1. Both are the product's decisions, made
// once; what this test does is stop either getting worse.
func TestTheAccentButtonMatchesTheProduct(t *testing.T) {
	for name, want := range map[string]struct {
		palette Palette
		floor   float64
	}{
		"light": {Colours, 3.7},
		"dark":  {Dark, 5.85},
	} {
		if ratio := contrast(t, want.palette.OnAccent, want.palette.Accent); ratio < want.floor {
			t.Errorf("%s: the accent button is %.2f:1, below the %.1f it was", name, ratio, want.floor)
		}
	}
}

// TestTheLinesAreVisible covers the colours that are edges rather than text: a
// text ratio does not apply to them, and a dark-mode override that lost one
// would take the layout with it.
func TestTheLinesAreVisible(t *testing.T) {
	// The card and the page are near neighbours on purpose; the 2px border
	// draws the edge, so that is what has to be seen against both.
	const edge = 1.5

	// The lines inside the card are quieter by design — a full-strength rule
	// chops a card into pieces — but they still have to be there in both
	// palettes, and to be about the same weight in each.
	const inside = 1.25

	for name, palette := range map[string]Palette{"light": Colours, "dark": Dark} {
		if ratio := contrast(t, palette.Border, palette.Card); ratio < edge {
			t.Errorf("%s: the border is %.2f:1 against the card, want at least %.2f", name, ratio, edge)
		}

		if ratio := contrast(t, palette.Border, palette.Page); ratio < edge {
			t.Errorf("%s: the border is %.2f:1 against the page, want at least %.2f", name, ratio, edge)
		}

		if ratio := contrast(t, palette.Rule, palette.Card); ratio < inside {
			t.Errorf("%s: a section rule is %.2f:1 against the card, want at least %.2f", name, ratio, inside)
		}
	}

	light := contrast(t, Colours.Rule, Colours.Card)
	dark := contrast(t, Dark.Rule, Dark.Card)

	if math.Abs(light-dark) > 0.35 {
		t.Errorf("the section rules are %.2f:1 light and %.2f:1 dark, which is two different designs", light, dark)
	}
}

// contrast is the WCAG ratio between two colours, both written as #rrggbb.
func contrast(t *testing.T, a, b template.CSS) float64 {
	t.Helper()

	high, low := luminance(t, a), luminance(t, b)
	if high < low {
		high, low = low, high
	}

	return (high + 0.05) / (low + 0.05)
}

// luminance is the WCAG relative luminance of one colour.
func luminance(t *testing.T, colour template.CSS) float64 {
	t.Helper()

	text := string(colour)
	if len(text) != 7 || text[0] != '#' {
		t.Fatalf("%q is not a #rrggbb colour", colour)
	}

	weights := [3]float64{0.2126, 0.7152, 0.0722}
	total := 0.0

	for i := range weights {
		value, err := strconv.ParseUint(text[1+i*2:3+i*2], 16, 8)
		if err != nil {
			t.Fatalf("%q is not a #rrggbb colour: %v", colour, err)
		}

		channel := float64(value) / 255

		if channel <= 0.03928 {
			channel /= 12.92
		} else {
			channel = math.Pow((channel+0.055)/1.055, 2.4)
		}

		total += weights[i] * channel
	}

	return total
}

// TestThePaletteIsTheProductsPalette is the guard against a second design.
//
// An email inlines every colour, so the values are copied out of ui/tokens.css
// rather than read from it. This reads that file and refuses a copy that has
// drifted — which is otherwise invisible, because the email and the app are
// never looked at side by side.
func TestThePaletteIsTheProductsPalette(t *testing.T) {
	light, dark := productTokens(t)

	for name, want := range map[string]struct {
		tokens  map[string]string
		palette Palette
	}{
		"light": {light, Colours},
		"dark":  {dark, Dark},
	} {
		for token, colour := range map[string]template.CSS{
			"--fs-page":       want.palette.Page,
			"--fs-card":       want.palette.Card,
			"--fs-heading":    want.palette.Ink,
			"--fs-body":       want.palette.Text,
			"--fs-muted":      want.palette.Muted,
			"--fs-accent":     want.palette.Accent,
			"--fs-fill-fg":    want.palette.OnAccent,
			"--fs-accent-ink": want.palette.AccentText,
			"--fs-subtle":     want.palette.FactsBackground,
			"--fs-warn":       want.palette.NoteBorder,
			"--fs-warn-ink":   want.palette.NoteText,
			"--fs-up-ink":     want.palette.Up,
			"--fs-down-ink":   want.palette.Down,
		} {
			if got := want.tokens[token]; got != string(colour) {
				t.Errorf("%s: %s is %q in the layout and %q in ui/tokens.css", name, token, colour, got)
			}
		}

		// A figure that did not move is --fs-muted. There is no "faint" in the
		// layout: the footer carries the legal address at 12px, and --fs-faint
		// is 3.85:1 on paper.
		if got := want.tokens["--fs-muted"]; got != string(want.palette.Flat) {
			t.Errorf("%s: a flat figure is %q, want --fs-muted %q", name, want.palette.Flat, got)
		}

		// An alarm kicker is a falling metric by another name.
		if got := want.tokens["--fs-down-ink"]; got != string(want.palette.Alarm) {
			t.Errorf("%s: the alarm tone is %q, want --fs-down-ink %q", name, want.palette.Alarm, got)
		}
	}
}

// TestTheRulesAreTheProductsRules covers the two colours that are not tokens
// but are derived from one: an email cannot layer a translucent line over a
// card, so the result is flattened here and has to match what the browser
// would have painted.
func TestTheRulesAreTheProductsRules(t *testing.T) {
	light, dark := productTokens(t)

	for name, want := range map[string]struct {
		tokens  map[string]string
		palette Palette
	}{
		"light": {light, Colours},
		"dark":  {dark, Dark},
	} {
		for token, pair := range map[string][2]template.CSS{
			"--fs-line":      {want.palette.Border, want.palette.Card},
			"--fs-line-soft": {want.palette.Rule, want.palette.Card},
		} {
			ink, alpha := translucent(t, want.tokens[token])
			flat := over(t, ink, alpha, pair[1])

			if flat != pair[0] {
				t.Errorf("%s: %s over the card is %s, and the layout inlines %s", name, token, flat, pair[0])
			}
		}
	}
}

// productTokens reads the light and dark custom properties out of ui/tokens.css.
func productTokens(t *testing.T) (light, dark map[string]string) {
	t.Helper()

	source, err := os.ReadFile(filepath.Join("..", "..", "ui", "tokens.css"))
	if err != nil {
		t.Fatal(err)
	}

	blocks := regexp.MustCompile(`(?s)(:root|\.dark)\s*\{(.*?)\n\}`).FindAllStringSubmatch(string(source), -1)
	declarations := regexp.MustCompile(`(--fs-[a-z-]+)\s*:\s*([^;]+);`)

	found := map[string]map[string]string{}

	for _, block := range blocks {
		into, ok := found[block[1]]
		if !ok {
			into = map[string]string{}
			found[block[1]] = into
		}

		for _, declaration := range declarations.FindAllStringSubmatch(block[2], -1) {
			into[declaration[1]] = strings.TrimSpace(declaration[2])
		}
	}

	if len(found[":root"]) == 0 || len(found[".dark"]) == 0 {
		t.Fatalf("ui/tokens.css yielded %d light and %d dark tokens", len(found[":root"]), len(found[".dark"]))
	}

	return found[":root"], found[".dark"]
}

// translucent splits an `rgb(r g b / a)` token into its colour and its alpha.
func translucent(t *testing.T, value string) (colour [3]float64, alpha float64) {
	t.Helper()

	parts := regexp.MustCompile(`rgb\(\s*(\d+)\s+(\d+)\s+(\d+)\s*/\s*([\d.]+)\s*\)`).FindStringSubmatch(value)
	if parts == nil {
		t.Fatalf("%q is not an rgb() with an alpha", value)
	}

	for i := range colour {
		channel, err := strconv.ParseFloat(parts[1+i], 64)
		if err != nil {
			t.Fatal(err)
		}

		colour[i] = channel
	}

	alpha, err := strconv.ParseFloat(parts[4], 64)
	if err != nil {
		t.Fatal(err)
	}

	return colour, alpha
}

// over flattens a translucent colour onto an opaque one, the way a browser
// would, and writes the result as #rrggbb.
func over(t *testing.T, colour [3]float64, alpha float64, ground template.CSS) template.CSS {
	t.Helper()

	text := string(ground)
	if len(text) != 7 || text[0] != '#' {
		t.Fatalf("%q is not a #rrggbb colour", ground)
	}

	out := "#"

	for i := range colour {
		below, err := strconv.ParseUint(text[1+i*2:3+i*2], 16, 8)
		if err != nil {
			t.Fatal(err)
		}

		blended := int(math.Round(alpha*colour[i] + (1-alpha)*float64(below)))
		out += strconv.FormatInt(int64(blended), 16)

		if blended < 16 {
			out = out[:len(out)-1] + "0" + out[len(out)-1:]
		}
	}

	return template.CSS(out)
}

// richMessage uses every block the layout has, so the golden below covers the
// markup the plain one does not reach.
func richMessage() Content {
	return Content{
		Subject:    "harbor.my — Weekly report for 31 August – 6 September 2026",
		Preheader:  "Visitors 4,182, pageviews 11,904",
		Kicker:     "Weekly report",
		Heading:    "harbor.my",
		Subheading: "31 August – 6 September 2026",
		Note:       "One day is missing from this period while we were not collecting.",
		Body:       []string{"A paragraph, so the block is covered too."},
		Link:       "https://app.feasible.lol/harbor.my",
		Code:       "483920",
		Figures: []Figure{
			{Label: "Visitors", Value: "4,182", Change: "+18%", Direction: "up"},
			{Label: "Pageviews", Value: "11,904", Change: "−4%", Direction: "down"},
			{Label: "Bounce rate", Value: "41%", Change: "no change", Direction: "flat"},
			{Label: "Visit duration", Value: "2m 14s"},
			{Label: "Visits", Value: "6,004", Change: "+2%", Direction: "up"},
		},
		Tables: []Table{
			{Title: "Top pages", Rows: []Row{{Label: "/", Value: "3,914"}}},
			{Title: "Top sources", Empty: "No referrers were recorded in this period."},
		},
		Facts:     []Fact{{Label: "Generated", Value: "Sun, 6 September 2026"}},
		Primary:   Button{Label: "Open the dashboard", URL: "https://app.feasible.lol/harbor.my"},
		Secondary: []Button{{Label: "Change this report", URL: "https://app.feasible.lol/settings/reports"}},
		Closing:   "You are receiving this because somebody added your address to this site's report.",
	}
}

// TestARichMessageMatchesItsGolden covers the kicker, subheading, note, code,
// link, wrapped metric row, both table states and the alarm tone, none of which
// the plain golden reaches.
func TestARichMessageMatchesItsGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "rich_message.html"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := richMessage().HTML()
	if err != nil {
		t.Fatal(err)
	}

	if got != string(want) {
		t.Errorf("the layout changed. If that was the point, regenerate testdata/rich_message.html."+
			"\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}
