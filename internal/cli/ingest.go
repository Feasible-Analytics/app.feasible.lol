//
// ingest.go
// The `ingest` subcommand: accept events, buffer them, forward to a shard.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"fmt"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/httpserver"
)

const ingestHelp = `feasible ingest — run the ingest tier only.

Accepts events, derives them, writes them to a local outbox, answers 202, then
forwards to the shard that owns the account. Exists so the production front door
scales horizontally without forking the payload-parsing code.

Flags:
`

// runIngest starts the ingest-only process. It resolves and reports the shard
// list on the way up because an ingestor with the wrong shards silently drops
// every event it receives, and that is the failure we least want to discover in
// production.
func runIngest(e *env, args []string) int {
	fs := newFlagSet("ingest", e, ingestHelp)
	listen := fs.String("listen", e.cfg.Ingest.Listen, "listen address (host:port)")
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding control.db and the account databases")
	check := fs.Bool("check", false, "resolve and print the configuration, then exit without listening")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	e.cfg.Ingest.Listen = *listen

	// An ingestor with the wrong shard list silently drops every event it
	// receives, so the list is reported before anything else happens — and
	// `ingest -check` prints it without needing a database to exist.
	e.log.Info("ingest configuration",
		"listen", e.cfg.Ingest.Listen,
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

	server := httpserver.New("ingest", e.cfg.Ingest.Listen, ingestRoutes(service))

	// An ingestor with an empty routing map would answer 202 to everything and
	// drop it all, so readiness waits for the site cache to hold something.
	server.Ready = func() bool { return service.Sites.Len() > 0 }

	// No roll-up worker: the ingest tier answers no reports, and summarising
	// from here would put a second process on the account's write lock.
	return serveUntilSignal(e, server, service, nil, manager.CloseAll, control.Close)
}
