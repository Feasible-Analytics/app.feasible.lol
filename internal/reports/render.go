//
// render.go
// Report and alert bodies, which refuse to render with a value nothing assigned.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/timefmt"
)

// blankedValue is what html/template writes in place of a URL it cannot prove
// is safe. It is the shape the failure below actually takes: not visibly
// missing text, but a dead href that looks like a working button.
const blankedValue = "ZgotmplZ"

// ErrUndefinedVariable is what a message with a value nothing ever assigned
// produces.
//
// This is a named error rather than a generic failure because of the specific
// bug it prevents: an incumbent's spike alert referenced a dashboard link that
// nothing ever set, their template language rendered it as nothing, and the
// emails shipped for months with a missing link and no error anywhere. A value
// that was never assigned has to be a hard failure, every time.
var ErrUndefinedVariable = fmt.Errorf("reports: a message carried a value that was never assigned")

// Figure and Entry are the layout's own types, so a number is formatted once
// and rendered as it was formatted.
type (
	Figure = mail.Figure
	Entry  = mail.Row
)

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

	// Unsubscribe is where this recipient stops receiving this report. It is
	// set per recipient at delivery, so it is empty on a Slack post and on a
	// preview.
	Unsubscribe string

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

	// Unsubscribe is where this recipient stops receiving alerts from this
	// rule. Set per recipient at delivery.
	Unsubscribe string
}

// Rendered is a subject and both bodies, ready to hand to a transport.
type Rendered struct {
	Subject string
	HTML    string
	Text    string

	// Unsubscribe is this recipient's link. It reaches the transport as the
	// List-Unsubscribe header, so a mail client can offer the control itself
	// rather than only showing the footer link.
	Unsubscribe string
}

// Message turns a rendering into one addressed mail message.
//
// It is one recipient rather than a list because that is the shape the shared
// mailer takes, and the shared mailer is where the wrapping and the "the relay
// declined this" check live. A list here would mean a second send path with
// neither.
func (r Rendered) Message(to, tag string) mail.Message {
	return mail.Message{
		To:          to,
		Subject:     r.Subject,
		HTML:        r.HTML,
		Text:        r.Text,
		Tag:         tag,
		Unsubscribe: r.Unsubscribe,
	}
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
		Preheader:  headline(report.Figures),
		Kicker:     kind + " report",
		Heading:    report.Domain,
		Subheading: report.PeriodLabel,
		Note:       report.Note,
		Figures:    report.Figures,
		Tables: []mail.Table{
			{Title: "Top pages", Rows: report.TopPages, Empty: "No pages were viewed in this period."},
			{Title: "Top sources", Rows: report.TopSources, Empty: "No referrers were recorded in this period."},
			{Title: "Top countries", Rows: report.Countries, Empty: "No locations were recorded in this period."},
		},
		Primary:     mail.Button{Label: "Open the dashboard", URL: report.DashboardURL},
		Unsubscribe: report.Unsubscribe,
		Closing: fmt.Sprintf("Generated %s. You are receiving this because somebody added your address "+
			"to this site's %s report.", generated, kind),
	}

	return render(content, []Assigned{
		{"Domain", report.Domain},
		{"Kind", kind},
		{"PeriodLabel", report.PeriodLabel},
		{"DashboardURL", report.DashboardURL},
		{"GeneratedAt", generated},
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

		// The headline is already the subject, so the preview carries the
		// numbers behind it instead of saying the same thing twice.
		Preheader: alert.Detail,
		Body:      []string{alert.Headline, alert.Detail},

		Figures: []mail.Figure{
			{Label: "Observed", Value: strconv.Itoa(alert.Observed)},
			{Label: "Threshold", Value: strconv.Itoa(alert.Threshold)},
		},
		Primary:     mail.Button{Label: "Open the dashboard", URL: alert.DashboardURL},
		Unsubscribe: alert.Unsubscribe,
		Closing: fmt.Sprintf("Triggered %s. At most two alerts are sent per site per day, so this "+
			"will not repeat every hour.", triggered),
	}

	return render(content, []Assigned{
		{"Domain", alert.Domain},
		{"Kind", kind},
		{"Headline", alert.Headline},
		{"Detail", alert.Detail},
		{"DashboardURL", alert.DashboardURL},
		{"TriggeredAt", triggered},
	})
}

