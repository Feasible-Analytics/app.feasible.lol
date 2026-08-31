//
// pages.go
// The server-rendered screens: pricing, billing, checkout, docs and the legal pages.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package pages serves everything in the product that is not the React
// dashboard: the pricing and upgrade screens, the billing screen with its usage
// meter, the documentation, and the three legal documents.
//
// They are server-rendered Go templates rather than part of the dashboard
// bundle because they have to work with no JavaScript toolchain installed and,
// more importantly, because they have to work when the dashboard is locked. A
// customer whose account has lapsed can still reach the page where they would
// pay us, which would not be true if it lived inside the thing we locked.
package pages

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/billing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

// templates and assets hold the rendered pages and the one stylesheet. Both are
// embedded so a release stays a single binary.
//
//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets/pages.css
var assetFS embed.FS

// The four page templates. Each is parsed with the shared layout so that the
// footer — which carries the postal address the law requires — cannot be left
// off one of them.
var (
	pricingPage = mustParse("pricing.html")
	billingPage = mustParse("billing.html")
	docsPage    = mustParse("docs.html")
	docPage     = mustParse("doc.html")
	messagePage = mustParse("message.html")
)

// mustParse builds one page template. A broken template is a programmer error
// caught by the first test run, so panicking is honest.
func mustParse(name string) *template.Template {
	return template.Must(template.ParseFS(templateFS, "templates/layout.html", "templates/"+name))
}

// Handler serves the pages. Everything it needs is injected, because each
// dependency is optional in a different deployment: a self-hosted install has no
// payment provider, and a fresh install has no usage yet.
type Handler struct {
	Billing   *billing.Service
	Lifecycle *lifecycle.Store
	Usage     *usage.Store
	Log       *logger.Logger

	// SalesEmail is where the "talk to us about volume" links point.
	SalesEmail string

	// Now is injectable so a screenshot or a test can render the billing screen
	// at a chosen point on the lifecycle clock.
	Now func() time.Time

	// CurrentTeam resolves the account a request is for.
	//
	// It is a function because sessions belong to the authentication package,
	// which owns its own code. Until that lands, the default resolves the `team`
	// parameter, and otherwise the only account on the install — which is right
	// for a self-hoster and for local development, and is replaced by a single
	// assignment once sessions exist.
	CurrentTeam func(r *http.Request) (int64, error)
}

// now returns the handler's clock.
func (h *Handler) now() time.Time {
	if h.Now == nil {
		return time.Now().UTC()
	}

	return h.Now().UTC()
}

// Routes returns every path this package answers on. They are registered from
// one place so that a new screen cannot be added without appearing in the list
// somebody reads to find out what the product serves.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /pricing", h.pricing)
	mux.HandleFunc("GET /billing", h.billing)
	mux.HandleFunc("GET /billing/upgrade", h.pricing)
	mux.HandleFunc("POST /billing/checkout", h.checkout)
	mux.HandleFunc("POST /billing/portal", h.portal)
	mux.HandleFunc("GET /billing/portal", h.portal)
	mux.HandleFunc("GET /billing/done", h.done)
	mux.HandleFunc("GET /billing/export", h.export)
	mux.HandleFunc("GET /billing/assets/pages.css", h.stylesheet)
	mux.HandleFunc("GET /docs", h.docs)
	mux.HandleFunc("GET /docs/{slug}", h.doc)
	mux.HandleFunc("GET /legal/{slug}", h.legal)
}

// shell is what every template's layout reads.
type shell struct {
	Title      string
	Nav        string
	SalesEmail string
	Enabled    bool
	TeamID     int64
}

// newShell builds the common part of a page.
func (h *Handler) newShell(title, nav string, teamID int64) shell {
	sales := h.SalesEmail
	if sales == "" {
		sales = "sales@feasible.lol"
	}

	return shell{
		Title:      title,
		Nav:        nav,
		SalesEmail: sales,
		Enabled:    h.Billing != nil && h.Billing.Enabled(),
		TeamID:     teamID,
	}
}

