//
// services.go
// Assembling the pieces that need control.db: sharing, reports, health and the worker.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/annotations"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/auth"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dashboard"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/reports"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/settings"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sharing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// services is everything the app process serves beyond the ingest pipeline.
//
// It is assembled in one place rather than inline in the route table because
// most of these share a store or a clock with another, and a wiring spread
// across a mux is a wiring where two handlers end up with two different views
// of the same data.
type services struct {
	Teams       *teams.Store
	Sharing     *sharing.Store
	Reports     *reports.Store
	Health      *health.Store
	Annotations *annotations.Store
	Mailer      *mail.Mailer
	Notifier    *reports.Notifier
	Recorder    *health.Recorder

	// Cron is the producer for the process's one queue. It does not claim or
	// run anything: it enqueues the hourly and ten-minute ticks through the
	// same client the import and export screens enqueue through, and the one
	// runner drains all of it.
	Cron *jobs.Cron

	// control and siteCache are held so the route table can hand them to the
	// handlers that need them without buildServices returning five values.
	control   *sql.DB
	siteCache *sites.Cache
}

// buildServices assembles them over an open control database.
//
// The mailer is the process's one mailer rather than a transport of this
// package's own. Every guarantee about outgoing mail — the body wrapped below
// the SMTP line limit, a relay that declined the message being an error rather
// than a silent success — lives inside it, and a second path to a mail server
// would be a second path that has none of them.
func buildServices(e *env, control *sql.DB, manager *accounts.Manager,
	service *ingest.Service, mailer *mail.Mailer) *services {
	s := &services{
		Teams:       teams.NewStore(control),
		Sharing:     sharing.NewStore(control),
		Reports:     reports.NewStore(control),
		Health:      health.NewStore(manager, service.Sites, control),
		Annotations: annotations.NewStore(manager),
		Mailer:      mailer,
		Recorder:    health.NewRecorder(manager, service.Sites, e.log),
		control:     control,
		siteCache:   service.Sites,
	}

	s.Notifier = &reports.Notifier{
		Store:   s.Reports,
		Source:  reports.NewQuerySource(manager),
		Sites:   reports.ControlSiteLookup(control),
		Mail:    mailer,
		Slack:   reports.NewSlack(),
		Log:     e.log,
		BaseURL: e.cfg.App.BaseURL,
	}

	s.Cron = jobs.NewCron(jobs.NewClient(control), e.log)

	// The ingest path hands every derived request to the health recorder. This
	// is the line that turns the counters into a panel that can name the
	// hostname, the header and the tracker version behind a number.
	attachIngestRecorder(service, s.Recorder)

	return s
}

// Register attaches the notifier's two jobs to the process's one runner, and
// their recurring ticks to the cron.
//
// It is separate from buildServices because the runner is built beside the
// import and export workers, and this is the line that puts both sets of work
// on the same queue. A second runner would be a second answer to "is anything
// stuck", and the metrics endpoint and the readiness probe can each only report
// on one of them.
func (s *services) Register(runner *jobs.Runner) {
	if s.Notifier == nil || runner == nil {
		return
	}

	s.Notifier.Register(runner, s.Cron)
}

// background is the loops this half of the process owns, folded into the hook
// shutdown waits on.
//
// The health recorder flushes on its way out, so being waited on is what keeps
// the last minute of evidence about whatever made somebody restart the process.
//
// The cron is a switch rather than always-on because a deployment with several
// app replicas wants the ticks coming from somewhere specific, and because a
// developer poking at the dashboard should not have a customer's weekly report
// going out from their laptop.
func (s *services) background(e *env) func(context.Context, func(func())) {
	return func(ctx context.Context, run func(func())) {
		run(func() { s.Recorder.Run(ctx) })

		if !e.cfg.App.Worker {
			e.log.Info("the recurring jobs are switched off in this process", "reason", "FEASIBLE_APP_WORKER=false")

			return
		}

		e.log.Info("the recurring jobs are running", "queues", s.Cron.Queues())

		run(func() { s.Cron.Run(ctx, nil) })
	}
}

// screens builds the team half of the settings surface.
//
// The application is passed in because these screens sit behind its session:
// they transfer ownership, mint API keys and publish a site's traffic, and every
// one of those is a decision about who is asking.
func (s *services) screens(e *env, app *auth.Handler) http.Handler {
	return settings.NewTeamHandler(&settings.TeamHandler{
		Control:  s.control,
		Teams:    s.Teams,
		Sharing:  s.Sharing,
		Reports:  s.Reports,
		Health:   s.Health,
		Notifier: s.Notifier,
		Sites:    s.siteCache,
		Mail:     s.Mailer,
		Log:      e.log,
		BaseURL:  e.cfg.App.BaseURL,
		CSRF:     app.FormToken,

		// Who is asking comes from the application's session rather than from
		// this package, so the permission checks on these screens are made
		// against exactly the person its own gates admitted.
		Identify: func(r *http.Request) (settings.Identity, error) {
			userID, teamID, err := app.Identify(r)
			if err != nil {
				return settings.Identity{}, err
			}

			return settings.Identity{UserID: userID, TeamID: teamID}, nil
		},
	})
}

// mount adds the routes these services own outside the settings surface: the
// public dashboard, the shared links, the health panel's API and annotations.
//
// The settings screens are not here. They go through settings.Mount, from the
// one table that lists every route on that segment, because a pattern
// registered beside that table rather than in it is a pattern nothing checks
// for shadowing.
func (s *services) mount(mux *http.ServeMux, e *env, app *auth.Handler, shell *dashboard.Handler, secret []byte) {
	security := sharing.NewSecurity(e.cfg.App.BaseURL)
	shareHandler := sharing.New(s.Sharing, shell, security, sharing.DeriveSecret(secret), e.log)

	// A public dashboard and a shared link are the two ways somebody sees these
	// numbers without an account. They are separate prefixes rather than one
	// because a public URL is meant to be stable and quotable and a share token
	// is meant to be revocable, and a single prefix would blur the two.
	mux.Handle(sharing.PublicPattern, shareHandler)
	mux.Handle(sharing.SharePattern, shareHandler)

	healthHandler := health.New(s.Health, e.cfg.App.BaseURL, e.log)

	healthAPI := app.GuardSiteAPI(func(r *http.Request) string { return r.PathValue("domain") },
		func(*http.Request) teams.Permission { return teams.PermManageSiteSettings }, healthHandler)

	mux.Handle("GET "+health.PanelPattern, healthAPI)
	mux.Handle("POST "+health.AllowPattern, healthAPI)
	mux.Handle("POST "+health.TestEventPattern, healthAPI)

	annotationHandler := annotations.New(s.Annotations, s.siteCache, e.log)
	annotationHandler.Identity = func(r *http.Request) (int64, string, bool) {
		user := auth.RequestUser(r)
		if user == nil {
			return 0, "", false
		}

		return user.ID, user.DisplayName(), true
	}
	annotationAPI := app.GuardSiteAPI(func(r *http.Request) string { return r.PathValue("domain") },
		func(r *http.Request) teams.Permission {
			if r.Method == http.MethodGet {
				return teams.PermViewDashboard
			}

			return teams.PermManageSiteSettings
		}, annotationHandler)

	mux.Handle(annotations.CollectionPattern, annotationAPI)
	mux.Handle(annotations.ItemPattern, annotationAPI)
}
