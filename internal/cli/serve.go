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
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/access"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/auth"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dashboard"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/destructive"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/goals"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/google"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/httpserver"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/metrics"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/pathclean"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/rollup"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/settings"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sharing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/shields"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/statsapi"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
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
	listen := fs.String("listen", e.cfg.App.Listen, "listen address (host:port)")
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding system.db and the account databases")
	check := fs.Bool("check", false, "resolve and print the configuration, then exit without listening")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	e.cfg.App.Listen = *listen
	e.cfg.App.DataDir = *dataDir

	// The configuration is reported before anything is opened, so that
	// `serve -check` answers "did I configure this right" on a box whose
	// databases are not there yet — which is exactly when the question is asked.
	e.log.Info("serve configuration",
		"listen", e.cfg.App.Listen,
		"base_url", e.cfg.App.BaseURL,
		"data_dir", e.cfg.App.DataDir,
		"transport", e.cfg.App.Transport,
		"shard_id", e.cfg.App.ShardID,
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

	// The site-scoped rules the two hot paths consult. Both snapshots are built
	// before the listener opens, because a process that started serving with an
	// empty rule set would let through exactly the traffic a customer asked us
	// to block, silently, for one refresh interval.
	site, err := buildSiteRules(context.Background(), e, service, manager)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	// The signed-in application. It is built before the listener binds so that a
	// broken template or an unreadable key is a start-up failure with a message,
	// rather than a 500 on somebody's sign-in page.
	app, err := buildApp(e, control, manager, service, secret, mailer, com.Gate, com.Purger)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	// Google credentials are not available yet, and the product has to work
	// without them. One line at start-up says which variable is missing, so
	// "where is the Google button" is answered by the log rather than by a
	// support ticket. The same client covers the Analytics import and Search
	// Console, so this one line covers all three features.
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

	// The roll-up worker keeps the pre-aggregated report tables current. It runs
	// beside the ingest service's own loops rather than on the job queue,
	// because it holds no state a restart could lose, and it is stopped by the
	// same context they are.
	worker := &rollup.Worker{
		Accounts: manager,
		Sites:    rollup.SystemLister(control),
		Log:      e.log,
	}

	// The shard counts billable events after each commit. This is assigned
	// before anything is listening, so nothing reads it concurrently with the
	// write.
	service.Writer.Usage = com.IngestRecorder()

	e.log.Info("billing configuration",
		"payments", com.Billing.Enabled(),
		"webhooks", com.Billing.Enabled(),
		"mail_from", e.cfg.App.MailFrom,
		"sales_email", e.cfg.App.SalesEmail,
	)

	// Teams, sharing, scheduled reports, annotations and the ingestion health
	// panel. They are assembled in one place so that the shared-link handler and
	// the authenticated dashboard render the same shell rather than two copies
	// of it that drift.
	extra := buildServices(e, control, manager, service, mailer)

	checks := &health.Set{}
	ingestHealth(checks, control, service, e.cfg.App.DataDir)
	if e.cfg.App.Worker {
		// A worker process is not ready when its durable scheduler has never run,
		// has failed its latest pass, or has silently stopped ticking.
		checks.Require("recurring_scheduler", func(context.Context) error {
			return extra.Cron.Health(time.Now().UTC())
		})
	}

	// The routing map is checked for having been built, not for holding
	// anything. An install with no sites yet is a fresh install, and a process
	// that refused traffic until somebody added a site would refuse them the
	// page they add it on.
	checks.Require("routing_map", health.Condition(
		func() bool { return !service.Sites.BuiltAt().IsZero() },
		"the routing map has not been built yet"))

	watchProcess(service, manager, e.cfg.App.DataDir, jobCounts(control))

	// Importing and exporting a site's history: the screens that start the work
	// and the runner that does it. Both halves are built here so an import can
	// only ever be started by a process that is also willing to run it.
	//
	// The runner is the one that drains everything, the notifier's hourly ticks
	// included. Two runners would be two answers to "is anything stuck", and the
	// metrics endpoint and the readiness probe can each only report on one.
	data := buildData(e, control, manager, service, site)
	extra.Register(data.runner)

	privateShard := &ingest.InternalShard{
		ID: e.cfg.App.ShardID, Sites: service.Sites, Shields: site.shields,
		Writer: service.Writer,
	}
	privateRoutes := ingest.VerifyInternal(e.cfg.Shared.InternalKey, privateShard.Handler())
	server := httpserver.New("app", e.cfg.App.Listen, processRoutes(
		serveRoutes(e, service, manager, secret, e.cfg.App.DataDir, app, public, com, data.settings, extra),
		privateRoutes,
	))
	server.Health = checks

	pruneCtx, stopPrune := context.WithCancel(context.Background())
	go app.RunPrune(pruneCtx)

	return serveUntilSignalWith(e, server, service, worker,
		backgroundLoops(com.Start, site.background(e), data.background(), extra.background(e)),
		func() error { stopPrune(); stopWorker(); return nil }, manager.CloseAll, control.Close)
}

