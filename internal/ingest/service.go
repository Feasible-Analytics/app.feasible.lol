//
// service.go
// Wiring the ingest pipeline together and taking it down without losing anything.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/geo"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/salts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/useragent"
)

// Options configures a Service. Everything here is a decision somebody could
// get wrong in a way that is invisible until it has cost a day, which is why
// each one is named rather than defaulted silently inside the constructor.
type Options struct {
	// DataDir is where the geolocation databases and refreshed bot lists live.
	// Session fold ownership is stored in each account database.
	DataDir string

	// TrustedProxies may set X-Feasible-IP. Empty means nobody, which is the
	// safe default for an instance exposed straight to the internet.
	TrustedProxies []string

	// SaltKey is the hex-encoded key that encrypts the salts table. Empty means
	// generate one under the data directory on first run, so a self-hoster who
	// configures nothing still gets encryption at rest.
	SaltKey string

	BufferSize    int
	FlushInterval time.Duration

	// Usage counts the billable volume an account stores. It is optional
	// because a self-hosted install has no billing, and ingestion must never
	// depend on billing existing.
	Usage UsageRecorder

	// Now replaces the system clock everywhere in the pipeline. It exists for
	// the replay harness, which has to drive a stream across a UTC midnight
	// without waiting for one, and it has to reach the salt store before its
	// first refresh — a store built on the real clock would create the wrong
	// day's salt and then refuse to load it.
	Now func() time.Time

	Log *logger.Logger
}

// Service is the whole ingest path assembled: the site cache, the salts, the
// derive pipeline, the write buffer, the direct transport and account writer. It
// exists so that both `feasible serve` in direct mode and `feasible ingest`
// build the same thing from the same code rather than two wirings that drift.
type Service struct {
	Sites    *sites.Cache
	Salts    *salts.Store
	Geo      geo.Locator
	Agents   *useragent.Cache
	Bots     *BotFilter
	Counters *Counters
	Pipeline *Pipeline
	Writer   *Writer
	Buffer   *Buffer
	Handler  *Handler
	Limiter  *RateLimiter

	log *logger.Logger

	// started guards the background goroutines so a double Start or a Stop
	// before Start cannot panic on a closed channel.
	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      sync.WaitGroup
}

// NewService builds the pipeline over an already-open control database and
// account manager. It reads the site list and the salts before returning,
// because a process that starts accepting traffic before it knows which domains
// it serves would drop everything as an unknown site.
func NewService(ctx context.Context, control *sql.DB, manager *accounts.Manager, opts Options) (*Service, error) {
	trusted, err := ParseTrustedProxies(opts.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("trusted proxies: %w", err)
	}

	key, err := salts.LoadKey(opts.DataDir, opts.SaltKey)
	if err != nil {
		return nil, err
	}

	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	saltStore, err := salts.NewStore(control, key)
	if err != nil {
		return nil, err
	}
	saltStore.SetClock(now)

	initialPair, err := saltStore.Refresh(ctx)
	initialPair.Erase()
	if err != nil {
		return nil, err
	}

	siteCache := sites.New(control)
	if err := siteCache.Refresh(ctx); err != nil {
		return nil, err
	}

	// A missing geolocation database degrades to unknown rather than failing.
	// An optional data file must never stop the app booting, and a country map
	// that is grey is a smaller problem than a process that will not start.
	locator, err := geo.Open(opts.DataDir)
	if err != nil {
		if opts.Log != nil {
			opts.Log.Warn("geolocation unavailable — countries will be unknown", "error", err)
		}
		locator = geo.Unknown{}
	}

	bots := NewBotFilter()
	if err := bots.LoadLists(opts.DataDir); err != nil && opts.Log != nil {
		opts.Log.Warn("bot lists could not be read — using the built-in baseline", "error", err)
	}

	counters := NewCounters()
	sessionCache := NewSessionCache()

	writer := NewWriter(manager, sessionCache)
	writer.Now = now
	writer.Usage = opts.Usage

	service := &Service{
		Sites:    siteCache,
		Salts:    saltStore,
		Geo:      locator,
		Agents:   useragent.NewCache(useragent.DefaultCapacity, useragent.DefaultTTL),
		Bots:     bots,
		Counters: counters,
		Writer:   writer,
		log:      opts.Log,
		Limiter:  NewRateLimiter(DefaultEventRate, DefaultEventBurst),
	}

	service.Pipeline = &Pipeline{
		Sites:     siteCache,
		Salts:     saltStore,
		Geo:       locator,
		Agents:    service.Agents,
		Bots:      bots,
		Trusted:   trusted,
		Shards:    DirectShard{},
		Shield:    NoShield{},
		Hostnames: NoHostnamePolicy{},
		Counters:  counters,
		Now:       now,
	}

	// Every event flows accept → derive → buffer → direct account write. Keeping
	// the transport seam makes batching testable without implying a network hop.
	service.Buffer = NewBuffer(NewDirect(writer), opts.BufferSize, opts.FlushInterval)
	service.Buffer.OnError = func(err error) {
		if opts.Log != nil {
			opts.Log.Error("write buffer flush failed", "error", err)
		}
	}

	service.Handler = &Handler{
		Pipeline: service.Pipeline,
		Buffer:   service.Buffer,
		Counters: counters,
		Limiter:  service.Limiter,
		Durable:  true,
		Log:      opts.Log,
	}

	return service, nil
}

// SetObserver attaches one observer to both halves of ingestion. The handler
// records request diagnostics and derive-time drops; the writer records the
// final accepted, classified, shielded and orphaned outcomes. Keeping the two
// assignments together prevents direct and standalone process wiring from
// quietly exposing different health histories.
func (s *Service) SetObserver(observer Observer) {
	s.Handler.Observer = observer
	s.Writer.Observer = observer
}

// Start launches the buffer, salt, site, and source-address limiter loops. Live
// session ownership is transactional account state, so there is no process-
// local session sweep to coordinate across serving processes.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		runCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel

		s.run(func() { s.Buffer.Run(runCtx) })
		s.run(func() { s.Salts.Run(runCtx, s.logError("salt refresh failed")) })
		s.run(func() { s.Sites.Run(runCtx, s.logError("site cache refresh failed")) })
		s.run(func() { s.Limiter.Run(runCtx) })
	})
}

// run starts one background loop and records it so Stop can wait for it.
func (s *Service) run(fn func()) {
	s.done.Add(1)

	go func() {
		defer s.done.Done()
		fn()
	}()
}

// logError adapts a message into the error callback the background loops take.
func (s *Service) logError(message string) func(error) {
	return func(err error) {
		if s.log != nil {
			s.log.Error(message, "error", err)
		}
	}
}

// Stop drains everything in the order that loses nothing: flush the write
// buffer so every accepted event reaches an account database. Session fold
// state commits with each fact transaction and needs no shutdown snapshot.
func (s *Service) Stop(ctx context.Context) error {
	var err error

	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}

		// The background loops flush on their way out; waiting for them is what
		// makes the shutdown ordered rather than hopeful.
		s.done.Wait()

		if flushErr := s.Buffer.Flush(ctx); flushErr != nil {
			err = flushErr
		}

		if s.Geo != nil {
			if closeErr := s.Geo.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
		}
	})

	return err
}
