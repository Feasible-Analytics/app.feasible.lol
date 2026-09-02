//
// site.go
// The three per-site screens: sharing, reports and ingestion health.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settings

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/reports"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sharing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// subscriptionView is one scheduled report as the form renders it.
type subscriptionView struct {
	Kind            string
	Title           string
	Enabled         bool
	RecipientList   string
	SlackWebhookURL string
	NextRun         string
	LastSent        string
}

// alertView is one alert rule as the form renders it.
type alertView struct {
	Kind            string
	Title           string
	Description     string
	Enabled         bool
	Threshold       int
	WindowHours     int
	RecipientList   string
	SlackWebhookURL string
}

// deliveryView is one ledger row.
type deliveryView struct {
	Kind       string
	PeriodKey  string
	Recipients int
	SentAt     string
}

// sharingRoute serves the sharing screen and its forms.
func (h *TeamHandler) sharingRoute(w http.ResponseWriter, r *http.Request, identity Identity, site sites.Site, action string) {
	ctx := r.Context()

	if action != "" {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)

			return
		}

		ownerTeamID, err := h.Teams.TeamIDForSite(ctx, site.ID)
		if err != nil {
			h.forbidden(w, err)

			return
		}
		site.TeamID = ownerTeamID

		// Publishing a dashboard and minting a credential for it are site
		// settings, which is a power an Editor and a Guest Editor have and a
		// Viewer does not.
		if _, err := h.Teams.AuthoriseSite(ctx, site.ID, identity.UserID, teams.PermManageSiteSettings); err != nil {
			h.forbidden(w, err)

			return
		}

		if err := r.ParseForm(); err != nil {
			h.redirect(w, r, sharingPath(site.Domain), "", tr(r, "settings.flash.form_unreadable"))

			return
		}
	}

	base := sharingPath(site.Domain)

	switch action {
	case "public":
		public := r.PostFormValue("public") == "1"

		if err := h.Sharing.SetPublicForOwner(ctx, site.ID, site.TeamID, public); err != nil {
			h.redirect(w, r, base, "", err.Error())

			return
		}

		if err := h.Sites.Refresh(ctx); err != nil && h.Log != nil {
			h.Log.Error("the site cache could not be refreshed", "error", err)
		}

		message := "This dashboard is now private."
		if public {
			message = "This dashboard is public at " + h.BaseURL + "/public/" + site.Domain
		}

		h.redirect(w, r, base, message, "")

		return

	case "links":
		password := r.PostFormValue("password")

		link, err := h.Sharing.CreateLinkForOwner(ctx, site.ID, site.TeamID,
			r.PostFormValue("name"), password, 0, identity.UserID)
		if err != nil {
			h.redirect(w, r, base, "", err.Error())

			return
		}

		message := "Shared link created: " + h.BaseURL + link.Path()
		if link.HasPassword {
			message += " — a password-protected link cannot be embedded."
		}

		h.redirect(w, r, base, message, "")

		return

	case "links/revoke":
		linkID, _ := strconv.ParseInt(r.PostFormValue("link_id"), 10, 64)

		if err := h.Sharing.RevokeLinkForOwner(ctx, site.ID, site.TeamID, linkID); err != nil {
			h.redirect(w, r, base, "", err.Error())

			return
		}

		h.redirect(w, r, base, tr(r, "settings.flash.link_revoked"), "")

		return

	case "":
		// Fall through to the page itself.

	default:
		http.NotFound(w, r)

		return
	}

	links, err := h.Sharing.Links(ctx, site.ID)
	if err != nil {
		h.internal(w, err)

		return
	}

	public, err := h.isPublic(r, site.ID)
	if err != nil {
		h.internal(w, err)

		return
	}

	notice, problem := flash(r)

	h.render(w, r, "sharing", screen{
		TitleID:  "settings.nav.sharing",
		Tab:      "sharing",
		Domain:   site.Domain,
		Message:  notice,
		Error:    problem,
		IsPublic: public,
		Links:    links,
		EmbedURL: h.embedURL(public, site.Domain, links),
		HTTPS:    sharing.NewSecurity(h.BaseURL).RequireHTTPS,
	})
}

