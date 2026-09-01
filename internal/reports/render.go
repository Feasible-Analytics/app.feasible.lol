//
// render.go
// Report and alert bodies, from templates that refuse to render a missing variable.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
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
func RenderReport(report Report) (Rendered, error) {
	subject := fmt.Sprintf("%s — %s report for %s", report.Domain, titleOf(report.Kind), report.PeriodLabel)

	data := map[string]any{
		"Domain":       report.Domain,
		"Kind":         titleOf(report.Kind),
		"PeriodLabel":  report.PeriodLabel,
		"DashboardURL": report.DashboardURL,
		"Figures":      report.Figures,
		"TopPages":     report.TopPages,
		"TopSources":   report.TopSources,
		"Countries":    report.Countries,
		"Note":         report.Note,
		"GeneratedAt":  report.GeneratedAt.UTC().Format("2 January 2006 15:04 MST"),
	}

	html, err := renderStrict(reportHTML, data)
	if err != nil {
		return Rendered{}, err
	}

	text, err := renderStrict(reportText, data)
	if err != nil {
		return Rendered{}, err
	}

	return Rendered{Subject: subject, HTML: mail.Wrap(html, mail.MaxLineLength), Text: text}, nil
}

// RenderAlert builds a spike or drop email.
func RenderAlert(alert Alert) (Rendered, error) {
	subject := fmt.Sprintf("%s — %s", alert.Domain, alert.Headline)

	data := map[string]any{
		"Domain":       alert.Domain,
		"Kind":         titleOf(alert.Kind),
		"Headline":     alert.Headline,
		"Detail":       alert.Detail,
		"Threshold":    alert.Threshold,
		"Observed":     alert.Observed,
		"DashboardURL": alert.DashboardURL,
		"TriggeredAt":  alert.TriggeredAt.UTC().Format("2 January 2006 15:04 MST"),
	}

	html, err := renderStrict(alertHTML, data)
	if err != nil {
		return Rendered{}, err
	}

	text, err := renderStrict(alertText, data)
	if err != nil {
		return Rendered{}, err
	}

	return Rendered{Subject: subject, HTML: mail.Wrap(html, mail.MaxLineLength), Text: text}, nil
}

// renderStrict parses and executes a template with every silent failure turned
// into a loud one.
//
// Two settings do the work. missingkey=error makes a lookup of a key the data
// map does not hold an execution error instead of an empty string, which is the
// exact failure this package refuses to ship. The scan for "<no value>"
// afterwards catches the one case missingkey cannot: a nil reaching the output
// from inside a slice element, where there is no map lookup to fail on.
func renderStrict(source string, data map[string]any) (string, error) {
	// A key that exists and holds nil is the incumbent's bug exactly: the
	// variable was referenced, something was meant to assign it, nothing did,
	// and html/template renders it as an empty string with no complaint. It has
	// to be rejected before rendering, because after rendering it is
	// indistinguishable from a value that is legitimately empty.
	for name, value := range data {
		if value == nil {
			return "", fmt.Errorf("%w: %s was assigned nothing", ErrUndefinedVariable, name)
		}
	}

	tmpl, err := template.New("body").Option("missingkey=error").Parse(source)
	if err != nil {
		return "", fmt.Errorf("reports: parse template: %w", err)
	}

	var out bytes.Buffer

	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("%w: %s", ErrUndefinedVariable, err)
	}

	body := out.String()

	for _, marker := range []string{missingValue, blankedValue} {
		if strings.Contains(body, marker) {
			return "", fmt.Errorf("%w: the rendered body contains %s", ErrUndefinedVariable, marker)
		}
	}

	if longest := mail.LongestLine(mail.Wrap(body, mail.MaxLineLength)); longest >= mail.MaxLineLength {
		return "", fmt.Errorf("reports: the rendered body has a %d-octet line, over the SMTP limit", longest)
	}

	return body, nil
}

// titleOf capitalises a kind for a subject line, without pulling in a
// dependency to upper-case one letter.
func titleOf(kind string) string {
	if kind == "" {
		return ""
	}

	return strings.ToUpper(kind[:1]) + kind[1:]
}

