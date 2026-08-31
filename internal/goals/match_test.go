//
// match_test.go
// What the two wildcards mean, asserted one path at a time.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import "testing"

// TestWildcardsMatchWhatTheyClaim is the table that defines the feature. One
// star stays inside a path segment and two cross them, and every row here is a
// path somebody would reasonably expect to match or not match.
func TestWildcardsMatchWhatTheyClaim(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		// An exact path is exact, including its trailing slash. Normalising
		// one here would make a goal disagree with Top Pages about which page
		// it is counting.
		{"/order/complete", "/order/complete", true},
		{"/order/complete", "/order/complete/", false},
		{"/order/complete", "/order/completely", false},
		{"/order/complete", "/shop/order/complete", false},

		// One star is one segment.
		{"/blog/*", "/blog/hello", true},
		{"/blog/*", "/blog/hello/world", false},
		{"/blog/*", "/blog/", true},
		{"/blog/*", "/blog", false},

		// Two stars cross segments.
		{"/blog/**", "/blog/hello", true},
		{"/blog/**", "/blog/hello/world", true},
		{"/blog/**", "/blog/", true},
		{"/blog/**", "/blogging", false},

		// A star in the middle of a segment.
		{"/checkout*", "/checkout", true},
		{"/checkout*", "/checkout-2", true},
		{"/checkout*", "/checkout/payment", false},

		// Two stars anywhere.
		{"/**/complete", "/order/complete", true},
		{"/**/complete", "/shop/order/complete", true},
		{"/**", "/anything/at/all", true},

		// Regular-expression characters in a path are literal characters. A
		// dot that matched any character would make /pricing.html a goal that
		// also counted /pricingXhtml.
		{"/pricing.html", "/pricing.html", true},
		{"/pricing.html", "/pricingXhtml", false},
		{"/a+b", "/a+b", true},
		{"/a+b", "/aab", false},

		// A pattern with a space around it is trimmed before matching, which
		// is the invisible failure this rule exists to stop.
		{"  /signup  ", "/signup", true},
	}

	for _, tc := range cases {
		if got := Matches(tc.pattern, tc.path); got != tc.want {
			t.Errorf("Matches(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// TestAnExactPathNeedsNoRegularExpression checks the fast path is taken. A
// goal without a wildcard compiles to an equality against one interned id
// rather than a scan of every distinct path a site has ever served.
func TestAnExactPathNeedsNoRegularExpression(t *testing.T) {
	if hasWildcard("/order/complete") {
		t.Error("an exact path must not be treated as a pattern")
	}

	if !hasWildcard("/blog/**") {
		t.Error("a pattern must be treated as one")
	}
}

// TestAPageGoalOnlyCountsPageviews pins the filter a page goal compiles to.
// Without the event-name half it would also count the engagement pings and
// custom events fired from that page, which is two or three times the number
// the customer expects.
func TestAPageGoalOnlyCountsPageviews(t *testing.T) {
	goal := Goal{Kind: KindPage, PagePattern: "/order/complete"}

	filters, err := goal.Filters()
	if err != nil {
		t.Fatal(err)
	}

	if len(filters) != 2 {
		t.Fatalf("a page goal compiled to %d filters, want 2", len(filters))
	}

	if filters[0].Dimension != "event:name" || filters[0].Values[0] != "pageview" {
		t.Errorf("first filter = %+v, want the pageview name", filters[0])
	}

	if filters[1].Dimension != "event:page" || filters[1].Operator != "is" {
		t.Errorf("second filter = %+v, want an exact page match", filters[1])
	}
}

// TestAPropertyConstraintBecomesAPropertyFilter checks the third filter a
// constrained goal carries.
func TestAPropertyConstraintBecomesAPropertyFilter(t *testing.T) {
	goal := Goal{
		Kind:       KindEvent,
		EventName:  "Purchase",
		Properties: []PropertyConstraint{{Name: "plan", Value: "growth"}},
	}

	filters, err := goal.Filters()
	if err != nil {
		t.Fatal(err)
	}

	if len(filters) != 2 {
		t.Fatalf("compiled to %d filters, want 2", len(filters))
	}

	if filters[1].Dimension != "event:props:plan" || filters[1].Values[0] != "growth" {
		t.Errorf("property filter = %+v", filters[1])
	}
}
