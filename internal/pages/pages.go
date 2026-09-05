//
// pages.go
// The server-rendered commerce screens: billing and checkout.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package pages serves the commerce screens: the billing screen, which carries
// the usage meter and the buttons a customer buys with, and the pages checkout
// returns to.
//
// They are server-rendered Go templates rather than part of the dashboard
// bundle because they have to work when the dashboard is locked. A customer
// whose account has lapsed can still reach the page where they would pay us,
// which would not be true if it lived inside the thing we locked.
//
// The plans are published on the marketing site, in its own repository. What
// is here is the part that needs an account: the prices are on both, but only
// this side knows who is buying.
package pages

import (
	"embed"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/billing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/i18n"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

// templates and assets hold the rendered pages and the one stylesheet. Both are
// embedded so a release stays a single binary.
//
//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets/pages.css
var assetFS embed.FS

// SiteURL is the public marketing site. Documentation and the legal documents
// are published there, from their own repository, because they are read by
// people who have not installed anything — and a self-hosted copy of them would
// be a second version that goes stale the first time a policy changes.
const SiteURL = "https://feasible.lol"

// docsURL is where a link out of the application lands.
const docsURL = SiteURL + "/docs"

// The two page templates. Each is parsed with the shared layout so that the
// footer — which carries the postal address the law requires — cannot be left
// off one of them.
var (
	billingPage = mustParse("billing.html")
	messagePage = mustParse("message.html")
)

// mustParse builds one page template. A broken template is a programmer error
// caught by the first test run, so panicking is honest.
func mustParse(name string) *template.Template {
	return template.Must(template.New(name).Funcs(templateFuncs()).
		ParseFS(templateFS, "templates/layout.html", "templates/"+name))
}

// templateFuncs is the one helper these templates may call.
//
// The locale is the function's first argument rather than something it closes
// over, because the templates are parsed once at start-up: there is no request
// at that moment and there never will be one, so a captured language would be
// whichever the process happened to boot with, for every reader.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"t": func(locale, id string, args ...any) string {
			return i18n.T(locale, id, args...)
		},
	}
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

	// Hosted selects the hosted service's own identity in the footer and the
	// links to its contract. A self-hosted install names its own operator and
	// links to no contract of ours, because we are not a party to it.
	Hosted          bool
	OperatorName    string
	OperatorAddress string
	OperatorEmail   string

	// Now is injectable so a screenshot or a test can render the billing screen
	// at a chosen point on the lifecycle clock.
	Now func() time.Time

	// RequireAccount protects every route here — all of them are one account's
	// money. It is injected so this package does not import auth.
	RequireAccount func(http.Handler) http.Handler

	// CurrentAccount reads only the identity established by the injected
	// middleware. FormToken and ValidateForm reuse the application's CSRF
	// implementation for forms rendered and handled by this package.
	CurrentAccount func(r *http.Request) (Account, error)
	FormToken      func(w http.ResponseWriter, r *http.Request) string
	ValidateForm   func(w http.ResponseWriter, r *http.Request) bool
}

// Account is the authenticated billing identity pages needs. The account id
// selects data and the email pre-fills hosted checkout; the auth boundary has
// already resolved any requested team through the user's membership.
type Account struct {
	ID    int64
	Email string
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
	mux.Handle("GET /billing", h.protected(h.billing, false))
	mux.Handle("POST /billing/checkout", h.protected(h.checkout, true))
	mux.Handle("POST /billing/portal", h.protected(h.portal, true))
	mux.Handle("GET /billing/done", h.protected(h.done, false))
	mux.Handle("GET /billing/export", h.protected(h.export, false))
	mux.HandleFunc("GET /billing/assets/pages.css", h.stylesheet)
}

// protected composes account authentication around the route and CSRF around
// authenticated POSTs. Authentication is outermost so a signed-out form post
// reaches sign-in rather than receiving a misleading token error.
func (h *Handler) protected(next http.HandlerFunc, csrf bool) http.Handler {
	handler := http.Handler(next)
	if csrf && h.ValidateForm != nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !h.ValidateForm(w, r) {
				return
			}

			next.ServeHTTP(w, r)
		})
	}
	if h.RequireAccount != nil {
		handler = h.RequireAccount(handler)
	}

	return handler
}

