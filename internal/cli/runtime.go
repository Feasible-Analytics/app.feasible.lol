//
// runtime.go
// Opening system.db, building the ingest service and running a listener until a signal.
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
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/geo"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/health"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/httpserver"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/metrics"
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

// openSystem opens system.db and refuses one the binary cannot read. It
// refuses rather than migrating, for the same reason migrations never run on
// boot: two processes racing them is a classic self-hosting failure, and with
// one database per account the operation has to be deliberate.
func openSystem(ctx context.Context, dataDir string) (*sql.DB, error) {
	path := filepath.Join(dataDir, config.SystemDatabaseName)
	legacy := filepath.Join(dataDir, config.LegacyDatabaseName)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if _, legacyErr := os.Stat(legacy); legacyErr == nil {
			return nil, fmt.Errorf("legacy database %s must be renamed to %s — stop every feasible process and run `feasible db migrate`", legacy, path)
		}
	}

	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}

	version, err := store.SchemaVersion(ctx, db)
	if err != nil {
		db.Close()
		return nil, err
	}

	expected := migrate.System().Version()

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
	control, err := openSystem(ctx, dataDir)
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

// attachIngestRecorder keeps the direct and standalone topologies on the same
// observation wiring. Both the request-side handler and final-outcome writer
// must report to one recorder or the health panel either loses diagnostics or
// claims buffered events were stored before the shard decided their fate.
func attachIngestRecorder(service *ingest.Service, recorder *health.Recorder) {
	service.SetObserver(recorder)
}

// ingestHealth registers what any process that accepts events depends on. Both
// process shapes call it, so neither can end up with a readiness probe that
// checks less than the other; what differs between them is registered by the
// caller.
//
// The geolocation database is deliberately optional. It is a data file this
// system is designed to run without — a missing one means countries are
// unknown, not that events are lost — and a readiness probe that failed on it
// would turn a downgraded dashboard into an outage.
func ingestHealth(checks *health.Set, control *sql.DB, service *ingest.Service, dataDir string) {
	checks.Require("system_db", health.Database(control))

	// Every account database is created under here on an account's first
	// event, so a directory that cannot be written to is a process that will
	// accept traffic and then fail to store it.
	checks.Require("account_directory", health.Directory(filepath.Join(dataDir, config.AccountDatabaseDir)))

	// Without a salt there is no visitor id and therefore no event. Readiness
	// fails and requests receive a retryable 503 without changing counters.
	checks.Require("salts", func(ctx context.Context) error {
		pair, err := service.Salts.Pair(ctx)
		pair.Erase()
		return err
	})

	checks.Optional("geolocation", health.Condition(func() bool {
		_, missing := service.Geo.(geo.Unknown)
		return !missing
	}, "no geolocation database is loaded — countries will be unknown"))
}

// watchProcess tells the metrics endpoint what this process can report on. The
// gauges are read on each scrape rather than pushed, so this is a set of
// accessors rather than a copy of anything.
//
// The job queue is passed separately because only the process that runs the
// worker should be reporting on it: an ingestor answering "no jobs are waiting"
// about a queue it does not run would be an all-clear from the wrong place.
func watchProcess(service *ingest.Service, manager *accounts.Manager, dataDir string, jobs func(context.Context) (metrics.JobCounts, error)) {
	bufferDepth := func() int { return service.Buffer.Len() }
	var bufferOldest func() time.Duration
	var bufferParked func() int
	var openAccounts func() int
	if service.Outbox != nil {
		bufferDepth = service.Outbox.Len
		bufferOldest = service.Outbox.OldestAge
		bufferParked = service.Outbox.Parked
	}
	if manager != nil {
		openAccounts = manager.OpenCount
	}
	metrics.Watch(metrics.Sources{
		BufferDepth: bufferDepth, BufferOldest: bufferOldest, BufferParked: bufferParked, Sites: service.Sites.Len,
		OpenAccounts: openAccounts, Jobs: jobs, DataDir: dataDir,
	})
}

// jobCounts reads the background queue's depth. The claim index covers the
// state prefix, so this is the same access pattern the worker already makes
// every few seconds rather than a new cost on the database.
func jobCounts(control *sql.DB) func(context.Context) (metrics.JobCounts, error) {
	return func(ctx context.Context) (metrics.JobCounts, error) {
		var counts metrics.JobCounts

		row := control.QueryRowContext(ctx, `
			SELECT
				COUNT(*) FILTER (WHERE state = 'available'),
				COUNT(*) FILTER (WHERE state = 'executing')
			FROM jobs`)

		if err := row.Scan(&counts.Available, &counts.Executing); err != nil {
			return metrics.JobCounts{}, fmt.Errorf("read the job queue: %w", err)
		}

		return counts, nil
	}
}

// processRoutes combines a process's customer-facing and operational handlers
// on one listener. Network policy and the edge proxy decide which paths are
// reachable externally; signed internal routes still authenticate every
// service request instead of treating socket placement as authentication.
func processRoutes(base http.Handler, internal ...http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	if len(internal) > 0 && internal[0] != nil {
		mux.Handle("/internal/", internal[0])
	}
	mux.Handle("/", base)

	return mux
}

// internalKeys converts configuration records into the protocol package's
// deliberately small signing type.
func internalKeys(keys []config.InternalKey) []ingest.InternalKey {
	converted := make([]ingest.InternalKey, 0, len(keys))
	for _, key := range keys {
		converted = append(converted, ingest.InternalKey{ID: key.ID, Secret: key.Secret})
	}

	return converted
}

// serveUntilSignalWith runs the listener until SIGINT or SIGTERM, then shuts
// everything down in order. Returning an exit code rather than calling os.Exit
// is what lets the whole command be driven from a test.
//
// The roll-up worker is optional so that the ingest-only process, which has no
// reports to make fast, does not summarise anything. The background hook
// carries the process's own loops — the billing sweeps, the
// usage flush, the access gate, the rule refreshes and the job runner. They are
// started here rather than by their own packages so that shutdown waits for
// them. The usage recorder flushes its last interval on the way out, and a
// process that exited without waiting would lose the events an account was
// billed for, every single deploy.
func serveUntilSignalWith(e *env, server *httpserver.Server, service *ingest.Service, worker *rollup.Worker, background func(context.Context, func(func())), closers ...func() error) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var loops sync.WaitGroup

	run := func(fn func()) {
		loops.Add(1)

		go func() {
			defer loops.Done()
			fn()
		}()
	}

	if err := server.Listen(); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	service.Start(ctx)

	// The roll-up worker is not waited on: it holds no unflushed state, and a
	// summary it did not finish is rebuilt on the next pass.
	if worker != nil {
		go worker.Run(ctx)
	}

	if background != nil {
		background(ctx, run)
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
	// half a second of every deploy: stop taking traffic, flush the in-memory
	// batch into its durable transport, then close databases. Session state is
	// already durable.
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

	// The loops observe the cancelled context and flush on their way out. The
	// databases below are closed only once they have, or the final usage flush
	// would land on a handle that is already gone.
	stop()
	loops.Wait()

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
	mux.Handle("/api/event", metrics.Instrument(metrics.HandlerEvent, service.Handler))
	mux.Handle(tracker.PixelPath, metrics.Instrument(metrics.HandlerEvent, &tracker.Pixel{Events: service.Handler}))

	return mux
}
