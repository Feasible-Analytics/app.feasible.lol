//
// revenue.go
// The money metrics: summing minor units across currencies without a float in sight.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// revenueJoin is the one join the money metrics need. Amounts live in the cold
// event_details table, so a revenue report pays for a join that the common
// query path never touches — which is exactly the trade the hot and cold split
// was made for.
const revenueJoin = "JOIN event_details ed ON ed.event_id = e.id"

// revenue answers every money metric the query asked for, in one pass.
//
// One pass rather than one per metric because the three of them share a sum:
// total revenue, average revenue and revenue per visitor differ only in what
// they divide it by, and running the scan three times would be three chances
// for them to disagree about the same money.
func (x *executor) revenue(ctx context.Context, r Resolved, groups *groupSet, keys map[int][]any) error {
	if x.revenueDone == nil {
		x.revenueDone = map[int64]bool{}
	}

	// The pass is keyed by segment, because a range answered from two sources
	// runs every special once per segment and the components add up.
	if x.revenueDone[r.Start.Unix()] {
		return nil
	}
	x.revenueDone[r.Start.Unix()] = true

	rates, err := x.revenueRates(ctx, r)
	if err != nil {
		return err
	}

	if len(rates) == 0 {
		// Nothing to convert and nothing to convert it into: this range holds
		// no revenue at all, so zero is the answer rather than a warning about
		// a rate nobody needed.
		return nil
	}

	dims, joins, extra, err := x.dimensions(tableEvents, dimEntry)
	if err != nil {
		return err
	}

	conditions, err := x.conditionsFor(tableEvents, r)
	if err != nil {
		return err
	}

	conditions = append(conditions, extra...)
	conditions = append(conditions, x.restrictions(dims, keys)...)

	converted, currencies := rateExpr(rates)

	conditions = append(conditions,
		expr{SQL: "ed.revenue_amount IS NOT NULL"},
		inText("ed.revenue_currency", currencies),
	)

	st := statement{
		table: tableEvents, alias: tableEvents.alias(),
		joins: withJoin(joins, revenueJoin),
		dims:  dims,
		columns: []expr{
			converted,
			{SQL: "COUNT(*)"},
		},
		conditions: conditions,
	}

	targets := []target{
		{metric: "total_revenue", slot: 0, column: 0},
		{metric: "average_revenue", slot: 0, column: 0},
		{metric: "average_revenue", slot: 1, column: 1},
		{metric: "revenue_per_visitor", slot: 0, column: 0},
	}

	if _, err := x.merge(ctx, st, groups, targets); err != nil {
		return err
	}

	if x.wants("revenue_per_visitor") {
		// Per visitor means per visitor who could have paid, so the divisor is
		// the audience with the goal stripped out. Dividing by the payers
		// instead would be the average order value under a second name.
		if err := x.visitorDenominator(ctx, r, groups, "revenue_per_visitor", 1, false); err != nil {
			return err
		}
	}

	return x.unconvertible(ctx, r, currencies)
}

// revenueRates resolves the reporting currency and the rates into it.
//
// A caller that named a currency gets that one. A caller that did not gets the
// only currency in the data, and is refused when there is more than one:
// picking one silently would either drop money or add two currencies together,
// and both of those are a total somebody would put in a board deck.
func (x *executor) revenueRates(ctx context.Context, r Resolved) (map[string]float64, error) {
	if x.rates != nil {
		return x.rates, nil
	}

	currency := x.query.Currency

	if currency == "" {
		found, err := x.currenciesInRange(ctx, r)
		if err != nil {
			return nil, err
		}

		switch len(found) {
		case 0:
			return nil, nil
		case 1:
			currency = found[0]
		default:
			return nil, invalid("this range holds revenue in %s — set currency to the one you want the "+
				"report totalled in", strings.Join(found, ", "))
		}
	}

	rates, err := x.engine.storedRates(ctx, currency)
	if err != nil {
		return nil, err
	}

	// A currency always converts to itself at one, whether or not anybody
	// stored a rate row for it. Without this a single-currency site would have
	// to run a rates job before it could see its own revenue.
	if rates == nil {
		rates = map[string]float64{}
	}
	rates[currency] = 1

	x.rates = rates
	x.currency = currency

	return rates, nil
}

