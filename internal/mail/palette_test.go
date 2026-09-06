//
// palette_test.go
// That every colour in the layout can be read on the ground it sits on.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"html/template"
	"math"
	"strconv"
	"testing"
)

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
			"the footer on the card":      {palette.Faint, palette.Card},
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

// TestTheAccentButtonIsAsGoodAsTheBrandAllows pins a number rather than hiding
// it.
//
// White on #ec3013 measures 4.20:1, short of the 4.5 AA asks for at this size.
// #ec3013 is the product's accent, shared with the dashboard, so a darker red
// here would make the email disagree with the app it links to. The number is
// pinned so it cannot quietly get worse, and moving it is a brand decision.
func TestTheAccentButtonIsAsGoodAsTheBrandAllows(t *testing.T) {
	const measured = 4.2

	for name, palette := range map[string]Palette{"light": Colours, "dark": Dark} {
		if ratio := contrast(t, palette.OnAccent, palette.Accent); ratio < measured {
			t.Errorf("%s: the accent button is %.2f:1, below the %.1f it was", name, ratio, measured)
		}
	}
}

// TestTheCardStandsOffThePage keeps the two grounds distinguishable, which is
// the failure a client's own dark-mode inversion produces.
func TestTheCardStandsOffThePage(t *testing.T) {
	// The card and the page are near neighbours on purpose; it is the 2px
	// border that draws the edge, so that is what has to be visible against
	// both of them.
	const separated = 1.5

	for name, palette := range map[string]Palette{"light": Colours, "dark": Dark} {
		if ratio := contrast(t, palette.Border, palette.Card); ratio < separated {
			t.Errorf("%s: the border is %.2f:1 against the card, want at least %.2f", name, ratio, separated)
		}

		if ratio := contrast(t, palette.Border, palette.Page); ratio < separated {
			t.Errorf("%s: the border is %.2f:1 against the page, want at least %.2f", name, ratio, separated)
		}
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
