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
	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
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

	// TrustedProxies may supply X-Feasible-IP, CF-Connecting-IP and
	// X-Forwarded-For. Empty means all forwarded headers are ignored.
	TrustedProxies []string

	// IngestSalt is the shared input used to derive the current and previous UTC
	// day's fingerprint salts without contacting an app shard.
	IngestSalt string

	BufferSize    int
	FlushInterval time.Duration

	// Usage counts the billable volume an account stores. It is optional
	// because a self-hosted install has no billing, and ingestion must never
	// depend on billing existing.
	Usage UsageRecorder

	// Now replaces the system clock everywhere in the pipeline. It exists for
	// the replay harness, which has to drive a stream across a UTC midnight
	// without waiting for one, including the local daily salt derivation.
	Now func() time.Time

	Log *logger.Logger
}

// Service is the whole ingest path assembled: routing, salts, derivation, the
// in-memory batch, and either a direct writer or the standalone durable outbox.
// It exists so both serving modes share one pipeline rather than two wirings
// that drift.
type Service struct {
	Sites    *sites.Cache
	Salts    SaltSource
	Geo      geo.Locator
	Agents   *useragent.Cache
	Bots     *BotFilter
	Counters *Counters
	Pipeline *Pipeline
	Writer   *Writer
	Buffer   *Buffer
	Handler  *Handler
	Limiter  *RateLimiter
	Outbox   *Outbox

	log *logger.Logger

	// started guards the background goroutines so a double Start or a Stop
	// before Start cannot panic on a closed channel.
	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      sync.WaitGroup
	routing   func(context.Context, func(error))
	delivery  func(context.Context)
}

// NewService builds the pipeline over an already-open system database and
// account manager. It reads the site list before returning because a process
// that accepts traffic before it knows its domains would drop everything.
func NewService(ctx context.Context, control *sql.DB, manager *accounts.Manager, opts Options) (*Service, error) {
	trusted, err := clientip.ParseTrustedProxies(opts.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("trusted proxies: %w", err)
	}

	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	saltSource, err := salts.New(opts.IngestSalt)
	if err != nil {
		return nil, err
	}
	saltSource.SetClock(now)

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

	writer := NewWriter(manager)
	writer.Now = now
	writer.Usage = opts.Usage

	service := &Service{
		Sites:    siteCache,
		Salts:    saltSource,
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
		Salts:     saltSource,
		Geo:       locator,
		Agents:    service.Agents,
		Bots:      bots,
		Trusted:   trusted,
		Shards:    DirectShard{},
		Counters:  counters,
		Now:       now,
	}

	// Direct mode flows accept → derive → buffer → account write. Hosted
	// mode builds the same pipeline over the durable outbox below.
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

// NewRemoteService builds the standalone production ingest role. It owns no
// system or account database handle: routing arrives over the signed app
// protocol, while salts derive locally and events cross SQLite before 202.
func NewRemoteService(ctx context.Context, outbox *Outbox, opts Options) (*Service, error) {
	trusted, err := clientip.ParseTrustedProxies(opts.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("trusted proxies: %w", err)
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	saltSource, err := salts.New(opts.IngestSalt)
	if err != nil {
		return nil, err
	}
	saltSource.SetClock(now)
	if err := outbox.Router.RefreshAll(ctx); err != nil && outbox.Router.Cache.Len() == 0 && opts.Log != nil {
		opts.Log.Warn("no app shard routing snapshot is currently reachable — unknown domains will be held", "error", err)
	}

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
	service := &Service{
		Sites: outbox.Router.Cache, Salts: saltSource, Geo: locator,
		Agents: useragent.NewCache(useragent.DefaultCapacity, useragent.DefaultTTL),
		Bots:   bots, Counters: counters, Limiter: NewRateLimiter(DefaultEventRate, DefaultEventBurst),
		Outbox: outbox, log: opts.Log,
	}
	service.Pipeline = &Pipeline{
		Sites: outbox.Router, Salts: saltSource, Geo: locator, Agents: service.Agents,
		Bots: bots, Trusted: trusted, Shards: outbox.Router, Shield: outbox.Router,
		Hostnames: outbox.Router, Counters: counters, Now: now,
	}
	service.Buffer = NewBuffer(outbox, opts.BufferSize, opts.FlushInterval)
	service.Buffer.OnError = service.logError("durable outbox append failed")
	service.Handler = &Handler{
		Pipeline: service.Pipeline, Buffer: service.Buffer, Counters: counters,
		Limiter: service.Limiter, Durable: true, Log: opts.Log,
	}
	service.routing = outbox.Router.Run
	service.delivery = outbox.Run

	return service, nil
}

// SetObserver attaches one observer to both halves of ingestion. The handler
// records request diagnostics and derive-time drops; the writer records the
// final accepted, classified, shielded and orphaned outcomes. Keeping the two
// assignments together prevents direct and standalone process wiring from
// quietly exposing different health histories.
func (s *Service) SetObserver(observer Observer) {
	s.Handler.Observer = observer
	if s.Writer != nil {
		s.Writer.Observer = observer
	}
}

// Start launches the buffer, site, delivery, and source-address limiter loops. Live
// session ownership is transactional account state, so there is no process-
// local session sweep to coordinate across serving processes.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		runCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel

		s.run(func() { s.Buffer.Run(runCtx) })
		if s.routing != nil {
			s.run(func() { s.routing(runCtx, s.logError("shard routing refresh failed")) })
		} else {
			s.run(func() { s.Sites.Run(runCtx, s.logError("site cache refresh failed")) })
		}
		if s.delivery != nil {
			s.run(func() { s.delivery(runCtx) })
		}
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

// Stop drains everything in the order that loses nothing: flush the in-memory
// batch so every accepted event reaches its durable transport. Direct sessions
// commit with each fact; hosted events remain in buffer.db for a later sender.
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
