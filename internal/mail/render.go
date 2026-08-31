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

// Content is a rendered message before it becomes HTML and text. Keeping the
// copy as data rather than as a template per email is what makes it possible to
// assert, in one test over every message, that each one names a real date and
// carries an upgrade link.
type Content struct {
	Subject   string
	Heading   string
	Body      []string
	Facts     []Fact
	Primary   Button
	Secondary []Button
	Closing   string
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

	b.WriteString(c.Heading)
	b.WriteString("\n\n")

	for _, paragraph := range c.Body {
		b.WriteString(paragraph)
		b.WriteString("\n\n")
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