// currenciesInRange lists the currencies revenue actually arrived in. It is one
// small query and it only runs when the caller did not name a currency, which
// is the case where guessing wrong is expensive.
func (x *executor) currenciesInRange(ctx context.Context, r Resolved) ([]string, error) {
	conditions, err := x.conditionsFor(tableEvents, r)
	if err != nil {
		return nil, err
	}

	conditions = append(conditions,
		expr{SQL: "ed.revenue_amount IS NOT NULL"},
		expr{SQL: "ed.revenue_currency IS NOT NULL AND ed.revenue_currency <> ''"},
	)

	where := and(conditions)

	rows, err := x.engine.db.QueryContext(ctx,
		"SELECT DISTINCT ed.revenue_currency FROM events e "+revenueJoin+" WHERE "+where.SQL+" ORDER BY 1",
		where.Args...)
	if err != nil {
		return nil, fmt.Errorf("query: read revenue currencies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var found []string

	for rows.Next() {
		var currency string

		if err := rows.Scan(&currency); err != nil {
			return nil, fmt.Errorf("query: read revenue currencies: %w", err)
		}

		found = append(found, currency)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read revenue currencies: %w", err)
	}

	return found, nil
}

// unconvertible warns when money was left out of the total because no rate
// covered its currency. Silence here would be a revenue report that is quietly
// missing a market.
func (x *executor) unconvertible(ctx context.Context, r Resolved, currencies []string) error {
	conditions, err := x.conditionsFor(tableEvents, r)
	if err != nil {
		return err
	}

	excluded := inText("ed.revenue_currency", currencies)

	conditions = append(conditions,
		expr{SQL: "ed.revenue_amount IS NOT NULL"},
		expr{SQL: "NOT (" + excluded.SQL + ")", Args: excluded.Args},
	)

	where := and(conditions)

	var (
		count sql.NullInt64
		list  sql.NullString
	)

	if err := x.engine.db.QueryRowContext(ctx,
		"SELECT COUNT(*), GROUP_CONCAT(DISTINCT ed.revenue_currency) FROM events e "+revenueJoin+" WHERE "+where.SQL,
		where.Args...).Scan(&count, &list); err != nil {
		return fmt.Errorf("query: read unconvertible revenue: %w", err)
	}

	if count.Int64 == 0 {
		return nil
	}

	x.warnRevenue(WarnMissingRate, fmt.Sprintf(
		"%d events carrying revenue in %s were left out because no exchange rate into %s is stored",
		count.Int64, list.String, x.currency))

	return nil
}

// warnRevenue attaches a warning to every money metric the query asked for. It
// is per metric rather than per query because a client greys out the figure it
// cannot trust, not the whole panel.
func (x *executor) warnRevenue(code, message string) {
	for _, name := range x.query.Metrics {
		switch name {
		case "total_revenue", "average_revenue", "revenue_per_visitor":
			x.warnings.add(name, code, message)
		}
	}
}

// wants reports whether the query asked for a metric.
func (x *executor) wants(name string) bool {
	for _, metric := range x.query.Metrics {
		if metric == name {
			return true
		}
	}

	return false
}

// rateExpr builds the converted sum and the list of currencies it covers.
//
// The multiplication happens inside the aggregate and the rounding happens once
// on the group total rather than per event, because rounding every row to a
// whole minor unit first would drift by half a unit per event — visible on any
// site with enough orders to care.
func rateExpr(rates map[string]float64) (expr, []string) {
	currencies := make([]string, 0, len(rates))
	for currency := range rates {
		currencies = append(currencies, currency)
	}

	// Sorted so the statement text is stable: the same query has to produce the
	// same SQL, or nothing downstream can cache or compare it.
	sort.Strings(currencies)

	var (
		sql  strings.Builder
		args []any
	)

	sql.WriteString("SUM(ed.revenue_amount * CASE ed.revenue_currency")

	for _, currency := range currencies {
		sql.WriteString(" WHEN ? THEN ?")
		args = append(args, currency, rates[currency])
	}

	sql.WriteString(" ELSE 0 END)")

	return expr{SQL: sql.String(), Args: args}, currencies
}

// inText builds "column IN (?,?,?)" for a list of strings, and a condition that
// matches nothing for an empty list.
func inText(column string, values []string) expr {
	if len(values) == 0 {
		return expr{SQL: "1 = 0"}
	}

	args := make([]any, 0, len(values))
	for _, value := range values {
		args = append(args, value)
	}

	return expr{SQL: column + " IN (" + placeholders(len(values)) + ")", Args: args}
}

// withJoin adds a join unless the dimension compiler already added it. A
// breakdown by a custom property joins the cold table too, and naming the same
// alias twice is a syntax error rather than a duplicate row.
func withJoin(joins []string, join string) []string {
	for _, existing := range joins {
		if existing == join {
			return joins
		}
	}

	return append(append([]string{}, joins...), join)
}

// storedRates reads the exchange rates into one reporting currency.
//
// The rates are written by the goals package, which owns the refresh policy;
// the read lives here because the conversion happens inside the aggregate and
// the compiler is the only thing that can put it there.
func (e *Engine) storedRates(ctx context.Context, currency string) (map[string]float64, error) {
	rows, err := e.db.QueryContext(ctx, "SELECT base, rate FROM currency_rates WHERE quote = ?", currency)
	if err != nil {
		return nil, fmt.Errorf("query: read exchange rates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	rates := map[string]float64{}

	for rows.Next() {
		var (
			base string
			rate float64
		)

		if err := rows.Scan(&base, &rate); err != nil {
			return nil, fmt.Errorf("query: read exchange rates: %w", err)
		}

		if rate > 0 {
			rates[base] = rate
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read exchange rates: %w", err)
	}

	return rates, nil
}
