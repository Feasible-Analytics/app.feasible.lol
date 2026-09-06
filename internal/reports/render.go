//
// render.go
// Report and alert bodies, from templates that refuse to render a missing variable.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/timefmt"
)

// The two markers Go's template package leaves behind when a value did not
// survive rendering.
//
// "<no value>" is what a nil writes in ordinary content. "ZgotmplZ" is what
// html/template substitutes when a value reaches a URL attribute and it cannot
// prove the value is safe — which is exactly what a nil dashboard link produces,
// and which renders as a dead href rather than as visibly missing text.
//
// Both are checked because missingkey cannot catch either: the key is present,
// it just holds nothing. A report that reaches a customer with a dead link
// where the dashboard should be is worse than one that did not arrive.
const (
	missingValue = "<no value>"
	blankedValue = "ZgotmplZ"
)

// ErrUndefinedVariable is what a template referencing something that was never
// assigned produces.
//
// This is a named error rather than a generic template failure because of the
// specific bug it prevents: an incumbent's spike alert referenced a dashboard
// link variable that nothing ever set, their template language rendered it as
// nothing, and the emails shipped for months with a missing link and no error
// anywhere. A template variable that is not assigned has to be a hard failure,
// every time, or that bug is only a matter of when.
var ErrUndefinedVariable = fmt.Errorf("reports: a template referenced a variable that was never assigned")

// Figure is one number on a report, already formatted.
type Figure struct {
	Label string
	Value string

	// Change is the comparison against the previous period, pre-rendered as
	// "+18%" or "−4%", and empty when there is nothing to compare against. It
	// is empty rather than "0%" because "no previous period" and "no change"
	// are different facts and a reader cannot tell them apart from a zero.
	Change string

	// Direction is "up", "down" or "flat", so the template can colour the
	// figure without parsing the string above.
	Direction string
}

// Entry is one row of a top-N list.
type Entry struct {
	Label string
	Value string
}

// Report is everything a scheduled report says. Building it as a struct and
// converting it in one place means there is exactly one list of the names a
// template may use, and a template that names anything else fails.
type Report struct {
	Domain       string
	Kind         string
	PeriodLabel  string
	DashboardURL string
	Figures      []Figure
	TopPages     []Entry
	TopSources   []Entry
	Countries    []Entry

	// Note is an optional sentence above the numbers, used when something about
	// the period is worth saying — a site with no traffic at all, for instance.
	Note string

	GeneratedAt time.Time
}

// Alert is everything a spike or drop alert says.
type Alert struct {
	Domain       string
	Kind         string
	Headline     string
	Detail       string
	Threshold    int
	Observed     int
	DashboardURL string
	TriggeredAt  time.Time
}

// Rendered is a subject and both bodies, ready to hand to a transport.
type Rendered struct {
	Subject string
	HTML    string
	Text    string
}

// Message turns a rendering into one addressed mail message.
//
// It is one recipient rather than a list because that is the shape the shared
// mailer takes, and the shared mailer is where the wrapping and the "the relay
// declined this" check live. A list here would mean a second send path with
// neither.
func (r Rendered) Message(to, tag string) mail.Message {
	return mail.Message{To: to, Subject: r.Subject, HTML: r.HTML, Text: r.Text, Tag: tag}
}

// RenderReport builds the weekly or monthly email.
//
// The HTML is wrapped before it is returned, not only when it is encoded. The
// log transport writes this string straight to disk and a Slack fallback reads
// it, so wrapping at the point of generation means every consumer sees a body
// that already obeys the 998-octet line limit rather than trusting one of them
// to remember.
func RenderReport(report Report, cycle string) (Rendered, error) {
	kind := titleOf(report.Kind)
	generated := timefmt.Clock(cycle, report.GeneratedAt.UTC(), "2 January 2006 15:04 MST")

	content := mail.Content{
		Subject:    fmt.Sprintf("%s — %s report for %s", report.Domain, kind, report.PeriodLabel),
		Kicker:     kind + " report",
		Heading:    report.Domain,
		Subheading: report.PeriodLabel,
		Note:       report.Note,
		Figures:    figuresOf(report.Figures),
		Tables: []mail.Table{
			{Title: "Top pages", Rows: rowsOf(report.TopPages), Empty: "No pages were viewed in this period."},
			{Title: "Top sources", Rows: rowsOf(report.TopSources), Empty: "No referrers were recorded in this period."},
			{Title: "Top countries", Rows: rowsOf(report.Countries), Empty: "No locations were recorded in this period."},
		},
		Primary: mail.Button{Label: "Open the dashboard", URL: report.DashboardURL},
		Closing: fmt.Sprintf("Generated %s. You are receiving this because somebody added your address "+
			"to this site's %s report.", generated, kind),
	}

	return render(content, map[string]string{
		"Domain":       report.Domain,
		"Kind":         kind,
		"PeriodLabel":  report.PeriodLabel,
		"DashboardURL": report.DashboardURL,
		"GeneratedAt":  generated,
	})
}

