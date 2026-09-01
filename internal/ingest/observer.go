//
// observer.go
// One hook, so the ingestion health panel can see what actually arrived.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

// Observation is one request as it looked after derivation.
//
// The counters in this package answer "how many", and that is most of what a
// customer needs. It is not all of it: "which header did you believe for my
// visitors' addresses", "which hostname is sending me events I did not
// expect", and "which build of the script is still out there" are all questions
// about the *content* of a request rather than its count, and none of them can
// be answered from a tally.
//
// So one observation is emitted per request, carrying the whole derived view.
// It is the same struct the X-Debug-Request header returns, which is the point:
// the debug output a customer would otherwise have to produce with curl is
// what the health panel shows them, without anybody having to ask.
type Observation struct {
	SiteID    int64
	AccountID int64

	// ReceivedAt is unix seconds, taken from the pipeline's clock so a replay
	// harness observes the times it is replaying rather than the times it ran.
	ReceivedAt int64

	Debug Debug

	// DropReason is empty for an accepted event. A classified event — a bot, a
	// datacentre address — carries its classification here and was still
	// stored, which the panel reports separately.
	DropReason string

	// Accepted is whether a row was written. It is a separate field rather than
	// derived from DropReason because a classified event has a reason and was
	// still accepted, and conflating the two is how a customer concludes their
	// traffic is being thrown away.
	Accepted bool

	// Pending means the request has been derived but the writer has not made
	// its final decision yet. Pending observations carry the request details
	// needed by the health panel, but must not increment accepted or dropped:
	// the shard may still apply a live shield or park an engagement ping whose
	// pageview has not arrived. The writer emits the final observation later.
	Pending bool

	// OutcomeOnly marks the writer's compact follow-up observation. Request
	// metadata was already recorded by the handler, so this observation changes
	// counts without replacing the panel's last-request debug view with blanks.
	OutcomeOnly bool

	UserAgent      string
	TrackerVersion int
	Truncation     Truncation
}

// Observer receives one observation per request.
//
// Implementations must be cheap and must not block: this runs inline on the
// busiest path in the system, and a health panel that slowed ingestion down
// would be a monitoring feature that caused the outage it reports. The one
// implementation aggregates in memory behind a mutex and flushes on a ticker.
type Observer interface {
	Observe(Observation)
}

// ObserverFunc adapts a function to the interface, for tests and for a caller
// that only wants to count one thing.
type ObserverFunc func(Observation)

// Observe calls the function.
func (f ObserverFunc) Observe(observation Observation) { f(observation) }
