//
// metrics_test.go
// Tests for the exported series, and for the rule that none of them names a customer.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// forbiddenLabels are the label names that would put customer data on an
// operations endpoint, or would grow a new series for every value that arrives.
// They are listed by name because the mistake is always made one label at a
// time, by somebody adding a very useful-looking dimension.
var forbiddenLabels = []string{
	"site", "site_id", "domain", "hostname", "account", "account_id",
	"path", "page", "url", "referrer", "country", "user", "user_id",
	"visitor", "ip", "address", "user_agent", "email",
}

// TestNoSeriesNamesACustomer walks every registered metric and fails on a label
// that would identify somebody or come from the traffic.
//
// It is a test rather than a review note because this endpoint will be added to
// for years, and the reviewer who has to catch it is the one who did not write
// the change.
func TestNoSeriesNamesACustomer(t *testing.T) {
	// Every metric is touched first: a CounterVec with no observations exports
	// no series at all, and a test that gathered nothing would pass forever.
	EventsAccepted.Inc()
	EventsDropped.WithLabelValues("unknown_site").Inc()
	EventsClassified.WithLabelValues("bot").Inc()
	FieldsTruncated.WithLabelValues("url_too_long").Inc()
	EventsWritten.Inc()
	Flushes.WithLabelValues(OutcomeOK).Inc()
	FlushDuration.Observe(0.01)
	FlushBatchSize.Observe(10)
	RollupRuns.WithLabelValues(OutcomeOK).Inc()
	RollupDuration.Observe(0.5)
	RollupLastSuccess.Set(1)
	QueryDuration.WithLabelValues("rollup").Observe(0.01)
	QueryFailures.WithLabelValues("caller").Inc()
	HTTPRequests.WithLabelValues(HandlerEvent, "2xx").Inc()
	HTTPDuration.WithLabelValues(HandlerEvent).Observe(0.01)

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}

	var ours int

	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), namespace+"_") {
			continue
		}

		ours++

		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				for _, forbidden := range forbiddenLabels {
					if label.GetName() == forbidden {
						t.Errorf("%s carries the label %q, which is customer data or unbounded",
							family.GetName(), label.GetName())
					}
				}
			}
		}
	}

	if ours == 0 {
		t.Fatal("no series of ours was gathered, so this test proved nothing")
	}
}

// TestHandlerServesTheTextFormat checks the endpoint answers something a
// Prometheus can scrape, including the process and runtime series that come
// with the default registry — which are what answer "is this process wedged".
func TestHandlerServesTheTextFormat(t *testing.T) {
	EventsAccepted.Inc()

	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", recorder.Code)
	}

	body, err := io.ReadAll(recorder.Body)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"feasible_ingest_events_accepted_total", "go_goroutines"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the scrape does not include %s", want)
		}
	}
}

// TestWatchReplacesThePreviousSampler checks registering twice is not a panic.
// A process that builds a second listener — and every test that builds two —
// must not die over a metric.
func TestWatchReplacesThePreviousSampler(t *testing.T) {
	depth := 3

	Watch(Sources{BufferDepth: func() int { return depth }})
	Watch(Sources{BufferDepth: func() int { return depth * 2 }})

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}

	for _, family := range families {
		if family.GetName() != "feasible_ingest_buffer_events" {
			continue
		}

		if got := family.GetMetric()[0].GetGauge().GetValue(); got != 6 {
			t.Fatalf("buffer depth = %v, want 6 from the second Watch", got)
		}

		return
	}

	t.Fatal("the sampler registered by the second Watch is not being gathered")
}