// RenderAlert builds a spike or drop email.
func RenderAlert(alert Alert, cycle string) (Rendered, error) {
	kind := titleOf(alert.Kind)
	triggered := timefmt.Clock(cycle, alert.TriggeredAt.UTC(), "2 January 2006 15:04 MST")

	content := mail.Content{
		Subject:    fmt.Sprintf("%s — %s", alert.Domain, alert.Headline),
		Kicker:     kind + " alert",
		KickerTone: mail.ToneAlarm,
		Heading:    alert.Domain,
		Body:       []string{alert.Headline, alert.Detail},

		// The same block the report uses for its metrics. An observed count
		// against the threshold that fired is two figures, not a new shape.
		Figures: []mail.Figure{
			{Label: "Observed", Value: strconv.Itoa(alert.Observed)},
			{Label: "Threshold", Value: strconv.Itoa(alert.Threshold)},
		},
		Primary: mail.Button{Label: "Open the dashboard", URL: alert.DashboardURL},
		Closing: fmt.Sprintf("Triggered %s. At most two alerts are sent per site per day, so this "+
			"will not repeat every hour.", triggered),
	}

	return render(content, map[string]string{
		"Domain":       alert.Domain,
		"Kind":         kind,
		"Headline":     alert.Headline,
		"Detail":       alert.Detail,
		"DashboardURL": alert.DashboardURL,
		"TriggeredAt":  triggered,
	})
}

// figuresOf converts this package's figures to the layout's.
//
// The two structs are the same shape and stay separate on purpose: internal/mail
// must not import internal/reports, and a report is built long before anybody
// decides how it is laid out.
func figuresOf(figures []Figure) []mail.Figure {
	converted := make([]mail.Figure, 0, len(figures))

	for _, figure := range figures {
		converted = append(converted, mail.Figure{
			Label:     figure.Label,
			Value:     figure.Value,
			Change:    figure.Change,
			Direction: figure.Direction,
		})
	}

	return converted
}

// rowsOf converts a top-N list to layout rows.
func rowsOf(entries []Entry) []mail.Row {
	rows := make([]mail.Row, 0, len(entries))

	for _, entry := range entries {
		rows = append(rows, mail.Row{Label: entry.Label, Value: entry.Value})
	}

	return rows
}

// render turns content into both bodies with every silent failure turned into a
// loud one.
//
// The required map is the list of values whose absence is invisible in a mail
// client — a dashboard link that renders as a dead href being the one that
// matters. Each is checked before rendering, because afterwards an unassigned
// value is indistinguishable from one that is legitimately empty. The scan for
// the two markers afterwards catches what a nil produces from inside a slice,
// where there is no field to check.
//
// The HTML is wrapped here rather than only when it is encoded. The log
// transport writes this string straight to disk and the Slack fallback reads
// it, so wrapping at the point of generation means every consumer sees a body
// that already obeys the 998-octet line limit.
func render(content mail.Content, required map[string]string) (Rendered, error) {
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return Rendered{}, fmt.Errorf("%w: %s was assigned nothing", ErrUndefinedVariable, name)
		}
	}

	html, err := content.HTML()
	if err != nil {
		return Rendered{}, fmt.Errorf("%w: %s", ErrUndefinedVariable, err)
	}

	text := content.Text()

	for _, body := range []string{html, text} {
		for _, marker := range []string{missingValue, blankedValue} {
			if strings.Contains(body, marker) {
				return Rendered{}, fmt.Errorf("%w: the rendered body contains %s", ErrUndefinedVariable, marker)
			}
		}
	}

	wrapped := mail.Wrap(html, mail.MaxLineLength)

	if longest := mail.LongestLine(wrapped); longest >= mail.MaxLineLength {
		return Rendered{}, fmt.Errorf("reports: the rendered body has a %d-octet line, over the SMTP limit", longest)
	}

	return Rendered{Subject: content.Subject, HTML: wrapped, Text: text}, nil
}

// titleOf capitalises a kind for a subject line, without pulling in a
// dependency to upper-case one letter.
func titleOf(kind string) string {
	if kind == "" {
		return ""
	}

	return strings.ToUpper(kind[:1]) + kind[1:]
}

// FallbackClock is the dial for a destination with no stored preference: an
// address belonging to no user, one whose preference is still "system" — which
// resolves from a browser cookie a background job does not have — and a webhook,
// which has no reader at all.
const FallbackClock = timefmt.Cycle24

// Renderings builds one email per clock format, on demand and at most once each.
//
// A report is one set of numbers read by up to twenty-five people who do not
// all read a clock the same way. There are only two dials, so a twenty-five
// recipient report costs two renderings rather than twenty-five.
//
// One Renderings belongs to one delivery and is used from one goroutine. The
// memo is not guarded, and both delivery loops are sequential.
type Renderings struct {
	build func(cycle string) (Rendered, error)
	made  map[string]Rendered
}

// ReportRenderings prepares a weekly or monthly report for either dial.
func ReportRenderings(report Report) *Renderings {
	return &Renderings{build: func(cycle string) (Rendered, error) { return RenderReport(report, cycle) }}
}

// AlertRenderings prepares a spike or drop alert for either dial.
func AlertRenderings(alert Alert) *Renderings {
	return &Renderings{build: func(cycle string) (Rendered, error) { return RenderAlert(alert, cycle) }}
}

// On returns the rendering for one dial, building it the first time it is asked
// for.
func (r *Renderings) On(cycle string) (Rendered, error) {
	if r.made == nil {
		r.made = map[string]Rendered{}
	}

	if made, ok := r.made[cycle]; ok {
		return made, nil
	}

	made, err := r.build(cycle)
	if err != nil {
		return Rendered{}, err
	}

	r.made[cycle] = made

	return made, nil
}
