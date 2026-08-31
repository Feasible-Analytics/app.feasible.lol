//
// special_test.go
// The two conversion rates, and the denominator a declared property scope picks.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// signupFilter is the goal every conversion test below measures.
var signupFilter = Filter{Operator: OpIs, Dimension: "event:name", Values: []string{"Signup"}}

// TestTheTwoConversionRatesUseDifferentDivisors is the test that stops the two
// from being confused for each other.
//
// One of the fixture's three visitors signed up, and that visitor's visit came
// from no source at all. So the global rate is one in three, and the rate
// within the direct group is one in one — the same conversion, measured
// against two different populations, and a report that reaches for the wrong
// one shows a plausible number that answers a question nobody asked.
func TestTheTwoConversionRatesUseDifferentDivisors(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("conversion_rate", "group_conversion_rate")
	q.Dimensions = []string{"visit:source"}
	q.Filters = []Filter{signupFilter}

	result := run(t, engine, q)

	if len(result.Results) != 1 {
		t.Fatalf("got %d rows, want the one source that converted", len(result.Results))
	}

	row := result.Results[0]

	closeTo(t, "conversion_rate", row.Metrics[0], 33.333)
	closeTo(t, "group_conversion_rate", row.Metrics[1], 100)

	if row.Metrics[0] == row.Metrics[1] {
		t.Error("the two conversion rates must not agree here — they divide by different things")
	}
}

// TestWithoutABreakdownBothRatesAgree is the other half of the pair. With no
// dimension there is only one group and it is the whole period, so the two
// divisors are the same set and the numbers have to match.
func TestWithoutABreakdownBothRatesAgree(t *testing.T) {
	engine := newEngine(t)

	q := baseQuery("conversion_rate", "group_conversion_rate")
	q.Filters = []Filter{signupFilter}

	row := run(t, engine, q).Results[0]

	closeTo(t, "conversion_rate", row.Metrics[0], 33.333)
	closeTo(t, "group_conversion_rate", row.Metrics[1], 33.333)
}

// TestGroupConversionRateWithoutAGoalIsRefused checks that the new metric is
// held to the same rule as the old one: a rate whose numerator and denominator
// are the same set is 100% everywhere and means nothing.
func TestGroupConversionRateWithoutAGoalIsRefused(t *testing.T) {
	engine := newEngine(t)

	if _, err := engine.Run(context.Background(), baseQuery("group_conversion_rate")); err == nil {
		t.Fatal("a group conversion rate with nothing to convert must be refused")
	}
}

// TestADeclaredPropertyScopeChangesTheDenominator is the decision the incumbent
// never made, measured.
//
// Two visitors are in the treatment variant and one of them signs up. Scoped
// as an event property, the variant is part of the conversion and the rate
// divides by everybody: one in five. Scoped as a session property it describes
// the audience, so the rate divides by the visitors in that variant: one in
// two. Only the second is a number an A/B test can be read from, and without a
// declared scope there is no way to know which one you are looking at.
func TestADeclaredPropertyScopeChangesTheDenominator(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeVariantVisits(t, account)

	q := baseQuery("group_conversion_rate")
	q.Dimensions = []string{"event:props:ab_test_group"}
	q.Filters = []Filter{signupFilter}

	// Registered as an event property: the breakdown is part of the goal, so
	// it is stripped from the denominator and the rate is measured against
	// every visitor in the period.
	allowProperty(t, account, "ab_test_group", propScopeEvent)

	closeTo(t, "event-scoped rate", run(t, engine, q).Results[0].Metrics[0], 20)

	// Registered as a session property: the breakdown describes the visitor,
	// so it stays in the denominator and the rate is measured against the
	// visitors who were in that variant.
	allowProperty(t, account, "ab_test_group", propScopeSession)

	closeTo(t, "session-scoped rate", run(t, engine, q).Results[0].Metrics[0], 50)
}

// TestAGlobalConversionRateIgnoresPropertyScope checks that the scope only
// moves the grouped divisor. The global one is every visitor in the period by
// definition, so it is one in five either way.
func TestAGlobalConversionRateIgnoresPropertyScope(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeVariantVisits(t, account)

	q := baseQuery("conversion_rate")
	q.Dimensions = []string{"event:props:ab_test_group"}
	q.Filters = []Filter{signupFilter}

	allowProperty(t, account, "ab_test_group", propScopeEvent)
	closeTo(t, "event-scoped global rate", run(t, engine, q).Results[0].Metrics[0], 20)

	allowProperty(t, account, "ab_test_group", propScopeSession)
	closeTo(t, "session-scoped global rate", run(t, engine, q).Results[0].Metrics[0], 20)
}

