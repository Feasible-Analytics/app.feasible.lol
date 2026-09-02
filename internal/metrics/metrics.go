//
// metrics.go
// The counters an operator reads at three in the morning, and nothing else.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package metrics is the process's own instrumentation, in the Prometheus text
// format. Every series here exists to answer a question somebody actually asks
// when something is wrong:
//
//	Are we accepting events, and is anything being dropped?  events_*
//	Is the write buffer keeping up, or growing?              buffer_*, flush_*
//	Is the roll-up worker current, or behind?                rollup_*
//	Are queries slow, and which ones?                        query_*, http_*
//	Is the database healthy?                                 database_*
//
// Two rules govern what may be added.
//
// Nothing here may carry customer data. A site id, a domain, a path, a country
// or a visitor is all data belonging to somebody who did not agree to it being
// on our metrics endpoint — and an IP address is never anywhere in this system
// at all, by the same rule that discards it in the ingest tier. Per-site drop
// counts already exist for the customer's own health panel; this endpoint is
// about the process.
//
// And every label must come from a closed set. A label whose values come from
// the traffic — a domain, a path, a user agent — multiplies the series count by
// however many distinct values arrive, which is how a metrics endpoint ends up
// costing more memory than the thing it measures.
package metrics

import (
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// namespace prefixes every series, so a shared Prometheus can tell ours from
// everything else on the box.
const namespace = "feasible"

// Outcome labels. They are constants because "ok" and "error" being spelt two
// ways is a dashboard that quietly stops adding up.
const (
	OutcomeOK    = "ok"
	OutcomeError = "error"
)

// The ingest counters: is the front door working, and is anything being lost.
//
// Drops and classifications are separate series because they are different
// things to be told. A classified event is still stored and can be un-hidden
// with a toggle; a dropped one is gone.
var (
	EventsAccepted = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "ingest", Name: "events_accepted_total",
		Help: "Events derived and buffered for writing.",
	})

	EventsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "ingest", Name: "events_dropped_total",
		Help: "Events thrown away, by reason. Every reason comes from a closed set.",
	}, []string{"reason"})

	EventsClassified = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "ingest", Name: "events_classified_total",
		Help: "Events stored but marked as bot, datacenter or referrer spam, by reason.",
	}, []string{"reason"})

	FieldsTruncated = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "ingest", Name: "fields_truncated_total",
		Help: "Fields an accepted event carried that we could not keep whole, by field.",
	}, []string{"field"})

	EventsWritten = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "ingest", Name: "events_written_total",
		Help: "Events a shard has durably committed. Accepted minus written is what the buffer still owes.",
	})
)

// The recurring scheduler has no labels: every value is process-wide, which
// keeps the series set fixed no matter how many job kinds or customers exist.
var (
	SchedulerRuns = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "scheduler", Name: "runs_total",
		Help: "Recurring scheduler enqueue passes attempted.",
	})

	SchedulerFailures = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "scheduler", Name: "failures_total",
		Help: "Recurring scheduler enqueue passes that returned an error.",
	})

	SchedulerCatchUpSlots = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "scheduler", Name: "catch_up_slots_total",
		Help: "Missed durable scheduling buckets examined during bounded catch-up.",
	})

	SchedulerCatchUpJobs = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "scheduler", Name: "catch_up_jobs_total",
		Help: "Jobs created for missed durable scheduling buckets.",
	})

	SchedulerCreatedJobs = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "scheduler", Name: "created_jobs_total",
		Help: "Recurring jobs created, including current and catch-up buckets.",
	})

	SchedulerDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "scheduler", Name: "duration_seconds",
		Help:    "How long one recurring scheduler enqueue pass took.",
		Buckets: prometheus.ExponentialBuckets(0.001, 3, 10),
	})

	SchedulerLastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "scheduler", Name: "last_success_timestamp_seconds",
		Help: "Scheduled time of the latest successful enqueue pass, as unix seconds.",
	})
)

