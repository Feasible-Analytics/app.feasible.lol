//
// runtime.go
// Opening control.db, building the ingest service and running a listener until a signal.
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
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/httpserver"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/rollup"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/tracker"
)

// drainDelay is how long a process keeps serving after readiness goes false. It
// gives a load balancer time to notice and stop sending, which is the whole of
// a zero-downtime deploy; without it the requests already in flight towards
// this replica are the ones that get dropped.
const drainDelay = 2 * time.Second

// openControl opens control.db and refuses one the binary cannot read. It
// refuses rather than migrating, for the same reason migrations never run on
// boot: two processes racing them is a classic self-hosting failure, and with
// one database per account the operation has to be deliberate.
func openControl(ctx context.Context, dataDir string) (*sql.DB, error) {
	path := filepath.Join(dataDir, config.ControlDatabaseName)

	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}

	version, err := store.SchemaVersion(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}

	expected := migrate.Control().Version()

	if version < expected {
		db.Close()
		return nil, fmt.Errorf("%s is at schema version %d and this build expects %d — run `feasible db migrate`", path, version, expected)
	}
	if version > expected {
		db.Close()
		return nil, fmt.Errorf("%s is at schema version %d but this build only knows up to %d", path, version, expected)
	}

	return db, nil
}

// buildIngest assembles the ingest service from the resolved configuration.
// Both `serve` in direct mode and `ingest` call it, so the two processes cannot
// end up with pipelines that differ in a way nobody notices until the numbers
// disagree.
func buildIngest(ctx context.Context, e *env, dataDir string) (*ingest.Service, *sql.DB, *accounts.Manager, error) {
	control, err := openControl(ctx, dataDir)
	if err != nil {
		return nil, nil, nil, err
	}

	manager := accounts.NewManager(dataDir)

	service, err := ingest.NewService(ctx, control, manager, ingest.Options{
		DataDir:        dataDir,
		TrustedProxies: e.cfg.Ingest.TrustedProxies,
		SaltKey:        e.cfg.Shared.SaltKey,
		Log:            e.log,
	})
	if err != nil {
		control.Close()
		return nil, nil, nil, err
	}

	return service, control, manager, nil
}

// serveUntilSignal runs a listener until SIGINT or SIGTERM, then shuts
// everything down in order. Returning an exit code rather than calling os.Exit
// is what lets the whole command be driven from a test.
//
// The roll-up worker is optional so that the ingest-only process, which has no
// reports to make fast, does not summarise anything.
func serveUntilSignal(e *env, server *httpserver.Server, service *ingest.Service, worker *rollup.Worker, closers ...func() error) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := server.Listen(); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	service.Start(ctx)

	if worker != nil {
		go worker.Run(ctx)
	}

	errs := make(chan error, 1)
	go func() { errs <- server.Serve() }()

	e.log.Info("listening",
		"addr", server.Addr(),
		"sites", service.Sites.Len(),
		"env", e.cfg.Shared.Env,
		"trace_events", e.cfg.Shared.TraceEvents,
	)

	code := ExitOK

	select {
	case err := <-errs:
		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			code = ExitError
		}

	case <-ctx.Done():
		e.log.Info("shutting down")
	}

	// The order matters and is the difference between a clean stop and losing
	// half a second of every deploy: stop taking traffic, then flush what is
	// buffered, then persist the session cache, then close the databases.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpserver.ShutdownGrace+drainDelay)
	defer cancel()

	if err := server.Shutdown(shutdownCtx, drainDelay); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		code = ExitError
	}

	if err := service.Stop(shutdownCtx); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		code = ExitError
	}

	for _, closer := range closers {
		if err := closer(); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			code = ExitError
		}
	}

	return code
}

// ingestRoutes is the public surface of the ingest tier.
//
// The noscript pixel is mounted here beside the scripted endpoint rather than
// on the app, because it *is* an event: it goes through the same handler, the
// same derivation and the same buffer, and a visitor with JavaScript disabled
// must not become a second code path with its own bugs.
func ingestRoutes(service *ingest.Service) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/event", service.Handler)
	mux.Handle(tracker.PixelPath, &tracker.Pixel{Events: service.Handler})

	return mux
}
