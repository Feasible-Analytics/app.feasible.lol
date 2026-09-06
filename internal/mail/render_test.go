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
		"up":   "#15803d",
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
		t.Errorf("the layout changed for a message using none of the new blocks:\n--- want ---\n%s\n--- got ---\n%s",
			want, got)
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
	if got := strings.Count(html, "font-size:12px; color:"); got != 5 {
		t.Errorf("%d figure labels rendered, want 5:\n%s", got, html)
	}
}