// backgroundLoops folds several independent sets of background work into the
// one hook shutdown waits on.
//
// They are combined rather than started as bare goroutines because every loop
// here holds state somebody can see — a billing sweep, a rule snapshot, a
// half-finished import — and a process that exited without waiting would lose
// it on every deploy.
func backgroundLoops(loops ...func(context.Context, func(func()))) func(context.Context, func(func())) {
	return func(ctx context.Context, run func(func())) {
		for _, loop := range loops {
			if loop != nil {
				loop(ctx, run)
			}
		}
	}
}

// dataStack is the import and export surface: the screens that start a job and
// the runner that executes it.
type dataStack struct {
	settings *settings.Handler
	runner   *jobs.Runner
}

// buildData assembles the import and export half of the app process.
//
// The runner is given only the workers this process is willing to execute.
// Registration is by kind, so a job this build does not know about is discarded
// with that reason on the row rather than retried forever by a process that can
// never run it.
func buildData(e *env, control *sql.DB, manager *accounts.Manager, service *ingest.Service, site *siteRules) *dataStack {
	queue := jobs.NewClient(control)
	runner := jobs.NewRunner(queue)
	runner.OnError = func(err error) { e.log.Error("job runner", "error", err) }

	workers := &dataio.Workers{Accounts: manager, Sites: service.Sites, DataDir: e.cfg.App.DataDir}
	workers.Register(runner)

	handler := &settings.Handler{
		Sites:    service.Sites,
		Accounts: manager,
		Jobs:     queue,
		Log:      e.log,
		DataDir:  e.cfg.App.DataDir,
		Trusted:  site.trusted,
		Shields:  site.shields,
		Paths:    site.paths,
	}

	// A nil application is what hides every Google feature. A button that sends
	// somebody to Google and brings them back to invalid_client is worse than
	// no button at all.
	if oauth, ok := google.NewApp(e.cfg.App.Google.ClientID, e.cfg.App.Google.ClientSecret, e.cfg.App.BaseURL); ok {
		handler.Google = oauth
	}

	return &dataStack{settings: handler, runner: runner}
}

// background runs the job queue for the life of the process.
func (d *dataStack) background() func(context.Context, func(func())) {
	return func(ctx context.Context, run func(func())) {
		run(func() { d.runner.Run(ctx) })
	}
}