// headline is the inbox preview for a report: its first two numbers.
//
// A report has no body copy, so with nothing here the preview would be the
// domain repeated from the subject. The numbers are what somebody wants to know
// without opening it.
func headline(figures []Figure) string {
	parts := make([]string, 0, 2)

	for _, figure := range figures {
		if len(parts) == 2 {
			break
		}

		parts = append(parts, strings.ToLower(figure.Label)+" "+figure.Value)
	}

	if len(parts) == 0 {
		return ""
	}

	parts[0] = strings.ToUpper(parts[0][:1]) + parts[0][1:]

	return strings.Join(parts, ", ")
}

// render turns content into both bodies with every silent failure turned into
// a loud one.
//
// The required list names the values whose absence is invisible in a mail
// client. Each is checked before rendering, because afterwards a value nothing
// assigned is indistinguishable from one that is legitimately empty. Every link
// is then looked for in the rendered HTML, which is what catches a URL
// html/template refused to trust and blanked into a dead href.
//
// Nothing scans the bodies for a marker. A page path is written by a visitor,
// so a scan for a literal like "<no value>" is a string one crafted page view
// could use to stop a site's reports rendering for good.
//
// The HTML is wrapped here rather than only when it is encoded. The log
// transport writes this string straight to disk and the Slack fallback reads
// it, so wrapping at the point of generation means every consumer sees a body
// that already obeys the 998-octet line limit.
func render(content mail.Content, required []Assigned) (Rendered, error) {
	for _, value := range required {
		if strings.TrimSpace(value.Value) == "" {
			return Rendered{}, fmt.Errorf("%w: %s was assigned nothing", ErrUndefinedVariable, value.Name)
		}
	}

	html, err := content.HTML()
	if err != nil {
		return Rendered{}, fmt.Errorf("%w: %s", ErrUndefinedVariable, err)
	}

	for _, button := range append([]mail.Button{content.Primary}, content.Secondary...) {
		if button.URL == "" {
			continue
		}

		if !strings.Contains(html, `href="`+template.HTMLEscapeString(button.URL)+`"`) {
			return Rendered{}, fmt.Errorf("%w: %q did not survive into the %s button, which now points at %s",
				ErrUndefinedVariable, button.URL, button.Label, blankedValue)
		}
	}

	wrapped := mail.Wrap(html, mail.MaxLineLength)

	if longest := mail.LongestLine(wrapped); longest >= mail.MaxLineLength {
		return Rendered{}, fmt.Errorf("reports: the rendered body has a %d-octet line, over the SMTP limit", longest)
	}

	return Rendered{
		Subject:     content.Subject,
		HTML:        wrapped,
		Text:        content.Text(),
		Unsubscribe: content.Unsubscribe,
	}, nil
}

// Assigned is one named value the guard above refuses to render without. It is
// a slice rather than a map so two blanks always name the same one.
type Assigned struct {
	Name  string
	Value string
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
	build func(cycle, unsubscribe string) (Rendered, error)
	made  map[string]Rendered
}

// ReportRenderings prepares a weekly or monthly report for either dial.
func ReportRenderings(report Report) *Renderings {
	return &Renderings{build: func(cycle, unsubscribe string) (Rendered, error) {
		report.Unsubscribe = unsubscribe

		return RenderReport(report, cycle)
	}}
}

// AlertRenderings prepares a spike or drop alert for either dial.
func AlertRenderings(alert Alert) *Renderings {
	return &Renderings{build: func(cycle, unsubscribe string) (Rendered, error) {
		alert.Unsubscribe = unsubscribe

		return RenderAlert(alert, cycle)
	}}
}

// On returns the rendering for one dial with no unsubscribe link — the Slack
// fallback, and a preview.
func (r *Renderings) On(cycle string) (Rendered, error) {
	return r.For(cycle, "")
}

// For returns the rendering for one dial and one recipient's unsubscribe link,
// building it the first time it is asked for.
//
// The memo is keyed on both, so twenty-five recipients on the same dial cost
// twenty-five renderings rather than one — which is the price of a link that
// only removes the person holding it, and is small beside twenty-five sends.
func (r *Renderings) For(cycle, unsubscribe string) (Rendered, error) {
	if r.made == nil {
		r.made = map[string]Rendered{}
	}

	key := cycle + "\x00" + unsubscribe

	if made, ok := r.made[key]; ok {
		return made, nil
	}

	made, err := r.build(cycle, unsubscribe)
	if err != nil {
		return Rendered{}, err
	}

	r.made[key] = made

	return made, nil
}
