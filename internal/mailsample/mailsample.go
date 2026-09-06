//
// mailsample.go
// Every message the product sends, built with sample data.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package mailsample builds one representative message for every tag in
// mail.Tags, using the same builders the product uses.
//
// It is a package rather than a test file so that the set has one name and one
// home: its own test is the guard that every message carries a postal address,
// and rendering the set to disk is how a change to the shared layout is looked
// at before it is sent to anybody.
package mailsample

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/reports"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

// Recipient is who every sample message is addressed to.
const Recipient = "sam@example.com"

// baseURL is the application the sample links point at.
const baseURL = "https://app.feasible.lol"

// at is the instant every sample date is measured from, so two runs produce the
// same bytes.
var at = time.Date(2026, 9, 6, 14, 32, 0, 0, time.UTC)

// Messages builds one message per tag, keyed by tag.
//
// Every entry goes through the same builder the product uses, so a message that
// would not render for a customer does not render here either.
func Messages() (map[string]mail.Message, error) {
	pieces, err := Contents()
	if err != nil {
		return nil, err
	}

	built := map[string]mail.Message{}

	for tag, content := range pieces {
		message, err := content.Message(Recipient, tag)
		if err != nil {
			return nil, fmt.Errorf("mailsample: %s: %w", tag, err)
		}

		built[tag] = message
	}

	produced, err := rendered()
	if err != nil {
		return nil, err
	}

	for tag, one := range produced {
		built[tag] = one.Message(Recipient, tag)
	}

	return built, nil
}

// Contents is every message the layout renders, before it is addressed. The
// report and the alert are absent: internal/reports hands back a rendered body
// rather than the content behind it.
func Contents() (map[string]mail.Content, error) {
	built := map[string]mail.Content{
		mail.TagVerifyEmail:          mail.VerificationContent("483920", baseURL+"/verify?token=abc123"),
		mail.TagPasswordReset:        mail.PasswordResetContent(baseURL + "/reset-password?token=9f2c4d8e"),
		mail.TagPasswordChanged:      mail.PasswordChangedContent(baseURL),
		mail.TagNewLogin:             mail.NewLoginContent(baseURL, "Chrome on macOS", reports.FallbackClock, at),
		mail.TagSettingsConfirmation: mail.SettingsConfirmationContent("704118"),
		mail.TagTeamInvitation: mail.InvitationContent("Harbor", "Sam Ellis", "an editor",
			baseURL+"/invitations/7f3a", at.AddDate(0, 0, 7)),
	}

	for _, scheduled := range lifecycle.Sequence {
		content, err := mail.LifecycleContent(lifecycleNotice(scheduled))
		if err != nil {
			return nil, fmt.Errorf("mailsample: lifecycle %s: %w", scheduled.Template, err)
		}

		built[scheduled.Template] = content
	}

	for _, level := range []usage.Level{usage.LevelWarn, usage.LevelNear, usage.LevelReached} {
		content, err := mail.UsageContent(usageNotice(level))
		if err != nil {
			return nil, fmt.Errorf("mailsample: usage %s: %w", level, err)
		}

		built["usage_"+string(level)] = content
	}

	return built, nil
}