// buildApp assembles the server-rendered application.
//
// Every dependency is resolved here rather than inside the package, so that a
// missing key or an unparseable template stops the process with a message that
// names the file — and so a test can build the same handler over a temporary
// database.
func buildApp(e *env, control *sql.DB, manager *accounts.Manager, service *ingest.Service, secret []byte, mailer *mail.Mailer, gate *access.Gate, purger auth.PermanentAccountDeleter) (*auth.Handler, error) {
	if err := provisionExistingSites(context.Background(), control, manager, time.Now().UTC()); err != nil {
		return nil, err
	}
	key, err := auth.LoadKey(e.cfg.App.DataDir, e.cfg.App.SecretKey)
	if err != nil {
		return nil, err
	}

	sealer, err := auth.NewSealer(key)
	if err != nil {
		return nil, err
	}

	store := auth.NewStore(control)
	return auth.NewHandler(auth.Options{
		Store:       store,
		Teams:       teams.NewStore(control),
		Traffic:     auth.NewTraffic(manager),
		Mailer:      mailer,
		Sealer:      sealer,
		Google:      auth.NewGoogle(e.cfg.App.Google.ClientID, e.cfg.App.Google.ClientSecret, e.cfg.App.BaseURL),
		Deleter:     auth.NewDeleter(purger, e.log),
		Destructive: &destructive.Service{DB: control, Accounts: manager},
		Keyer:       tracker.NewKeyer(secret, service.Sites),
		SiteCache:   service.Sites,
		ProvisionSite: func(ctx context.Context, accountID, siteID int64, now time.Time) error {
			lease, err := manager.Acquire(ctx, accountID)
			if err != nil {
				return err
			}
			defer lease.Release() //nolint:errcheck // the provisioning error is more actionable than an unlock error
			_, err = goals.EnsureAutomatic(ctx, lease.Account.Writer(), siteID, now)
			return err
		},
		Access:              gate.Blocked,
		DisableRegistration: !e.cfg.App.Hosted,
		DisableCommerce:     !e.cfg.App.Hosted,
		BaseURL:             e.cfg.App.BaseURL,
		Log:                 e.log,
	})
}

// siteRules is the pair of snapshots the ingest and write paths consult, plus
// the proxy allow-list the settings page has to resolve an address through.
type siteRules struct {
	shields *shields.Cache
	paths   *pathclean.Cache
	trusted *ingest.TrustedProxies
}

// buildSiteRules loads the shield and path cleaning rules and attaches them to
// the running pipeline.
//
// The two halves land in different stages on purpose. An IP rule runs while the
// raw address still exists. Country, page, and hostname rules run in the writer
// against the live account rule snapshot.
func buildSiteRules(ctx context.Context, e *env, service *ingest.Service, manager *accounts.Manager) (*siteRules, error) {
	trusted, err := ingest.ParseTrustedProxies(e.cfg.Ingest.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("trusted proxies: %w", err)
	}

	shieldCache := shields.New(service.Sites, manager)
	shieldCache.Rejections = shields.NewRejections(manager)
	if err := shieldCache.Refresh(ctx); err != nil {
		return nil, err
	}

	pathCache := pathclean.New(service.Sites, manager)
	if err := pathCache.Refresh(ctx); err != nil {
		return nil, err
	}

	service.Pipeline.Shield = shieldCache
	service.Pipeline.Hostnames = shieldCache
	service.Writer.Shield = shieldCache
	service.Writer.Counters = service.Counters
	service.Writer.Paths = pathCache

	return &siteRules{shields: shieldCache, paths: pathCache, trusted: trusted}, nil
}

// background refreshes both rule snapshots for the life of the process.
//
// They go through the shared hook rather than bare goroutines so that shutdown
// waits for a refresh to finish rather than cancelling one half-read.
func (s *siteRules) background(e *env) func(context.Context, func(func())) {
	return func(ctx context.Context, run func(func())) {
		run(func() {
			s.shields.Run(ctx, func(err error) { e.log.Error("shield refresh failed", "error", err) })
		})

		run(func() {
			s.paths.Run(ctx, func(err error) { e.log.Error("path cleaning refresh failed", "error", err) })
		})
	}
}

