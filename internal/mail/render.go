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
	"strconv"
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

// Palette is every colour the layout paints with.
//
// An email cannot use a CSS variable — every colour is inlined on the element
// it applies to — so the values are resolved here instead. Every one comes from
// ui/tokens.css, and a test reads that file and refuses a value that has
// drifted from it.
type Palette struct {
	Page   template.CSS // --fs-page, behind the card
	Card   template.CSS // --fs-card
	Border template.CSS // --fs-line over the card: the card's edge
	Rule   template.CSS // --fs-line-soft over the card: the lines inside it
	Ink    template.CSS // --fs-heading
	Text   template.CSS // --fs-body
	Muted  template.CSS // --fs-muted: labels, the footer, a figure that did not move

	// Accent is the primary button's ground and OnAccent its text. AccentText
	// is the accent used as text, which is several shades off the fill.
	Accent     template.CSS // --fs-accent
	OnAccent   template.CSS // --fs-fill-fg
	AccentText template.CSS // --fs-accent-ink

	FactsBackground template.CSS // --fs-subtle

	NoteText       template.CSS // --fs-warn-ink
	NoteBackground template.CSS
	NoteBorder     template.CSS // --fs-warn

	// A figure that moved, and the tone of a kicker on a message about
	// something wrong. All three are the ink variants: these are text, and the
	// fill variants are a shade too light to read at 12px.
	Up    template.CSS // --fs-up-ink
	Down  template.CSS // --fs-down-ink
	Flat  template.CSS // --fs-muted
	Alarm template.CSS // --fs-down-ink
}

// Colours is the light palette.
var Colours = Palette{
	Page:            "#eae9e9",
	Card:            "#f3f2f2",
	Border:          "#9f9d9d",
	Rule:            "#d1d0d0",
	Ink:             "#201e1d",
	Text:            "#444141",
	Muted:           "#605d5d",
	Accent:          "#ec3013",
	OnAccent:        "#f3f2f2",
	AccentText:      "#ae1800",
	FactsBackground: "#eae7e7",
	NoteText:        "#854d0e",
	NoteBackground:  "#f7f0dd",
	NoteBorder:      "#a16207",
	Up:              "#166534",
	Down:            "#991b1b",
	Flat:            "#605d5d",
	Alarm:           "#991b1b",
}

// Dark is the palette a client in dark mode is given instead.
//
// It is here rather than left to the client: Apple Mail and Outlook invert a
// light palette themselves, and a near-white card on a near-white page becomes
// two identical greys with only the border holding the layout together.
var Dark = Palette{
	Page:            "#161514",
	Card:            "#201e1d",
	Border:          "#5f5e5d",
	Rule:            "#3e3c3b",
	Ink:             "#f3f2f2",
	Text:            "#eae7e7",
	Muted:           "#bab6b6",
	Accent:          "#ff5a3c",
	OnAccent:        "#161514",
	AccentText:      "#ff8f77",
	FactsBackground: "#2b2827",
	NoteText:        "#fcd34d",
	NoteBackground:  "#2b2827",
	NoteBorder:      "#fbbf24",
	Up:              "#86efac",
	Down:            "#fca5a5",
	Flat:            "#bab6b6",
	Alarm:           "#fca5a5",
}

// Tone picks the kicker's colour. It is a type rather than a string so a
// misspelling is a compile error instead of a kicker that quietly renders in
// the wrong colour.
type Tone string

// ToneAlarm is for a message that reports something wrong. The zero tone is the
// brand accent.
const ToneAlarm Tone = "alarm"

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

	// Change is pre-rendered as "+18%" or "−4%". It is empty when there is
	// nothing to compare against, which is a different fact from "no change"
	// and one a reader cannot tell apart from a zero.
	Change string

	// Direction is "up", "down" or "flat", and picks the colour.
	Direction string
}

// changes maps a direction onto the class the dark block overrides and the
// colour inlined for every other client. One table, because a class and a
// colour that disagree render a falling metric green at night and nothing about
// the light rendering says so.
var changes = map[string]struct {
	Class string
	Light template.CSS
	Dark  template.CSS
}{
	"up":   {"up", Colours.Up, Dark.Up},
	"down": {"down", Colours.Down, Dark.Down},
	"":     {"flat", Colours.Flat, Dark.Flat},
}

// change is the entry for a direction, defaulting to flat.
func change(direction string) (class string, light, dark template.CSS) {
	entry, ok := changes[direction]
	if !ok {
		entry = changes[""]
	}

	return entry.Class, entry.Light, entry.Dark
}

// Class is the dark-mode hook for the change colour, since a media query can
// only reach a class and the light colour is inlined.
func (f Figure) Class() string {
	class, _, _ := change(f.Direction)

	return class
}

