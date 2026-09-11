//
// dimension_test.go
// The registry's own rules, checked against every entry rather than a sample.
//
// Created: 2026-09-11
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import "testing"

// TestEveryUTMTagIsAFullDimension is the guard on a family that was split for
// no reason anybody chose.
//
// The tracker has always captured all five UTM tags, and for a while only three
// of them had a column. The other two went to the cold table as free text, so a
// campaign run with two creatives was one undivided row on the dashboard and
// the tag that said which creative it was could not be grouped by, filtered on,
// or summarised. Nothing about content or term makes them different in kind
// from the source, medium and campaign beside them, so this walks all five and
// requires the same of each.
func TestEveryUTMTagIsAFullDimension(t *testing.T) {
	summarised := map[string]bool{}
	for _, dimension := range RollupDims() {
		summarised[dimension.Name] = true
	}

	for _, name := range []string{
		"visit:utm_source",
		"visit:utm_medium",
		"visit:utm_campaign",
		"visit:utm_content",
		"visit:utm_term",
	} {
		t.Run(name, func(t *testing.T) {
			resolved, err := resolveDimension(name)
			if err != nil {
				t.Fatal(err)
			}

			// Both columns, because a UTM tag describes a visit and is stamped
			// on every event of it: one column alone would answer either the
			// visit questions or the event ones and not both.
			if resolved.EventColumn == "" || resolved.SessionColumn == "" {
				t.Errorf("columns are %q on events and %q on sessions, want both",
					resolved.EventColumn, resolved.SessionColumn)
			}

			// Interned, or the value is an id the dashboard cannot render back
			// into the campaign somebody actually typed into a link.
			if resolved.Interned == "" {
				t.Error("the values are not interned, so no report could label them")
			}

			if !summarised[name] {
				t.Error("the summary cannot answer it, so every campaign report falls back to a raw scan")
			}
		})
	}
}