// stylesheet serves the one CSS file. It is cached for an hour rather than
// forever because it carries no digest in its URL, and a deploy that nobody can
// see for a year is worse than one extra request an hour.
func (h *Handler) stylesheet(w http.ResponseWriter, _ *http.Request) {
	body, err := assetFS.ReadFile("assets/pages.css")
	if err != nil {
		http.Error(w, "stylesheet missing", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(body)
}

// pricingData is the pricing and upgrade screen.
type pricingData struct {
	shell
}

// pricing renders the plans. It is deliberately reachable without a session:
// somebody deciding whether to pay should not have to log in to see the price,
// and somebody whose dashboard is locked has to be able to reach it.
func (h *Handler) pricing(w http.ResponseWriter, r *http.Request) {
	teamID, _ := h.team(r)

	h.render(w, pricingPage, pricingData{shell: h.newShell("Pricing", "pricing", teamID)})
}

// billingData is everything the billing screen shows.
type billingData struct {
	shell

	Account struct {
		Name string
	}

	Status   *statusBanner
	Usage    usageMeter
	History  []historyRow
	Plan     planPanel
	Timeline []timelineRow
	Growing  bool
}

// statusBanner is the one message at the top of the billing screen. It is a
// pointer so that a healthy account renders no banner at all rather than a
// green box saying nothing.
type statusBanner struct {
	Tone    string
	Heading string
	Detail  string
}

// usageMeter is the in-app volume meter.
type usageMeter struct {
	Billable  string
	Limit     string
	Percent   int
	BarWidth  int
	Projected string
	Tone      string
}

// historyRow is one month on the usage table.
type historyRow struct {
	Period       string
	Pageviews    string
	CustomEvents string
	Billable     string
}

// planPanel is what the customer is paying.
type planPanel struct {
	Label             string
	Status            string
	PaymentState      string
	RenewsOn          string
	CancelAtPeriodEnd bool
	HasCustomer       bool
}

// timelineRow is one step of the lifecycle, shown only when a clock is running.
type timelineRow struct {
	When string
	What string
	Now  bool
}

// billing renders the account's own billing screen: what it is using, what it
// is paying, and — if a clock is running — exactly what happens next and when.
func (h *Handler) billing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := h.now()

	teamID, err := h.team(r)
	if err != nil {
		h.message(w, r, "Billing", "No account selected",
			[]string{err.Error()}, nil)
		return
	}

	data := billingData{shell: h.newShell("Billing", "billing", teamID)}
	data.Account.Name = fmt.Sprintf("Account %d", teamID)

	if h.Lifecycle != nil {
		if name, _, err := h.Lifecycle.Contact(ctx, teamID); err == nil && name != "" {
			data.Account.Name = name
		}
	}

	period := usage.Period(now)
	counts := usage.Counts{}

	if h.Usage != nil {
		counts, err = h.Usage.Get(ctx, teamID, period)
		if err != nil {
			h.fail(w, err)
			return
		}

		history, err := h.Usage.History(ctx, teamID, 6)
		if err != nil {
			h.fail(w, err)
			return
		}

		for _, entry := range history {
			data.History = append(data.History, historyRow{
				Period:       entry.Period,
				Pageviews:    thousands(entry.Counts.Pageviews),
				CustomEvents: thousands(entry.Counts.CustomEvents),
				Billable:     thousands(entry.Counts.Billable()),
			})
		}
	}

	data.Usage = meterFor(counts, now)
	data.Growing = usage.LevelFor(counts.Billable()) != usage.LevelOK

	if h.Billing != nil {
		mirror, err := h.Billing.Store.Load(ctx, teamID)
		if err != nil {
			h.fail(w, err)
			return
		}

		plan := stripe.Describe(mirror.PriceID, h.Billing.Plans.Monthly, h.Billing.Plans.Yearly)

		data.Plan = planPanel{
			Label:             firstNonEmpty(plan.Label, "No plan"),
			Status:            firstNonEmpty(mirror.Status, "none"),
			PaymentState:      mirror.PaymentState,
			CancelAtPeriodEnd: mirror.CancelAtPeriodEnd,
			HasCustomer:       mirror.CustomerID != "",
		}

		if !mirror.CurrentPeriodEnd.IsZero() {
			data.Plan.RenewsOn = mirror.CurrentPeriodEnd.Format("2 January 2006")
		}
	}

	if h.Lifecycle != nil {
		state, err := h.Lifecycle.Load(ctx, teamID)
		if err != nil {
			h.fail(w, err)
			return
		}

		data.Status = bannerFor(state, now)
		data.Timeline = timelineFor(state, now)
	}

	h.render(w, billingPage, data)
}

// meterFor builds the usage meter. The bar is capped at 100% while the number
// beside it is not, because a bar wider than its track looks like a rendering
// bug while a percentage over 100 is simply the truth.
func meterFor(counts usage.Counts, now time.Time) usageMeter {
	billable := counts.Billable()
	percent := usage.Percent(billable)

	meter := usageMeter{
		Billable: thousands(billable),
		Limit:    thousands(usage.MonthlyLimit),
		Percent:  percent,
		BarWidth: min(percent, 100),
	}

	switch usage.LevelFor(billable) {
	case usage.LevelReached:
		meter.Tone = "over"
	case usage.LevelNear, usage.LevelWarn:
		meter.Tone = "warn"
	}

	if projected := usage.Projection(billable, now); projected > 0 {
		meter.Projected = thousands(projected)
	}

	return meter
}

// bannerFor turns the lifecycle state into the one message at the top of the
// screen. Each phase names the next date, for the same reason every email does:
// nobody should have to work out when their data disappears.
func bannerFor(state lifecycle.State, now time.Time) *statusBanner {
	if !state.Running() {
		return nil
	}

	locks := state.Boundary(lifecycle.PhaseLocked).Format("2 January 2006")
	stops := state.Boundary(lifecycle.PhaseDormant).Format("2 January 2006")
	deletes := state.Boundary(lifecycle.PhaseDeleted).Format("2 January 2006")

	trial := state.Trigger == lifecycle.TriggerTrial

	switch state.At(now) {
	case lifecycle.PhaseGrace:
		heading := "Your trial ends on " + locks
		detail := "Everything works normally until then. After that the dashboard locks, but we keep collecting your data until " + stops + " — so upgrading before that date leaves no gap in your history."

		if !trial {
			heading = "We could not charge your card"
			detail = "Nothing has changed yet. Your dashboard locks on " + locks + ", and we keep collecting until " + stops + ". Updating your card fixes it immediately."
		}

		return &statusBanner{Tone: "warn", Heading: heading, Detail: detail}

	case lifecycle.PhaseLocked:
		return &statusBanner{
			Tone:    "warn",
			Heading: "Your dashboard is locked — and we are still collecting",
			Detail:  "Nothing is lost. Every event from your sites is still being recorded, and it all comes back the moment you pay. We stop collecting on " + stops + ", and delete everything on " + deletes + ".",
		}

	case lifecycle.PhaseDormant:
		return &statusBanner{
			Tone:    "bad",
			Heading: "We have stopped collecting",
			Detail:  "Everything already recorded is safe until " + deletes + ", and you can export all of it right now. The days since " + stops + " will show on your graphs as a labelled gap rather than as zeroes.",
		}

	default:
		return &statusBanner{
			Tone:    "bad",
			Heading: "This account is scheduled for deletion",
			Detail:  "Everything we hold for this account is deleted on " + deletes + ". Download it below if you want to keep it.",
		}
	}
}

// timelineFor lists the three remaining boundaries, marking the phase the
// account is in now.
func timelineFor(state lifecycle.State, now time.Time) []timelineRow {
	if !state.Running() {
		return nil
	}

	phase := state.At(now)

	return []timelineRow{
		{
			When: state.Boundary(lifecycle.PhaseLocked).Format("2 January 2006"),
			What: "The dashboard locks. We keep collecting everything.",
			Now:  phase == lifecycle.PhaseGrace,
		},
		{
			When: state.Boundary(lifecycle.PhaseDormant).Format("2 January 2006"),
			What: "We stop collecting. A labelled gap starts on your graphs.",
			Now:  phase == lifecycle.PhaseLocked,
		},
		{
			When: state.Boundary(lifecycle.PhaseDeleted).Format("2 January 2006"),
			What: "Everything is permanently deleted. This cannot be undone.",
			Now:  phase == lifecycle.PhaseDormant,
		},
	}
}

// checkout starts a hosted checkout and redirects to it. The redirect is a 303
// so the browser turns the POST into a GET; a 302 here leaves some clients
// re-posting the form to the payment provider.
func (h *Handler) checkout(w http.ResponseWriter, r *http.Request) {
	teamID, err := h.team(r)
	if err != nil {
		h.message(w, r, "Upgrade", "No account selected", []string{err.Error()}, nil)
		return
	}

	if h.Billing == nil || !h.Billing.Enabled() {
		h.message(w, r, "Upgrade", "This install cannot take payments",
			[]string{"No payment provider is configured here. That is the normal state of a self-hosted install, and nothing about the software you are running is limited by it."},
			[]link{{Label: "Back to billing", URL: "/billing"}})
		return
	}

	session, err := h.Billing.Checkout(r.Context(), teamID, r.FormValue("plan"), r.FormValue("email"))
	if err != nil {
		h.fail(w, err)
		return
	}

	http.Redirect(w, r, session.URL, http.StatusSeeOther)
}

// portal redirects to the payment provider's Customer Portal, where card
// updates, plan switches, invoices and cancellation all live.
func (h *Handler) portal(w http.ResponseWriter, r *http.Request) {
	teamID, err := h.team(r)
	if err != nil {
		h.message(w, r, "Billing", "No account selected", []string{err.Error()}, nil)
		return
	}

	if h.Billing == nil || !h.Billing.Enabled() {
		h.message(w, r, "Billing", "This install cannot take payments",
			[]string{"No payment provider is configured here, so there is no billing portal to open."},
			[]link{{Label: "Back to billing", URL: "/billing"}})
		return
	}

	session, err := h.Billing.Portal(r.Context(), teamID)
	if err != nil {
		h.message(w, r, "Billing", "No billing portal yet",
			[]string{err.Error(), "A portal exists once an account has been through checkout at least once."},
			[]link{{Label: "See the plans", URL: "/pricing"}})
		return
	}

	http.Redirect(w, r, session.URL, http.StatusSeeOther)
}

// done is where the payment provider returns after checkout.
//
// It deliberately does not activate anything. The webhook does that, from the
// provider's signed payment result, and a return page that also flipped a switch
// would be a second source of truth reachable by anyone who guessed the URL.
func (h *Handler) done(w http.ResponseWriter, r *http.Request) {
	status := ""
	if h.Billing != nil {
		status, _ = h.Billing.CheckoutPaymentStatus(r.Context(), r.URL.Query().Get("session"))
	}

	heading := "Checkout submitted"
	paragraphs := []string{
		"We are confirming the payment with Stripe. Your account changes only after the signed payment notification arrives.",
		"Link sends a receipt after payment is confirmed. You can return to billing to see the current account state.",
	}

	switch status {
	case "paid", "no_payment_required":
		heading = "Payment confirmed"
		paragraphs = []string{
			"Stripe has confirmed your payment. Your account activates from the signed notification; if billing has not updated yet, give it a few seconds.",
			"Link sends your receipt by email and keeps your invoices with the transaction.",
		}
	case "unpaid":
		heading = "Payment processing"
		paragraphs = []string{
			"Checkout is complete, but your payment method has not settled yet. Your account remains in its current state until Stripe confirms payment.",
			"We will update billing automatically when processing succeeds or fails. Link sends the receipt only after payment is confirmed.",
		}
	}

	h.message(w, r, "Thank you", heading, paragraphs,
		[]link{{Label: "Open the dashboard", URL: "/dashboard/"}, {Label: "Billing", URL: "/billing"}})
}

// export is the download-everything page. It is reachable in every phase,
// including a locked or dormant account and the day before a deletion, because
// data portability is not something we may switch off for non-payment.
func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	h.message(w, r, "Export", "Download everything we hold",
		[]string{
			"Your export contains every event we have stored for this account, in a portable format you can load somewhere else.",
			"It works in every state an account can be in — including a locked dashboard, a stopped collection, and the day before a scheduled deletion. It is your data.",
		},
		[]link{{Label: "Billing", URL: "/billing"}, {Label: "How exports work", URL: "/docs/api"}})
}

