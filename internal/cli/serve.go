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
	"fmt"
	"net/http"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/httpserver"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
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

	server := httpserver.New("app", e.cfg.App.Listen, serveRoutes(e, service, manager, secret))

	return serveUntilSignal(e, server, service, manager.CloseAll, control.Close)
}

// serveRoutes is the app process's public surface: the tracker script, the
// stats API the dashboard runs on, and — with the direct transport — the ingest
// endpoint, which this process serves rather than a separate tier.
func serveRoutes(e *env, service *ingest.Service, manager *accounts.Manager, secret []byte) http.Handler {
	mux := http.NewServeMux()

	// Every report in the product is this one endpoint with different metrics
	// and dimensions. It reads the same in-memory site snapshot the ingest path
	// does, so a dashboard query never touches control.db.
	mux.Handle(statsapi.Pattern, statsapi.New(service.Sites, manager, e.log))

	// The script is served by the app rather than the ingest tier because it is
	// a cacheable static asset with a database lookup behind it, and putting
	// that on the front door would make the busiest process in the system do
	// the one thing it is built to avoid.
	mux.Handle(tracker.PathPrefix, tracker.New(secret, service.Sites))

	// With the http transport a separate ingest tier owns /api/event, and
	// answering it here too would mean two processes deriving the same event
	// with two different site caches.
	if e.cfg.App.Transport == config.TransportDirect {
		mux.Handle("/api/event", service.Handler)
		mux.Handle(tracker.PixelPath, &tracker.Pixel{Events: service.Handler})
	}

	return mux
}
