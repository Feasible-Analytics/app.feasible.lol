//
// http.go
// Counting and timing requests under a name, never under a path.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package metrics

import (
	"net/http"
	"strconv"
	"time"
)

// Handler names. They are constants because they are the label values, and a
// label value that is spelt two ways is two graphs that each show half the
// traffic.
const (
	HandlerEvent     = "event"
	HandlerStats     = "stats"
	HandlerDashboard = "dashboard"
	HandlerTracker   = "tracker"
	HandlerApp       = "app"
	HandlerAPI       = "api"
)

// Instrument wraps a handler so its requests are counted and timed under a
// fixed name.
//
// The name is passed in rather than derived from the request. A label taken
// from the URL would carry a customer's domain or a visitor's path, which is
// both their data and an unbounded set — one crawler walking a site would then
// cost more memory than the site's traffic does.
func Instrument(name string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()

		recorder := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		HTTPDuration.WithLabelValues(name).Observe(time.Since(started).Seconds())
		HTTPRequests.WithLabelValues(name, statusClass(recorder.status)).Inc()
	})
}

// statusWriter remembers the status code. It defaults to 200 because a handler
// that writes a body without calling WriteHeader has sent one.
type statusWriter struct {
	http.ResponseWriter

	status int
}

// WriteHeader records the code on its way out.
func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}

	w.ResponseWriter.WriteHeader(status)
}

// Write records an implicit 200 for a handler that never set a status.
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}

	return w.ResponseWriter.Write(b)
}

// Unwrap hands the original writer back, so that flushing and hijacking still
// reach it through http.ResponseController. Without it, wrapping a handler
// would quietly break streaming.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// statusClass reduces a status to its family. Five values instead of forty
// keeps the series count flat, and no alert has ever needed to tell a 502 from
// a 504 before it fired.
func statusClass(status int) string {
	if status == 0 {
		status = http.StatusOK
	}

	return strconv.Itoa(status/100) + "xx"
}