// isPublic reads the site's public flag.
func (h *TeamHandler) isPublic(r *http.Request, siteID int64) (bool, error) {
	var public int

	err := h.System.QueryRowContext(r.Context(), `SELECT is_public FROM sites WHERE id = ?`, siteID).Scan(&public)

	return public != 0, err
}

// embedURL picks the URL the snippet should use.
//
// A password-protected link is never offered, because it cannot be embedded at
// all — handing somebody a snippet built from one would produce a frame that
// renders a refusal page, which is a worse way to learn the rule than reading
// it on this screen.
func (h *TeamHandler) embedURL(public bool, domain string, links []sharing.Link) string {
	for _, link := range links {
		if link.Embeddable() {
			return h.BaseURL + link.Path()
		}
	}

	if public {
		return h.BaseURL + sharing.PublicPrefix + domain
	}

	return ""
}

// sharingPath is the sharing screen's URL for one site.
func sharingPath(domain string) string {
	return "/settings/sites/" + domain + "/sharing"
}

// reportsPath is the reports screen's URL for one site.
func reportsPath(domain string) string {
	return "/settings/sites/" + domain + "/reports"
}

// healthPath is the health screen's URL for one site.
func healthPath(domain string) string {
	return "/settings/sites/" + domain + "/health"
}

// reportsRoute serves the reports screen and its forms.
func (h *TeamHandler) reportsRoute(w http.ResponseWriter, r *http.Request, identity Identity, site sites.Site, action string) {
	ctx := r.Context()
	base := reportsPath(site.Domain)

	if action != "" {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)

			return
		}

		ownerTeamID, err := h.Teams.TeamIDForSite(ctx, site.ID)
		if err != nil {
			h.forbidden(w, err)

			return
		}
		site.TeamID = ownerTeamID

		if _, err := h.Teams.AuthoriseSite(ctx, site.ID, identity.UserID, teams.PermManageSiteSettings); err != nil {
			h.forbidden(w, err)

			return
		}

		if err := r.ParseForm(); err != nil {
			h.redirect(w, r, base, "", tr(r, "settings.flash.form_unreadable"))

			return
		}
	}

	switch action {
	case "subscription":
		h.saveSubscription(w, r, site, base)

		return

	case "alert":
		h.saveAlert(w, r, site, base)

		return

	case "":
		// Fall through to the page itself.

	default:
		http.NotFound(w, r)

		return
	}

	notice, problem := flash(r)

	view, err := h.reportsPage(r, site)
	if err != nil {
		h.internal(w, err)

		return
	}

	view.Message, view.Error = notice, problem

	h.render(w, r, "reports", view)
}

// reportsPage builds the reports screen's view model.
func (h *TeamHandler) reportsPage(r *http.Request, site sites.Site) (screen, error) {
	ctx := r.Context()

	subscriptions, err := h.Reports.Subscriptions(ctx, site.ID)
	if err != nil {
		return screen{}, err
	}

	rules, err := h.Reports.AlertRulesFor(ctx, site.ID)
	if err != nil {
		return screen{}, err
	}

	deliveries, err := h.Reports.Deliveries(ctx, site.ID, 20)
	if err != nil {
		return screen{}, err
	}

	byKind := map[string]reports.Subscription{}
	for _, subscription := range subscriptions {
		byKind[subscription.Kind] = subscription
	}

	rulesByKind := map[string]reports.AlertRule{}
	for _, rule := range rules {
		rulesByKind[rule.Kind] = rule
	}

	lastByKind := map[string]int64{}
	for _, delivery := range deliveries {
		if _, seen := lastByKind[delivery.Kind]; !seen && delivery.Recipients > 0 {
			lastByKind[delivery.Kind] = delivery.SentAt
		}
	}

	now := time.Now().UTC()

	view := screen{
		TitleID:  "settings.nav.reports",
		Tab:      "reports",
		Domain:   site.Domain,
		Timezone: site.Timezone,
	}

	for _, kind := range []string{reports.KindWeekly, reports.KindMonthly} {
		subscription := byKind[kind]

		view.Subscriptions = append(view.Subscriptions, subscriptionView{
			Kind:            kind,
			Title:           strings.ToUpper(kind[:1]) + kind[1:],
			Enabled:         subscription.Enabled,
			RecipientList:   strings.Join(subscription.Recipients, ", "),
			SlackWebhookURL: subscription.SlackWebhookURL,
			NextRun:         nextRun(kind, site.Timezone, now),
			LastSent:        lastSent(lastByKind[kind]),
		})
	}

	for _, kind := range []string{reports.KindSpike, reports.KindDrop} {
		rule, configured := rulesByKind[kind]

		if !configured {
			rule = reports.AlertRule{
				Kind:        kind,
				Threshold:   reports.DefaultSpikeThreshold,
				WindowHours: reports.DefaultDropWindowHours,
			}

			if kind == reports.KindDrop {
				rule.Threshold = reports.DefaultDropThreshold
			}
		}

		view.Alerts = append(view.Alerts, alertView{
			Kind:            kind,
			Title:           strings.ToUpper(kind[:1]) + kind[1:],
			Description:     alertDescription(rule),
			Enabled:         rule.Enabled,
			Threshold:       rule.Threshold,
			WindowHours:     rule.WindowHours,
			RecipientList:   strings.Join(rule.Recipients, ", "),
			SlackWebhookURL: rule.SlackWebhookURL,
		})
	}

	for _, delivery := range deliveries {
		view.Deliveries = append(view.Deliveries, deliveryView{
			Kind:       delivery.Kind,
			PeriodKey:  delivery.PeriodKey,
			Recipients: delivery.Recipients,
			SentAt:     stamp(delivery.SentAt),
		})
	}

	return view, nil
}

