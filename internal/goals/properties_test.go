//
// properties_test.go
// The allow-list, the declared scopes, and the bucket for events that had none.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import (
	"context"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// TestAPropertyIsRegisteredWithAScope checks the registry the whole feature
// turns on. There is no default scope on purpose: an unscoped property is
// exactly the thing this table exists to make impossible.
func TestAPropertyIsRegisteredWithAScope(t *testing.T) {
	db, _ := newFixture(t)

	ctx := context.Background()

	if _, err := Allow(ctx, db, siteID, "plan", ScopeEvent, fixtureNow); err != nil {
		t.Fatal(err)
	}

	if _, err := Allow(ctx, db, siteID, "ab_test_group", ScopeSession, fixtureNow); err != nil {
		t.Fatal(err)
	}

	if _, err := Allow(ctx, db, siteID, "whatever", "hit", fixtureNow); err == nil {
		t.Error("a property registered with an unknown scope must be refused")
	}

	scopes, err := Scopes(ctx, db, []int64{siteID})
	if err != nil {
		t.Fatal(err)
	}

	if scopes["plan"] != string(ScopeEvent) {
		t.Errorf("plan is scoped %q, want %q", scopes["plan"], ScopeEvent)
	}

	if scopes["ab_test_group"] != string(ScopeSession) {
		t.Errorf("ab_test_group is scoped %q, want %q", scopes["ab_test_group"], ScopeSession)
	}
}

// TestAPropertyCanBeRescoped checks the fix path. The first guess is often
// wrong, and correcting it has to be one dropdown rather than a support
// ticket, because the scope decides what every rate filtered by it divides by.
func TestAPropertyCanBeRescoped(t *testing.T) {
	db, _ := newFixture(t)

	ctx := context.Background()

	if _, err := Allow(ctx, db, siteID, "ab_test_group", ScopeEvent, fixtureNow); err != nil {
		t.Fatal(err)
	}

	if _, err := Allow(ctx, db, siteID, "ab_test_group", ScopeSession, fixtureNow); err != nil {
		t.Fatal(err)
	}

	list, err := Allowed(ctx, db, siteID)
	if err != nil {
		t.Fatal(err)
	}

	if len(list) != 1 {
		t.Fatalf("re-scoping made %d rows, want 1", len(list))
	}

	if list[0].Scope != ScopeSession {
		t.Errorf("scope = %q, want %q", list[0].Scope, ScopeSession)
	}

	if err := Disallow(ctx, db, siteID, "ab_test_group"); err != nil {
		t.Fatal(err)
	}

	list, err = Allowed(ctx, db, siteID)
	if err != nil {
		t.Fatal(err)
	}

	if len(list) != 0 {
		t.Errorf("after removal the allow-list has %d rows, want 0", len(list))
	}
}

// TestAPropertyNameIsBounded checks the limits a name has to satisfy to be
// storable and reportable at all.
func TestAPropertyNameIsBounded(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", false},
		{"plan", true},
		{strings.Repeat("x", ingest.MaxPropNameLength), true},
		{strings.Repeat("x", ingest.MaxPropNameLength+1), false},
		{`say "hello"`, false},
		{`back\slash`, false},
	}

	for _, tc := range cases {
		err := validatePropertyName(tc.name)

		if (err == nil) != tc.want {
			t.Errorf("validatePropertyName(%q) = %v, want ok=%v", tc.name, err, tc.want)
		}
	}
}

// TestThePropertyReportHasANoneBucket is the diagnostic the card exists for.
// Two of the fixture's twenty-one events carry a plan; the other nineteen are
// the bucket, and dropping them would hide the one thing worth knowing —
// that most events do not carry the property at all.
func TestThePropertyReportHasANoneBucket(t *testing.T) {
	db, _ := newFixture(t)

	ctx := context.Background()

	rows, err := Values(ctx, db, PropertyRequest{
		SiteID: siteID,
		Window: fixtureWindow(t),
		Name:   "plan",
	})
	if err != nil {
		t.Fatal(err)
	}

	var none, growth, starter int64

	for _, row := range rows {
		switch {
		case row.Missing:
			none = row.Events
		case row.Value == "growth":
			growth = row.Events
		case row.Value == "starter":
			starter = row.Events
		}
	}

	if none != 19 {
		t.Errorf("(none) bucket = %d events, want 19", none)
	}

	if growth != 1 || starter != 1 {
		t.Errorf("plan values = growth %d, starter %d, want 1 and 1", growth, starter)
	}
}

