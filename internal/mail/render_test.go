//
// render_test.go
// What the shared layout puts on the page, and what it leaves off.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"os"
	"path/filepath"
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
	// The literals, not the constants. Comparing a constant against itself
	// passes just as happily with growth coloured red.
	for direction, want := range map[string]string{
		"up":   "#146c33",
		"down": "#b91c1c",
		"flat": "#616e7c",
		"":     "#616e7c",
	} {
		if got := string((Figure{Direction: direction}).Colour()); got != want {
			t.Errorf("direction %q coloured %s, want %s", direction, got, want)
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

// TestAMessageUsingOnlyTheOldBlocksIsUnchanged is the proof the other messages
// did not move when the report and the alert arrived.
//
// The golden file was rendered before any of the new blocks existed. A block
// that leaks whitespace or markup into a message that does not use it changes
// this file, and changing it has to be a deliberate act.
func TestAMessageUsingOnlyTheOldBlocksIsUnchanged(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "plain_message.html"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := plainMessage().HTML()
	if err != nil {
		t.Fatal(err)
	}

	if got != string(want) {
		t.Errorf("the layout changed for a message using none of the new blocks. If the office moved, "+
			"regenerate testdata/plain_message.html.\n--- want ---\n%s\n--- got ---\n%s", want, got)
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
		"page": true, "card": true, "ink": true, "text": true, "muted": true, "faint": true,
		"accent": true, "rule": true, "facts": true, "note": true, "outline": true,
		"up": true, "down": true, "flat": true,
	} {
		if !strings.Contains(html, `class="`+class+`"`) && !strings.Contains(html, class+`"`) {
			t.Errorf("no element carries the %q class, so its dark colour never applies", class)
		}
	}

	if !strings.Contains(alarmHTML, `class="alarm"`) {
		t.Error("an alarm kicker carries no alarm class, so its dark colour never applies")
	}
}