// The email bodies are inline strings rather than embedded files because they
// are the one place in the product where the markup has to be table-based,
// inline-styled and readable in a mail client from 2009 — and keeping them next
// to the struct that fills them in is what makes an unassigned variable
// obvious in review as well as at run time.
const reportHTML = `<!doctype html>
<html lang="en">
<body style="margin:0;padding:0;background:#f4f5f7;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;color:#1f2933;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f4f5f7;padding:24px 12px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:620px;background:#ffffff;border:1px solid #e4e7eb;border-radius:8px;">
<tr><td style="padding:22px 24px 8px 24px;">
<div style="font-size:12px;letter-spacing:.08em;text-transform:uppercase;color:#0d9488;font-weight:700;">{{.Kind}} report</div>
<div style="font-size:22px;font-weight:700;margin-top:4px;">{{.Domain}}</div>
<div style="font-size:14px;color:#616e7c;margin-top:2px;">{{.PeriodLabel}}</div>
{{if .Note}}<div style="font-size:14px;color:#8a5300;background:#fff7e6;border:1px solid #ffe0a3;border-radius:6px;padding:10px 12px;margin-top:14px;">{{.Note}}</div>{{end}}
</td></tr>
<tr><td style="padding:8px 24px 0 24px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0">
<tr>
{{range .Figures}}
<td style="padding:12px 8px;border-top:1px solid #e4e7eb;vertical-align:top;">
<div style="font-size:12px;color:#616e7c;">{{.Label}}</div>
<div style="font-size:20px;font-weight:700;margin-top:2px;">{{.Value}}</div>
{{if .Change}}<div style="font-size:12px;margin-top:2px;color:{{if eq .Direction "up"}}#0f7b47{{else if eq .Direction "down"}}#b42318{{else}}#616e7c{{end}};">{{.Change}}</div>{{end}}
</td>
{{end}}
</tr>
</table>
</td></tr>
<tr><td style="padding:6px 24px 0 24px;">
<div style="font-size:13px;font-weight:700;margin-top:16px;border-top:1px solid #e4e7eb;padding-top:14px;">Top pages</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="margin-top:6px;">
{{range .TopPages}}
<tr><td style="font-size:14px;padding:4px 0;color:#3e4c59;">{{.Label}}</td><td align="right" style="font-size:14px;padding:4px 0;font-variant-numeric:tabular-nums;">{{.Value}}</td></tr>
{{else}}
<tr><td style="font-size:14px;padding:4px 0;color:#9aa5b1;">No pages were viewed in this period.</td></tr>
{{end}}
</table>
<div style="font-size:13px;font-weight:700;margin-top:18px;">Top sources</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="margin-top:6px;">
{{range .TopSources}}
<tr><td style="font-size:14px;padding:4px 0;color:#3e4c59;">{{.Label}}</td><td align="right" style="font-size:14px;padding:4px 0;font-variant-numeric:tabular-nums;">{{.Value}}</td></tr>
{{else}}
<tr><td style="font-size:14px;padding:4px 0;color:#9aa5b1;">No referrers were recorded in this period.</td></tr>
{{end}}
</table>
<div style="font-size:13px;font-weight:700;margin-top:18px;">Top countries</div>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="margin-top:6px;">
{{range .Countries}}
<tr><td style="font-size:14px;padding:4px 0;color:#3e4c59;">{{.Label}}</td><td align="right" style="font-size:14px;padding:4px 0;font-variant-numeric:tabular-nums;">{{.Value}}</td></tr>
{{else}}
<tr><td style="font-size:14px;padding:4px 0;color:#9aa5b1;">No locations were recorded in this period.</td></tr>
{{end}}
</table>
</td></tr>
<tr><td style="padding:20px 24px 24px 24px;">
<a href="{{.DashboardURL}}" style="display:inline-block;background:#0d9488;color:#ffffff;text-decoration:none;font-size:14px;font-weight:600;padding:10px 16px;border-radius:6px;">Open the dashboard</a>
<div style="font-size:12px;color:#9aa5b1;margin-top:16px;">Generated {{.GeneratedAt}}. You are receiving this because somebody added your address to this site's {{.Kind}} report.</div>
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`

const reportText = `{{.Domain}} — {{.Kind}} report
{{.PeriodLabel}}
{{if .Note}}
{{.Note}}
{{end}}
{{range .Figures}}{{.Label}}: {{.Value}}{{if .Change}} ({{.Change}}){{end}}
{{end}}
Top pages
{{range .TopPages}}  {{.Label}}  {{.Value}}
{{else}}  No pages were viewed in this period.
{{end}}
Top sources
{{range .TopSources}}  {{.Label}}  {{.Value}}
{{else}}  No referrers were recorded in this period.
{{end}}
Top countries
{{range .Countries}}  {{.Label}}  {{.Value}}
{{else}}  No locations were recorded in this period.
{{end}}
Open the dashboard: {{.DashboardURL}}

Generated {{.GeneratedAt}}.
`

const alertHTML = `<!doctype html>
<html lang="en">
<body style="margin:0;padding:0;background:#f4f5f7;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;color:#1f2933;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f4f5f7;padding:24px 12px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px;background:#ffffff;border:1px solid #e4e7eb;border-radius:8px;">
<tr><td style="padding:22px 24px;">
<div style="font-size:12px;letter-spacing:.08em;text-transform:uppercase;color:#b42318;font-weight:700;">{{.Kind}} alert</div>
<div style="font-size:20px;font-weight:700;margin-top:4px;">{{.Domain}}</div>
<div style="font-size:16px;margin-top:10px;">{{.Headline}}</div>
<div style="font-size:14px;color:#3e4c59;margin-top:8px;">{{.Detail}}</div>
<table role="presentation" cellpadding="0" cellspacing="0" style="margin-top:16px;border-top:1px solid #e4e7eb;width:100%;">
<tr>
<td style="padding:12px 0;"><div style="font-size:12px;color:#616e7c;">Observed</div><div style="font-size:20px;font-weight:700;">{{.Observed}}</div></td>
<td style="padding:12px 0;"><div style="font-size:12px;color:#616e7c;">Threshold</div><div style="font-size:20px;font-weight:700;">{{.Threshold}}</div></td>
</tr>
</table>
<a href="{{.DashboardURL}}" style="display:inline-block;background:#0d9488;color:#ffffff;text-decoration:none;font-size:14px;font-weight:600;padding:10px 16px;border-radius:6px;margin-top:8px;">Open the dashboard</a>
<div style="font-size:12px;color:#9aa5b1;margin-top:16px;">Triggered {{.TriggeredAt}}. At most two alerts are sent per site per day, so this will not repeat every hour.</div>
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`

const alertText = `{{.Domain}} — {{.Kind}} alert

{{.Headline}}
{{.Detail}}

Observed:  {{.Observed}}
Threshold: {{.Threshold}}

Open the dashboard: {{.DashboardURL}}

Triggered {{.TriggeredAt}}. At most two alerts are sent per site per day.
`
