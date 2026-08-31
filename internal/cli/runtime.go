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
	checks.Require("control_db", health.Database(control))

	// Every account database is created under here on an account's first
	// event, so a directory that cannot be written to is a process that will
	// accept traffic and then fail to store it.
	checks.Require("account_directory", health.Directory(filepath.Join(dataDir, config.AccountDatabaseDir)))

	// Without a salt there is no visitor id and therefore no event: every
	// request would be accepted, counted as our own internal error, and
	// thrown away.
	checks.Require("salts", func(ctx context.Context) error {
		_, err := service.Salts.Pair(ctx)
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
	metrics.Watch(metrics.Sources{
		BufferDepth:  func() int { return service.Buffer.Len() },
		Sessions:     func() int { return service.Writer.Sessions().Len() },
		Sites:        service.Sites.Len,
		OpenAccounts: manager.OpenCount,
		Jobs:         jobs,
		DataDir:      dataDir,
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

// internalServer builds the loopback listener every process runs beside its
// public one: the metrics endpoint, and the same two health probes.
//
// Metrics live here rather than on the public listener because /metrics is an
// operations endpoint. Nothing on it is customer data, but our event rate,
// error rate and account count are not the internet's business, and no operator
// expects to have to firewall a path.
func internalServer(name, addr string, checks *health.Set) *httpserver.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())

	server := httpserver.New(name, addr, mux)
	server.Health = checks

	return server
}

// serveUntilSignalWith runs the listeners until SIGINT or SIGTERM, then shuts
// everything down in order. Returning an exit code rather than calling os.Exit
// is what lets the whole command be driven from a test.
//
// The roll-up worker is optional so that the ingest-only process, which has no
// reports to make fast, does not summarise anything. The internal listener is
// optional for the same reason a test does not want a second port bound.
//
// The background hook is where the process's own loops go — the billing
// sweeps, the usage flush, the access gate, the rule refresh and the import
// runner. They are started here rather than as bare goroutines so that
// shutdown waits for them: every one holds state somebody can see, and a
// process that exited without waiting would lose it on every deploy.
//
// The background hook carries the process's own loops — the billing sweeps, the
// usage flush, the access gate, the rule refreshes and the job runner. They are
// started here rather than by their own packages so that shutdown waits for
// them. The usage recorder flushes its last interval on the way out, and a
// process that exited without waiting would lose the events an account was
// billed for, every single deploy.
func serveUntilSignalWith(e *env, server, internal *httpserver.Server, service *ingest.Service, worker *rollup.Worker, background func(context.Context, func(func())), closers ...func() error) int {
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

	// The internal listener is bound before anything starts, so that a port
	// already in use is a start-up error naming the port rather than a metrics
	// endpoint that silently never came up.
	if internal != nil {
		if err := internal.Listen(); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		go func() {
			if err := internal.Serve(); err != nil {
				e.log.Error("the internal listener stopped", "error", err)
			}
		}()

		e.log.Info("internal listener", "addr", internal.Addr(), "metrics", "/metrics")
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
	// half a second of every deploy: stop taking traffic, then flush what is
	// buffered, then persist the session cache, then close the databases.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpserver.ShutdownGrace+drainDelay)
	defer cancel()

	if err := server.Shutdown(shutdownCtx, drainDelay); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		code = ExitError
	}

	// The internal listener goes last and without a drain: nothing is in front
	// of it, and keeping it up through the public drain means a scrape taken
	// during the shutdown still gets an answer.
	if internal != nil {
		if err := internal.Shutdown(shutdownCtx, 0); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			code = ExitError
		}
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
