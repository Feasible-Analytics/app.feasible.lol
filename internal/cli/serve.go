//
// serve.go
// The `serve` subcommand: the whole product in one process.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/access"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/auth"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dashboard"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/httpserver"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/metrics"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/rollup"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/statsapi"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/tracker"
)

const serveHelp = `feasible serve — run the whole product in one process.

This is the default mode and the only thing a self-hoster ever runs: the
dashboard, the API, the tracker and — with the direct transport — ingestion too.

Flags:
`

// runServe wires up and starts the app process. With the direct transport it
// also runs the ingest path in-process, which is the self-hoster's entire
// deployment: one binary, one data directory, no queue and no second service.
func runServe(e *env, args []string) int {
	fs := newFlagSet("serve", e, serveHelp)
	listen := fs.String("listen", e.cfg.App.Listen, "public listen address (host:port)")
	internalListen := fs.String("internal-listen", e.cfg.App.InternalListen, "private listen address for /internal/*")
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding control.db and the account databases")
	check := fs.Bool("check", false, "resolve and print the configuration, then exit without listening")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	e.cfg.App.Listen = *listen
	e.cfg.App.InternalListen = *internalListen
	e.cfg.App.DataDir = *dataDir

	// The configuration is reported before anything is opened, so that
	// `serve -check` answers "did I configure this right" on a box whose
	// databases are not there yet — which is exactly when the question is asked.
	e.log.Info("serve configuration",
		"listen", e.cfg.App.Listen,
		"internal_listen", e.cfg.App.InternalListen,
		"base_url", e.cfg.App.BaseURL,
		"data_dir", e.cfg.App.DataDir,
		"transport", e.cfg.App.Transport,
		"mail_transport", e.cfg.App.MailTransport,
		"env", e.cfg.Shared.Env,
		"trace_events", e.cfg.Shared.TraceEvents,
	)

	if *check {
		return ExitOK
	}

	service, control, manager, err := buildIngest(context.Background(), e, e.cfg.App.DataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	// The secret that per-site script paths derive from. It is read here rather
	// than lazily on the first request so that an unreadable data directory is
	// a start-up failure with a path in the message, not a 404 on a customer's
	// snippet an hour later.
	secret, err := tracker.LoadSecret(e.cfg.App.DataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	// One mailer for the whole process. Every message the product sends — the
	// verification code, the password reset and the ten lifecycle notices —
	// leaves through the same transport, so the From address and the SMTP relay
	// can only be wrong in one place, and a misconfigured transport is a
	// start-up failure rather than a deletion warning nobody receives.
	mailer, err := buildMailer(e)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	// The commercial half: taking money, counting billable volume, and the clock
	// that eventually deletes an account that stops paying. It is built before
	// the two front ends because both of them ask its access gate whether an
	// account may still read its own data, and none of it fails when no payment
	// provider is configured.
	com := buildCommerce(e, control, manager, service.Sites, mailer)

	// The signed-in application. It is built before the listener binds so that a
	// broken template or an unreadable key is a start-up failure with a message,
	// rather than a 500 on somebody's sign-in page.
	app, err := buildApp(e, control, manager, service, secret, mailer, com.Gate)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	// Google credentials are not available yet, and the product has to work
	// without them. One line at start-up says which variable is missing, so
	// "where is the Google button" is answered by the log rather than by a
	// support ticket.
	if reason := app.Google.DisabledReason(); reason != "" {
		e.log.Info(reason)
	}

	// The public API, the MCP server and the webhook worker are built here and
	// in every build. There is no plan check and no build tag in front of any
	// of them, which is the difference between this and the product it competes
	// with: their self-hosted build inherited a subscription check and showed
	// people a paywall on their own instance.
	public := buildPublic(e, control, service.Sites, manager, com.Gate)

	// The worker's lifetime is tied to the process rather than to a request, so
	// it gets a context of its own that shutdown cancels.
	workerCtx, stopWorker := context.WithCancel(context.Background())
	public.startWorker(workerCtx, e)

	// The roll-up worker keeps the pre-aggregated report tables current. There
	// is no job runner yet, so it runs here beside the ingest service's own
	// loops, and it is stopped by the same context they are.
	worker := &rollup.Worker{
		Accounts: manager,
		Sites:    rollup.ControlLister(control),
		Log:      e.log,
	}

	// The shard counts billable events after each commit. This is assigned
	// before anything is listening, so nothing reads it concurrently with the
	// write.
	service.Writer.Usage = com.IngestRecorder()

	e.log.Info("billing configuration",
		"payments", com.Billing.Enabled(),
		"webhooks", e.cfg.App.Stripe.WebhookSecret != "",
		"mail_from", e.cfg.App.MailFrom,
		"sales_email", e.cfg.App.SalesEmail,
	)

	checks := &health.Set{}
	ingestHealth(checks, control, service, e.cfg.App.DataDir)

	// The routing map is checked for having been built, not for holding
	// anything. An install with no sites yet is a fresh install, and a process
	// that refused traffic until somebody added a site would refuse them the
	// page they add it on.
	checks.Require("routing_map", health.Condition(
		func() bool { return !service.Sites.BuiltAt().IsZero() },
		"the routing map has not been built yet"))

	watchProcess(service, manager, e.cfg.App.DataDir)

	server := httpserver.New("app", e.cfg.App.Listen, serveRoutes(e, service, manager, secret, e.cfg.App.DataDir, app, public, com))
	server.Health = checks

	internal := internalServer("app-internal", e.cfg.App.InternalListen, checks)

	pruneCtx, stopPrune := context.WithCancel(context.Background())
	go app.RunPrune(pruneCtx)

	return serveUntilSignalWith(e, server, internal, service, worker, com.Start,
		func() error { stopPrune(); stopWorker(); return nil }, manager.CloseAll, control.Close)
}

// buildApp assembles the server-rendered application.
//
// Every dependency is resolved here rather than inside the package, so that a
// missing key or an unparseable template stops the process with a message that
// names the file — and so a test can build the same handler over a temporary
// database.
func buildApp(e *env, control *sql.DB, manager *accounts.Manager, service *ingest.Service, secret []byte, mailer *mail.Mailer, gate *access.Gate) (*auth.Handler, error) {
	key, err := auth.LoadKey(e.cfg.App.DataDir, e.cfg.App.SecretKey)
	if err != nil {
		return nil, err
	}

	sealer, err := auth.NewSealer(key)
	if err != nil {
		return nil, err
	}

	store := auth.NewStore(control)
	stripe := auth.NewStripe(e.cfg.App.Stripe.SecretKey, e.log)

	return auth.NewHandler(auth.Options{
		Store:     store,
		Traffic:   auth.NewTraffic(manager),
		Mailer:    mailer,
		Sealer:    sealer,
		Google:    auth.NewGoogle(e.cfg.App.Google.ClientID, e.cfg.App.Google.ClientSecret, e.cfg.App.BaseURL),
		Deleter:   auth.NewDeleter(store, manager, e.cfg.App.DataDir, stripe, e.log),
		Keyer:     tracker.NewKeyer(secret, service.Sites),
		SiteCache: service.Sites,
		Access:    gate.Blocked,
		BaseURL:   e.cfg.App.BaseURL,
		Log:       e.log,
	})
}

// serveRoutes is the app process's public surface: the tracker script, the
// stats API the dashboard runs on, the server-rendered application, the pages
// that sell it, and — with the direct transport — the ingest endpoint, which
// this process serves rather than a separate tier.
func serveRoutes(e *env, service *ingest.Service, manager *accounts.Manager, secret []byte, dataDir string, app http.Handler, public *publicStack, com *commerce) http.Handler {
	mux := http.NewServeMux()

	// Every mount is wrapped with the name it is counted under. The name is
	// given here rather than derived from the URL because a label taken from a
	// path would carry a customer's domain and a visitor's page, and would
	// grow a new series for every URL a crawler invents.
	//
	// The signed-in application is mounted at the root, so it owns every path
	// the more specific patterns below do not claim. Go's mux picks the most
	// specific pattern, so /api/event and /js/ still reach their own handlers.
	mux.Handle("/", metrics.Instrument(metrics.HandlerApp, app))

	// Every report in the product is this one endpoint with different metrics
	// and dimensions. It reads the same in-memory site snapshot the ingest path
	// does, so a dashboard query never touches control.db.
	//
	// The access gate wraps it as well as the dashboard, and that is the point:
	// the numbers come from here, so a lock that only covered the HTML would be
	// no lock at all.
	mux.Handle(statsapi.Pattern, metrics.Instrument(metrics.HandlerStats,
		com.Gate.Protect(statsapi.New(service.Sites, manager, e.log))))

	// The compiled React dashboard, served out of the binary. It reads the site
	// snapshot only to render the site picker; every number on it comes from
	// the stats endpoint above.
	mux.Handle(dashboard.PathPrefix, metrics.Instrument(metrics.HandlerDashboard,
		com.Gate.Protect(dashboard.New(service.Sites))))

	// Pricing, billing, docs and the legal pages, plus the payment provider's
	// webhook. They are deliberately outside the gate: somebody whose dashboard
	// is locked has to be able to reach the page where they would pay us, and
	// the export link on it.
	com.Routes(mux)

	// The source icons the report rows are drawn with. Fetching them here
	// rather than from the reader's browser is what keeps a dashboard from
	// telling every site that ever linked to yours that somebody is looking at
	// their referral traffic.
	mux.Handle(dashboard.FaviconPattern, dashboard.NewFavicons(filepath.Join(dataDir, "favicons"), e.log))

	// The root is the dashboard until the marketing site and the auth screens
	// exist. A bare hostname answering 404 looks like a failed deploy, which is
	// the first thing anybody checks and the last thing we want it to look like.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dashboard.PathPrefix, http.StatusFound)
	})
	// /api/v1/*, /api/v2/*, /mcp and the OAuth endpoints. They are mounted on
	// their own prefixes rather than at the root because /api/event and the
	// tracker script must not sit behind a bearer-token check.
	public.mount(mux)
	// The script is served by the app rather than the ingest tier because it is
	// a cacheable static asset with a database lookup behind it, and putting
	// that on the front door would make the busiest process in the system do
	// the one thing it is built to avoid.
	mux.Handle(tracker.PathPrefix, metrics.Instrument(metrics.HandlerTracker, tracker.New(secret, service.Sites)))

	// With the http transport a separate ingest tier owns /api/event, and
	// answering it here too would mean two processes deriving the same event
	// with two different site caches.
	if e.cfg.App.Transport == config.TransportDirect {
		mux.Handle("/api/event", metrics.Instrument(metrics.HandlerEvent, service.Handler))
		mux.Handle(tracker.PixelPath, metrics.Instrument(metrics.HandlerEvent, &tracker.Pixel{Events: service.Handler}))
	}

	return mux
}