// docs renders the documentation index.
func (h *Handler) docs(w http.ResponseWriter, r *http.Request) {
	teamID, _ := h.team(r)

	h.render(w, docsPage, struct {
		shell
		Index []Doc
	}{shell: h.newShell("Documentation", "docs", teamID), Index: documentation})
}

// doc renders one documentation page.
func (h *Handler) doc(w http.ResponseWriter, r *http.Request) {
	page, ok := findDoc(documentation, r.PathValue("slug"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	teamID, _ := h.team(r)

	h.render(w, docPage, struct {
		shell
		Doc   Doc
		Index []Doc
	}{shell: h.newShell(page.Title, "docs", teamID), Doc: page, Index: documentation})
}

// legal renders one of the three legal documents.
func (h *Handler) legal(w http.ResponseWriter, r *http.Request) {
	page, ok := findDoc(legal, r.PathValue("slug"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	teamID, _ := h.team(r)

	h.render(w, docPage, struct {
		shell
		Doc   Doc
		Index []Doc
	}{shell: h.newShell(page.Title, "legal", teamID), Doc: page, Index: legal})
}

// link is one button on a message page.
type link struct {
	Label string
	URL   string
}

// message renders a simple one-panel page. Several outcomes — a finished
// checkout, an install with no payment provider, an account with no portal yet —
// are all "here is what happened and here is where to go next", and giving each
// its own template would be five templates that drift apart.
func (h *Handler) message(w http.ResponseWriter, r *http.Request, title, heading string, paragraphs []string, links []link) {
	teamID, _ := h.team(r)

	h.render(w, messagePage, struct {
		shell
		Heading    string
		Paragraphs []string
		Links      []link
		Extra      template.HTML
	}{shell: h.newShell(title, "billing", teamID), Heading: heading, Paragraphs: paragraphs, Links: links})
}

// render writes one page, or reports the failure rather than sending half a
// document. A template error after the first byte has been written produces a
// truncated page with a 200 on it, which is the worst possible way to fail.
func (h *Handler) render(w http.ResponseWriter, tmpl *template.Template, data any) {
	var buf strings.Builder

	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		h.fail(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(buf.String()))
}

// fail logs and answers with a plain error. The message is deliberately not the
// error text: a billing screen is one place where leaking an internal detail to
// a browser is a real risk.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	if h.Log != nil {
		h.Log.Error("billing page failed", "error", err)
	}

	http.Error(w, "something went wrong rendering this page", http.StatusInternalServerError)
}

// team resolves which account a request is about.
func (h *Handler) team(r *http.Request) (int64, error) {
	if h.CurrentTeam != nil {
		return h.CurrentTeam(r)
	}

	if raw := r.FormValue("team"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			return 0, fmt.Errorf("%q is not an account id", raw)
		}

		return id, nil
	}

	return 0, fmt.Errorf("no account was named — add ?team=<id> until sign-in exists")
}

// thousands formats a count with separators, because the numbers on this screen
// are seven digits long and "1000000" is genuinely hard to read at a glance.
func thousands(value int64) string {
	digits := strconv.FormatInt(value, 10)

	var out []byte
	for i := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, digits[i])
	}

	return string(out)
}

// firstNonEmpty returns the first value that is set.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
