//
// render.go
// One layout, one plain-text twin, and the dates every message has to name.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"strings"
	"time"
)

// layoutHTML is the one shell every message renders into. There is a single
// layout rather than a file per email because the footer carries the postal
// address CAN-SPAM requires, and a per-message layout is a per-message chance
// to leave it out.
//
//go:embed layout.html
var layoutHTML string

// layout is parsed once. A broken template is a programmer error caught by the
// first test run, so failing here is honest: the binary cannot send mail and
// should not pretend it can.
var layout = template.Must(template.New("layout").Parse(layoutHTML))

// DateFormat is how every date in every email is written. A long form with the
// weekday is used deliberately: "Tue, 29 September 2026" cannot be misread as
// a US or European ordering, and these messages announce the day somebody's
// data is destroyed.
const DateFormat = "Mon, 2 January 2006"

// The colours the layout uses for a value that moved and for the two kicker
// tones. They are constants rather than hex literals in the markup because an
// email cannot use a CSS variable — every colour is inlined on the element —
// so the only way to have one definition is to put it here.
const (
	colourAccent = "#ae1800"
	colourAlarm  = "#b91c1c"
	colourUp     = "#15803d"
	colourDown   = "#b91c1c"
	colourFlat   = "#616e7c"
)

// ToneAlarm is the kicker tone for a message that reports something wrong. The
// default tone is the brand accent.
const ToneAlarm = "alarm"

// Button is a link rendered as a call to action.
type Button struct {
	Label string
	URL   string
}

// Fact is one labelled date in the summary block. The block exists so that the
// three dates that matter are visible without reading the prose, which is what
// somebody skimming an email on a phone actually does.
type Fact struct {
	Label string
	Value string
}

// Figure is one number with an optional comparison against a previous period.
type Figure struct {
	Label string
	Value string

	// Change is pre-rendered as "+18%" or "−4%", and empty when there is
	// nothing to compare against. Empty rather than "0%" because "no previous
	// period" and "no change" are different facts a reader cannot tell apart
	// from a zero.
	Change string

	// Direction is "up", "down" or "flat", so the colour is chosen from a word
	// rather than by parsing the string above.
	Direction string
}

// Colour is the change colour for a direction.
func (f Figure) Colour() string {
	switch f.Direction {
	case "up":
		return colourUp
	case "down":
		return colourDown
	default:
		return colourFlat
	}
}

// Row is one line of a table: a label and a number that lines up with the
// numbers above and below it.
type Row struct {
	Label string
	Value string
}

// Table is a titled top-N list. Empty is what is shown in place of the rows
// when there are none, because a section that vanishes reads as a bug and a
// section that says why it is empty reads as an answer.
type Table struct {
	Title string
	Rows  []Row
	Empty string
}

// Content is a rendered message before it becomes HTML and text. Keeping the
// copy as data rather than as a template per email is what makes it possible to
// assert, in one test over every message, that each one names a real date and
// carries an upgrade link.
type Content struct {
	Subject string

	// Kicker is the small uppercase line above the heading, and KickerTone
	// picks its colour: ToneAlarm for something wrong, otherwise the accent.
	Kicker     string
	KickerTone string

	Heading string

	// Subheading is the quiet line under the heading — the period a report
	// covers, for instance, which is neither the title nor body copy.
	Subheading string

	Body []string

	// Note is a boxed sentence about the whole message, used when something
	// about it needs saying before the numbers rather than after them.
	Note string

	// Link is a URL shown as its own text, and clickable. A button hides where
	// it goes and there is no hover on a phone, so the messages that hand over
	// account control show their destination.
	Link string

	// Code is a short credential the reader retypes. It is its own field rather
	// than a Body entry or a Fact because it has to be the largest thing on the
	// screen, and a facts row is a small right-aligned value.
	Code string

	Figures   []Figure
	Tables    []Table
	Facts     []Fact
	Primary   Button
	Secondary []Button
	Closing   string
}

