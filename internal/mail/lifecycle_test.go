//
// lifecycle_test.go
// Every lifecycle email, checked for the four things each one has to carry.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
)

// start is a fixed clock so a failure reproduces exactly.
var start = time.Date(2026, time.March, 3, 9, 0, 0, 0, time.UTC)

// noticeFor builds the notice the sweeper would produce for one template, using
// the state machine's own boundaries so the dates under test are the real ones.
func noticeFor(template string, trigger lifecycle.Trigger, day int) lifecycle.Notice {
	state := lifecycle.State{Trigger: trigger, StartedAt: start}

	notice := lifecycle.Notice{
		TeamID:     1,
		TeamName:   "Example Co",
		To:         "owner@example.com",
		Template:   template,
		Trigger:    trigger,
		Phase:      state.At(start.Add(time.Duration(day) * lifecycle.Day)),
		Day:        day,
		LocksAt:    state.Boundary(lifecycle.PhaseLocked),
		StopsAt:    state.Boundary(lifecycle.PhaseDormant),
		DeletesAt:  state.Boundary(lifecycle.PhaseDeleted),
		UpgradeURL: "https://feasible.lol/billing/upgrade",
		ExportURL:  "https://feasible.lol/billing/export",
	}

	if trigger == lifecycle.TriggerLapse {
		notice.PortalURL = "https://feasible.lol/billing/portal"
	}

	return notice
}

// TestEveryTemplateRendersInBothVoices walks the whole sequence twice and checks
// the four promises the specification makes about every message: it names the
// exact date the next thing happens, it carries a one-click upgrade link, it
// carries the postal address, and no line in it is long enough to lose the
// message.
func TestEveryTemplateRendersInBothVoices(t *testing.T) {
	for _, trigger := range []lifecycle.Trigger{lifecycle.TriggerTrial, lifecycle.TriggerLapse} {
		for _, entry := range lifecycle.Sequence {
			notice := noticeFor(entry.Template, trigger, entry.Day)

			content, err := LifecycleContent(notice)
			if err != nil {
				t.Fatalf("%s/%s: %v", trigger, entry.Template, err)
			}

			msg, err := content.Message(notice.To, entry.Template)
			if err != nil {
				t.Fatalf("%s/%s: %v", trigger, entry.Template, err)
			}

			if strings.TrimSpace(msg.Subject) == "" {
				t.Errorf("%s/%s has no subject", trigger, entry.Template)
			}

			if !strings.Contains(msg.HTML, notice.UpgradeURL) {
				t.Errorf("%s/%s has no upgrade link", trigger, entry.Template)
			}
			if !strings.Contains(msg.Text, notice.UpgradeURL) {
				t.Errorf("%s/%s has no upgrade link in the text part", trigger, entry.Template)
			}

			// CAN-SPAM requires a physical postal address in marketing email.
			// Transactional mail is exempt and carries it anyway, because
			// arguing about which category a dunning notice falls into costs
			// more than four lines.
			for _, line := range []string{"Cloudmanic Labs, LLC", "901 Brutscher Street, D112", "Newberg, OR 97132"} {
				if !strings.Contains(msg.HTML, line) {
					t.Errorf("%s/%s is missing %q from the footer", trigger, entry.Template, line)
				}
				if !strings.Contains(msg.Text, line) {
					t.Errorf("%s/%s is missing %q from the text footer", trigger, entry.Template, line)
				}
			}

			wrapped := Wrap(msg.HTML, MaxLineLength)
			if longest := LongestLine(wrapped); longest > MaxLineLength {
				t.Errorf("%s/%s has a %d byte line", trigger, entry.Template, longest)
			}
		}
	}
}

// TestEveryWarningNamesItsDate is the "nobody is ever surprised" promise,
// checked message by message. The deletion confirmation is excluded because by
// the time it is sent there is no next date — that is the point of it.
func TestEveryWarningNamesItsDate(t *testing.T) {
	for _, trigger := range []lifecycle.Trigger{lifecycle.TriggerTrial, lifecycle.TriggerLapse} {
		for _, entry := range lifecycle.Sequence {
			if entry.Template == lifecycle.TemplateAccountDeleted {
				continue
			}

			notice := noticeFor(entry.Template, trigger, entry.Day)

			content, err := LifecycleContent(notice)
			if err != nil {
				t.Fatal(err)
			}

			state := lifecycle.State{Trigger: trigger, StartedAt: start}
			announced := day(state.Boundary(entry.Announces))

			body := strings.Join(append(content.Body, content.Subject, content.Heading), " ")

			if !strings.Contains(body, announced) {
				t.Errorf("%s/%s never names %s, the date it announces", trigger, entry.Template, announced)
			}
		}
	}
}

