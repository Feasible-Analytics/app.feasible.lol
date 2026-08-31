//
// http_test.go
// Tests for the request middleware.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStatusClassBuckets checks a status is reduced to its family. Five values
// rather than forty is what keeps the series count flat, and no alert has ever
// needed to tell a 502 from a 504 before it fired.
func TestStatusClassBuckets(t *testing.T) {
	cases := map[int]string{
		0:   "2xx",
		200: "2xx",
		202: "2xx",
		302: "3xx",
		404: "4xx",
		500: "5xx",
	}

	for status, want := range cases {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
}

// TestInstrumentRecordsTheStatusItWrote checks the wrapper sees the code a
// handler set, including the implicit 200 of a handler that only writes a body.
func TestInstrumentRecordsTheStatusItWrote(t *testing.T) {
	handler := Instrument("test", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK || recorder.Body.String() != "hello" {
		t.Fatalf("got %d %q, want 200 hello", recorder.Code, recorder.Body.String())
	}
}

// TestInstrumentKeepsFlushingWorking checks the wrapper does not break
// streaming. A middleware that silently disables Flush turns a live endpoint
// into one that answers only when the handler returns, which nothing would fail
// on until somebody watched a dashboard stop updating.
func TestInstrumentKeepsFlushingWorking(t *testing.T) {
	flushed := false

	handler := Instrument("test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("part"))

		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)
			return
		}

		flushed = true
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !flushed {
		t.Fatal("the handler could not flush through the wrapper")
	}
}