// serveRoutes is the app process's public surface: the tracker script, the
// stats API the dashboard runs on, the server-rendered application, the pages
// that sell it, the settings screens, and — with the direct transport — the
// ingest endpoint, which this process serves rather than a separate tier.
func serveRoutes(e *env, service *ingest.Service, manager *accounts.Manager, secret []byte, dataDir string,
	app *auth.Handler, public *publicStack, com *commerce, site *settings.Handler, extra *services) http.Handler {
	mux := http.NewServeMux()

	// Legacy per-site settings pages share the signed-in application's CSRF
	// cookie and verifier even though their handlers live in another package.
	site.CSRF = app.IssueCSRF
	site.CheckCSRF = app.CheckCSRF
	site.Role = func(r *http.Request, current sites.Site) teams.Role {
		return app.RoleForSite(r, current.ID)
	}

	// Every mount is wrapped with the name it is counted under. The name is
	// given here rather than derived from the URL because a label taken from a
	// path would carry a customer's domain and a visitor's page, and would
	// grow a new series for every URL a crawler invents.
	//
	// The signed-in application is mounted at the root, so it owns every path
	// the more specific patterns below do not claim. Go's mux picks the most
	// specific pattern, so /api/event and /js/ still reach their own handlers.
	mux.Handle("/", metrics.Instrument(metrics.HandlerApp, app))

	// The settings surface: shields, path cleaning, import and export on one
	// handler; the team screen, sharing, scheduled reports and the ingestion
	// health panel on the other. They are server-rendered rather than part of
	// the React bundle, because a form that posts and redirects needs no API
	// endpoint per field.
	//
	// The two gates are different because the two questions are. Configuring a
	// site is "does this person own this site". Publishing one, mailing reports
	// about it or administering the team is "what may this person do in this
	// team", which is the signed-in application's full check plus the team's
	// own permission table.
	//
	// Registration goes through settings.Mount rather than a loop here, so
	// every route on the segment comes from the one table a test can walk. A
	// pattern registered beside that table rather than in it is a pattern
	// nothing checks for shadowing — which is how this has broken three times.
	if site != nil || extra != nil {
		settings.Mount(mux,
			app.GuardSite(settings.DomainOf, site),
			app.Protect(extra.screens(e, app)))
	}

	// Every report in the product is this one endpoint with different metrics
	// and dimensions. It reads the same in-memory site snapshot the ingest path
	// does, so a dashboard query never touches system.db.
	//
	// The access gate wraps it as well as the dashboard, and that is the point:
	// the numbers come from here, so a lock that only covered the HTML would be
	// no lock at all.
	stats := statsapi.New(service.Sites, manager, e.log)
	capabilities := sharing.StatsAuthorizer{Secret: sharing.DeriveSecret(secret)}
	if extra != nil {
		capabilities.Store = extra.Sharing
	}

	stats.Authorize = func(r *http.Request, site sites.Site) (statsapi.Authorization, error) {
		if capabilities.Store != nil {
			capability, presented, err := capabilities.Authorize(r, site.Domain)
			if presented {
				switch {
				case err == nil:
					return statsapi.Authorization{PinnedFilters: capability.Filters, CacheKey: capability.CacheKey}, nil
				case errors.Is(err, sharing.ErrPasswordRequired):
					return statsapi.Authorization{}, statsapi.Refuse(http.StatusUnauthorized, "the shared link password has not been verified")
				case errors.Is(err, sharing.ErrNotFound):
					return statsapi.Authorization{}, statsapi.Refuse(http.StatusNotFound, "the sharing capability is no longer valid")
				default:
					return statsapi.Authorization{}, err
				}
			}
		}

		if _, err := app.AuthoriseSiteRequest(r, site.ID, teams.PermViewDashboard); err != nil {
			if errors.Is(err, auth.ErrUnauthenticated) {
				return statsapi.Authorization{}, statsapi.Refuse(http.StatusUnauthorized, "an authenticated session or validated sharing capability is required")
			}

			return statsapi.Authorization{}, statsapi.Refuse(http.StatusNotFound, "no such site")
		}

		return statsapi.Authorization{CacheKey: "session"}, nil
	}
	stats.SampleThreshold = e.cfg.API.QuerySampleThreshold

	mux.Handle(statsapi.Pattern, metrics.Instrument(metrics.HandlerStats,
		com.Gate.Protect(stats)))

	// Goal definitions need their own report wrapper, but every count inside it
	// still runs through the same query engine and authorization choices as the
	// rest of the dashboard. Converting the stats authorization result here keeps
	// session, public, shared-link, and pinned-segment access in lockstep.
	goalReport := goals.NewHandler(service.Sites, manager, e.log)
	goalReport.Authorize = func(r *http.Request, site sites.Site) (goals.Authorization, error) {
		authorized, err := stats.Authorize(r, site)
		if err != nil {
			var refusal *statsapi.AuthorizationError
			if errors.As(err, &refusal) {
				return goals.Authorization{}, goals.Refuse(refusal.Status, refusal.Message)
			}

			return goals.Authorization{}, err
		}

		return goals.Authorization{PinnedFilters: authorized.PinnedFilters}, nil
	}
	mux.Handle(goals.ReportPattern, com.Gate.Protect(goalReport))
	mux.Handle(goals.GoalsPattern, com.Gate.Protect(goalReport))
	mux.Handle(goals.PropertiesPattern, com.Gate.Protect(goalReport))
	mux.Handle(goals.PropertyReportPattern, com.Gate.Protect(goalReport))
	mux.Handle(goals.FunnelsPattern, com.Gate.Protect(goalReport))
	mux.Handle(goals.FunnelReportPattern, com.Gate.Protect(goalReport))
	mux.Handle(goals.JourneyPattern, com.Gate.Protect(goalReport))

	// The compiled React dashboard, served out of the binary. It reads the site
	// snapshot only to render the site picker; every number on it comes from
	// the stats endpoint above.
	shell := dashboard.New(service.Sites)
	shell.Resolve = func(w http.ResponseWriter, r *http.Request) dashboard.Bootstrap {
		domains, err := app.AccessibleDomains(r)
		if err != nil {
			e.log.Warn("could not list dashboard sites", "error", err)
			return dashboard.Bootstrap{}
		}

		navRequest := r
		if strings.TrimPrefix(r.URL.Path, dashboard.PathPrefix) == "" && len(domains) > 0 {
			copy := r.Clone(r.Context())
			copy.URL.Path = dashboard.PathPrefix + domains[0]
			navRequest = copy
		}
		nav := app.NavigationForDashboard(w, navRequest)
		boot := dashboard.Bootstrap{
			Sites: domains,
			Navigation: &dashboard.Navigation{
				Name: nav.Name, Email: nav.Email, SitesURL: nav.SitesURL,
				SiteSettingsURL: nav.SiteSettingsURL, ConversionsURL: nav.ConversionsURL,
				AccountURL: nav.AccountURL,
				BillingURL: nav.BillingURL, ExportURL: nav.ExportURL,
				LogoutURL: nav.LogoutURL, CSRF: nav.CSRF, TeamID: nav.TeamID,
			},
		}
		if refusal, locked := com.Gate.Check(nav.TeamID); locked {
			boot.Lock = &dashboard.Lock{Reason: string(refusal.Reason), Error: refusal.Error}
		}

		return boot
	}
	mux.Handle(dashboard.PathPrefix, metrics.Instrument(metrics.HandlerDashboard,
		app.GuardDashboard(shell)))

	// The public dashboard, the shared links, the annotations endpoint and the
	// health panel's API. The shared-link handler is handed the same shell the
	// authenticated dashboard uses, so a public dashboard and a signed-in one
	// can never render two different builds of the front end.
	if extra != nil {
		extra.mount(mux, e, app, shell, secret)
	}

	// Pricing, billing, docs and legal pages remain reachable outside the
	// payment gate, while commerce applies the current account and CSRF guards.
	com.Routes(mux, app)

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