// TestThePropertyReportCanBeScopedToOneEvent checks the way the card is
// actually read: the plans people signed up on, not every plan value anywhere
// on the site.
func TestThePropertyReportCanBeScopedToOneEvent(t *testing.T) {
	db, _ := newFixture(t)

	rows, err := Values(context.Background(), db, PropertyRequest{
		SiteID:    siteID,
		Window:    fixtureWindow(t),
		Name:      "plan",
		EventName: "Signup",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 2 {
		t.Fatalf("signup plans returned %d rows, want 2", len(rows))
	}

	for _, row := range rows {
		if row.Missing {
			t.Error("every signup carried a plan, so there is no (none) bucket")
		}

		if row.Events != 1 || row.Visitors != 1 {
			t.Errorf("plan %q = %d events by %d visitors, want 1 and 1", row.Value, row.Events, row.Visitors)
		}
	}
}

// TestSeenPropertiesComeFromTheData checks the list the settings screen offers.
// A customer should be choosing from the names their own tracker is sending
// rather than typing one from memory and wondering why the report is empty.
func TestSeenPropertiesComeFromTheData(t *testing.T) {
	db, _ := newFixture(t)

	names, err := Seen(context.Background(), db, siteID, fixtureWindow(t), 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(names) != 1 || names[0] != "plan" {
		t.Errorf("seen properties = %v, want [plan]", names)
	}
}

// TestTruncatedPropertiesAreSurfaced checks that the ingest counters become a
// sentence somebody can act on. The cap is thirty properties an event and it
// is not configurable, which is only defensible if the customer can see when
// they hit it.
func TestTruncatedPropertiesAreSurfaced(t *testing.T) {
	counters := ingest.NewCounters()

	counters.Truncated(siteID, ingest.Truncation{
		PropsDropped:        20,
		PropNamesTruncated:  1,
		PropValuesTruncated: 2,
		PropsUnsupported:    3,
	})

	// Another site's numbers must not leak into this one's panel.
	counters.Truncated(2, ingest.Truncation{PropsDropped: 99})

	health := PropertyHealth(counters.Snapshot(), siteID)

	if health.OverLimit != 20 {
		t.Errorf("over-limit properties = %d, want 20", health.OverLimit)
	}

	if health.NamesTruncated != 1 || health.ValuesTruncated != 2 || health.Unsupported != 3 {
		t.Errorf("health = %+v, want 1 name, 2 values and 3 unsupported", health)
	}

	if !strings.Contains(health.Message, "30") {
		t.Errorf("the message must name the limit, got %q", health.Message)
	}

	quiet := PropertyHealth(ingest.NewCounters().Snapshot(), siteID)
	if quiet.Message != "" {
		t.Errorf("a site with nothing truncated says %q, want nothing", quiet.Message)
	}
}

// TestThePIINoticeExists checks that the sentence the settings screen has to
// show lives beside the behaviour. Properties are customer-controlled free
// text that lands verbatim in API responses, and the only defence that works
// is telling people plainly.
func TestThePIINoticeExists(t *testing.T) {
	if PIINotice == "" {
		t.Error("the properties screen has no sentence to show about personal data")
	}
}

// fixtureWindow is the fixture's two days as an absolute window, which is what
// the reports that do not go through the compiler take.
func fixtureWindow(t *testing.T) Window {
	t.Helper()

	return Window{Start: at(29, 0, 0), End: at(31, 0, 0)}
}
