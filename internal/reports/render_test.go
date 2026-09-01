//
// render_test.go
// No line over 998 octets, and no variable that renders as nothing.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
)

// bigReport is a realistic report with the longest values a real site produces:
// five figures, and three top-five lists whose labels are long paths. Rendered
// on one line it is well past the SMTP limit, which is the point.
func bigReport() Report {
	report := Report{
		Domain:       "a-fairly-long-customer-domain.example",
		Kind:         KindWeekly,
		PeriodLabel:  "27 July – 2 August 2026",
		DashboardURL: "https://feasible.lol/dashboard/a-fairly-long-customer-domain.example",
		GeneratedAt:  time.Date(2026, 8, 3, 0, 5, 0, 0, time.UTC),
		Figures: []Figure{
			{Label: "Unique visitors", Value: "128,442", Change: "+18%", Direction: "up"},
			{Label: "Visits", Value: "201,908", Change: "+11%", Direction: "up"},
			{Label: "Pageviews", Value: "512,334", Change: "−4%", Direction: "down"},
			{Label: "Bounce rate", Value: "42%", Change: "no change", Direction: "flat"},
			{Label: "Visit duration", Value: "2m 14s", Change: "+6%", Direction: "up"},
		},
	}

	for i := 0; i < TopN; i++ {
		suffix := strconv.Itoa(i)

		report.TopPages = append(report.TopPages, Entry{
			Label: "/blog/2026/08/a-reasonably-long-article-slug-that-people-really-do-write-" + suffix,
			Value: "12,004",
		})
		report.TopSources = append(report.TopSources, Entry{
			Label: "news.ycombinator.com/item?id=123456789" + suffix,
			Value: "8,110",
		})
		report.Countries = append(report.Countries, Entry{Label: "United Kingdom " + suffix, Value: "4,221"})
	}

	return report
}

// TestARenderedReportHasNoLineOver998Octets is the acceptance criterion.
//
// RFC 5321 caps a line at 998 octets and a server may reject the message after
// it has already accepted DATA, so the send looks successful and nothing
// arrives. This silently broke weekly reports for an incumbent's self-hosters
// entirely, for everyone, for as long as the feature existed.
func TestARenderedReportHasNoLineOver998Octets(t *testing.T) {
	rendered, err := RenderReport(bigReport())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if longest := mail.LongestLine(rendered.HTML); longest >= mail.MaxLineLength {
		t.Fatalf("the rendered HTML has a %d-octet line, over the %d limit", longest, mail.MaxLineLength)
	}

	if longest := mail.LongestLine(rendered.Text); longest >= mail.MaxLineLength {
		t.Fatalf("the rendered text has a %d-octet line", longest)
	}

	// And the same through the shared renderer, which is what actually goes on
	// the wire.
	encoded := mail.Render("reports@example.com", rendered.Message("anna@example.com", "report_weekly"))

	if longest := mail.LongestLine(encoded); longest >= mail.MaxLineLength {
		t.Fatalf("the encoded message has a %d-octet line", longest)
	}
}

