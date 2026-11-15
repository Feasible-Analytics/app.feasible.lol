//
// onboarding_test.go
// The four onboarding emails: what each says, and what none may say.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"context"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/onboarding"
)

// onboardingNotice is an account with a site and nothing counted.
func onboardingNotice(template string) onboarding.Notice {
	return onboarding.Notice{
		TeamID:     7,
		Template:   template,
		To:         "sam@example.com",
		FirstName:  "Sam",
		MessageKey: "lifecycle-7-1-" + template,
		SiteDomain: "harbor.my",
		InstallURL: "https://app.example/onboarding/3",
		AddSiteURL: "https://app.example/sites/new",
		StopURL:    "https://app.example/unsubscribe/token",
	}
}

// TestEveryOnboardingEmailIsSignedReplyableAndStoppable is the set of promises
// all four make: a person signs it, a reply reaches that person, and one click
// stops the rest.
func TestEveryOnboardingEmailIsSignedReplyableAndStoppable(t *testing.T) {
	for _, template := range onboarding.Templates() {
		content, err := OnboardingContent(onboardingNotice(template))
		if err != nil {
			t.Fatalf("%s: %v", template, err)
		}

		message, err := content.Message("sam@example.com", template)
		if err != nil {
			t.Fatalf("%s: %v", template, err)
		}

		if message.ReplyTo != FounderAddress {
			t.Errorf("%s replies to %q", template, message.ReplyTo)
		}

		if message.Unsubscribe != "https://app.example/unsubscribe/token" {
			t.Errorf("%s carries the stop link %q", template, message.Unsubscribe)
		}

		for _, body := range []string{message.HTML, message.Text} {
			for _, want := range []string{"Hi Sam,", "~Spicer", "Stop these setup emails", "https://app.example/unsubscribe/token", "eply to this email"} {
				if !strings.Contains(body, want) {
					t.Errorf("%s is missing %q", template, want)
				}
			}
		}

		if !strings.Contains(message.Text, "Open the install screen: https://app.example/onboarding/3") {
			t.Errorf("%s does not link the install screen", template)
		}
	}
}

// TestOnboardingCopyFollowsTheHouseRules holds the words to the rules a person
// would be held to: no em dashes, no exclamation marks, and no pointer at an
// add-on that is not published.
func TestOnboardingCopyFollowsTheHouseRules(t *testing.T) {
	for _, template := range onboarding.Templates() {
		for _, site := range []string{"harbor.my", ""} {
			notice := onboardingNotice(template)
			notice.SiteDomain = site

			content, err := OnboardingContent(notice)
			if err != nil {
				t.Fatalf("%s: %v", template, err)
			}

			text := content.Subject + "\n" + content.Preheader + "\n" + content.Text()

			for _, banned := range []string{"—", "–", "!", "wordpress.org", "npmjs", "npm install", "tagmanager.google.com"} {
				if strings.Contains(text, banned) {
					t.Errorf("%s (site %q) contains %q", template, site, banned)
				}
			}
		}
	}
}

// TestTheFourEmailsAreDifferent keeps the sequence from repeating itself.
func TestTheFourEmailsAreDifferent(t *testing.T) {
	subjects := map[string]string{}

	for _, template := range onboarding.Templates() {
		content, err := OnboardingContent(onboardingNotice(template))
		if err != nil {
			t.Fatal(err)
		}

		if earlier, seen := subjects[content.Subject]; seen {
			t.Errorf("%s and %s share the subject %q", earlier, template, content.Subject)
		}
		subjects[content.Subject] = template
	}

	for template, wants := range map[string][]string{
		onboarding.TemplateWelcome:    {"Add your site", "two-line snippet", "Check my installation", "https://feasible.lol/integrations/wordpress/", "https://feasible.lol/docs/mcp/"},
		onboarding.TemplateNoDataDay1: {"harbor.my", "ad blocker", "Content-Security-Policy", "cached page", "wrong field"},
		onboarding.TemplateNoDataDay3: {"harbor.my", "https://feasible.lol/integrations/shopify/", "https://feasible.lol/integrations/", "WPCode"},
		onboarding.TemplateNoDataDay7: {"harbor.my", "Reply to this email with your site's address and we'll take a look", "last email about setup"},
	} {
		content, err := OnboardingContent(onboardingNotice(template))
		if err != nil {
			t.Fatal(err)
		}

		text := content.Text()
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s is missing %q", template, want)
			}
		}
	}
}

// TestAnAccountWithNoSiteIsNotToldAboutOne covers the owner who never added a
// site: no domain to name, and the button adds one.
func TestAnAccountWithNoSiteIsNotToldAboutOne(t *testing.T) {
	for _, template := range onboarding.Templates() {
		notice := onboardingNotice(template)
		notice.SiteDomain = ""
		notice.InstallURL = ""
		notice.FirstName = ""

		content, err := OnboardingContent(notice)
		if err != nil {
			t.Fatal(err)
		}

		text := content.Text()

		if strings.Contains(text, "harbor.my") || strings.Contains(text, "/onboarding/") {
			t.Errorf("%s names a site the account does not have", template)
		}

		if !strings.Contains(text, "Add your site: https://app.example/sites/new") {
			t.Errorf("%s does not offer to add a site", template)
		}

		if !strings.HasPrefix(content.Body[0], "Hi,") {
			t.Errorf("%s greets a nameless owner with %q", template, content.Body[0])
		}
	}
}

// TestTheOnboardingMailerSendsUnderTheOutboxKey checks the message handed to
// the transport, and that a declined send is an error.
func TestTheOnboardingMailerSendsUnderTheOutboxKey(t *testing.T) {
	capture := &captureSender{}
	mailer := NewOnboardingMailer(capture)

	notice := onboardingNotice(onboarding.TemplateWelcome)

	outcome, err := mailer.Notify(context.Background(), notice)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outcome, "accepted") {
		t.Fatalf("outcome = %q", outcome)
	}

	sent := capture.messages[0]
	if sent.To != "sam@example.com" || sent.Tag != onboarding.TemplateWelcome || sent.MessageID != notice.MessageKey {
		t.Fatalf("sent = to %q, tag %q, id %q", sent.To, sent.Tag, sent.MessageID)
	}

	capture.decline = true
	if _, err := mailer.Notify(context.Background(), notice); err == nil {
		t.Fatal("a declined message was reported as sent")
	}

	notice.Template = "onboarding_unknown"
	if _, err := mailer.Notify(context.Background(), notice); err == nil {
		t.Fatal("an unknown template was sent")
	}
}

// captureSender records what it is handed, and can decline it.
type captureSender struct {
	messages []Message
	decline  bool
}

// Send keeps the message.
func (c *captureSender) Send(_ context.Context, msg Message) (Result, error) {
	c.messages = append(c.messages, msg)

	return Result{Transport: "test", Accepted: !c.decline, Detail: "captured"}, nil
}
