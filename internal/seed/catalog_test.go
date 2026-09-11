//
// catalog_test.go
// The seeded events match the names the tracker really sends.
//
// Created: 2026-09-11
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/goals"
)

// TestTheSeedFiresEveryAutomaticGoal is the guard on a mistake that leaves no
// trace.
//
// Every seeded site is given the four goals we provision, and they are matched
// by exact event name. A catalogue entry spelled "Outbound Link Click" instead
// of "Outbound Link: Click" still seeds perfectly good traffic, and the goals
// report then says the feature we ship to every customer has never once fired
// — which reads as a broken report rather than as a typo in a fixture.
func TestTheSeedFiresEveryAutomaticGoal(t *testing.T) {
	fired := map[string]bool{}
	for _, event := range customEvents {
		fired[event.Name] = true
	}

	for _, name := range []string{
		goals.EventNotFound,
		goals.EventOutboundClick,
		goals.EventFileDownload,
		goals.EventFormSubmission,
	} {
		if !fired[name] {
			t.Errorf("no seeded event is named %q, so that automatic goal reports zero conversions", name)
		}
	}
}

// TestClickGoalsCarryTheURLTheDashboardReads pins the property behind two of
// the four breakdowns. The dashboard groups outbound clicks and downloads by
// `event:props:url` because that is what the tracker attaches, so a fixture
// sending the same value under another name produces a tab with one "(none)"
// row in it.
func TestClickGoalsCarryTheURLTheDashboardReads(t *testing.T) {
	for _, event := range customEvents {
		if event.Name != goals.EventOutboundClick && event.Name != goals.EventFileDownload {
			continue
		}

		if event.Props["url"] == "" {
			t.Errorf("seeded %q carries no url property", event.Name)
		}
	}
}

// TestOnlyTheMissingPageOverridesItsAddress keeps the override narrow. A
// custom event fires where the visitor already is; a 404 is the one that has
// to name an address of its own, and any other event doing so would appear in
// the pages report as a page nobody ever visited.
func TestOnlyTheMissingPageOverridesItsAddress(t *testing.T) {
	for _, event := range customEvents {
		if event.Path != "" && event.Name != goals.EventNotFound {
			t.Errorf("seeded %q sets its own path %q", event.Name, event.Path)
		}
	}
}
