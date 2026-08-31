//
// revenue_test.go
// Rates, staleness, and money that stays an integer.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRatesAreStoredWithTheirFetchTime checks the round trip, including the
// self-rate: a currency converts to itself at one, so a site with one currency
// can see its own revenue without ever running a rates job.
func TestRatesAreStoredWithTheirFetchTime(t *testing.T) {
	db, _ := newFixture(t)

	ctx := context.Background()

	if err := StoreRates(ctx, db, "USD", map[string]float64{"EUR": 1.08, "GBP": 1.27}, fixtureNow); err != nil {
		t.Fatal(err)
	}

	rates, fetchedAt, err := ReadRates(ctx, db, "USD")
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]float64{}
	for _, rate := range rates {
		got[rate.Base] = rate.Rate
	}

	if got["EUR"] != 1.08 || got["GBP"] != 1.27 {
		t.Errorf("rates = %v, want EUR 1.08 and GBP 1.27", got)
	}

	if got["USD"] != 1 {
		t.Errorf("the self-rate is %v, want 1", got["USD"])
	}

	if !fetchedAt.Equal(fixtureNow) {
		t.Errorf("fetched at %v, want %v", fetchedAt, fixtureNow)
	}
}

// TestABadRateIsRefused checks the two ways a stored rate would poison every
// revenue figure that used it.
func TestABadRateIsRefused(t *testing.T) {
	db, _ := newFixture(t)

	ctx := context.Background()

	if err := StoreRates(ctx, db, "USD", map[string]float64{"EUR": 0}, fixtureNow); err == nil {
		t.Error("a rate of zero must be refused")
	}

	if err := StoreRates(ctx, db, "USD", map[string]float64{"euros": 1.08}, fixtureNow); err == nil {
		t.Error("a currency that is not a three-letter code must be refused")
	}

	if err := StoreRates(ctx, db, "dollars", map[string]float64{"EUR": 1.08}, fixtureNow); err == nil {
		t.Error("a reporting currency that is not a three-letter code must be refused")
	}
}

// TestRatesGoStaleAfterADay pins the refresh interval. A day is the right
// granularity for reporting: intra-day movement is noise against a month's
// revenue, and a rate per hour would change yesterday's total on every reload.
func TestRatesGoStaleAfterADay(t *testing.T) {
	if !RatesStale(time.Time{}, fixtureNow) {
		t.Error("rates that were never fetched are stale")
	}

	if RatesStale(fixtureNow.Add(-23*time.Hour), fixtureNow) {
		t.Error("rates fetched 23 hours ago are still current")
	}

	if !RatesStale(fixtureNow.Add(-25*time.Hour), fixtureNow) {
		t.Error("rates fetched 25 hours ago are stale")
	}
}

// fixedSource is a rate source that answers from a map and counts its calls,
// so a test can tell a refresh that happened from one that was skipped.
type fixedSource struct {
	rates map[string]float64
	calls int
	err   error
}

// Fetch answers the configured rates and records that it was asked.
func (f *fixedSource) Fetch(_ context.Context, _ string) (map[string]float64, error) {
	f.calls++

	return f.rates, f.err
}

// TestRefreshOnlyFetchesWhenTheRatesAreStale checks that the daily job is
// actually daily, rather than a fetch on every report.
func TestRefreshOnlyFetchesWhenTheRatesAreStale(t *testing.T) {
	db, _ := newFixture(t)

	ctx := context.Background()
	source := &fixedSource{rates: map[string]float64{"EUR": 1.1}}

	refreshed, err := RefreshRates(ctx, db, source, "USD", fixtureNow)
	if err != nil {
		t.Fatal(err)
	}

	if !refreshed || source.calls != 1 {
		t.Fatalf("the first refresh fetched %v after %d calls, want true after 1", refreshed, source.calls)
	}

	refreshed, err = RefreshRates(ctx, db, source, "USD", fixtureNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	if refreshed || source.calls != 1 {
		t.Errorf("an hour later it fetched again: refreshed=%v calls=%d", refreshed, source.calls)
	}

	refreshed, err = RefreshRates(ctx, db, source, "USD", fixtureNow.Add(30*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	if !refreshed || source.calls != 2 {
		t.Errorf("a day later it did not fetch: refreshed=%v calls=%d", refreshed, source.calls)
	}
}

// TestAFailedFetchLeavesTheStoredRatesAlone checks that a rates provider being
// down is a job that failed rather than a revenue report that emptied.
func TestAFailedFetchLeavesTheStoredRatesAlone(t *testing.T) {
	db, _ := newFixture(t)

	ctx := context.Background()

	if err := StoreRates(ctx, db, "USD", map[string]float64{"EUR": 1.08}, fixtureNow); err != nil {
		t.Fatal(err)
	}

	source := &fixedSource{err: errors.New("the rates provider is down")}

	if _, err := RefreshRates(ctx, db, source, "USD", fixtureNow.Add(48*time.Hour)); err == nil {
		t.Fatal("a failed fetch must be reported")
	}

	rates, _, err := ReadRates(ctx, db, "USD")
	if err != nil {
		t.Fatal(err)
	}

	for _, rate := range rates {
		if rate.Base == "EUR" && rate.Rate != 1.08 {
			t.Errorf("the stored EUR rate changed to %v", rate.Rate)
		}
	}
}

// TestMinorUnitsRenderAsMoney checks the one place a stored integer becomes a
// human number.
func TestMinorUnitsRenderAsMoney(t *testing.T) {
	cases := map[int64]string{
		0:     "0.00",
		5:     "0.05",
		50:    "0.50",
		5000:  "50.00",
		12345: "123.45",
		-250:  "-2.50",
	}

	for amount, want := range cases {
		if got := FormatMinor(amount); got != want {
			t.Errorf("FormatMinor(%d) = %q, want %q", amount, got, want)
		}
	}
}

// TestTheAttributionNoticeExists checks that the sentence explaining why
// payment providers never appear as sources lives with the code that makes it
// true. It is the correct behaviour and it looks exactly like a bug.
func TestTheAttributionNoticeExists(t *testing.T) {
	if AttributionNotice == "" {
		t.Error("nothing documents why a payment provider is not a source")
	}
}
