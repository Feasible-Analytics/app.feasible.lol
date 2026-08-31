//
// health.go
// Alive and ready are different questions, and readiness has to say which part is not.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package health answers "can this process serve" as a list of components
// rather than a single yes.
//
// The split from liveness is the whole point. Liveness asks whether the process
// is running at all and must check nothing: a liveness probe that fails on a
// slow database turns one slow database into a restart loop across every
// replica at once. Readiness asks whether this process can do its job right
// now, and a load balancer needs both — the first decides whether to kill it,
// the second decides whether to send it traffic.
//
// A readiness failure that says only "not ready" costs whoever is holding the
// pager the first twenty minutes of an outage, so a report names every
// component and what was wrong with it.
package health

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// The states one component can be in.
//
// Degraded is not a failure and never keeps traffic away. It is for the things
// this system deliberately runs without: a missing geolocation database means
// countries are unknown, which is a worse dashboard, not a broken one, and a
// process that refused traffic over it would turn an optional data file into an
// outage.
const (
	StatusOK       = "ok"
	StatusDegraded = "degraded"
	StatusFailed   = "failed"
)

// The states the process as a whole can be in.
const (
	StatusReady    = "ready"
	StatusNotReady = "not_ready"
	StatusDraining = "draining"
)

// Probe reports on one dependency. A nil error means it is working; an error is
// shown to whoever reads the probe, so it should say what is wrong rather than
// merely that something is.
type Probe func(ctx context.Context) error

// check is one registered dependency.
type check struct {
	name string

	// required decides whether a failure keeps traffic away. Not everything
	// this process needs is something it cannot serve without, and treating
	// those the same is how an install with no city database reports itself
	// down.
	required bool

	probe Probe
}

// Set is a process's dependencies. Each process registers its own, because the
// answer genuinely differs: an ingestor with an empty routing map drops every
// event it accepts, while an app with no sites yet is a fresh install waiting
// for somebody to add one.
type Set struct {
	mu     sync.Mutex
	checks []check
}

// Require registers a dependency this process cannot serve without.
func (s *Set) Require(name string, probe Probe) {
	s.add(check{name: name, required: true, probe: probe})
}

// Optional registers a dependency whose absence degrades the service without
// stopping it. It is reported, and it never makes the process unready.
func (s *Set) Optional(name string, probe Probe) {
	s.add(check{name: name, required: false, probe: probe})
}

// add records one check.
func (s *Set) add(c check) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.checks = append(s.checks, c)
}

// Component is one dependency's answer.
type Component struct {
	Name   string `json:"name"`
	Status string `json:"status"`

	// Detail carries the error when there was one. It is our own message about
	// our own dependency, never anything a request supplied.
	Detail string `json:"detail,omitempty"`
}

// Report is the whole answer, and the body of the readiness probe.
type Report struct {
	Status     string      `json:"status"`
	Components []Component `json:"components"`
}

// Ready reports whether the process can take traffic.
func (r Report) Ready() bool {
	return r.Status == StatusReady
}

// Run probes every dependency and assembles the report. Every check runs even
// after one has failed: the first failure is rarely the whole story, and a
// report that stopped at it would send somebody to fix the wrong thing.
func (s *Set) Run(ctx context.Context) Report {
	s.mu.Lock()
	checks := append([]check(nil), s.checks...)
	s.mu.Unlock()

	report := Report{Status: StatusReady, Components: make([]Component, 0, len(checks))}

	for _, c := range checks {
		component := Component{Name: c.name, Status: StatusOK}

		if err := c.probe(ctx); err != nil {
			component.Detail = err.Error()

			if c.required {
				component.Status = StatusFailed
				report.Status = StatusNotReady
			} else {
				component.Status = StatusDegraded
			}
		}

		report.Components = append(report.Components, component)
	}

	return report
}

// Database probes a database handle. Ping rather than a query, because the
// question is whether the connection is usable at all — a probe that ran a real
// statement would take the write lock away from ingestion every few seconds.
func Database(db *sql.DB) Probe {
	return func(ctx context.Context) error {
		if db == nil {
			return fmt.Errorf("the handle is not open")
		}

		return db.PingContext(ctx)
	}
}

// Directory probes that a directory exists and can be written to. Existing is
// not enough: a full disk, a read-only remount and a permissions change all
// leave a directory that still stats perfectly well and cannot take an event.
//
// The result is cached briefly so that a load balancer probing every second
// does not create and delete a file every second.
func Directory(path string) Probe {
	var (
		mu       sync.Mutex
		checked  time.Time
		lastErr  error
		interval = 10 * time.Second
	)

	return func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()

		if !checked.IsZero() && time.Since(checked) < interval {
			return lastErr
		}

		checked = time.Now()
		lastErr = writable(path)

		return lastErr
	}
}

// writable creates and removes a file, which is the only check that answers the
// question being asked.
func writable(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("%s cannot be created: %w", path, err)
	}

	probe := filepath.Join(path, ".write-probe")

	file, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", path, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("%s is not writable: %w", path, err)
	}

	if err := os.Remove(probe); err != nil {
		return fmt.Errorf("%s is not writable: %w", path, err)
	}

	return nil
}

// Condition adapts a plain boolean into a probe, with the message to show when
// it is false. Most of what a process knows about itself — whether a cache has
// been built, whether a map has anything in it — is already a bool somewhere.
func Condition(fn func() bool, whenFalse string) Probe {
	return func(context.Context) error {
		if fn == nil || !fn() {
			return fmt.Errorf("%s", whenFalse)
		}

		return nil
	}
}
