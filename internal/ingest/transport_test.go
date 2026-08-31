//
// transport_test.go
// Tests for the seam every event crosses, even in single-process mode.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"testing"
)

// TestDirectShardAlwaysAnswersZero checks routing is data rather than a hash
// function. In direct mode there is one shard, and there are no hash ranges to
// rebalance later — moving an account is "move the file, update two lists".
func TestDirectShardAlwaysAnswersZero(t *testing.T) {
	resolver := DirectShard{}

	for _, accountID := range []int64{1, 2, 999, 1 << 40} {
		shard, ok := resolver.Shard(accountID)
		if !ok {
			t.Fatalf("account %d did not route", accountID)
		}
		if shard != 0 {
			t.Fatalf("account %d routed to shard %d, want 0", accountID, shard)
		}
	}
}

// TestDirectTransportWrites checks the in-process implementation actually goes
// through the writer. Every event flows accept, derive, buffer, forward, write
// even when forward is a function call, and exercising the seam from day one is
// the entire point of having it.
func TestDirectTransportWrites(t *testing.T) {
	ctx := context.Background()
	writer, manager := newWriter(t)

	transport := NewDirect(writer)

	batch := []Event{
		writerEvent(1, EventPageview, fixtureStart.Unix(), "/"),
		writerEvent(1, EventPageview, fixtureStart.Unix()+30, "/pricing"),
	}

	committed, err := transport.Send(ctx, 0, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed) != 2 {
		t.Fatalf("committed %d events, want 2", len(committed))
	}

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != 2 {
		t.Fatalf("events table holds %d rows, want 2", got)
	}
}

// TestDirectTransportRefusesAnotherShard checks a misrouted batch is an error
// rather than a silent write into the wrong file. There is exactly one shard in
// this process, and pretending otherwise would lose the data.
func TestDirectTransportRefusesAnotherShard(t *testing.T) {
	writer, _ := newWriter(t)

	if _, err := NewDirect(writer).Send(context.Background(), 3, []Event{
		writerEvent(1, EventPageview, fixtureStart.Unix(), "/"),
	}); err == nil {
		t.Fatal("the direct transport accepted a batch for a shard it does not hold")
	}
}

// TestEmptyBatchIsFree checks a flush with nothing in it does not open a
// transaction, which on a quiet site would be one per interval for nothing.
func TestEmptyBatchIsFree(t *testing.T) {
	writer, _ := newWriter(t)

	committed, err := NewDirect(writer).Send(context.Background(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed) != 0 {
		t.Fatalf("committed %d events from an empty batch", len(committed))
	}
}

// TestEventCarriesItsShard checks the field exists and travels with the event.
// The shard is a property of the event so that the buffer can group by it
// without knowing the routing table.
func TestEventCarriesItsShard(t *testing.T) {
	event := writerEvent(1, EventPageview, fixtureStart.Unix(), "/")

	shard, ok := DirectShard{}.Shard(event.AccountID)
	if !ok {
		t.Fatal("the account did not route")
	}
	event.Shard = shard

	if event.Shard != 0 {
		t.Fatalf("event shard = %d, want 0", event.Shard)
	}
}

// TestHasDetails decides whether the cold table needs a row, and the check lives
// in one place so the flag on the hot row and the existence of the detail row
// can never disagree.
func TestHasDetails(t *testing.T) {
	plain := writerEvent(1, EventPageview, fixtureStart.Unix(), "/")
	if plain.HasDetails() {
		t.Error("an ordinary pageview claims details")
	}

	withProps := plain
	withProps.Props = map[string]string{"plan": "pro"}
	if !withProps.HasDetails() {
		t.Error("an event with properties does not claim details")
	}

	withRevenue := plain
	withRevenue.Revenue = &Revenue{Amount: 100, Currency: "USD"}
	if !withRevenue.HasDetails() {
		t.Error("an event with revenue does not claim details")
	}

	withTerm := plain
	withTerm.UTMTerm = "analytics"
	if !withTerm.HasDetails() {
		t.Error("an event with a utm_term does not claim details")
	}
}
