//
// lifecycle.go
// The copy for the ten lifecycle emails, in both the trial and the dunning voice.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"context"
	"fmt"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
)

// Three rules govern every message below, and the test suite asserts all three
// over the whole set rather than trusting a reviewer to notice a lapse:
//
//  1. Every email names the exact date the next thing happens. Nobody should
//     ever have to work out when their data disappears.
//  2. Every email carries a one-click upgrade link, and the dunning path also
//     carries a direct link to update a card, because that is the actual fix.
//  3. No dark patterns and no fake urgency. The deadlines are real, they are
//     stated plainly once, and there is no countdown, no "act now", and no
//     hidden unsubscribe.

// LifecycleMailer turns a lifecycle notice into a sent message. It is the
// bridge between the state machine, which knows the dates, and the transport,
// which knows whether anything left the building.
type LifecycleMailer struct {
	Sender Sender
}

// NewLifecycleMailer builds the notifier the sweeper drives.
func NewLifecycleMailer(sender Sender) *LifecycleMailer {
	return &LifecycleMailer{Sender: sender}
}

// Notify renders and sends one lifecycle email, returning what the transport
// observed. The transport's own answer is passed back rather than a bare nil,
// because the caller writes it to the row that proves the customer was warned.
func (m *LifecycleMailer) Notify(ctx context.Context, notice lifecycle.Notice) (string, error) {
	content, err := LifecycleContent(notice)
	if err != nil {
		return "", err
	}

	msg, err := content.Message(notice.To, notice.Template)
	if err != nil {
		return "", err
	}
	msg.MessageID = notice.MessageKey

	result, err := m.Sender.Send(ctx, msg)
	if err != nil {
		return result.String(), err
	}

	// A transport that returns no error but did not accept the message is the
	// exact failure this product refuses to have: it would be recorded as a
	// warning the customer never received.
	if !result.Accepted {
		return result.String(), fmt.Errorf("mail: %s for team %d was not accepted: %s", notice.Template, notice.TeamID, result.Detail)
	}

	return result.String(), nil
}

