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

	msg, err := content.Message(notice.To, "usage_"+string(notice.Level))
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

	case "second_month":
		content.Subject = "Two months over your plan — can we talk by " + day(notice.Deadline) + "?"
		content.Heading = "Two months running, above your plan"
		content.Body = []string{
			"You have now been over the volume included in your plan for two consecutive months — " + used + " this month against " + limit + " included.",
			"We would like to move you onto something that fits. Reply to this email, or write to " + notice.SalesEmail + ", any time before " + day(notice.Deadline) + ".",
			"If we have not heard from you by then, your dashboard will lock until we do. We will keep collecting every event the whole time, your settings and exports stay open, and the moment your usage is back inside the plan — or you reply — it unlocks immediately. Nothing is deleted and no data is lost.",
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
