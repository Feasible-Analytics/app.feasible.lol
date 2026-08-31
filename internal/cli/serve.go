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

	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/httpserver"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
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

	server := httpserver.New("app", e.cfg.App.Listen, serveRoutes(e, service))

	return serveUntilSignal(e, server, service, manager.CloseAll, control.Close)
}

// serveRoutes is the app process's public surface. The dashboard, the stats API
// and the tracker script land here in later milestones; what is real today is
// the ingest endpoint, which the direct transport serves from this process
// rather than from a separate tier.
func serveRoutes(e *env, service *ingest.Service) http.Handler {
	mux := http.NewServeMux()

	// With the http transport a separate ingest tier owns /api/event, and
	// answering it here too would mean two processes deriving the same event
	// with two different site caches.
	if e.cfg.App.Transport == config.TransportDirect {
		mux.Handle("/api/event", service.Handler)
	}

	return mux
}
