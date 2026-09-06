//
// render_test.go
// What the shared layout puts on the page, and what it leaves off.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
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

// TestAContentWithNoNewBlocksRendersNoneOfTheirMarkup is what proves the
// nineteen messages that carry no kicker, figures or tables did not move when
// the report and the alert arrived.
func TestAContentWithNoNewBlocksRendersNoneOfTheirMarkup(t *testing.T) {
	content := Content{
		Subject: "A plain message",
		Heading: "A plain message",
		Body:    []string{"One paragraph."},
		Primary: Button{Label: "Do the thing", URL: "https://example.com"},
		Closing: "That is all.",
	}

	html, err := content.HTML()
	if err != nil {
		t.Fatal(err)
	}

	for name, fragment := range map[string]string{
		"a kicker":        "text-transform:uppercase",
		"a note panel":    "#f7f0dd",
		"a figures row":   "padding:12px 8px 12px 0; vertical-align:top;",
		"a table":         "tabular-nums",
		"a change colour": colourUp,
	} {
		if strings.Contains(html, fragment) {
			t.Errorf("a plain message rendered %s:\n%s", name, html)
		}
	}

	if strings.Contains(content.Text(), "\n\n\n") {
		t.Errorf("a plain message rendered a blank gap where a new block would be:\n%q", content.Text())
	}
}

// TestAFigureColoursItselfFromItsDirection keeps the three change colours in
// one place, since they are inlined on the element and cannot be a variable.
func TestAFigureColoursItselfFromItsDirection(t *testing.T) {
	for direction, want := range map[string]string{
		"up":   colourUp,
		"down": colourDown,
		"flat": colourFlat,
		"":     colourFlat,
	} {
		if got := (Figure{Direction: direction}).Colour(); got != want {
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
