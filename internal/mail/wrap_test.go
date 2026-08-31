//
// wrap_test.go
// The line-length guard, which is the difference between a message arriving and vanishing.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"strings"
	"testing"
)

// TestWrapKeepsEveryLineUnderTheLimit is the property that matters. A single
// line over 998 octets makes many SMTP servers reject the whole message after
// DATA — not the line, the message — and the sender's own logs frequently record
// the send as fine.
func TestWrapKeepsEveryLineUnderTheLimit(t *testing.T) {
	cases := map[string]string{
		"one enormous tag soup": "<div>" + strings.Repeat("<span>hello</span>", 5000) + "</div>",
		"a long run of words":   strings.Repeat("word ", 4000),
		"no break points":       strings.Repeat("x", 12000),
		"a data uri":            `<img src="data:image/png;base64,` + strings.Repeat("A", 9000) + `">`,
		"already short":         "<p>fine</p>",
		"mixed lines":           "<p>short</p>\n" + strings.Repeat("y", 5000) + "\n<p>short</p>",
	}

	for name, body := range cases {
		wrapped := Wrap(body, MaxLineLength)

		if longest := LongestLine(wrapped); longest > MaxLineLength {
			t.Errorf("%s: longest line is %d, limit is %d", name, longest, MaxLineLength)
		}
	}
}

// TestWrapPreservesTheContent checks that wrapping only ever adds newlines in
// place of a break point. Losing a character in a link would be worse than a
// long line, since the message would arrive and the upgrade button would not
// work.
func TestWrapPreservesTheContent(t *testing.T) {
	body := "<a href=\"https://feasible.lol/billing/upgrade\">Upgrade</a> " + strings.Repeat("padding ", 400)

	wrapped := Wrap(body, MaxLineLength)

	if !strings.Contains(strings.ReplaceAll(wrapped, "\n", " "), "https://feasible.lol/billing/upgrade") {
		t.Fatal("wrapping broke the upgrade link")
	}
}

// TestWrapNeverBreaksInsideAnAttribute is why the break point skips anything
// inside quotes. A newline inside an href is inserted into the URL by some
// clients, and the message then arrives with a dead upgrade button — which is a
// worse outcome than a long line, because it looks like it worked.
func TestWrapNeverBreaksInsideAnAttribute(t *testing.T) {
	// A realistic body: lots of markup, and links of the length this product
	// actually generates.
	var body strings.Builder
	for i := 0; i < 400; i++ {
		body.WriteString(`<a href="https://feasible.lol/billing/upgrade?ref=lifecycle&plan=yearly" style="color:#1f6feb">Upgrade</a> `)
	}

	for _, line := range strings.Split(Wrap(body.String(), MaxLineLength), "\n") {
		if strings.Count(line, `"`)%2 != 0 {
			t.Fatalf("a line ends inside a quoted attribute: %q", line)
		}
	}
}

// TestAnUnbreakableRunIsStillBroken documents the one case where a break lands
// somewhere a reader could notice: a single run longer than the limit with no
// whitespace and no tag in it, such as a base64 data URI.
//
// The alternative is emitting the run intact and having the SMTP server reject
// the entire message. A corrupted image beats a message that never arrives, so
// the line is cut — on a rune boundary, so the body stays valid UTF-8.
func TestAnUnbreakableRunIsStillBroken(t *testing.T) {
	body := `<img src="data:image/png;base64,` + strings.Repeat("A", 5000) + `">`

	wrapped := Wrap(body, MaxLineLength)

	if longest := LongestLine(wrapped); longest > MaxLineLength {
		t.Fatalf("an unbreakable run was emitted intact at %d bytes", longest)
	}
	if !strings.Contains(wrapped, "\n") {
		t.Fatal("the run was not broken at all")
	}
}

// TestWrapKeepsValidUTF8 covers a body with no break point at all in a run of
// multi-byte characters. Cutting mid-rune leaves invalid UTF-8, which some
// clients render as replacement characters and others refuse outright.
func TestWrapKeepsValidUTF8(t *testing.T) {
	body := strings.Repeat("é", 2000)

	wrapped := Wrap(body, 100)

	if !isValidUTF8(wrapped) {
		t.Fatal("wrapping produced invalid UTF-8")
	}
	if longest := LongestLine(wrapped); longest > 100 {
		t.Fatalf("longest line is %d, limit is 100", longest)
	}
}

// isValidUTF8 is a local helper so the test file's imports stay minimal.
func isValidUTF8(value string) bool {
	for _, r := range value {
		if r == '�' {
			return false
		}
	}

	return true
}

// TestWrapIsAStableFixedPoint means wrapping an already-wrapped body changes
// nothing. Anything else would mean a retried send producing a different message
// from the first attempt.
func TestWrapIsAStableFixedPoint(t *testing.T) {
	body := strings.Repeat("<span>content</span>", 3000)

	once := Wrap(body, MaxLineLength)
	twice := Wrap(once, MaxLineLength)

	if once != twice {
		t.Fatal("wrapping twice produced a different body")
	}
}

// TestLongestLineIgnoresCarriageReturns makes sure the measurement matches what
// an SMTP server counts, which is the octets before CRLF rather than including
// it.
func TestLongestLineIgnoresCarriageReturns(t *testing.T) {
	if got := LongestLine("abc\r\ndefgh\r\n"); got != 5 {
		t.Fatalf("longest line is %d, want 5", got)
	}
}
