//
// conversions_test.go
// Public conversion adapter contract tests.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"testing"
	"time"
)

// TestFunnelDatesAcceptTheAdvertisedForms keeps the MCP description and the
// real public adapter aligned: people may use a calendar date for convenience
// or RFC3339 when a precise instant matters.
func TestFunnelDatesAcceptTheAdvertisedForms(t *testing.T) {
	date, dateOnly, err := parseFunnelDate("2026-09-01")
	if err != nil || !dateOnly || date.Format("2006-01-02") != "2026-09-01" {
		t.Fatalf("date-only parse = %v, %t, %v", date, dateOnly, err)
	}
	instant, dateOnly, err := parseFunnelDate("2026-09-01T12:34:56-07:00")
	if err != nil || dateOnly || instant.Format(time.RFC3339) != "2026-09-01T12:34:56-07:00" {
		t.Fatalf("RFC3339 parse = %v, %t, %v", instant, dateOnly, err)
	}
	if _, _, err := parseFunnelDate("September 1"); err == nil {
		t.Fatal("an undocumented funnel date format was accepted")
	}
}