// alertDescription says in words what a rule's numbers mean.
func alertDescription(rule reports.AlertRule) string {
	if rule.Kind == reports.KindSpike {
		return "Fires at " + strconv.Itoa(rule.Threshold) + " or more current visitors"
	}

	return "Fires below " + strconv.Itoa(rule.Threshold) + " unique visitors in " +
		strconv.Itoa(rule.WindowHours) + " hours"
}

// nextRun works out when a scheduled report goes out next, in the site's own
// timezone. It is computed rather than described so nobody has to work out what
// "Monday 00:00" means for a site in Kathmandu.
func nextRun(kind, timezone string, now time.Time) string {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return "unknown — " + timezone + " is not a timezone we can load"
	}

	local := now.In(location)

	if kind == reports.KindMonthly {
		next := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location).AddDate(0, 1, 0)

		return next.Format("Mon 2 Jan 15:04 MST")
	}

	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)

	// Days until the next Monday, and a full week when it is already Monday
	// past midnight — the report for this week has gone out.
	days := (int(time.Monday) - int(local.Weekday()) + 7) % 7
	if days == 0 {
		days = 7
	}

	return midnight.AddDate(0, 0, days).Format("Mon 2 Jan 15:04 MST")
}

// lastSent renders the last delivery, or says there was not one.
func lastSent(at int64) string {
	if at == 0 {
		return ""
	}

	return stamp(at)
}

// saveSubscription writes one scheduled report, and optionally sends one now.
func (h *TeamHandler) saveSubscription(w http.ResponseWriter, r *http.Request, site sites.Site, base string) {
	ctx := r.Context()

	subscription := reports.Subscription{
		SiteID:          site.ID,
		Kind:            r.PostFormValue("kind"),
		Recipients:      splitList(r.PostFormValue("recipients")),
		SlackWebhookURL: strings.TrimSpace(r.PostFormValue("slack_webhook_url")),
		Enabled:         r.PostFormValue("enabled") == "1",
	}

	if err := h.Reports.SaveSubscriptionForOwner(ctx, subscription, site.TeamID); err != nil {
		h.redirect(w, r, base, "", err.Error())

		return
	}

	if r.PostFormValue("send_now") != "1" {
		h.redirect(w, r, base, tr(r, "settings.flash.saved"), "")

		return
	}

	if h.Notifier == nil {
		h.redirect(w, r, base, "", tr(r, "settings.flash.no_notifier"))

		return
	}

	if _, err := h.Notifier.SendNow(ctx, site.ID, subscription.Kind, subscription.Recipients); err != nil {
		h.redirect(w, r, base, "", tr(r, "settings.flash.report_failed", "reason", err.Error()))

		return
	}

	h.redirect(w, r, base, tr(r, "settings.flash.saved_and_sent"), "")
}