// TestAnAlertAlsoStaysUnderTheLimit checks the other template.
func TestAnAlertAlsoStaysUnderTheLimit(t *testing.T) {
	rendered, err := RenderAlert(Alert{
		Domain:       "a-fairly-long-customer-domain.example",
		Kind:         KindSpike,
		Headline:     "412 visitors are on the site right now",
		Detail:       strings.Repeat("Something is sending you traffic. ", 40),
		Threshold:    10,
		Observed:     412,
		DashboardURL: "https://feasible.lol/dashboard/a-fairly-long-customer-domain.example",
		TriggeredAt:  time.Date(2026, 8, 3, 9, 15, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if longest := mail.LongestLine(rendered.HTML); longest >= mail.MaxLineLength {
		t.Fatalf("the rendered alert has a %d-octet line", longest)
	}
}

// TestAnUndefinedVariableFailsLoudly is the second acceptance criterion.
//
// An incumbent's spike alert referenced a dashboard-link variable that nothing
// ever assigned. Their template language rendered it as nothing, and the emails
// shipped for months with a missing link and no error anywhere. A variable that
// was never assigned has to be a hard failure, every time.
func TestAnUndefinedVariableFailsLoudly(t *testing.T) {
	_, err := renderStrict(`<a href="{{.DashboardURL}}">{{.NeverAssigned}}</a>`, map[string]any{
		"DashboardURL": "https://example.com",
	})

	if !errors.Is(err, ErrUndefinedVariable) {
		t.Fatalf("an unassigned variable rendered without error: %v", err)
	}
}

// TestANilValueIsAlsoRefused checks the case missingkey cannot catch: a key
// that exists but holds nothing, which Go renders as the literal "<no value>".
func TestANilValueIsAlsoRefused(t *testing.T) {
	_, err := renderStrict(`<a href="{{.DashboardURL}}">link</a>`, map[string]any{
		"DashboardURL": nil,
	})

	if !errors.Is(err, ErrUndefinedVariable) {
		t.Fatalf("a nil value rendered without error: %v", err)
	}
}

// TestEveryReportVariableIsAssigned walks the real template. It is the test
// that would have caught the incumbent's missing link, so it is written against
// the shipped template rather than a fixture.
func TestEveryReportVariableIsAssigned(t *testing.T) {
	rendered, err := RenderReport(bigReport())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, body := range []string{rendered.HTML, rendered.Text} {
		for _, marker := range []string{missingValue, blankedValue} {
			if strings.Contains(body, marker) {
				t.Fatalf("the rendered body contains %s", marker)
			}
		}
	}

	// The dashboard link is the one variable whose absence is invisible in a
	// mail client, so it is checked by name.
	if !strings.Contains(rendered.HTML, "https://feasible.lol/dashboard/") {
		t.Fatal("the report has no dashboard link in it")
	}

	if !strings.Contains(rendered.Text, "https://feasible.lol/dashboard/") {
		t.Fatal("the text alternative has no dashboard link in it")
	}
}

// TestAnEmptyReportStillRenders checks the site with no traffic, which is both
// a real state and the one a drop alert is about.
func TestAnEmptyReportStillRenders(t *testing.T) {
	rendered, err := RenderReport(Report{
		Domain:       "quiet.example",
		Kind:         KindMonthly,
		PeriodLabel:  "August 2026",
		DashboardURL: "https://feasible.lol/dashboard/quiet.example",
		Note:         "No visitors were recorded in this period.",
		GeneratedAt:  time.Date(2026, 9, 1, 0, 5, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, want := range []string{
		"No visitors were recorded in this period.",
		"No pages were viewed in this period.",
		"No referrers were recorded in this period.",
	} {
		if !strings.Contains(rendered.HTML, want) {
			t.Errorf("the empty report is missing %q", want)
		}
	}
}

// TestTheSubjectNamesTheSiteAndThePeriod checks what somebody sees in a list of
// forty unread emails.
func TestTheSubjectNamesTheSiteAndThePeriod(t *testing.T) {
	rendered, err := RenderReport(bigReport())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, want := range []string{"a-fairly-long-customer-domain.example", "Weekly", "27 July – 2 August 2026"} {
		if !strings.Contains(rendered.Subject, want) {
			t.Errorf("the subject %q is missing %q", rendered.Subject, want)
		}
	}
}

// TestSlackTextCarriesTheSameNumbers checks that the chat message is built from
// the same rendering as the email, so the two cannot disagree.
func TestSlackTextCarriesTheSameNumbers(t *testing.T) {
	rendered, err := RenderReport(bigReport())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	text := SlackText(rendered, "https://feasible.lol/dashboard/x")

	if !strings.Contains(text, "128,442") {
		t.Fatal("the Slack message does not carry the visitor count")
	}

	if !strings.Contains(text, rendered.Subject) {
		t.Fatal("the Slack message does not carry the subject")
	}
}