// TestDunningCarriesTheCardLink is the difference between the two voices that
// actually matters. A lapsed subscription is usually an expired card, and
// sending somebody to a pricing page to fix that wastes their time.
func TestDunningCarriesTheCardLink(t *testing.T) {
	for _, entry := range lifecycle.Sequence {
		if entry.Template == lifecycle.TemplateAccountDeleted {
			continue
		}

		lapse, err := LifecycleContent(noticeFor(entry.Template, lifecycle.TriggerLapse, entry.Day))
		if err != nil {
			t.Fatal(err)
		}

		found := false
		for _, button := range lapse.Secondary {
			if button.Label == "Update your card" {
				found = true
			}
		}

		if !found {
			t.Errorf("%s on the dunning path has no card-update link", entry.Template)
		}

		// The trial path must not offer one: there is no customer at the payment
		// provider yet, so the link would lead to an error page.
		trial, err := LifecycleContent(noticeFor(entry.Template, lifecycle.TriggerTrial, entry.Day))
		if err != nil {
			t.Fatal(err)
		}

		for _, button := range trial.Secondary {
			if button.Label == "Update your card" {
				t.Errorf("%s on the trial path offers a card-update link", entry.Template)
			}
		}
	}
}

// TestTheTwoVoicesDiffer guards against the dunning path quietly telling
// somebody their trial ended when in fact their card was declined.
func TestTheTwoVoicesDiffer(t *testing.T) {
	trial, err := LifecycleContent(noticeFor(lifecycle.TemplateDashboardLocked, lifecycle.TriggerTrial, 30))
	if err != nil {
		t.Fatal(err)
	}

	lapse, err := LifecycleContent(noticeFor(lifecycle.TemplateDashboardLocked, lifecycle.TriggerLapse, 30))
	if err != nil {
		t.Fatal(err)
	}

	if trial.Body[0] == lapse.Body[0] {
		t.Fatal("the trial and dunning day-30 emails open with the same sentence")
	}

	if !strings.Contains(trial.Body[0], "trial") {
		t.Errorf("the trial email does not mention a trial: %q", trial.Body[0])
	}
	if !strings.Contains(lapse.Body[0], "card") {
		t.Errorf("the dunning email does not mention a card: %q", lapse.Body[0])
	}
}

// TestNoFakeUrgency checks the copy for the patterns the specification rules
// out. It is a blunt instrument, and it is worth having: this kind of language
// arrives one small edit at a time.
func TestNoFakeUrgency(t *testing.T) {
	banned := []string{"act now", "hurry", "last chance", "don't miss", "limited time", "expires soon", "only a few"}

	for _, trigger := range []lifecycle.Trigger{lifecycle.TriggerTrial, lifecycle.TriggerLapse} {
		for _, entry := range lifecycle.Sequence {
			content, err := LifecycleContent(noticeFor(entry.Template, trigger, entry.Day))
			if err != nil {
				t.Fatal(err)
			}

			body := strings.ToLower(strings.Join(append(content.Body, content.Subject, content.Heading), " "))

			for _, phrase := range banned {
				if strings.Contains(body, phrase) {
					t.Errorf("%s/%s uses %q", trigger, entry.Template, phrase)
				}
			}
		}
	}
}

// TestUnknownTemplateIsAnError makes sure a typo in a template name fails loudly
// rather than sending a blank message.
func TestUnknownTemplateIsAnError(t *testing.T) {
	if _, err := LifecycleContent(noticeFor("not_a_template", lifecycle.TriggerTrial, 1)); err == nil {
		t.Fatal("an unknown template rendered")
	}
}

// TestNotifyReportsTheTransportResult is the second mail trap: a send function
// returning without an error is not delivery. A transport that declines the
// message must produce an error here rather than a recorded success.
func TestNotifyReportsTheTransportResult(t *testing.T) {
	mailer := NewLifecycleMailer(senderFunc(func(context.Context, Message) (Result, error) {
		return Result{Transport: "test", Accepted: false, Detail: "relay said no"}, nil
	}))

	_, err := mailer.Notify(context.Background(), noticeFor(lifecycle.TemplateDeletionTomorrow, lifecycle.TriggerLapse, 89))
	if err == nil {
		t.Fatal("a message the transport did not accept was reported as sent")
	}
	if !strings.Contains(err.Error(), "relay said no") {
		t.Errorf("the error does not carry the transport's answer: %v", err)
	}
}

// TestNotifyPassesTheTransportDetailBack checks the success path records what
// the transport said, which is what makes "were they warned" answerable later.
func TestNotifyPassesTheTransportDetailBack(t *testing.T) {
	mailer := NewLifecycleMailer(senderFunc(func(context.Context, Message) (Result, error) {
		return Result{Transport: "smtp", Accepted: true, Detail: "accepted by relay:587"}, nil
	}))

	outcome, err := mailer.Notify(context.Background(), noticeFor(lifecycle.TemplateEndingSoon, lifecycle.TriggerTrial, 23))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outcome, "accepted by relay:587") {
		t.Errorf("the outcome is %q", outcome)
	}
}

// senderFunc adapts a function to the Sender interface.
type senderFunc func(ctx context.Context, msg Message) (Result, error)

// Send calls the function.
func (f senderFunc) Send(ctx context.Context, msg Message) (Result, error) {
	return f(ctx, msg)
}