// saveAlert writes one alert rule.
func (h *TeamHandler) saveAlert(w http.ResponseWriter, r *http.Request, site sites.Site, base string) {
	threshold, _ := strconv.Atoi(r.PostFormValue("threshold"))
	window, _ := strconv.Atoi(r.PostFormValue("window_hours"))

	rule := reports.AlertRule{
		SiteID:          site.ID,
		Kind:            r.PostFormValue("kind"),
		Threshold:       threshold,
		WindowHours:     window,
		Recipients:      splitList(r.PostFormValue("recipients")),
		SlackWebhookURL: strings.TrimSpace(r.PostFormValue("slack_webhook_url")),
		Enabled:         r.PostFormValue("enabled") == "1",
	}

	if err := h.Reports.SaveAlertRuleForOwner(r.Context(), rule, site.TeamID); err != nil {
		h.redirect(w, r, base, "", err.Error())

		return
	}

	h.redirect(w, r, base, tr(r, "settings.flash.alert_saved"), "")
}

// healthRoute serves the ingestion health panel and its two actions.
func (h *TeamHandler) healthRoute(w http.ResponseWriter, r *http.Request, identity Identity, site sites.Site, action string) {
	ctx := r.Context()
	base := healthPath(site.Domain)

	var result *health.TestEventResult

	if action != "" {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)

			return
		}

		if _, err := h.Teams.AuthoriseSite(ctx, site.ID, identity.UserID, teams.PermManageSiteSettings); err != nil {
			h.forbidden(w, err)

			return
		}

		if err := r.ParseForm(); err != nil {
			h.redirect(w, r, base, "", tr(r, "settings.flash.form_unreadable"))

			return
		}
	}

	switch action {
	case "allow":
		hostname := strings.TrimSpace(r.PostFormValue("hostname"))

		if err := h.Health.AllowHostname(ctx, site.Domain, hostname); err != nil {
			h.redirect(w, r, base, "", err.Error())

			return
		}

		h.redirect(w, r, base,
			hostname+" is now allowed, and so is "+site.Domain+". Events from any other hostname will be dropped.", "")

		return

	case "test-event":
		// The test event renders in place rather than redirecting, because its
		// whole output is the answer and stuffing a derived event into a query
		// string would truncate the one thing somebody pressed the button for.
		sent := h.sendTestEvent(r, site.Domain)
		result = &sent

	case "":
		// Fall through to the page itself.

	default:
		http.NotFound(w, r)

		return
	}

	panel, err := h.Health.Panel(ctx, site.Domain)
	if err != nil {
		h.internal(w, err)

		return
	}

	notice, problem := flash(r)

	var truncated int64
	for _, count := range panel.Truncations {
		truncated += count.Count
	}

	lastAt := "—"
	if panel.LastRequest != nil {
		lastAt = stamp(panel.LastRequest.ReceivedAt)
	}

	h.render(w, r, "health", screen{
		TitleID:          "settings.nav.health",
		Tab:              "health",
		Domain:           site.Domain,
		Message:          notice,
		Error:            problem,
		Panel:            panel,
		TruncatedTotal:   truncated,
		LastRequestAt:    lastAt,
		TestEvent:        result,
		TestEventDerived: prettyJSON(result),
	})
}

// sendTestEvent runs the round trip through the health handler's own client.
func (h *TeamHandler) sendTestEvent(r *http.Request, domain string) health.TestEventResult {
	checker := health.New(h.Health, h.BaseURL, h.Log)

	return checker.Check(r.Context(), domain)
}

// prettyJSON renders a derived event for the page. It is indented because the
// point of showing it is that somebody reads it, and a single line of JSON is
// not something anybody reads.
func prettyJSON(result *health.TestEventResult) string {
	if result == nil || len(result.Derived) == 0 {
		return ""
	}

	encoded, err := json.MarshalIndent(result.Derived, "", "  ")
	if err != nil {
		return ""
	}

	return string(encoded)
}

// splitList reads a comma-separated list of addresses out of a form field.
func splitList(raw string) []string {
	var out []string

	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}
