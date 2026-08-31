//
// seed.go
// The `seed` subcommand: realistic fake traffic, in bulk or over the wire.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/httpserver"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/seed"
)

const seedHelp = `feasible seed — generate realistic fake traffic.

Nothing can be built or measured against an empty database: every report card,
filter, comparison and empty state needs data, and the performance numbers in
the plan stay estimates until something generates enough rows to time.

The generator calls the same functions the ingest path calls — the fingerprint,
the user-agent parser, the referrer and channel rules, the session fold — and
skips only the network. Geolocation comes from the country distribution rather
than the database, because the lookup is not what a seeded dataset is for.

Every dimension is power-law distributed, the week and the day have a shape, and
the deliberately awkward cases are always present: a day with no traffic, a
spike, a page with one pageview, a thirty-property event, revenue in three
currencies, a VPN visitor, a locked account, a dormant one and a site with no
data at all.

  feasible seed                                 six weeks across the fixture
  feasible seed --pageviews 1000000 --days 30 --sites 1
  feasible seed --http                          a few hundred events over HTTP

Flags:
`

// runSeed generates a dataset, or sends a couple of hundred events over the
// wire. They are two different tools with one name because they answer two
// halves of the same question — is there data to build against, and does the
// real path still work end to end — and nobody would remember two commands.
func runSeed(e *env, args []string) int {
	fs := newFlagSet("seed", e, seedHelp)

	pageviews := fs.Int64("pageviews", seed.DefaultPageviews, "total pageviews to generate across every site")
	days := fs.Int("days", seed.DefaultDays, "days of history, ending today")
	sites := fs.Int("sites", seed.DefaultSites, "how many sites carry traffic")
	rngSeed := fs.Int64("seed", seed.DefaultSeed, "random seed — the same value produces the same database")
	fresh := fs.Bool("fresh", false, "delete the seeded databases first")
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding control.db and the account databases")

	overWire := fs.Bool("http", false, "send events over real HTTP instead of generating in bulk")
	wireURL := fs.String("url", "", "post to an already-running instance instead of starting one")
	wireEvents := fs.Int("http-events", seed.DefaultHTTPEvents, "how many events the wire check sends")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	// Seeding writes fake traffic into whatever data directory it is pointed
	// at, and --fresh deletes what is there first. Neither belongs anywhere
	// near a production box, and the check is a refusal rather than a warning
	// because a warning scrolls past.
	if e.cfg.IsProduction() {
		fmt.Fprintln(e.stderr, "refusing to seed with FEASIBLE_ENV=production")
		return ExitError
	}

	ctx := context.Background()

	if *overWire {
		return runSeedHTTP(ctx, e, *dataDir, *wireURL, *wireEvents, *rngSeed)
	}

	e.log.Info("seeding",
		"data_dir", *dataDir,
		"pageviews", *pageviews,
		"days", *days,
		"sites", *sites,
		"seed", *rngSeed,
		"fresh", *fresh,
	)

	result, err := seed.Run(ctx, seed.Options{
		DataDir:   *dataDir,
		Pageviews: *pageviews,
		Days:      *days,
		Sites:     *sites,
		Seed:      *rngSeed,
		Fresh:     *fresh,
		Out:       e.stdout,
		Log:       e.log,
	})
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	result.Report.Write(e.stdout)

	fmt.Fprintf(e.stdout, "\n%d events (%d pageviews) across %d visits in %s — %.0f events/second\n",
		result.Events, result.Pageviews, result.Sessions,
		result.Duration.Round(time.Millisecond),
		float64(result.Events)/result.Generating.Seconds(),
	)

	// The phases are reported separately because they answer different
	// questions: whether the generator is fast enough to run, what a bulk index
	// build costs, and how long a first real query takes on the result.
	fmt.Fprintf(e.stdout, "generating %s · rebuilding indexes %s · verifying %s\n",
		result.Generating.Round(time.Millisecond),
		result.Indexing.Round(time.Millisecond),
		result.Verifying.Round(time.Millisecond),
	)

	if result.Dropped > 0 {
		// Dropped events are deliberate here — the dormant account's traffic is
		// meant to be refused — so they are reported rather than hidden.
		fmt.Fprintf(e.stdout, "%d events were dropped by the pipeline, which is what the dormant account is for\n", result.Dropped)
	}

	// A seed that quietly produced the wrong shape is worse than one that
	// failed: every measurement taken against it would be wrong in a way nobody
	// can see.
	if failed := result.Report.Failed(); len(failed) > 0 {
		fmt.Fprintf(e.stderr, "\n%d shape check(s) failed — the generated data is not what it claims to be\n", len(failed))
		return ExitError
	}

	return ExitOK
}

