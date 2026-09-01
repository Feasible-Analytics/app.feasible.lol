//
// commerce.go
// Assembling billing, the lifecycle clock, the volume ladder and the mailer.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/access"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/auth"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/billing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/pages"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"

	// The package is imported under an alias because this one already has a
	// package-level `usage` — the help text every subcommand prints.
	volume "github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

// commerce is the commercial half of the app process: taking money, counting
// what we bill for, and running the clock that eventually deletes an account
// that stops paying.
//
// It is assembled in one place rather than in serve.go so that the wiring — in
// particular which component drives which — is readable as a single unit. The
// order below is the dependency order, and it is the order somebody debugging
// "why was this account not deleted" will want to read it in.
type commerce struct {
	Lifecycle      *lifecycle.Service
	LifecycleStore *lifecycle.Store
	Purger         *lifecycle.Purger
	Billing        *billing.Service
	Webhook        *billing.Webhook
	Usage          *volume.Store
	Recorder       *volume.Recorder
	Volume         *volume.Sweeper
	Gate           *access.Gate
	Pages          *pages.Handler
	Mailer         *mail.Mailer
}

// buildCommerce assembles everything from the resolved configuration.
//
// Nothing here fails when the payment provider is absent. A self-hosted install
// has no Stripe account, and every part of this has to degrade to "billing is
// not available on this install" rather than refusing to boot — the software is
// AGPL and none of its features are conditional on paying us.
func buildCommerce(e *env, control *sql.DB, manager *accounts.Manager, siteCache *sites.Cache, mailer *mail.Mailer) *commerce {
	lifecycleStore := lifecycle.NewStore(control)
	usageStore := volume.NewStore(control)

	stripeClient := stripe.New(e.cfg.App.Stripe.SecretKey)

	billingService := &billing.Service{
		Stripe: stripeClient,
		Store:  billing.NewStore(control),
		Plans: billing.Plans{
			Product: e.cfg.App.Stripe.Product,
			Monthly: e.cfg.App.Stripe.PriceMonthly,
			Yearly:  e.cfg.App.Stripe.PriceYearly,
		},
		Log:           e.log,
		WebhookSecret: e.cfg.App.Stripe.WebhookSecret,
		BaseURL:       e.cfg.App.BaseURL,
	}

	// The purger holds the account manager because the database file cannot be
	// removed while this process still has a writer open on it — on some
	// filesystems the unlink succeeds and the handle keeps writing to an inode
	// nothing can find.
	purger := &lifecycle.Purger{
		Store:     lifecycleStore,
		Accounts:  manager,
		DataDir:   e.cfg.App.DataDir,
		Customers: billingService,
		Payments:  billingService,
		Log:       e.log,
	}

	lifecycleService := &lifecycle.Service{
		Store:  lifecycleStore,
		Notify: mail.NewLifecycleMailer(mailer),
		Purger: purger,
		Links:  lifecycle.Links{BaseURL: e.cfg.App.BaseURL},
		Log:    e.log,
	}

	billingService.Lifecycle = lifecycleService

	recorder := volume.NewRecorder(usageStore)
	recorder.OnError = func(err error) {
		// A flush that keeps failing is a customer being under-billed and, far
		// worse, a limit warning that never fires. It is an error, not a debug
		// line.
		e.log.Error("usage counters could not be written", "error", err)
	}

	sweeper := &volume.Sweeper{
		Store:      usageStore,
		Notify:     mail.NewUsageMailer(mailer),
		Contacts:   lifecycleStore,
		Log:        e.log,
		SalesEmail: e.cfg.App.SalesEmail,
		BillingURL: e.cfg.App.BaseURL + "/billing",
	}

	return &commerce{
		Lifecycle:      lifecycleService,
		LifecycleStore: lifecycleStore,
		Purger:         purger,
		Billing:        billingService,
		Webhook:        billing.NewWebhook(billingService, e.log),
		Usage:          usageStore,
		Recorder:       recorder,
		Volume:         sweeper,
		Gate:           access.New(lifecycleStore, usageStore, siteCache, e.log),
		Pages: &pages.Handler{
			Billing:         billingService,
			Lifecycle:       lifecycleStore,
			Usage:           usageStore,
			Log:             e.log,
			SalesEmail:      e.cfg.App.SalesEmail,
			Hosted:          e.cfg.App.Hosted,
			Subprocessors:   e.cfg.App.Subprocessors,
			OperatorName:    e.cfg.App.OperatorName,
			OperatorAddress: e.cfg.App.OperatorAddress,
			OperatorEmail:   e.cfg.App.OperatorEmail,
		},
		Mailer: mailer,
	}
}

// ingestUsage adapts the billing recorder to the shape the shard writer takes.
//
// The writer deals in two plain integers rather than the billing package's own
// type, so that nothing on the ingest path depends on billing existing at all —
// a self-hosted install has no billing and must still ingest.
type ingestUsage struct {
	recorder *volume.Recorder
}

// Record forwards one account's committed counts.
func (a ingestUsage) Record(accountID int64, pageviews, customEvents int64) {
	a.recorder.Record(accountID, volume.Counts{Pageviews: pageviews, CustomEvents: customEvents})
}

// IngestRecorder is what the shard writer is given.
func (c *commerce) IngestRecorder() ingestUsage {
	return ingestUsage{recorder: c.Recorder}
}

// buildMailer resolves the one mailer a process uses.
//
// Every command that can send — `serve` and `billing sweep` — goes through here
// so the From address, the relay and the transport choice are read from the
// configuration in exactly one place. A transport name the binary does not know
// is an error rather than a fallback to writing files, because a box that
// quietly stopped sending would look healthy while nobody could reset a
// password or hear that their data is about to be deleted.
func buildMailer(e *env) (*mail.Mailer, error) {
	return mail.New(mail.Options{
		Transport:    e.cfg.App.MailTransport,
		From:         e.cfg.App.MailFrom,
		BaseURL:      e.cfg.App.BaseURL,
		SMTPHost:     e.cfg.App.SMTP.Host,
		SMTPPort:     e.cfg.App.SMTP.Port,
		SMTPUser:     e.cfg.App.SMTP.Username,
		SMTPPass:     e.cfg.App.SMTP.Password,
		SMTPStartTLS: e.cfg.App.SMTP.StartTLS,
		Log:          e.log,
	})
}

// Routes adds the commercial surface to the mux: authenticated account pages,
// public pricing and documentation, and the provider webhook. Authentication
// is injected here so pages remains usable without importing auth.
func (c *commerce) Routes(mux *http.ServeMux, app *auth.Handler) {
	c.Pages.OptionalAccount = app.OptionalAccount
	c.Pages.RequireAccount = app.RequireAccount
	c.Pages.CurrentAccount = func(r *http.Request) (pages.Account, error) {
		teamID, email, err := app.CurrentAccount(r)

		return pages.Account{ID: teamID, Email: email}, err
	}
	c.Pages.FormToken = app.FormToken
	c.Pages.ValidateForm = app.ValidateForm

	c.Pages.Routes(mux)
	mux.Handle(billing.WebhookPath, c.Webhook)
}

// Start launches the three background loops. Each runs independently, because a
// failure in one must not silence the others: a usage sweep that stops still
// leaves the deletion clock running, and that is the one that must never stop
// quietly.
func (c *commerce) Start(ctx context.Context, run func(func())) {
	run(func() { c.Recorder.Run(ctx) })
	run(func() { c.Gate.Run(ctx) })
	run(func() { c.Lifecycle.Run(ctx) })
	run(func() { c.Volume.Run(ctx) })
}
