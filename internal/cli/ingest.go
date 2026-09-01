//
// ingest.go
// The `ingest` subcommand: serve event traffic separately over shared storage.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"fmt"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/httpserver"
)

const ingestHelp = `feasible ingest — run the ingest tier only.

Accepts events and writes them directly to the account databases on shared
storage. It answers 202 only after the account transaction commits, and exists
so the event listener scales without forking the payload-parsing code.

Flags:
`

// runIngest starts the ingest-only process and reports its resolved settings.
func runIngest(e *env, args []string) int {
	fs := newFlagSet("ingest", e, ingestHelp)
	listen := fs.String("listen", e.cfg.Ingest.Listen, "listen address (host:port)")
	internalListen := fs.String("internal-listen", e.cfg.Ingest.InternalListen, "private listen address for /metrics (host:port)")
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding control.db and the account databases")
	check := fs.Bool("check", false, "resolve and print the configuration, then exit without listening")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	e.cfg.Ingest.Listen = *listen
	e.cfg.Ingest.InternalListen = *internalListen

	// Compatibility topology fields remain visible while deployments move to
	// shared storage, even though direct account writes no longer use them.
	e.log.Info("ingest configuration",
		"listen", e.cfg.Ingest.Listen,
		"internal_listen", e.cfg.Ingest.InternalListen,
		"shards", e.cfg.Ingest.Shards,
		"buffer_path", e.cfg.Ingest.BufferPath,
		"internal_keys", len(e.cfg.Shared.InternalKeys),
		"trusted_proxies", len(e.cfg.Ingest.TrustedProxies),
		"env", e.cfg.Shared.Env,
		"trace_events", e.cfg.Shared.TraceEvents,
	)

	if *check {
		return ExitOK
	}

	service, control, manager, err := buildIngest(context.Background(), e, *dataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	checks := &health.Set{}
	ingestHealth(checks, control, service, *dataDir)

	// The blocked-address rules have to be loaded here as well as in the app
	// process. This endpoint is the only place the raw IP still exists.
	rules, err := buildSiteRules(context.Background(), e, service, manager)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	// An ingestor with an empty routing map answers 202 to everything and drops
	// it all, so it is not ready until the map holds something. This is where
	// the two process shapes genuinely differ: an app with no sites is a fresh
	// install waiting for somebody to add one, and refusing it traffic would
	// mean nobody could ever reach the page that adds one.
	checks.Require("routing_map", health.Condition(
		func() bool { return service.Sites.Len() > 0 },
		"the routing map is empty — every event would be dropped as an unknown site"))

	watchProcess(service, manager, *dataDir, nil)

	server := httpserver.New("ingest", e.cfg.Ingest.Listen, ingestRoutes(service))
	server.Health = checks

	internal := internalServer("ingest-internal", e.cfg.Ingest.InternalListen, checks)

	// No roll-up worker: the ingest tier answers no reports, and summarising
	// from here would put a second process on the account's write lock. The rule
	// refresh loops go through the shared background hook so that shutdown waits
	// for them rather than cancelling a refresh mid-read.
	return serveUntilSignalWith(e, server, internal, service, nil, rules.background(e), manager.CloseAll, control.Close)
}
