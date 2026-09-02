//
// ingest.go
// The `ingest` subcommand: durably accept events and forward them to app shards.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/httpserver"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

const ingestHelp = `feasible ingest — run the ingest tier only.

Accepts events, derives privacy-safe facts, writes them to a local SQLite
outbox, answers 202, and forwards them until the owning app shard commits.

Flags:
`

// runIngest starts the ingest-only process and reports its resolved settings.
func runIngest(e *env, args []string) int {
	fs := newFlagSet("ingest", e, ingestHelp)
	listen := fs.String("listen", e.cfg.Ingest.Listen, "listen address (host:port)")
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding geolocation and classification data; account databases are never opened")
	replayParked := fs.Bool("replay-parked", false, "return operator-reviewed parked events to delivery")
	check := fs.Bool("check", false, "resolve and print the configuration, then exit without listening")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	e.cfg.Ingest.Listen = *listen
	if err := validateIngestTopology(e); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

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
		if *replayParked {
			fmt.Fprintln(e.stderr, "-replay-parked cannot be used with -check")
			return ExitUsage
		}
		return ExitOK
	}

	ctx := context.Background()
	signer := &ingest.InternalSigner{Keys: internalKeys(e.cfg.Shared.InternalKeys)}
	outbox, err := ingest.OpenOutbox(ctx, e.cfg.Ingest.BufferPath, e.cfg.Ingest.Shards, signer)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	if *replayParked {
		count, replayErr := outbox.ReplayParked(ctx)
		if replayErr != nil {
			_ = outbox.Close()
			fmt.Fprintf(e.stderr, "%v\n", replayErr)
			return ExitError
		}
		e.log.Info("parked ingest events returned to delivery", "events", count)
	}
	service, err := ingest.NewRemoteService(ctx, outbox, ingest.Options{
		DataDir: *dataDir, TrustedProxies: e.cfg.Ingest.TrustedProxies,
		IngestSalt: e.cfg.Shared.IngestSalt, Log: e.log,
	})
	if err != nil {
		_ = outbox.Close()
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	checks := &health.Set{}
	checks.Require("outbox", health.Database(outbox.DB))
	checks.Require("routing_map", health.Condition(
		func() bool { return !service.Sites.BuiltAt().IsZero() },
		"no live or disk-cached routing snapshot has been built"))

	watchProcess(service, nil, filepath.Dir(e.cfg.Ingest.BufferPath), nil)

	server := httpserver.New("ingest", e.cfg.Ingest.Listen, processRoutes(ingestRoutes(service)))
	server.Health = checks

	return serveUntilSignalWith(e, server, service, nil, nil, outbox.Close)
}

// validateIngestTopology rejects a standalone ingester that has no complete
// destination list, shared salt, or signing identity. These values are not
// required by a direct-mode app, so validation belongs to this command rather
// than the shared configuration loader.
func validateIngestTopology(e *env) error {
	if len(e.cfg.Ingest.Shards) == 0 {
		return fmt.Errorf("FEASIBLE_INGEST_SHARDS: ingest requires at least one app shard")
	}
	if len(e.cfg.Shared.InternalKeys) == 0 {
		return fmt.Errorf("FEASIBLE_INTERNAL_KEYS: ingest requires at least one signing key")
	}

	return nil
}