// rendered is every message built in internal/reports.
func rendered() (map[string]reports.Rendered, error) {
	report := reports.Report{
		Domain:       "harbor.my",
		PeriodLabel:  "31 August – 6 September 2026",
		DashboardURL: baseURL + "/harbor.my",
		Figures: []reports.Figure{
			{Label: "Visitors", Value: "4,182", Change: "+18%", Direction: "up"},
			{Label: "Pageviews", Value: "11,904", Change: "+9%", Direction: "up"},
			{Label: "Bounce rate", Value: "41%", Change: "−4%", Direction: "down"},
			{Label: "Visit duration", Value: "2m 14s"},
		},
		TopPages: []reports.Entry{
			{Label: "/", Value: "3,914"},
			{Label: "/pricing", Value: "1,220"},
			{Label: "/docs/getting-started", Value: "806"},
		},
		TopSources: []reports.Entry{
			{Label: "Direct / None", Value: "2,104"},
			{Label: "news.ycombinator.com", Value: "988"},
		},
		Countries: []reports.Entry{
			{Label: "United States", Value: "1,902"},
			{Label: "Germany", Value: "604"},
		},
		GeneratedAt: at,
	}

	alert := reports.Alert{
		Domain:       "harbor.my",
		Headline:     "Traffic is 3.4× the usual for a Sunday morning",
		Detail:       "412 visitors in the last hour, against a typical 121 for this hour of the week.",
		Threshold:    250,
		Observed:     412,
		DashboardURL: baseURL + "/harbor.my",
		TriggeredAt:  at,
	}

	built := map[string]reports.Rendered{}

	for tag, kind := range map[string]string{
		mail.TagReportWeekly:  reports.KindWeekly,
		mail.TagReportMonthly: reports.KindMonthly,
		mail.TagReportPreview: reports.KindWeekly,
	} {
		report.Kind = kind

		one, err := reports.RenderReport(report, reports.FallbackClock)
		if err != nil {
			return nil, fmt.Errorf("mailsample: %s: %w", tag, err)
		}

		built[tag] = one
	}

	for tag, kind := range map[string]string{
		mail.TagAlertSpike: reports.KindSpike,
		mail.TagAlertDrop:  reports.KindDrop,
	} {
		alert.Kind = kind

		one, err := reports.RenderAlert(alert, reports.FallbackClock)
		if err != nil {
			return nil, fmt.Errorf("mailsample: %s: %w", tag, err)
		}

		built[tag] = one
	}

	return built, nil
}

// lifecycleNotice is a trial account partway through the clock.
func lifecycleNotice(scheduled lifecycle.Scheduled) lifecycle.Notice {
	return lifecycle.Notice{
		TeamID:     1,
		TeamName:   "Harbor",
		MessageKey: "sample-" + scheduled.Template,
		To:         Recipient,
		Template:   scheduled.Template,
		Trigger:    lifecycle.TriggerTrial,
		Phase:      scheduled.Announces,
		Day:        scheduled.Day,
		LocksAt:    at.AddDate(0, 0, 3),
		StopsAt:    at.AddDate(0, 0, 17),
		DeletesAt:  at.AddDate(0, 0, 47),
		Announced:  at.AddDate(0, 0, 3),
		BillingURL: baseURL + "/billing",
		ExportURL:  baseURL + "/settings/export",
	}
}

// usageNotice is an account partway up the volume ladder.
func usageNotice(level usage.Level) usage.Notice {
	return usage.Notice{
		TeamID:     1,
		TeamName:   "Harbor",
		To:         Recipient,
		Level:      level,
		Period:     "September 2026",
		Billable:   870_412,
		Limit:      1_000_000,
		Projected:  1_140_000,
		Deadline:   at.AddDate(0, 0, 21),
		SalesEmail: mail.SalesAddress,
		BillingURL: baseURL + "/billing",
	}
}

// Index is a page showing every rendered message side by side, because a
// shared layout is judged on whether the set looks like one product.
func Index(tags []string) string {
	var b strings.Builder

	b.WriteString(`<!doctype html>
<meta charset="utf-8">
<title>feasible.lol — every email</title>
<style>
  body { margin: 0; background: #332f2f; color: #f3f2f2;
         font: 13px -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; }
  h1 { font-size: 15px; padding: 16px 16px 0 16px; margin: 0; }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
          gap: 16px; padding: 16px; }
  .cap { font-weight: 700; padding: 0 0 6px 2px; }
  iframe { width: 100%; height: 760px; border: 0; background: #eae9e9; }
</style>
<h1>Every email the product sends</h1>
<div class="grid">
`)

	for _, tag := range tags {
		b.WriteString(`<div><div class="cap">` + template.HTMLEscapeString(tag) + `</div>`)
		b.WriteString(`<iframe src="` + template.HTMLEscapeString(tag) + `.html"></iframe></div>` + "\n")
	}

	b.WriteString("</div>\n")

	return b.String()
}