// LifecycleContent is the copy table. Every one of the ten templates appears
// once with both voices side by side, so that a change to the trial wording and
// its dunning twin is one edit in one place — which is the only way they stay
// consistent about the same dates.
func LifecycleContent(notice lifecycle.Notice) (Content, error) {
	lapse := notice.Trigger == lifecycle.TriggerLapse

	content := Content{
		Facts:   lifecycleFacts(notice),
		Primary: Button{Label: "Upgrade — $9.99/month or $100/year", URL: notice.UpgradeURL},
		Closing: "Questions, or want to talk about a plan that fits better? Reply to this email and a person will answer.",
	}

	// The dunning path gets the card-update link beside every upgrade link. A
	// lapsed subscription is usually an expired card rather than a decision,
	// and sending somebody to a pricing page to fix that wastes their time.
	if lapse && notice.PortalURL != "" {
		content.Secondary = append(content.Secondary, Button{Label: "Update your card", URL: notice.PortalURL})
	}

	if notice.ExportURL != "" {
		content.Secondary = append(content.Secondary, Button{Label: "Download your data", URL: notice.ExportURL})
	}

	switch notice.Template {
	case lifecycle.TemplateEndingSoon:
		if lapse {
			content.Subject = "We could not charge your card — your dashboard locks on " + day(notice.LocksAt)
			content.Heading = "Your last payment did not go through"
			content.Body = []string{
				"We have not been able to charge the card on your account. It is almost always an expired card rather than anything you did.",
				"Nothing has changed yet. Your dashboard works, and we are still collecting every event from your sites. On " + day(notice.LocksAt) + " the dashboard locks, and we keep collecting for another thirty days after that so nothing is missing when you come back.",
				"Updating the card fixes it immediately.",
			}
		} else {
			content.Subject = "Your trial ends on " + day(notice.LocksAt)
			content.Heading = "Seven days left on your trial"
			content.Body = []string{
				"Your trial of feasible.lol ends on " + day(notice.LocksAt) + ". Everything works normally until then.",
				"It is $9.99 a month, or $100 a year. One price, every feature, unlimited sites, unlimited team members, unlimited retention, and a million pageviews and custom events a month.",
				"If you do not upgrade, the dashboard locks on " + day(notice.LocksAt) + " — but we keep collecting your data for another thirty days, so paying any time before " + day(notice.StopsAt) + " brings back a complete history with no gap in it.",
			}
		}

	case lifecycle.TemplateEndingTomorrow:
		if lapse {
			content.Subject = "Your dashboard locks tomorrow, " + day(notice.LocksAt)
			content.Heading = "Your dashboard locks tomorrow"
			content.Body = []string{
				"We still have not been able to charge your card, so your dashboard locks tomorrow, " + day(notice.LocksAt) + ".",
				"We keep collecting your events the whole time it is locked. Nothing is lost, and nothing on your website changes — the tracking script keeps working exactly as it does now.",
				"Update your card and everything comes straight back.",
			}
		} else {
			content.Subject = "Your trial ends tomorrow, " + day(notice.LocksAt)
			content.Heading = "Your trial ends tomorrow"
			content.Body = []string{
				"This is the last reminder before anything changes. Your trial ends tomorrow, " + day(notice.LocksAt) + ".",
				"When it does, the dashboard locks. We keep collecting your events for another thirty days, until " + day(notice.StopsAt) + ", so upgrading any time before then gives you back a complete history rather than one with a hole in it.",
				"Your export stays available the whole time, whether you upgrade or not.",
			}
		}

	case lifecycle.TemplateDashboardLocked:
		content.Subject = "Your dashboard is locked — we are still collecting until " + day(notice.StopsAt)
		content.Heading = "Your dashboard is locked. Nothing is lost."
		content.Body = []string{
			lockedOpening(lapse),
			"We are still collecting your data. Every pageview and event from your sites is being recorded right now, and it will be there in full the moment you come back. Upgrade any time before " + day(notice.StopsAt) + " and it all comes back with no gap.",
			"On " + day(notice.StopsAt) + " we stop collecting, and a real gap starts. We will email you before that happens, and again before anything is deleted on " + day(notice.DeletesAt) + ".",
			"Your export works throughout. It is your data.",
		}

	case lifecycle.TemplateCollectionStopsIn15:
		content.Subject = "Fifteen days until we stop collecting, on " + day(notice.StopsAt)
		content.Heading = "We stop collecting on " + day(notice.StopsAt)
		content.Body = []string{
			"Your dashboard has been locked since " + day(notice.LocksAt) + ", but we have kept recording everything your sites send us.",
			"That ends on " + day(notice.StopsAt) + ". After that date there will be a genuine gap in your history — the days we did not collect are days nobody can reconstruct later.",
			"Everything already recorded stays safe until " + day(notice.DeletesAt) + ".",
		}

	case lifecycle.TemplateCollectionStopsTomorrow:
		content.Subject = "We stop collecting tomorrow, " + day(notice.StopsAt)
		content.Heading = "We stop collecting tomorrow"
		content.Body = []string{
			"Tomorrow, " + day(notice.StopsAt) + ", we stop recording events from your sites.",
			"This is the last day a gap can still be avoided. Everything collected so far stays safe until " + day(notice.DeletesAt) + ", and you can download all of it at any time.",
			"Your tracking script will keep running and your website will not break — we will simply stop counting.",
		}

	case lifecycle.TemplateCollectionStopped:
		content.Subject = "We have stopped collecting — your data is safe until " + day(notice.DeletesAt)
		content.Heading = "We have stopped collecting"
		content.Body = []string{
			"As of today we are no longer recording events from your sites. Your site itself is unaffected; the tracking script keeps working and returns normally.",
			"Your existing data is safe until " + day(notice.DeletesAt) + ". On that date the hourly sweep removes it from live systems immediately, and payment-provider deletion retries hourly until complete.",
			"If you come back before then, everything you had is still here. The days between today and the day you return will show on your graphs as a labelled gap, not as zeroes — we would rather say we were not counting than pretend nobody visited.",
		}

	case lifecycle.TemplateDeletionIn15:
		content.Subject = "Fifteen days until your data is deleted, on " + day(notice.DeletesAt)
		content.Heading = "Your live data is removed on " + day(notice.DeletesAt)
		content.Body = []string{
			"On " + day(notice.DeletesAt) + " we delete the live database holding your analytics and your account records, and request deletion of your customer record with our payment provider.",
			"Storage and recovery systems outside this application follow their operators' retention controls and are not used to reactivate a deleted account. A failed payment-provider deletion is retried hourly.",
			"You can download everything we hold at any time before then, and you can bring the account back simply by paying.",
		}

	case lifecycle.TemplateDeletionIn5:
		content.Subject = "Five days until your data is deleted, on " + day(notice.DeletesAt)
		content.Heading = "Five days until deletion"
		content.Body = []string{
			"On " + day(notice.DeletesAt) + " the hourly sweep immediately removes your account and analytics from live systems, and payment-provider deletion is retried hourly until it succeeds.",
			"If you want to keep the history without keeping the account, download it now — the link below gives you everything we hold in a portable format, and it works whether or not you ever pay us again.",
		}

	case lifecycle.TemplateDeletionTomorrow:
		content.Subject = "We delete your account tomorrow, " + day(notice.DeletesAt)
		content.Heading = "We delete your account tomorrow"
		content.Body = []string{
			"This is the final notice. Tomorrow, " + day(notice.DeletesAt) + ", we delete your live analytics database and account records and request deletion of your customer record with our payment provider.",
			"Download everything below if you want to keep it. Paying before tomorrow stops the deletion and restores full access immediately.",
			"Storage and recovery systems outside this application follow their operators' retention controls and are not used to reactivate a deleted account. Payment-provider deletion is retried on every hourly lifecycle sweep until it succeeds.",
		}

	case lifecycle.TemplateAccountDeleted:
		content.Subject = "Your feasible.lol account has been deleted"
		content.Heading = "Your account has been deleted"
		content.Body = []string{
			"As we said we would, we have deleted your account today.",
			"What we deleted: the database holding every pageview, event and session for your sites; your account, team, site and API-key records; and your customer record with our payment provider, including the stored card.",
			"Storage and recovery systems outside this application follow their operators' retention controls and are not used to reactivate the account.",
			"After this message is accepted, its destination address and your team name are erased from the deletion record. We keep a minimal record of the internal account id and deletion timestamps, plus invoices for as long as tax law requires.",
			"You are very welcome back. Signing up again starts a fresh account with a fresh trial; deleted account data is never used to reactivate the account.",
		}
		content.Facts = nil
		content.Secondary = nil
		content.Primary = Button{Label: "Start again", URL: notice.UpgradeURL}

	default:
		return Content{}, fmt.Errorf("mail: no copy for lifecycle template %q", notice.Template)
	}

	return content, nil
}

// lockedOpening picks the first sentence of the day-30 email. It is the one
// place the two paths genuinely have to say different things — "your trial
// ended" and "we could not charge you" are not interchangeable, and telling
// somebody their trial ended when their card expired is how a support ticket
// starts.
func lockedOpening(lapse bool) string {
	if lapse {
		return "We were not able to charge your card, so your dashboard is now locked."
	}

	return "Your trial has ended, so your dashboard is now locked."
}

// lifecycleFacts builds the three-date summary. All three dates appear in every
// message, in past or future tense as appropriate, because somebody deciding
// whether to pay wants the whole timetable rather than only the next step.
func lifecycleFacts(notice lifecycle.Notice) []Fact {
	lockLabel := "Dashboard locks"
	if notice.Day >= lifecycle.GraceDays {
		lockLabel = "Dashboard locked"
	}

	stopLabel := "We stop collecting"
	if notice.Day >= lifecycle.LockedDays {
		stopLabel = "We stopped collecting"
	}

	return []Fact{
		{Label: lockLabel, Value: day(notice.LocksAt)},
		{Label: stopLabel, Value: day(notice.StopsAt)},
		{Label: "Live account deletion", Value: day(notice.DeletesAt)},
	}
}
