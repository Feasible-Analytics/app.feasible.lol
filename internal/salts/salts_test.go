//
// salts_test.go
// Tests deterministic UTC-day salt derivation from a shared value.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package salts

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestSourcesAgreeForTheSameUTCDate proves independently running ingesters
// derive identical material without communicating with an app or each other.
func TestSourcesAgreeForTheSameUTCDate(t *testing.T) {
	now := time.Date(2026, time.September, 1, 18, 30, 0, 0, time.FixedZone("offset", -7*60*60))
	first, err := New("shared-test-salt")
	if err != nil {
		t.Fatal(err)
	}
	second, err := New("shared-test-salt")
	if err != nil {
		t.Fatal(err)
	}
	first.SetClock(func() time.Time { return now })
	second.SetClock(func() time.Time { return now.UTC() })

	a, err := first.Pair(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Pair(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(a.Current, b.Current) || !bytes.Equal(a.Previous, b.Previous) {
		t.Fatal("sources with the same shared value and UTC date disagreed")
	}
}

// TestPairChangesAtUTCMidnight verifies local timezone does not affect the day
// boundary and yesterday becomes the new pair's fallback.
func TestPairChangesAtUTCMidnight(t *testing.T) {
	now := time.Date(2026, time.September, 1, 23, 59, 0, 0, time.UTC)
	source, err := New("shared-test-salt")
	if err != nil {
		t.Fatal(err)
	}
	source.SetClock(func() time.Time { return now })

	before, err := source.Pair(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	after, err := source.Pair(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(before.Current, after.Current) {
		t.Fatal("current salt did not change at UTC midnight")
	}
	if !bytes.Equal(before.Current, after.Previous) {
		t.Fatal("previous salt is not yesterday's derived value")
	}
}

// TestDifferentSharedValuesDoNotAgree verifies deployments cannot accidentally
// share visitor identifiers merely because their UTC clocks agree.
func TestDifferentSharedValuesDoNotAgree(t *testing.T) {
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	first, _ := New("first")
	second, _ := New("second")
	first.SetClock(func() time.Time { return now })
	second.SetClock(func() time.Time { return now })
	a, _ := first.Pair(context.Background())
	b, _ := second.Pair(context.Background())

	if bytes.Equal(a.Current, b.Current) {
		t.Fatal("different shared values derived the same current salt")
	}
}

// TestNewRejectsEmptySharedValue keeps an accidental globally shared default
// from reaching the fingerprint pipeline.
func TestNewRejectsEmptySharedValue(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("empty shared salt was accepted")
	}
}

// TestEraseOverwritesDerivedMaterial verifies request-local secrets are cleared
// before their slices are released.
func TestEraseOverwritesDerivedMaterial(t *testing.T) {
	source, _ := New("shared-test-salt")
	pair, _ := source.Pair(context.Background())
	current := pair.Current
	previous := pair.Previous
	pair.Erase()

	if !bytes.Equal(current, make([]byte, Size)) || !bytes.Equal(previous, make([]byte, Size)) {
		t.Fatal("Erase left derived salt bytes behind")
	}
}