// KickerColour is the colour of the kicker line.
func (c Content) KickerColour() string {
	if c.KickerTone == ToneAlarm {
		return colourAlarm
	}

	return colourAccent
}

// HTML renders the content through the shared layout.
func (c Content) HTML() (string, error) {
	var buf bytes.Buffer

	if err := layout.Execute(&buf, c); err != nil {
		return "", fmt.Errorf("mail: render %q: %w", c.Subject, err)
	}

	return buf.String(), nil
}

// Text renders the plain-text twin. It is generated from the same Content
// rather than written separately, because two hand-maintained copies of a
// deletion date is two chances for them to disagree.
func (c Content) Text() string {
	var b strings.Builder

	if c.Kicker != "" {
		b.WriteString(strings.ToUpper(c.Kicker))
		b.WriteString("\n")
	}

	b.WriteString(c.Heading)
	b.WriteString("\n")

	if c.Subheading != "" {
		b.WriteString(c.Subheading)
		b.WriteString("\n")
	}

	b.WriteString("\n")

	if c.Note != "" {
		b.WriteString(c.Note)
		b.WriteString("\n\n")
	}

	for _, paragraph := range c.Body {
		b.WriteString(paragraph)
		b.WriteString("\n\n")
	}

	// The link is skipped when a button already carries it, so the text part
	// does not print the same URL twice.
	if c.Link != "" && c.Link != c.Primary.URL {
		b.WriteString(c.Link)
		b.WriteString("\n\n")
	}

	if c.Code != "" {
		b.WriteString(c.Code)
		b.WriteString("\n\n")
	}

	for _, figure := range c.Figures {
		b.WriteString(figure.Label)
		b.WriteString(": ")
		b.WriteString(figure.Value)

		if figure.Change != "" {
			b.WriteString(" (")
			b.WriteString(figure.Change)
			b.WriteString(")")
		}

		b.WriteString("\n")
	}

	if len(c.Figures) > 0 {
		b.WriteString("\n")
	}

	for _, table := range c.Tables {
		b.WriteString(table.Title)
		b.WriteString("\n")

		for _, row := range table.Rows {
			b.WriteString("  ")
			b.WriteString(row.Label)
			b.WriteString("  ")
			b.WriteString(row.Value)
			b.WriteString("\n")
		}

		if len(table.Rows) == 0 {
			b.WriteString("  ")
			b.WriteString(table.Empty)
			b.WriteString("\n")
		}

		b.WriteString("\n")
	}

	for _, fact := range c.Facts {
		b.WriteString(fact.Label)
		b.WriteString(": ")
		b.WriteString(fact.Value)
		b.WriteString("\n")
	}

	if len(c.Facts) > 0 {
		b.WriteString("\n")
	}

	if c.Primary.URL != "" {
		b.WriteString(c.Primary.Label)
		b.WriteString(": ")
		b.WriteString(c.Primary.URL)
		b.WriteString("\n")
	}

	for _, button := range c.Secondary {
		b.WriteString(button.Label)
		b.WriteString(": ")
		b.WriteString(button.URL)
		b.WriteString("\n")
	}

	if c.Closing != "" {
		b.WriteString("\n")
		b.WriteString(c.Closing)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(PostalAddress)
	b.WriteString("\n")

	return b.String()
}

// Message turns content into a sendable message for one recipient.
func (c Content) Message(to, tag string) (Message, error) {
	html, err := c.HTML()
	if err != nil {
		return Message{}, err
	}

	return Message{To: to, Subject: c.Subject, HTML: html, Text: c.Text(), Tag: tag}, nil
}

// day formats a date for a customer. A zero time renders as an em dash rather
// than as "Mon, 1 January 1", which is what an unset date would otherwise print
// into an email announcing a deletion.
func day(at time.Time) string {
	if at.IsZero() {
		return "—"
	}

	return at.UTC().Format(DateFormat)
}
