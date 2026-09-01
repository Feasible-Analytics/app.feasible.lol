//
// write.go
// Sustained events per second through the real accept path, into N account databases.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package bench

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// benchSaltKey pins the salt encryption key so a run never has to generate one,
// which would otherwise write a file into the data directory as a side effect
// of measuring something.
const benchSaltKey = "3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b3b"

// userAgents are the browsers the load is sent as. There are several because
// the user-agent cache is on the hot path, and a run with one string would
// measure a cache that always hits — which is not a number anybody can use.
var userAgents = []string{
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36",
	"Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1",
	"Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0",
	"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
}

// paths are the pages the load walks. A handful is enough: the dimension cache
// is per account, and a run against one path would measure it never missing.
var paths = []string{"/", "/pricing", "/docs", "/docs/install", "/blog", "/blog/why", "/signup", "/about"}

// referrers include the empty string, because direct traffic skips the referrer
// parse entirely and a load with none of it overstates the cost of an event.
var referrers = []string{"", "", "https://www.google.com/", "https://news.ycombinator.com/", "https://x.com/"}

// WriteOptions describes one load run.
type WriteOptions struct {
	// DataDir is where control.db and the account databases are written. It
	// should be empty, and it should be on the disk the answer is about.
	DataDir string

	// Accounts is how many account databases the traffic is spread across.
	// This is the axis that matters: one database is one write lock and one
	// WAL, and the question is what happens when a process is holding fifty.
	Accounts int

	// Events is how many to send in total, split evenly across the accounts.
	Events int

	// Visitors is how many distinct people the load comes from. It decides how
	// much of the work is session creation and how much is session lookup,
	// which are very different costs.
	Visitors int

	// BufferSize and FlushInterval override the buffer's bounds. Zero means the
	// production default, which is what a run should normally measure.
	BufferSize    int
	FlushInterval time.Duration

	// Concurrency is how many requests may wait for durable acknowledgement at
	// once. Zero tracks BufferSize, because production batching requires enough
	// simultaneous requests to fill a batch while each caller waits for commit.
	Concurrency int

	// ControlMigrations lets tests select an actual embedded schema prefix when
	// the benchmark does not exercise later control tables. Zero uses the
	// production control migration set and retains its gap validation.
	ControlMigrations migrate.Set
}

// WriteResult is one load run's numbers.
type WriteResult struct {
	Events   int
	Accounts int

	// Elapsed covers accepting every event and flushing the last of them, so
	// the rate below is a sustained figure rather than the rate at which
	// events can be put into memory.
	Elapsed         time.Duration
	EventsPerSecond float64

	// Accept is how long one call to the event endpoint took, including the
	// durable SQLite commit required before its 202 response. It is the number
	// the visitor's browser experiences as account concurrency increases.
	Accept Latencies

	// Flush is how long one buffer flush took: a batch of up to BufferSize
	// events, grouped by account and written in one transaction each.
	Flush Latencies

	Batches int
	Dropped int64

	// Written is how many event rows the account databases actually hold when
	// the run finishes. It must equal Events, or the rate above is a rate for
	// something other than writing.
	Written int64
}

// timing wraps a transport to record how long each flush took. It sits outside
// the buffer rather than inside the writer so that what it measures is exactly
// what the buffer waits on.
type timing struct {
	inner ingest.Transport

	mu        sync.Mutex
	durations []time.Duration
}

// Send times one flush.
func (t *timing) Send(ctx context.Context, shard int, batch []ingest.Event) ([]uuid.UUID, error) {
	started := time.Now()
	committed, err := t.inner.Send(ctx, shard, batch)
	took := time.Since(started)

	t.mu.Lock()
	t.durations = append(t.durations, took)
	t.mu.Unlock()

	return committed, err
}

// samples copies the recorded flush times out.
func (t *timing) samples() []time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	return append([]time.Duration(nil), t.durations...)
}