// The write buffer: accepted events wait here, and a buffer that only grows is
// a transport that has stopped accepting.
var (
	Flushes = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "ingest", Name: "flushes_total",
		Help: "Write-buffer flushes, by outcome. A failed flush requeues its batch rather than losing it.",
	}, []string{"outcome"})

	FlushDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "ingest", Name: "flush_duration_seconds",
		Help: "How long one flush took: a batch grouped by account and written in a transaction each.",
		// Five milliseconds to twenty seconds. The top of the range is not
		// padding: a flush that slow is the shape of a stalled disk, and a
		// histogram that ends at one second cannot tell ten from a thousand.
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 13),
	})

	FlushBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "ingest", Name: "flush_batch_events",
		Help: "Events in one flush. Batches far above the buffer size mean flushes are not keeping up.",
		// The buffer's own size is 250, so the interesting range is a few
		// events (a quiet site on the ticker) to tens of thousands (a buffer
		// that grew while a flush was stuck).
		Buckets: prometheus.ExponentialBuckets(1, 4, 9),
	})
)

// The roll-up worker: a dashboard reads summaries, so a worker that has stopped
// is a dashboard that is slow and then wrong.
var (
	RollupRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "rollup", Name: "runs_total",
		Help: "Roll-up passes over every site, by outcome.",
	}, []string{"outcome"})

	RollupDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "rollup", Name: "duration_seconds",
		Help:    "How long one pass over every site took. It runs hourly, so a pass approaching an hour is the alarm.",
		Buckets: prometheus.ExponentialBuckets(0.05, 3, 10),
	})

	// RollupLastSuccess is a timestamp rather than an age, because a gauge that
	// reports an age is only correct at the instant it was scraped. Subtracting
	// it from the current time is the query; "behind" is a decision the alert
	// makes, not one this process makes for it.
	RollupLastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Subsystem: "rollup", Name: "last_success_timestamp_seconds",
		Help: "When the last complete pass finished, in unix seconds. Zero means none has since start-up.",
	})
)

// Reports: which reports are slow, and whether the summaries are earning their
// keep. The source label is the only one — a site or a domain here would be
// both customer data and unbounded.
var (
	QueryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "query", Name: "duration_seconds",
		Help: "How long one report took, by where it was answered from.",
		// One millisecond to about twenty seconds: a summary answers in single
		// milliseconds and a raw twelve-month scan is measured in seconds, and
		// both have to fit on the same graph.
		Buckets: prometheus.ExponentialBuckets(0.001, 3, 10),
	}, []string{"source"})

	QueryFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "query", Name: "failures_total",
		Help: "Reports that did not answer, split by whose fault it was.",
	}, []string{"kind"})
)

// HTTP: the same numbers for every endpoint, so "the dashboard is slow" can be
// told from "the box is slow".
var (
	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Subsystem: "http", Name: "requests_total",
		Help: "Requests by handler and status class. The handler label is a fixed set of route names, never a path.",
	}, []string{"handler", "status"})

	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Subsystem: "http", Name: "request_duration_seconds",
		Help:    "How long a request took, by handler.",
		Buckets: prometheus.DefBuckets,
	}, []string{"handler"})
)

// sampled holds the collector currently registered, so that Watch can replace
// it rather than fail on a duplicate. Rebuilding process services in tests must
// not panic over a metric registered by the previous service.
var sampled atomic.Pointer[sampler]

// Watch registers the gauges that have to be read at scrape time rather than
// counted as they happen: buffer depth, cache sizes, database sizes. Calling it
// again replaces the previous set.
func Watch(s Sources) {
	next := newSampler(s)

	if previous := sampled.Swap(next); previous != nil {
		prometheus.DefaultRegisterer.Unregister(previous)
	}

	prometheus.DefaultRegisterer.MustRegister(next)
}

// Handler serves the metrics in the Prometheus text format.
//
// It belongs on a loopback listener, not the public one. Nothing here is
// customer data, but an endpoint that tells the internet our event rate,
// account count and error rate is free reconnaissance, and no operator expects
// /metrics to be world-readable.
func Handler() http.Handler {
	return promhttp.Handler()
}