// shell is what every template's layout reads.
type shell struct {
	Title           string
	Nav             string
	Lang            string
	Site            string
	SalesEmail      string
	Enabled         bool
	SignedIn        bool
	TeamID          int64
	CSRF            string
	Hosted          bool
	OperatorName    string
	OperatorAddress []string
	OperatorEmail   string
}

// newShell builds the common part of a page.
//
// The title arrives as a catalogue id rather than as text so that a page's tab
// and its heading cannot end up in two different languages, which is what
// happens the moment one of them is translated and the other is a literal.
func (h *Handler) newShell(w http.ResponseWriter, r *http.Request, lang, titleID, nav string, account Account) shell {
	sales := h.SalesEmail
	if sales == "" {
		sales = "sales@feasible.lol"
	}

	csrf := ""
	if account.ID > 0 && h.FormToken != nil {
		csrf = h.FormToken(w, r)
	}

	return shell{
		Title:           i18n.T(lang, titleID),
		Nav:             nav,
		Lang:            lang,
		Site:            SiteURL,
		SalesEmail:      sales,
		Enabled:         h.Billing != nil && h.Billing.Enabled(),
		SignedIn:        account.ID > 0,
		TeamID:          account.ID,
		CSRF:            csrf,
		Hosted:          h.Hosted,
		OperatorName:    h.OperatorName,
		OperatorAddress: strings.Split(h.OperatorAddress, "\n"),
		OperatorEmail:   h.OperatorEmail,
	}
}