// RunWrite sends a load through the real accept path — the same handler, the
// same derivation, the same buffer and the same writer a request takes — and
// reports what it cost.
//
// It drives the handler directly rather than over a socket on purpose. The
// question is what the storage layer can absorb, and a loopback round trip per
// event would put the kernel's scheduler in the middle of the answer.
func RunWrite(ctx context.Context, opts WriteOptions) (WriteResult, error) {
	if opts.Accounts < 1 {
		opts.Accounts = 1
	}
	if opts.Events < 1 {
		opts.Events = 1
	}
	if opts.Visitors < 1 {
		opts.Visitors = 1000
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = opts.BufferSize
		if opts.Concurrency < 1 {
			opts.Concurrency = ingest.DefaultBufferSize
		}
	}
	if opts.Concurrency > opts.Events {
		opts.Concurrency = opts.Events
	}

	control, err := newControl(ctx, opts.DataDir, opts.Accounts, opts.ControlMigrations)
	if err != nil {
		return WriteResult{}, err
	}
	defer control.Close()

	manager := accounts.NewManager(opts.DataDir)
	defer func() { _ = manager.CloseAll() }()

	service, err := ingest.NewService(ctx, control, manager, ingest.Options{
		DataDir: opts.DataDir,
		SaltKey: benchSaltKey,
	})
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = service.Stop(ctx) }()

	flushes := &timing{inner: ingest.NewDirect(service.Writer)}

	service.Buffer = ingest.NewBuffer(flushes, opts.BufferSize, opts.FlushInterval)
	service.Handler.Buffer = service.Buffer

	// This benchmark measures durable account writes, not abuse protection. All
	// direct requests originate from httptest's one synthetic peer, so leaving
	// the public limiter enabled would measure that fixture instead of SQLite.
	service.Handler.Limiter = nil

	// The background loops run, because they run in production: without the
	// ticker the only thing that ever writes is the size trigger, and a size
	// trigger that is already flushing skips rather than queueing — so a load
	// with no ticker measures a buffer that grows instead of one that drains.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	service.Start(runCtx)

	accepts := make([]time.Duration, opts.Events)

	started := time.Now()

	// Public requests wait for a durable commit, so a sequential driver can
	// only produce one-event timer flushes. A bounded worker set represents the
	// concurrent requests that let the production buffer form real batches.
	jobs := make(chan int)
	var workers sync.WaitGroup
	var requestErr error
	var requestErrOnce sync.Once

	for i := 0; i < opts.Concurrency; i++ {
		workers.Add(1)

		go func() {
			defer workers.Done()

			for eventIndex := range jobs {
				request := buildRequest(eventIndex, opts).WithContext(ctx)
				recorder := httptest.NewRecorder()

				at := time.Now()
				service.Handler.ServeHTTP(recorder, request)
				accepts[eventIndex] = time.Since(at)

				if recorder.Code != http.StatusAccepted {
					requestErrOnce.Do(func() {
						requestErr = fmt.Errorf("bench: event %d answered %d, want 202: %s",
							eventIndex, recorder.Code, recorder.Body.String())
					})
				}
			}
		}()
	}

	for i := 0; i < opts.Events; i++ {
		jobs <- i
	}
	close(jobs)
	workers.Wait()

	if requestErr != nil {
		return WriteResult{}, requestErr
	}

	// The last partial batch is still in memory when the final request returns,
	// and a rate that excluded it would be the rate at which events reach a
	// slice rather than a database.
	if err := service.Buffer.Flush(ctx); err != nil {
		return WriteResult{}, err
	}

	elapsed := time.Since(started)

	var dropped int64
	for _, count := range service.Counters.Snapshot().Dropped {
		dropped += count.Count
	}

	// Counting the rows back is what makes the rate a measurement rather than a
	// claim: a load that reported a wonderful number and wrote nothing would
	// otherwise look exactly like a fast one.
	written, err := countEvents(ctx, manager, opts.Accounts)
	if err != nil {
		return WriteResult{}, err
	}

	flushSamples := flushes.samples()

	return WriteResult{
		Events:          opts.Events,
		Accounts:        opts.Accounts,
		Elapsed:         elapsed,
		EventsPerSecond: float64(opts.Events) / elapsed.Seconds(),
		Accept:          summarise(accepts),
		Flush:           summarise(flushSamples),
		Batches:         len(flushSamples),
		Dropped:         dropped,
		Written:         written,
	}, nil
}

// countEvents reads back how many rows actually landed, across every account
// the load wrote to.
func countEvents(ctx context.Context, manager *accounts.Manager, count int) (int64, error) {
	var total int64

	for i := 0; i < count; i++ {
		lease, err := manager.Acquire(ctx, int64(i+1))
		if err != nil {
			return 0, err
		}

		var events int64
		if err := lease.Account.Reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM events").Scan(&events); err != nil {
			_ = lease.Release()
			return 0, fmt.Errorf("bench: count events: %w", err)
		}
		if err := lease.Release(); err != nil {
			return 0, fmt.Errorf("bench: release account: %w", err)
		}

		total += events
	}

	return total, nil
}

// buildRequest makes one event, deterministically from its index so that two
// runs of the same size send the same traffic and can be compared.
func buildRequest(i int, opts WriteOptions) *http.Request {
	account := i % opts.Accounts
	visitor := i % opts.Visitors

	domain := domainFor(account)
	path := paths[i%len(paths)]

	body := fmt.Sprintf(`{"n":"pageview","u":"https://%s%s","d":"%s","r":"%s"}`,
		domain, path, domain, referrers[i%len(referrers)])

	// text/plain, because that is what the trackers send: it avoids a CORS
	// preflight, so it is the content type the real load arrives with.
	request := httptest.NewRequest(http.MethodPost, "/api/event", strings.NewReader(body))
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("User-Agent", userAgents[visitor%len(userAgents)])
	request.Header.Set("X-Forwarded-For", addressFor(visitor))

	return request
}

// domainFor names one account's only site.
func domainFor(account int) string {
	return fmt.Sprintf("site-%d.example", account)
}

// addressFor spreads the visitors over 198.18.0.0/15, the benchmarking range,
// so no generated address can geolocate to somewhere real.
func addressFor(visitor int) string {
	return fmt.Sprintf("198.18.%d.%d", (visitor/250)%256, visitor%250)
}

// newControl builds the control database the load routes against: one team and
// one site per account, which is the shape that puts every write on its own
// database file and its own lock.
func newControl(ctx context.Context, dataDir string, count int, controlMigrations migrate.Set) (*sql.DB, error) {
	db, err := store.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		return nil, err
	}

	if controlMigrations.Name == "" {
		controlMigrations = migrate.Control()
	}

	if _, err := migrate.Run(ctx, db, controlMigrations); err != nil {
		db.Close()
		return nil, err
	}

	now := time.Now().Unix()

	for i := 0; i < count; i++ {
		id := int64(i + 1)

		if _, err := db.ExecContext(ctx,
			"INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, ?, ?, ?)",
			id, fmt.Sprintf("Account %d", id), now, now,
		); err != nil {
			db.Close()
			return nil, err
		}

		if _, err := db.ExecContext(ctx,
			"INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
			id, id, domainFor(i), now, now,
		); err != nil {
			db.Close()
			return nil, err
		}
	}

	return db, nil
}