// TestASessionScopedPropertyCanCarryAVisitMetric checks the capability a
// declared scope buys. A property with one value per visit has a visit-grain
// answer, so a bounce rate per variant is a question with an answer — where
// the same question about an event-scoped property is refused.
func TestASessionScopedPropertyCanCarryAVisitMetric(t *testing.T) {
	engine, account := newEngineWithAccount(t)

	writeVariantVisits(t, account)

	q := baseQuery("visits", "bounce_rate")
	q.Dimensions = []string{"event:props:ab_test_group"}

	allowProperty(t, account, "ab_test_group", propScopeEvent)

	if _, err := engine.Run(context.Background(), q); err == nil {
		t.Error("a bounce rate per event-scoped property has no correct value and must be refused")
	}

	allowProperty(t, account, "ab_test_group", propScopeSession)

	result := run(t, engine, q)

	if len(result.Results) != 1 {
		t.Fatalf("got %d rows, want one variant", len(result.Results))
	}

	// Both variant visits are the ones written below, and neither bounced.
	closeTo(t, "visits in the variant", result.Results[0].Metrics[0], 2)
}

// writeVariantVisits adds two visits carrying an A/B variant property, one of
// which converts. They are written on top of the shared fixture so that the
// period has visitors who are not in the variant at all, which is what makes
// the two denominators different numbers.
func writeVariantVisits(t *testing.T, account *accounts.Account) {
	t.Helper()

	ctx := context.Background()

	id := func(dimension intern.Dimension, value string) int64 {
		got, err := account.Intern.ID(ctx, dimension, value)
		if err != nil {
			t.Fatal(err)
		}

		return got
	}

	pageview := id(intern.EventName, "pageview")
	signup := id(intern.EventName, "Signup")

	// Two more visitors, both in the treatment variant, both on the fixture's
	// last day. One of them signs up.
	visits := []struct {
		session int64
		user    int64
		event   int64
		signed  bool
	}{
		{session: 10, user: 2001, event: 100, signed: true},
		{session: 11, user: 2002, event: 102, signed: false},
	}

	for _, visit := range visits {
		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration, is_bounce, pageviews, events)
			VALUES (?, 1, ?, ?, ?, 30, 0, 2, 2)`,
			visit.session, visit.user, at(30, 11, 0), at(30, 11, 30)); err != nil {
			t.Fatal(err)
		}

		writeVariantEvent(t, account, visit.event, visit.session, visit.user, pageview, at(30, 11, 0))

		if !visit.signed {
			continue
		}

		writeVariantEvent(t, account, visit.event+1, visit.session, visit.user, signup, at(30, 11, 1))
	}
}

// writeVariantEvent writes one event carrying the variant property, hot row and
// cold row together so the has_details flag can never claim a row that is not
// there.
func writeVariantEvent(t *testing.T, account *accounts.Account, id, session, user, name, timestamp int64) {
	t.Helper()

	ctx := context.Background()

	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id, scroll_depth, has_details)
		VALUES (?, 1, ?, ?, ?, ?, 255, 1)`, id, timestamp, name, user, session); err != nil {
		t.Fatal(err)
	}

	props, err := json.Marshal(map[string]string{"ab_test_group": "treatment"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := account.Writer().ExecContext(ctx,
		"INSERT INTO event_details (event_id, props) VALUES (?, ?)", id, string(props)); err != nil {
		t.Fatal(err)
	}
}

// allowProperty registers a property under a scope, replacing whatever scope it
// had. It writes the row the compiler reads rather than going through the goals
// package, which would be an import cycle: goals is built on this compiler.
func allowProperty(t *testing.T, account *accounts.Account, name, scope string) {
	t.Helper()

	if _, err := account.Writer().ExecContext(context.Background(), `
		INSERT INTO allowed_properties (site_id, name, scope, created_at)
		VALUES (1, ?, ?, 0)
		ON CONFLICT(site_id, name) DO UPDATE SET scope = excluded.scope`, name, scope); err != nil {
		t.Fatal(err)
	}
}
