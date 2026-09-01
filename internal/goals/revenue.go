//
// revenue.go
// Money on a goal: minor units, and the rates a cross-currency report divides by.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// RateRefreshInterval is how often exchange rates are refreshed. A day is the
// right granularity for reporting: intra-day movement is noise against a
// month's revenue, and a rate per hour would make yesterday's total change
// every time somebody reloaded the page.
const RateRefreshInterval = 24 * time.Hour

// MinorUnits is the divisor between a stored amount and a displayed one. Every
// amount in the system is an integer count of hundredths, because a currency
// amount held in a float is a rounding error waiting for a large enough report
// to make it visible.
//
// A hundred is not universal — a yen has no minor unit and a dinar has three —
// but it is what every payment provider reports and matching them is what
// keeps reconciliation possible. Where it is wrong it is wrong by a constant,
// which is recoverable; a float is not.
const MinorUnits = 100

// AttributionNotice is what the revenue documentation has to say plainly.
//
// Revenue is attributed to the session, and a session's source is fixed at its
// first event. So a purchase that came back from a payment gateway is credited
// to the search or the campaign that started the visit, and the gateway itself
// never appears as a source. That is the correct behaviour and it looks
// exactly like a bug, which is why it is written down here rather than left
// for somebody to rediscover.
const AttributionNotice = "Revenue is credited to the source that started the visit, not to the last page before payment. " +
	"Payment providers therefore never appear in your sources report, even though visitors return through them."

// Rate is one stored exchange rate.
type Rate struct {
	// Base is the currency money is held in; Quote is the currency a report is
	// being read in.
	Base  string `json:"base"`
	Quote string `json:"quote"`

	Rate float64 `json:"rate"`

	FetchedAt int64 `json:"fetched_at"`
}

// validateCurrency refuses anything that is not an ISO 4217 alphabetic code.
// It is three uppercase letters or it is a typo, and a typo stored here is a
// revenue report that silently matches no events at all.
func validateCurrency(code string) error {
	if len(code) != 3 {
		return invalid("a currency is a three-letter code such as USD, not %q", code)
	}

	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return invalid("a currency is three uppercase letters such as USD, not %q", code)
		}
	}

	return nil
}

// StoreRates writes the rates for one reporting currency, stamping them all
// with the same instant. They are written together because a report that
// converted three currencies with rates from three different afternoons is a
// total nobody could reproduce.
//
// The rates are read back by the query compiler, which owns the read side
// because conversion happens inside the aggregate; this package owns the write
// side and the refresh policy.
func StoreRates(ctx context.Context, db *sql.DB, quote string, rates map[string]float64, now time.Time) error {
	quote = strings.ToUpper(strings.TrimSpace(quote))

	if err := validateCurrency(quote); err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("goals: store rates: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	fetched := now.Unix()

	for base, rate := range rates {
		base = strings.ToUpper(strings.TrimSpace(base))

		if err := validateCurrency(base); err != nil {
			return err
		}

		if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return invalid("the rate from %s to %s must be a positive number, not %v", base, quote, rate)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO currency_rates (base, quote, rate, fetched_at)
			VALUES (?,?,?,?)
			ON CONFLICT(base, quote) DO UPDATE SET rate = excluded.rate, fetched_at = excluded.fetched_at`,
			base, quote, rate, fetched,
		); err != nil {
			return fmt.Errorf("goals: store rates: %w", err)
		}
	}

	// A currency converts to itself at one. It is stored rather than special-
	// cased so that a report in the currency the money is already in reads the
	// same row as every other conversion, and a missing self-rate can never be
	// the reason a total comes back empty.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO currency_rates (base, quote, rate, fetched_at)
		VALUES (?,?,1,?)
		ON CONFLICT(base, quote) DO UPDATE SET rate = 1, fetched_at = excluded.fetched_at`,
		quote, quote, fetched,
	); err != nil {
		return fmt.Errorf("goals: store rates: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("goals: store rates: %w", err)
	}

	return nil
}

// ReadRates returns every rate into one reporting currency, with the oldest
// fetch time among them. The age is returned rather than hidden so a report
// can say how old the number it converted with is instead of implying it is
// live.
func ReadRates(ctx context.Context, db *sql.DB, quote string) ([]Rate, time.Time, error) {
	quote = strings.ToUpper(strings.TrimSpace(quote))

	rows, err := db.QueryContext(ctx,
		"SELECT base, quote, rate, fetched_at FROM currency_rates WHERE quote = ? ORDER BY base", quote)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("goals: read rates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		list   []Rate
		oldest int64
	)

	for rows.Next() {
		var rate Rate

		if err := rows.Scan(&rate.Base, &rate.Quote, &rate.Rate, &rate.FetchedAt); err != nil {
			return nil, time.Time{}, fmt.Errorf("goals: read rates: %w", err)
		}

		if oldest == 0 || rate.FetchedAt < oldest {
			oldest = rate.FetchedAt
		}

		list = append(list, rate)
	}

	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("goals: read rates: %w", err)
	}

	if oldest == 0 {
		return list, time.Time{}, nil
	}

	return list, time.Unix(oldest, 0).UTC(), nil
}

// RatesStale reports whether the stored rates are older than the refresh
// interval. It is a function rather than a comparison at the call site so that
// "how old is too old" has one answer in the whole product.
func RatesStale(fetchedAt, now time.Time) bool {
	if fetchedAt.IsZero() {
		return true
	}

	return now.Sub(fetchedAt) >= RateRefreshInterval
}

// RateSource fetches current rates for one reporting currency. It is an
// interface because where the numbers come from is an operator's decision — a
// public rates API, a bank feed, or a fixed table on a self-hosted box with no
// outbound network at all — and none of those belong wired into a report.
type RateSource interface {
	// Fetch returns the rate from each currency into quote.
	Fetch(ctx context.Context, quote string) (map[string]float64, error)
}

// RefreshRates fetches and stores rates when the stored ones have gone stale,
// and does nothing when they have not. It answers whether it wrote anything,
// so a scheduled job can log the refresh it did rather than the twenty-three
// it skipped.
func RefreshRates(ctx context.Context, db *sql.DB, source RateSource, quote string, now time.Time) (bool, error) {
	if source == nil {
		return false, nil
	}

	_, fetchedAt, err := ReadRates(ctx, db, quote)
	if err != nil {
		return false, err
	}

	if !RatesStale(fetchedAt, now) {
		return false, nil
	}

	rates, err := source.Fetch(ctx, quote)
	if err != nil {
		return false, fmt.Errorf("goals: refresh rates: %w", err)
	}

	if len(rates) == 0 {
		return false, nil
	}

	if err := StoreRates(ctx, db, quote, rates, now); err != nil {
		return false, err
	}

	return true, nil
}

// FormatMinor renders an amount in minor units as a decimal string. It exists
// so that the one place a stored integer becomes a human number is a function
// with a test on it, rather than a division scattered across templates.
func FormatMinor(amount int64) string {
	sign := ""
	if amount < 0 {
		sign = "-"
		amount = -amount
	}

	return fmt.Sprintf("%s%d.%02d", sign, amount/MinorUnits, amount%MinorUnits)
}
