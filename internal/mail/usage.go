//
// usage.go
// The volume ladder's four emails: three warnings and one conversation.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

// None of these messages threatens anything. Going over the plan is not a
// payment failure, it never touches the deletion clock, and the product never
// silently drops a customer's data for growing — so the copy says exactly that,
// and points at a person rather than at a checkout.

// UsageMailer turns a volume notice into a sent message.
type UsageMailer struct {
	Sender Sender
}

// NewUsageMailer builds the notifier the volume sweeper drives.
func NewUsageMailer(sender Sender) *UsageMailer {
	return &UsageMailer{Sender: sender}
}

// Notify renders and sends one volume email, returning what the transport
// observed rather than assuming the send worked.
func (m *UsageMailer) Notify(ctx context.Context, notice usage.Notice) (string, error) {
	content, err := UsageContent(notice)
	if err != nil {
		return "", err
	}

	msg, err := content.Message(notice.To, usageTag(notice.Level))
	if err != nil {
		return "", err
	}

	result, err := m.Sender.Send(ctx, msg)
	if err != nil {
		return result.String(), err
	}

	if !result.Accepted {
		return result.String(), fmt.Errorf("mail: usage %s for team %d was not accepted: %s", notice.Level, notice.TeamID, result.Detail)
	}

	return result.String(), nil
}

// UsageContent is the copy for the four rungs of the ladder.
func UsageContent(notice usage.Notice) (Content, error) {
	used := thousands(notice.Billable)
	limit := thousands(notice.Limit)

	content := Content{
		Facts: []Fact{
			{Label: "Used this month", Value: used},
			{Label: "Included in your plan", Value: limit},
		},
		Primary: Button{Label: "Talk to us about Enterprise", URL: "mailto:" + notice.SalesEmail},
		Secondary: []Button{
			{Label: "See your usage", URL: notice.BillingURL},
		},
		Closing: "Reply to this email and a person will answer. There is no automated sales sequence behind this address.",
	}

	if notice.Projected > 0 {
		content.Facts = append(content.Facts, Fact{Label: "On track to finish the month at", Value: thousands(notice.Projected)})
	}

	switch notice.Level {
	case usage.LevelWarn:
		content.Subject = "You are at 70% of your monthly volume"
		content.Heading = "You are growing"
		content.Body = []string{
			"You have used " + used + " of the " + limit + " pageviews and custom events included in your plan this month.",
			"Nothing happens at the limit — we keep collecting, your dashboard keeps working, and your bill does not change. We are telling you now, while there is still most of a month left, because there is usually a plan that fits better once a site is at this volume.",
			"If you would rather not hear from us again this month, ignore this — there are two more of these, at 85% and at 100%, and then nothing.",
		}

	case usage.LevelNear:
		content.Subject = "You are at 85% of your monthly volume"
		content.Heading = "You are at 85% of your plan"
		content.Body = []string{
			"You have used " + used + " of " + limit + " this month.",
			projectionSentence(notice),
			"Still nothing to do. We do not throttle, we do not drop events, and we do not add overage charges without asking you first.",
		}

	case usage.LevelReached:
		content.Subject = "You have reached your monthly volume"
		content.Heading = "You have reached the volume included in your plan"
		content.Body = []string{
			"You have used " + used + " pageviews and custom events this month, which is everything your plan includes.",
			"We are still collecting. Your dashboard is still open. Your bill has not changed. One month over does nothing at all.",
			"If this becomes the normal shape of your traffic, let us talk — that is a better outcome for both of us than either of us discovering it on an invoice.",
		}

	case usage.LevelSecondMonth:
		// This email is about the two finished months the rule counted. The
		// month in progress stays out of it: it goes out in the first hour of
		// a month, when that count is a few thousand, and printed beside the
		// limit it reads as if the account were nowhere near it.
		if len(notice.Over) == 0 {
			return Content{}, fmt.Errorf("mail: the second-month email for team %d has no months to name", notice.TeamID)
		}

		content.Facts = nil
		for _, month := range notice.Over {
			content.Facts = append(content.Facts, Fact{Label: monthName(month.Period), Value: thousands(month.Counts.Billable())})
		}
		content.Facts = append(content.Facts, Fact{Label: "Included each month", Value: limit})

		content.Subject = "Two months over your plan. Can we talk by " + day(notice.Deadline) + "?"
		content.Heading = "Two months over your plan"
		content.Body = []string{
			"You used " + overSentence(notice.Over) + ". Your plan includes " + limit + " a month.",
			"We'd like to move you onto a plan that fits. Reply to this email, or write to " + notice.SalesEmail + ", any time before " + day(notice.Deadline) + ".",
			"If we haven't heard from you by then, your dashboard will lock until we do. We keep collecting every event the whole time, your settings and exports stay open, and nothing is deleted.",
			"It unlocks immediately when you reply, or as soon as a full month finishes under the plan.",
		}

	default:
		return Content{}, fmt.Errorf("mail: no copy for usage level %q", notice.Level)
	}

	return content, nil
}

// projectionSentence names the month-end estimate, or says nothing when the
// month is too young for one to mean anything. A projection from two hours of
// data is noise, and printing it as a number would make it look like a fact.
func projectionSentence(notice usage.Notice) string {
	if notice.Projected <= 0 {
		return "At this point in the month that is worth knowing about, but it is too early to say where you will finish."
	}

	return "At the current rate you will finish the month at around " + thousands(notice.Projected) + "."
}

// overSentence lists each finished month's count, as in "1,321,002 pageviews
// and custom events in September and 1,649,270 in October". The unit is named
// once, on the first month, so the sentence stays short.
func overSentence(months []usage.PeriodCounts) string {
	parts := make([]string, 0, len(months))

	for i, month := range months {
		unit := " in "
		if i == 0 {
			unit = " pageviews and custom events in "
		}

		parts = append(parts, thousands(month.Counts.Billable())+unit+monthName(month.Period))
	}

	if len(parts) == 1 {
		return parts[0]
	}

	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// monthName turns a billing period like "2026-10" into "October". A period
// that does not parse is printed as it is, which is still the right month.
func monthName(period string) string {
	at, err := time.Parse("2006-01", period)
	if err != nil {
		return period
	}

	return at.Format("January")
}

// thousands formats a count with separators. Seven-digit numbers are the whole
// subject of these emails, and "1000000" is genuinely hard to read at a glance.
func thousands(value int64) string {
	digits := strconv.FormatInt(value, 10)

	negative := ""
	if len(digits) > 0 && digits[0] == '-' {
		negative, digits = "-", digits[1:]
	}

	var out []byte
	for i, r := range []byte(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, r)
	}

	return negative + string(out)
}