// language applies an explicit language choice and returns a locale whose page
// catalogue is complete. A partial translation may fall back string by string,
// but the document must not label that English fallback as another language.
func (h *Handler) language(w http.ResponseWriter, r *http.Request) string {
	requested := i18n.Apply(w, r)
	if requested == i18n.DefaultLocale {
		return requested
	}

	for _, id := range i18n.Default.IDs() {
		if strings.HasPrefix(id, "pages.") && !i18n.Default.Has(requested, id) {
			return i18n.DefaultLocale
		}
	}

	return requested
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
	Comped   bool
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

	lang := h.language(w, r)
	account, err := h.account(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	teamID := account.ID

	data := billingData{shell: h.newShell(w, r, lang, "pages.title.billing", "billing", account)}
	data.Account.Name = i18n.T(lang, "pages.account.fallback", "id", teamID)

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

		plan := h.Billing.Plans.Describe(mirror.PriceID)

		data.Plan = planPanel{
			Label:             firstNonEmpty(plan.Label, i18n.T(lang, "pages.billing.plan.none")),
			Status:            firstNonEmpty(mirror.Status, i18n.T(lang, "pages.billing.plan.status_none")),
			PaymentState:      mirror.PaymentState,
			CancelAtPeriodEnd: mirror.CancelAtPeriodEnd,
			HasCustomer:       mirror.CustomerID != "",
		}

		if !mirror.CurrentPeriodEnd.IsZero() {
			data.Plan.RenewsOn = mirror.CurrentPeriodEnd.Format("2 January 2006")
		}
	}

	if h.Lifecycle != nil {
		data.Comped, err = h.Lifecycle.IsComped(ctx, teamID)
		if err != nil {
			h.fail(w, err)
			return
		}

		state, err := h.Lifecycle.Load(ctx, teamID)
		if err != nil {
			h.fail(w, err)
			return
		}

		if data.Comped {
			data.Plan = planPanel{
				Label:  i18n.T(lang, "pages.billing.plan.comped"),
				Status: i18n.T(lang, "pages.billing.plan.status_active"),
			}
			data.Status = &statusBanner{
				Tone:    "ok",
				Heading: i18n.T(lang, "pages.billing.comped.heading"),
				Detail:  i18n.T(lang, "pages.billing.comped.detail"),
			}
		} else {
			data.Status = bannerFor(lang, state, now)
			data.Timeline = timelineFor(lang, state, now)
		}
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
func bannerFor(lang string, state lifecycle.State, now time.Time) *statusBanner {
	if !state.Running() {
		return nil
	}

	locks := state.Boundary(lifecycle.PhaseLocked).Format("2 January 2006")
	stops := state.Boundary(lifecycle.PhaseDormant).Format("2 January 2006")
	deletes := state.Boundary(lifecycle.PhaseDeleted).Format("2 January 2006")

	trial := state.Trigger == lifecycle.TriggerTrial

	switch state.At(now) {
	case lifecycle.PhaseGrace:
		// The two ids are written out rather than assembled from a prefix so
		// that the completeness check can see them. A message id built by
		// concatenation is a string no scan can find and no test can prove is
		// translated.
		heading, detail := "pages.banner.trial.heading", "pages.banner.trial.detail"
		if !trial {
			heading, detail = "pages.banner.card.heading", "pages.banner.card.detail"
		}

		return &statusBanner{
			Tone:    "warn",
			Heading: i18n.T(lang, heading, "locks", locks),
			Detail:  i18n.T(lang, detail, "locks", locks, "stops", stops),
		}

	case lifecycle.PhaseLocked:
		return &statusBanner{
			Tone:    "warn",
			Heading: i18n.T(lang, "pages.banner.locked.heading"),
			Detail:  i18n.T(lang, "pages.banner.locked.detail", "stops", stops, "deletes", deletes),
		}

	case lifecycle.PhaseDormant:
		return &statusBanner{
			Tone:    "bad",
			Heading: i18n.T(lang, "pages.banner.dormant.heading"),
			Detail:  i18n.T(lang, "pages.banner.dormant.detail", "stops", stops, "deletes", deletes),
		}

	default:
		return &statusBanner{
			Tone:    "bad",
			Heading: i18n.T(lang, "pages.banner.deleted.heading"),
			Detail:  i18n.T(lang, "pages.banner.deleted.detail", "deletes", deletes),
		}
	}
}

// timelineFor lists the three remaining boundaries, marking the phase the
// account is in now.
func timelineFor(lang string, state lifecycle.State, now time.Time) []timelineRow {
	if !state.Running() {
		return nil
	}

	phase := state.At(now)

	return []timelineRow{
		{
			When: state.Boundary(lifecycle.PhaseLocked).Format("2 January 2006"),
			What: i18n.T(lang, "pages.timeline.locked"),
			Now:  phase == lifecycle.PhaseGrace,
		},
		{
			When: state.Boundary(lifecycle.PhaseDormant).Format("2 January 2006"),
			What: i18n.T(lang, "pages.timeline.dormant"),
			Now:  phase == lifecycle.PhaseLocked,
		},
		{
			When: state.Boundary(lifecycle.PhaseDeleted).Format("2 January 2006"),
			What: i18n.T(lang, "pages.timeline.deleted"),
			Now:  phase == lifecycle.PhaseDormant,
		},
	}
}

// checkout starts a hosted checkout and redirects to it. The redirect is a 303
// so the browser turns the POST into a GET; a 302 here leaves some clients
// re-posting the form to the payment provider.
func (h *Handler) checkout(w http.ResponseWriter, r *http.Request) {
	lang := h.language(w, r)
	account, err := h.account(r)
	if err != nil {
		h.fail(w, err)
		return
	}

	if h.Billing == nil || !h.Billing.Enabled() {
		h.message(w, r, lang, "pages.title.upgrade", i18n.T(lang, "pages.pricing.disabled.heading"),
			[]string{i18n.T(lang, "pages.checkout.disabled.body")},
			[]link{{Label: i18n.T(lang, "pages.link.back_to_billing"), URL: accountURL("/billing", account.ID, nil)}})
		return
	}

	session, err := h.Billing.Checkout(r.Context(), account.ID, r.FormValue("plan"), account.Email)
	if err != nil {
		h.fail(w, err)
		return
	}

	http.Redirect(w, r, session.URL, http.StatusSeeOther)
}

// portal redirects to the payment provider's Customer Portal, where card
// updates, plan switches, invoices and cancellation all live.
func (h *Handler) portal(w http.ResponseWriter, r *http.Request) {
	lang := h.language(w, r)
	account, err := h.account(r)
	if err != nil {
		h.fail(w, err)
		return
	}

	if h.Billing == nil || !h.Billing.Enabled() {
		h.message(w, r, lang, "pages.title.billing", i18n.T(lang, "pages.pricing.disabled.heading"),
			[]string{i18n.T(lang, "pages.portal.disabled.body")},
			[]link{{Label: i18n.T(lang, "pages.link.back_to_billing"), URL: accountURL("/billing", account.ID, nil)}})
		return
	}

	session, err := h.Billing.Portal(r.Context(), account.ID)
	if err != nil {
		h.message(w, r, lang, "pages.title.billing", i18n.T(lang, "pages.portal.missing.heading"),
			[]string{err.Error(), i18n.T(lang, "pages.portal.missing.body")},
			[]link{{Label: i18n.T(lang, "pages.link.plans"), URL: accountURL("/billing", account.ID, nil)}})
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
	lang := h.language(w, r)
	account, err := h.account(r)
	if err != nil {
		h.fail(w, err)
		return
	}

	status := ""
	sessionID := r.URL.Query().Get("session")
	if h.Billing != nil && h.Billing.Stripe != nil && h.Billing.Stripe.Configured() && sessionID != "" {
		session, sessionErr := h.Billing.Stripe.GetCheckoutSession(r.Context(), sessionID)
		if sessionErr == nil {
			if session.Metadata.TeamID() != account.ID {
				http.NotFound(w, r)
				return
			}

			status = session.PaymentStatus
		}
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

	links := []link{{Label: "Open the dashboard", URL: "/dashboard/"},
		{Label: "Billing", URL: accountURL("/billing", account.ID, nil)}}
	if status == "unpaid" {
		links = append(links, link{Label: "Retry checkout", URL: accountURL("/billing", account.ID, nil)})
	}
	h.message(w, r, lang, "pages.title.thanks", heading, paragraphs, links)
}

// export is the page that explains where the export lives. It is reachable in
// every phase, including a locked or dormant account and the day before a
// deletion, because data portability is not something we may switch off for
// non-payment — and because these pages sit outside the lock, this is the one
// route to it that a locked customer can always find.
func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	lang := h.language(w, r)
	account, err := h.account(r)
	if err != nil {
		h.fail(w, err)
		return
	}

	h.message(w, r, lang, "pages.title.export", i18n.T(lang, "pages.export.heading"),
		[]string{
			i18n.T(lang, "pages.export.contents"),
			i18n.T(lang, "pages.export.always"),
			i18n.T(lang, "pages.export.not_yet"),
		},
		[]link{
			{Label: i18n.T(lang, "pages.link.billing"), URL: accountURL("/billing", account.ID, nil)},
			{Label: i18n.T(lang, "pages.link.export_docs"), URL: docsURL + "/api"},
		})
}

// accountURL carries an already-authorized team through local billing links.
// Every destination revalidates membership; the query merely prevents an
// intentional non-default selection from silently becoming the default team.
func accountURL(path string, teamID int64, values url.Values) string {
	if values == nil {
		values = url.Values{}
	}
	values.Set("team", strconv.FormatInt(teamID, 10))

	return path + "?" + values.Encode()
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
func (h *Handler) message(w http.ResponseWriter, r *http.Request, lang, titleID, heading string, paragraphs []string, links []link) {
	account, _ := h.account(r)

	h.render(w, messagePage, struct {
		shell
		Heading    string
		Paragraphs []string
		Links      []link
		Extra      template.HTML
	}{shell: h.newShell(w, r, lang, titleID, "billing", account), Heading: heading, Paragraphs: paragraphs, Links: links})
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

	http.Error(w, i18n.T(i18n.DefaultLocale, "pages.error.render"), http.StatusInternalServerError)
}

// account resolves the authenticated billing identity. The form-value fallback
// keeps the standalone pages package usable for a single-user self-hosted
// installation; the assembled app always injects CurrentAccount and therefore
// never consults caller-controlled account or email values.
func (h *Handler) account(r *http.Request) (Account, error) {
	if h.CurrentAccount != nil {
		return h.CurrentAccount(r)
	}

	lang := i18n.Negotiate(r)

	if raw := r.FormValue("team"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			return Account{}, errors.New(i18n.T(lang, "pages.error.bad_account_id", "value", raw))
		}

		return Account{ID: id, Email: r.FormValue("email")}, nil
	}

	return Account{}, errors.New(i18n.T(lang, "pages.error.no_account_named"))
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
