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
	// DataDir is where the geolocation databases, the refreshed bot lists and
	// the session snapshot live.
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

	// Now replaces the system clock everywhere in the pipeline. It exists for
	// the replay harness, which has to drive a stream across a UTC midnight
	// without waiting for one, and it has to reach the salt store before its
	// first refresh — a store built on the real clock would create the wrong
	// day's salt and then refuse to load it.
	Now func() time.Time

	Log *logger.Logger
}

// Service is the whole ingest path assembled: the site cache, the salts, the
// derive pipeline, the write buffer, the transport and the shard writer. It
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

	dataDir string
	log     *logger.Logger
	now     func() time.Time

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

	if _, err := saltStore.Refresh(ctx); err != nil {
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

	// An engagement ping whose pageview never arrived is a real dropped event,
	// and the sweep is the only place that fact is ever established — by the
	// time it expires the response was sent half an hour ago, so the counter is
	// the only way the customer hears about it at all.
	sessionCache.OnOrphanExpired = func(event *Event) {
		counters.Dropped(event.SiteID, ReasonNoSessionForEngage)

		if opts.Log != nil {
			opts.Log.EventReceived("", itoa(event.SiteID), "", ReasonNoSessionForEngage)
		}
	}

	// A dirty session evicted long after its last event means a write never
	// landed. That is data loss rather than housekeeping, so it is logged as an
	// error even though nothing can be done about it here.
	sessionCache.OnSessionAbandoned = func(session *Session) {
		if opts.Log != nil {
			opts.Log.Error("a session was evicted before its rows were written",
				"site", session.SiteID, "session", session.ID, "events", session.Events)
		}
	}

	writer := NewWriter(manager, sessionCache)
	writer.Now = now

	service := &Service{
		now:      now,
		Sites:    siteCache,
		Salts:    saltStore,
		Geo:      locator,
		Agents:   useragent.NewCache(useragent.DefaultCapacity, useragent.DefaultTTL),
		Bots:     bots,
		Counters: counters,
		Writer:   writer,
		dataDir:  opts.DataDir,
		log:      opts.Log,
	}

	service.Pipeline = &Pipeline{
		Sites:    siteCache,
		Salts:    saltStore,
		Geo:      locator,
		Agents:   service.Agents,
		Bots:     bots,
		Trusted:  trusted,
		Shards:   DirectShard{},
		Shield:   NoShield{},
		Counters: counters,
		Now:      now,
	}

	// Every event flows accept → derive → buffer → forward → write, even when
	// "forward" is a function call in the same process. Exercising the seam
	// from day one is the entire point of having it.
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
		Log:      opts.Log,
	}

	// Reloading the session cache is what stops a restart splitting every
	// in-flight session in two.
	restored, err := RestoreSessions(sessionCache, SessionFilePath(opts.DataDir), now().Unix())
	if err != nil && opts.Log != nil {
		opts.Log.Warn("session cache could not be restored", "error", err)
	}
	if restored > 0 && opts.Log != nil {
		opts.Log.Info("session cache restored", "sessions", restored)
	}

	return service, nil
}

// Start launches the background loops: the buffer flush, the salt refresh, the
// site-cache refresh and the session sweep. Each one runs independently,
// because a process that stopped refreshing its salts would fragment sessions
// with nothing reporting an error.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		runCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel

		s.run(func() { s.Buffer.Run(runCtx) })
		s.run(func() { s.Salts.Run(runCtx, s.logError("salt refresh failed")) })
		s.run(func() { s.Sites.Run(runCtx, s.logError("site cache refresh failed")) })
		s.run(func() { s.sweep(runCtx) })
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

// sweep drops expired sessions on a ticker. An abandoned session is never
// touched again by definition, so nothing else in the system would ever notice
// it and the cache would grow for the life of the process.
func (s *Service) sweep(ctx context.Context) {
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Writer.Sessions().Sweep(s.now().Unix())
		}
	}
}

// Stop drains everything in the order that loses nothing: flush the write
// buffer first so every accepted event reaches a database, then persist the
// live session cache so a restart does not split visits in two.
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

		if persistErr := PersistSessions(s.Writer.Sessions(), SessionFilePath(s.dataDir)); persistErr != nil && err == nil {
			err = persistErr
		}

		if s.Geo != nil {
			if closeErr := s.Geo.Close(); closeErr != nil && err == nil {
				err = closeErr
			}
		}
	})

	return err
}
