//
// logger.go
// Structured, levelled logging to stdout, plus the named events worth debugging.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package logger wraps log/slog with the handful of events this system is
// actually debugged through. Naming them here — rather than leaving every call
// site to invent its own message and field names — is what makes a log search
// like "every event we dropped for this domain" possible at 2am.
package logger

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Logger is the application logger. It embeds *slog.Logger so any standard slog
// call still works, and adds the named helpers below.
type Logger struct {
	*slog.Logger

	// traceEvents mirrors --trace-events. It is held here so call sites can skip
	// building an expensive trace payload when nobody will read it.
	traceEvents bool
}

// Options configures a logger. It is a struct rather than a long parameter list
// because every caller sets the same three things and a positional bool is the
// classic way to end up with JSON logs on a laptop.
type Options struct {
	Level       string
	Format      string
	TraceEvents bool

	// Output defaults to stdout. Everything goes to stdout on purpose: files and
	// syslog are the supervisor's job, not ours.
	Output io.Writer
}

// New builds a logger from options. An unrecognised level falls back to info
// rather than failing, because a process that refuses to boot over a log level
// is worse than one that logs slightly too much.
func New(opts Options) *Logger {
	out := opts.Output
	if out == nil {
		out = os.Stdout
	}

	handlerOpts := &slog.HandlerOptions{Level: ParseLevel(opts.Level)}

	var handler slog.Handler
	if strings.EqualFold(opts.Format, "json") {
		handler = slog.NewJSONHandler(out, handlerOpts)
	} else {
		handler = slog.NewTextHandler(out, handlerOpts)
	}

	return &Logger{Logger: slog.New(handler), traceEvents: opts.TraceEvents}
}

// ParseLevel maps our configuration strings onto slog levels. It is exported
// because the config package validates the same set and tests assert on it.
func ParseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// With returns a logger carrying extra attributes, keeping the concrete type so
// the named helpers survive. Without this override the embedded slog method
// would hand back a bare *slog.Logger and every helper would be lost.
func (l *Logger) With(args ...any) *Logger {
	return &Logger{Logger: l.Logger.With(args...), traceEvents: l.traceEvents}
}

// TraceEventsEnabled reports whether --trace-events is on, so a caller can avoid
// deriving a full event payload that would be thrown away.
func (l *Logger) TraceEventsEnabled() bool {
	return l.traceEvents
}

// EventReceived records one inbound tracking event. The drop reason is the
// whole point of the line: an event that vanishes with no explanation is the
// single most common complaint about the products we compete with, so a drop is
// logged at warn where it cannot be missed.
func (l *Logger) EventReceived(domain, site, shard, dropReason string) {
	attrs := []any{"domain", domain, "site", site, "shard", shard}

	if dropReason != "" {
		l.Warn("event dropped", append(attrs, "drop_reason", dropReason)...)
		return
	}

	l.Debug("event received", attrs...)
}

// ShardPoll records one poll of a shard's domain list. Ingestors drop anything
// not in that map, so when events go missing the first question is always
// whether the poll succeeded and how many domains came back.
func (l *Logger) ShardPoll(shard string, notModified bool, domains int, took time.Duration) {
	status := "changed"
	if notModified {
		status = "304"
	}

	l.Debug("shard poll",
		"shard", shard,
		"status", status,
		"domains", domains,
		"duration", took,
	)
}

// OutboxFlush records one attempt to forward buffered events to a shard. Store
// and forward hides failure by design — the client already got its 202 — so the
// flush log is the only place a stuck outbox becomes visible.
func (l *Logger) OutboxFlush(rows int, shard, outcome string, took time.Duration) {
	l.Debug("outbox flush",
		"rows", rows,
		"shard", shard,
		"outcome", outcome,
		"duration", took,
	)
}

// NotMine records a shard rejecting a forwarded event as belonging to someone
// else. Whether the ingestor's routing map was complete decides whether this is
// a harmless race after a site moved, or events being thrown away.
func (l *Logger) NotMine(domain string, mapComplete bool, action string) {
	l.Warn("shard returned not_mine",
		"domain", domain,
		"map_complete", mapComplete,
		"action", action,
	)
}

// RollupRun records one roll-up pass. Dashboards read roll-ups, so when a chart
// is missing an hour this line says whether the pass ran and what it wrote.
func (l *Logger) RollupRun(site, period string, rows int, took time.Duration) {
	l.Debug("rollup run",
		"site", site,
		"period", period,
		"rows", rows,
		"duration", took,
	)
}

// SlowQuery records a query that took too long, with the SQL and how much work
// it did. On a per-account SQLite file the fix is nearly always a missing index,
// and that is unfindable without the statement itself.
func (l *Logger) SlowQuery(query string, took time.Duration, rowsExamined int64) {
	l.Warn("slow query",
		"sql", query,
		"duration", took,
		"rows_examined", rowsExamined,
	)
}

// SlowReport records a report that took too long, with enough of the request to
// reproduce it. A metrics histogram says how many reports were slow; this says
// which one, and on a per-account SQLite file the answer is nearly always the
// range and the source it was answered from.
func (l *Logger) SlowReport(domain, source string, took time.Duration, metrics, dimensions []string, dateRange string) {
	l.Warn("slow report",
		"domain", domain,
		"source", source,
		"duration", took,
		"metrics", strings.Join(metrics, ","),
		"dimensions", strings.Join(dimensions, ","),
		"date_range", dateRange,
	)
}

// AuthFailure records a rejected internal request with the real reason and the
// observed clock skew. A signature that fails only because two machines drifted
// apart looks exactly like an attack until the skew is on the line, and that
// mistake costs an hour every time.
func (l *Logger) AuthFailure(reason string, skew time.Duration) {
	l.Warn("internal auth failure",
		"reason", reason,
		"clock_skew", skew,
	)
}

// EmailSent records an outgoing message. The rendered body path is included so
// the log transport used in development is a click away from the actual HTML.
func (l *Logger) EmailSent(recipient, template, bodyPath string) {
	l.Info("email sent",
		"recipient", recipient,
		"template", template,
		"body_path", bodyPath,
	)
}

// TraceEvent prints a fully derived event — geo, fingerprint, channel, every
// parsed field — when --trace-events is on. It logs at info rather than debug so
// that turning the flag on is enough on its own; needing to also raise the log
// level would defeat the point of a one-flag answer to "why is this event
// wrong?".
func (l *Logger) TraceEvent(attrs ...any) {
	if !l.traceEvents {
		return
	}

	l.Info("event trace", attrs...)
}