// runSeedHTTP sends a couple of hundred events over real HTTP. With no --url it
// starts a listener of its own on an ephemeral loopback port and stops it again
// before returning — including on failure, which is why the shutdown is both
// deferred and called explicitly once the events have been sent.
func runSeedHTTP(ctx context.Context, e *env, dataDir, url string, events int, rngSeed int64) int {
	now := time.Now().UTC()

	// The wire check needs a site that routes before it can send anything, and
	// making somebody generate six weeks of history first would make the quick
	// check the slow one.
	if url == "" {
		if err := seed.EnsureFixture(ctx, dataDir, now); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}
	}

	endpoint := url
	stop := func() {}

	if endpoint == "" {
		started, shutdown, err := startSeedServer(ctx, e, dataDir)
		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		// Deferred as well as called below, so a failure anywhere in the middle
		// still takes the listener down. A seed command that left a server
		// running would be a seed command nobody runs twice.
		defer shutdown()

		endpoint, stop = started, shutdown
	}

	domain := seed.PrimaryDomain()

	e.log.Info("sending events over http", "endpoint", endpoint, "domain", domain, "events", events)

	result, err := seed.SendHTTP(ctx, seed.HTTPOptions{
		Endpoint: endpoint,
		Domain:   domain,
		Events:   events,
		Seed:     rngSeed,
		Out:      e.stdout,
	})
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	// Stopping before counting is the point: the write buffer flushes on the
	// way down, and counting before that would measure what is still in memory.
	stop()

	result.Write(e.stdout)

	if result.Accepted == 0 {
		fmt.Fprintln(e.stderr, "no events were accepted — the path is not working end to end")
		return ExitError
	}

	// A 202 is not evidence that anything was written. The failure this check
	// exists to catch is an event that was accepted, said nothing and landed
	// nowhere, so the last word belongs to the database rather than the
	// response.
	written, err := seed.CountSince(ctx, dataDir, domain, now.Add(-time.Minute))
	if err != nil {
		fmt.Fprintf(e.stdout, "  stored    could not be counted here: %v\n", err)
		return ExitOK
	}

	fmt.Fprintf(e.stdout, "  stored    %d event(s) in %s\n", written, domain)

	if written == 0 {
		fmt.Fprintln(e.stderr, "events were accepted but none reached a database")
		return ExitError
	}

	return ExitOK
}

// startSeedServer brings up an ingest listener on an ephemeral loopback port
// and returns its URL with the function that takes it down. The shutdown runs
// once however many times it is called, so it can be both deferred and called
// at the point the events have landed.
func startSeedServer(ctx context.Context, e *env, dataDir string) (string, func(), error) {
	service, control, manager, err := buildIngest(ctx, e, dataDir)
	if err != nil {
		return "", nil, err
	}

	server := httpserver.New("seed", "127.0.0.1:0", ingestRoutes(service))

	if err := server.Listen(); err != nil {
		_ = manager.CloseAll()
		_ = control.Close()

		return "", nil, err
	}

	service.Start(ctx)

	go func() { _ = server.Serve() }()

	var once sync.Once

	shutdown := func() {
		once.Do(func() {
			grace, cancel := context.WithTimeout(context.Background(), httpserver.ShutdownGrace)
			defer cancel()

			// The order is what loses nothing: stop taking traffic, flush the
			// buffer and persist the sessions, then close the databases.
			_ = server.Shutdown(grace, 0)
			_ = service.Stop(grace)
			_ = manager.CloseAll()
			_ = control.Close()
		})
	}

	return "http://" + server.Addr(), shutdown, nil
}
