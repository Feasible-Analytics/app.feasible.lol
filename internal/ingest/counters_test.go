//
// counters_test.go
// Tests for the counts that make "never fail silently" true.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import "testing"

// TestCountsArePerSite checks the number is actionable. A customer needs to know
// that *their* events are being dropped, not that some events somewhere are.
func TestCountsArePerSite(t *testing.T) {
	counters := NewCounters()

	counters.Accepted(1)
	counters.Accepted(1)
	counters.Accepted(2)
	counters.Dropped(1, ReasonBot)
	counters.Dropped(1, ReasonBot)
	counters.Dropped(2, ReasonUnknownSite)

	snapshot := counters.Snapshot()

	if snapshot.Accepted[1] != 2 || snapshot.Accepted[2] != 1 {
		t.Fatalf("accepted = %v, want 2 for site 1 and 1 for site 2", snapshot.Accepted)
	}

	want := map[int64]map[string]int64{
		1: {ReasonBot: 2},
		2: {ReasonUnknownSite: 1},
	}

	for _, count := range snapshot.Dropped {
		if want[count.SiteID][count.Reason] != count.Count {
			t.Errorf("site %d %s = %d, want %d", count.SiteID, count.Reason, count.Count, want[count.SiteID][count.Reason])
		}
	}
}

// TestSnapshotOrderIsStable checks two reads a second apart produce a diff about
// the traffic rather than about Go's map iteration.
func TestSnapshotOrderIsStable(t *testing.T) {
	counters := NewCounters()

	for _, reason := range Reasons {
		counters.Dropped(2, reason)
		counters.Dropped(1, reason)
	}

	first := counters.Snapshot().Dropped

	for i := 0; i < 20; i++ {
		again := counters.Snapshot().Dropped

		if len(again) != len(first) {
			t.Fatalf("snapshot length changed: %d vs %d", len(again), len(first))
		}
		for j := range again {
			if again[j] != first[j] {
				t.Fatalf("snapshot order changed at %d: %+v vs %+v", j, again[j], first[j])
			}
		}
	}
}

// TestTruncationCountsEveryKind checks the whole Truncation is recorded at once,
// so a caller cannot report three of the four and leave the fourth invisible —
// which is precisely the failure being designed out.
func TestTruncationCountsEveryKind(t *testing.T) {
	counters := NewCounters()

	counters.Truncated(1, Truncation{
		PropsDropped:        7,
		PropNamesTruncated:  2,
		PropValuesTruncated: 3,
		PropsUnsupported:    4,
		URLTruncated:        true,
		EngagementClamped:   true,
	})

	got := map[string]int64{}
	for _, count := range counters.Snapshot().Truncations {
		got[count.Reason] = count.Count
	}

	want := map[string]int64{
		TruncationProps:           7,
		TruncationPropName:        2,
		TruncationPropValue:       3,
		TruncationPropUnsupported: 4,
		TruncationURL:             1,
		TruncationEngagement:      1,
	}

	for reason, wanted := range want {
		if got[reason] != wanted {
			t.Errorf("%s = %d, want %d", reason, got[reason], wanted)
		}
	}
}

// TestNothingTruncatedCostsNothing checks the hot path skips the bookkeeping in
// the overwhelmingly common case where nothing was cut.
func TestNothingTruncatedCostsNothing(t *testing.T) {
	counters := NewCounters()

	counters.Truncated(1, Truncation{})

	if got := len(counters.Snapshot().Truncations); got != 0 {
		t.Fatalf("recorded %d truncations for an untouched event, want 0", got)
	}
}

// TestUnidentifiedSiteIsStillCounted checks a drop before we knew which site it
// was is not lost. Site zero is what an unknown domain looks like, and it is
// worth seeing.
func TestUnidentifiedSiteIsStillCounted(t *testing.T) {
	counters := NewCounters()

	counters.Dropped(0, ReasonUnknownSite)

	snapshot := counters.Snapshot()
	if len(snapshot.Dropped) != 1 || snapshot.Dropped[0].SiteID != 0 {
		t.Fatalf("dropped = %+v, want one entry against site 0", snapshot.Dropped)
	}
}

// TestCountersAreConcurrent runs under -race, which is the whole assertion:
// these are written from every request goroutine at once.
func TestCountersAreConcurrent(t *testing.T) {
	counters := NewCounters()

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(id int) {
			for j := 0; j < 500; j++ {
				counters.Accepted(int64(id%3) + 1)
				counters.Dropped(int64(id%3)+1, ReasonBot)
				counters.Truncated(int64(id%3)+1, Truncation{PropsDropped: 1})
			}
			done <- struct{}{}
		}(i)
	}

	for i := 0; i < 8; i++ {
		<-done
	}

	snapshot := counters.Snapshot()

	var total int64
	for _, count := range snapshot.Accepted {
		total += count
	}
	if total != 4000 {
		t.Fatalf("accepted %d events, want 4000", total)
	}
}
