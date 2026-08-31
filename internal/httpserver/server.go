//
// server.go
// One HTTP listener with timeouts, health probes and a graceful stop.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package httpserver is the one place an HTTP listener is configured. Both the
// app and the ingest tier use it, so that timeouts, the shutdown grace period
// and the shape of the health probes are the same on every process rather than
// whatever each one happened to be written with.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Timeouts. They exist because Go's zero value for all of them is "no timeout",
// which is how a process ends up holding thousands of connections that will
// never send anything.
const (
	// ReadHeaderTimeout is the guard against a connection that opens and then
	// says nothing. It is the shortest of the three because a header is small
	// and immediate.
	ReadHeaderTimeout = 5 * time.Second

	// ReadTimeout and WriteTimeout are generous enough for a slow mobile
	// connection posting an event and short enough to cap the damage.
	ReadTimeout  = 15 * time.Second
	WriteTimeout = 30 * time.Second

	// IdleTimeout closes a kept-alive connection nobody is using.
	IdleTimeout = 90 * time.Second
)

// ShutdownGrace is how long in-flight requests have to finish. Fifteen seconds
// is long enough for a slow write to land and short enough that a deploy does
// not stall on one hung connection.
const ShutdownGrace = 15 * time.Second

// Health paths. They are separate probes because they answer different
// questions, and a deployment that conflates them either restarts a process
// that was merely busy or sends traffic to one that is not ready.
const (
	// PathLive answers "is this process alive". It is true from the moment the
	// listener is up and stays true until the process exits.
	PathLive = "/health/live"

	// PathReady answers "may this process take traffic". It goes false the
	// instant a shutdown begins, so a load balancer drains the process before
	// the listener closes.
	PathReady = "/health/ready"
)

// Server is one listener and its lifecycle. It is a struct rather than a
// function so that shutdown can flip the readiness flag before it stops
// accepting, which is the whole of a zero-downtime deploy.
type Server struct {
	name   string
	server *http.Server

	// ready is read by the readiness probe on every request, so it is atomic
	// rather than guarded: a mutex here would serialise the health checks of
	// every replica behind one lock.
	ready atomic.Bool

	// Ready is an extra condition the process supplies — the site cache being
	// loaded, for example. Returning false keeps traffic away without the
	// process appearing dead.
	Ready func() bool

	listener net.Listener
}

// New builds a server around a handler, wrapping it with the health probes.
// The probes are added here rather than by each caller so that every process in
// the system answers the same two paths in the same way.
func New(name, addr string, handler http.Handler) *Server {
	s := &Server{name: name}

	mux := http.NewServeMux()
	mux.HandleFunc(PathLive, s.handleLive)
	mux.HandleFunc(PathReady, s.handleReady)
	mux.Handle("/", handler)

	s.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
	}

	return s
}

// Listen binds the address without serving. Binding early is what turns "port
// already in use" into an error at start-up, next to the log line that says
// which port it was, rather than a goroutine failing silently a moment later.
func (s *Server) Listen() error {
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("%s: listen on %s: %w", s.name, s.server.Addr, err)
	}

	s.listener = listener
	s.ready.Store(true)

	return nil
}

// Addr reports the address actually bound, which matters when the configured
// port was zero and the kernel chose one — the shape every test uses.
func (s *Server) Addr() string {
	if s.listener == nil {
		return s.server.Addr
	}

	return s.listener.Addr().String()
}

// Serve accepts connections until the server is stopped. A closed server is a
// normal end rather than a failure, so that a caller can treat any non-nil
// error as something worth reporting.
func (s *Server) Serve() error {
	if s.listener == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}

	if err := s.server.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s: %w", s.name, err)
	}

	return nil
}

// Shutdown drains the server. Readiness is flipped first and the process then
// waits a moment before refusing connections, so a load balancer has time to
// notice and stop sending — without that pause a rolling deploy drops the
// requests that were already in flight towards this replica.
func (s *Server) Shutdown(ctx context.Context, drain time.Duration) error {
	s.ready.Store(false)

	if drain > 0 {
		timer := time.NewTimer(drain)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-ctx.Done():
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ShutdownGrace)
	defer cancel()

	if err := s.server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("%s: shutdown: %w", s.name, err)
	}

	return nil
}

// handleLive answers the liveness probe. It deliberately checks nothing: a
// liveness probe that fails on a downstream dependency turns one slow database
// into a restart loop across every process at once.
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleReady answers the readiness probe, including whatever extra condition
// the process supplied.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")

	if !s.ready.Load() || (s.Ready != nil && !s.Ready()) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("draining\n"))
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}
