//
// onboarding.go
// The copy for the welcome and the three install-help emails.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"context"
	"fmt"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/onboarding"
)

// Where the onboarding emails point. These are pages of the hosted product's
// own site, and the emails are sent only by the hosted service.
const (
	docsInstallGuides = "https://feasible.lol/integrations/"
	docsInstallation  = "https://feasible.lol/docs/installation/"
	docsProxying      = "https://feasible.lol/docs/proxying/"
	docsAssistant     = "https://feasible.lol/docs/mcp/"
	docsGoals         = "https://feasible.lol/docs/goals-funnels/"
	helpInvite        = "https://feasible.lol/help/how-do-i-invite-someone-to-my-team/"
)

// OnboardingMailer turns an onboarding notice into a sent message.
type OnboardingMailer struct {
	Sender Sender
}

// NewOnboardingMailer builds the notifier the onboarding sweeper drives.
func NewOnboardingMailer(sender Sender) *OnboardingMailer {
	return &OnboardingMailer{Sender: sender}
}

// Notify renders and sends one onboarding email, returning what the transport
// observed so the caller can write it to the sent marker.
func (m *OnboardingMailer) Notify(ctx context.Context, notice onboarding.Notice) (string, error) {
	content, err := OnboardingContent(notice)
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

	if !result.Accepted {
		return result.String(), fmt.Errorf("mail: %s for team %d was not accepted: %s", notice.Template, notice.TeamID, result.Detail)
	}

	return result.String(), nil
}