// Colour is the change colour for a direction.
func (f Figure) Colour() template.CSS {
	_, light, _ := change(f.Direction)

	return light
}

// figuresPerRow is how many figures share a line before the row wraps.
//
// A 560px card leaves about 124px a column at four. A fifth wraps the labels
// onto two lines and leaves the values on different baselines, which is what
// the metric row exists not to do.
const figuresPerRow = 4

// FigureRow is one line of the metric block, already padded to a full row so
// the columns above and below it line up.
type FigureRow struct {
	Cells []Figure

	// Width is the column width, as a percentage, since an email table has no
	// grid to divide itself by.
	Width template.CSS
}

// FigureRows splits the figures into lines of at most figuresPerRow, as evenly
// as they divide. Five become three and two rather than four and one.
func (c Content) FigureRows() []FigureRow {
	if len(c.Figures) == 0 {
		return nil
	}

	lines := (len(c.Figures) + figuresPerRow - 1) / figuresPerRow
	perLine := (len(c.Figures) + lines - 1) / lines
	width := template.CSS(strconv.Itoa(100/perLine) + "%")

	rows := make([]FigureRow, 0, lines)

	for start := 0; start < len(c.Figures); start += perLine {
		end := min(start+perLine, len(c.Figures))

		cells := make([]Figure, perLine)
		copy(cells, c.Figures[start:end])

		rows = append(rows, FigureRow{Cells: cells, Width: width})
	}

	return rows
}

// Row is one line of a table: a label and a number that lines up with the
// numbers above and below it.
type Row struct {
	Label string
	Value string
}

// Table is a titled top-N list. Empty is shown in place of the rows when there
// are none, so a quiet period says so rather than losing the section.
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
	KickerTone Tone

	Heading string

	// Preheader is the line a phone shows beside the subject in the inbox
	// list. Left empty it falls back to the first body paragraph, which is
	// right for most messages and wrong for the ones whose opening sentence is
	// not the summary.
	Preheader string

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

	Figures []Figure
	Tables  []Table
	// Unsubscribe is where this recipient stops receiving this message. Only
	// the report and the alert set it; see Message.Unsubscribe for why.
	Unsubscribe string

	Facts     []Fact
	Primary   Button
	Secondary []Button
	Closing   string
}

// Company hands the layout the sender's name and address, since a template can
// only reach what its data holds.
func (c Content) Company() Business {
	return Company
}

// PreheaderText is what the inbox list shows beside the subject.
//
// The fallback matters more than the field: a message with nothing here shows
// whatever the client scrapes first, which for a message that opens with a
// heading is the heading repeated.
func (c Content) PreheaderText() string {
	if strings.TrimSpace(c.Preheader) != "" {
		return c.Preheader
	}

	for _, paragraph := range c.Body {
		if strings.TrimSpace(paragraph) != "" {
			return paragraph
		}
	}

	if c.Subheading != "" {
		return c.Subheading
	}

	return c.Heading
}

// DarkColours hands the layout the dark palette for its media query.
func (c Content) DarkColours() Palette {
	return Dark
}

// Colours hands the layout the palette, since a template can only reach what
// its data holds.
func (c Content) Colours() Palette {
	return Colours
}

// kicker is the class and colour for a tone. Paired for the same reason the
// change table is: two switches over one input drift apart silently.
var kickers = map[Tone]struct {
	Class string
	Light template.CSS
	Dark  template.CSS
}{
	ToneAlarm: {"alarm", Colours.Alarm, Dark.Alarm},
	"":        {"accent", Colours.AccentText, Dark.AccentText},
}

// tone is the entry for a kicker tone, defaulting to the accent.
func tone(t Tone) (class string, light, dark template.CSS) {
	entry, ok := kickers[t]
	if !ok {
		entry = kickers[""]
	}

	return entry.Class, entry.Light, entry.Dark
}

// KickerClass is the dark-mode hook for the kicker's tone.
func (c Content) KickerClass() string {
	class, _, _ := tone(c.KickerTone)

	return class
}

// KickerColour is the colour of the kicker line.
func (c Content) KickerColour() template.CSS {
	_, light, _ := tone(c.KickerTone)

	return light
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

	if c.Unsubscribe != "" {
		b.WriteString("\nStop receiving this email: ")
		b.WriteString(c.Unsubscribe)
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(PostalAddress())
	b.WriteString("\n")

	return b.String()
}

// Message turns content into a sendable message for one recipient.
func (c Content) Message(to, tag string) (Message, error) {
	html, err := c.HTML()
	if err != nil {
		return Message{}, err
	}

	return Message{
		To:          to,
		Subject:     c.Subject,
		HTML:        html,
		Text:        c.Text(),
		Tag:         tag,
		Unsubscribe: c.Unsubscribe,
	}, nil
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