// OnboardingContent is the copy table for the four onboarding emails. Each one
// reads differently for an account that has added a site and one that has not,
// because "we have not seen example.com" means nothing to somebody who never
// added it. The welcome has a third version for an account that was already
// counting when it went out.
func OnboardingContent(notice onboarding.Notice) (Content, error) {
	hasSite := notice.SiteDomain != ""

	content := Content{
		Signature:        []string{"Best,", "~Spicer"},
		Closing:          "Stuck? Reply to this email and we'll help.",
		ReplyTo:          FounderAddress,
		Unsubscribe:      notice.StopURL,
		UnsubscribeLabel: "Stop these setup emails",
		Primary:          Button{Label: "Add your site", URL: notice.AddSiteURL},
	}
	if hasSite {
		content.Primary = Button{Label: "Open the install screen", URL: notice.InstallURL}
	}

	greeting := "Hi,"
	if notice.FirstName != "" {
		greeting = "Hi " + notice.FirstName + ","
	}

	switch notice.Template {
	case onboarding.TemplateWelcome:
		if hasSite && notice.Collecting {
			welcomeCollecting(&content, notice, greeting)
			break
		}

		content.Subject = "Three steps to your first pageview"
		content.Heading = "Welcome to Feasible"
		content.Preheader = "Add your site, paste the snippet, and press Check my installation."
		first := "1. Add your site."
		if hasSite {
			first = "1. Add your site. You've done that for " + notice.SiteDomain + "."
		}
		content.Body = []string{
			greeting,
			"I'm Spicer. I built Feasible. Nothing shows up in your dashboard until the snippet is on your site, so that's the one thing to do today. It takes about five minutes.",
			first,
			"2. Paste the two-line snippet into your site's <head>.",
			"3. Press \"Check my installation\". It fetches your page and tells you whether the snippet is there.",
			"Every platform keeps its <head> box somewhere different. These guides name the exact field:",
		}
		content.Links = []Button{
			{Label: "WordPress", URL: docsInstallGuides + "wordpress/"},
			{Label: "Shopify", URL: docsInstallGuides + "shopify/"},
			{Label: "Webflow", URL: docsInstallGuides + "webflow/"},
			{Label: "Squarespace", URL: docsInstallGuides + "squarespace/"},
			{Label: "Next.js", URL: docsInstallGuides + "nextjs/"},
			{Label: "Every other platform", URL: docsInstallGuides},
		}
		content.After = []string{
			"Once it's counting, you can connect Claude, ChatGPT, Cursor or VS Code to your numbers by pasting one URL: " + docsAssistant,
		}

	case onboarding.TemplateNoDataDay1:
		content.Subject = "No pageviews yet. The four usual reasons"
		content.Heading = "Nothing has arrived yet"
		content.Preheader = "The snippet's place, your ad blocker, a content security policy, or a cached page."
		opening := "You haven't added a site yet, so there's nothing for Feasible to count. That's the first step. Once the snippet is pasted, these are the four things that most often stop the first pageview."
		if hasSite {
			opening = "Feasible hasn't counted a pageview from " + notice.SiteDomain + " yet. It's nearly always one of these four."
		}
		content.Body = []string{
			greeting,
			opening,
			"The snippet isn't on the page. Open your site, view the page source and search for \"feasible\". If it isn't there, it went into the wrong field, or into a template your live pages don't use. Both lines belong in the <head>.",
			"Your own ad blocker is hiding your visit. Blockers stop analytics scripts, so your test visit never reaches us. Try a private window with extensions off, or your phone.",
			"A content security policy is blocking it. If your site sends a Content-Security-Policy header, it has to allow our script in script-src and our address in connect-src.",
			"You're looking at a cached page. Purge your cache or CDN after pasting. Otherwise you're testing a page built before the snippet existed.",
			"The \"Check my installation\" button on the install screen fetches your page and tells you whether the snippet is there and whether a policy is blocking it.",
		}
		content.Links = []Button{
			{Label: "If nothing arrives at all", URL: docsInstallation + "#if-nothing-arrives-at-all"},
			{Label: "Getting past ad blockers", URL: docsProxying},
		}

	case onboarding.TemplateNoDataDay3:
		content.Subject = "Where the snippet goes on your platform"
		content.Heading = "The install guide for your platform"
		content.Preheader = "WordPress, Shopify, Webflow, Squarespace, Wix, Ghost, Framer, Next.js and the rest."
		opening := "There's still no site on your account. When you add one, the snippet has to go into your platform's <head> field, and every platform keeps that somewhere different. Each of these guides names the exact field and the one thing that platform gets wrong."
		if hasSite {
			opening = "Still no pageviews from " + notice.SiteDomain + ". Most of the time the snippet is in the wrong field, because every platform keeps its <head> box somewhere different. Each of these guides names the exact field and the one thing that platform gets wrong."
		}
		content.Body = []string{greeting, opening}
		content.Links = []Button{
			{Label: "WordPress", URL: docsInstallGuides + "wordpress/"},
			{Label: "Shopify", URL: docsInstallGuides + "shopify/"},
			{Label: "Webflow", URL: docsInstallGuides + "webflow/"},
			{Label: "Squarespace", URL: docsInstallGuides + "squarespace/"},
			{Label: "Wix", URL: docsInstallGuides + "wix/"},
			{Label: "Ghost", URL: docsInstallGuides + "ghost/"},
			{Label: "Framer", URL: docsInstallGuides + "framer/"},
			{Label: "Next.js", URL: docsInstallGuides + "nextjs/"},
			{Label: "Astro", URL: docsInstallGuides + "astro/"},
			{Label: "Every other platform", URL: docsInstallGuides},
		}
		content.After = []string{
			"On WordPress, paste the snippet with a header-scripts plugin such as WPCode. Our own plugin isn't in the WordPress directory yet.",
			"Using a tag manager? The install screen has a second form of the snippet for that, under \"Need the data-domain form?\".",
		}

	case onboarding.TemplateNoDataDay7:
		content.Subject = "Reply with your site's address and we'll take a look"
		content.Heading = "Want us to look at it?"
		content.Preheader = "A week in and nothing has arrived. Send us the address."
		opening := "It's been a week and there's no site on your account yet. If something put you off, reply and tell us what it was."
		ask := "If you're stuck on the install, reply with your site's address and we'll take a look at the page and tell you what's stopping it."
		if hasSite {
			opening = "It's been a week and Feasible still hasn't counted a pageview from " + notice.SiteDomain + "."
			ask = "Reply to this email with your site's address and we'll take a look at the page and tell you what's stopping it."
		}
		content.Body = []string{
			greeting,
			opening,
			ask,
			"If you use a tag manager, a consent tool or a caching plugin, say so. That's usually where the answer is.",
			"This is the last email about setup.",
		}
		content.Closing = ""

	default:
		return Content{}, fmt.Errorf("mail: no copy for onboarding template %q", notice.Template)
	}

	return content, nil
}

// welcomeCollecting writes the welcome for an owner whose site was counting
// before the welcome went out. Telling them the snippet is the one thing to do
// today would describe a job they have already finished, so this version says
// it is done and points at what is worth doing next.
func welcomeCollecting(content *Content, notice onboarding.Notice, greeting string) {
	content.Subject = "Your first pageview is in"
	content.Heading = "Welcome to Feasible"
	content.Preheader = "The install is done. Three things worth five minutes this week."
	content.Primary = Button{Label: "Open your dashboard", URL: notice.DashboardURL}
	content.Closing = "Questions? Reply to this email and we'll help."
	content.Body = []string{
		greeting,
		"I'm Spicer. I built Feasible. Your first pageview from " + notice.SiteDomain + " has already arrived, so the install is done.",
		"Three things worth five minutes this week:",
		"1. Add a goal for the one action that matters most, like a sign-up or a purchase. Downloads, outbound links, forms and missing pages are counted already.",
		"2. Invite a teammate or a client. There's no charge per person.",
		"3. Connect Claude, ChatGPT, Cursor or VS Code by pasting one URL, then ask about your traffic in plain words.",
	}
	content.Links = []Button{
		{Label: "Goals and funnels", URL: docsGoals},
		{Label: "Inviting your team", URL: helpInvite},
		{Label: "Connecting an assistant", URL: docsAssistant},
	}
}
