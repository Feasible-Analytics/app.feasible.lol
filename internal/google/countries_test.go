//
// countries_test.go
// One country, one code, on every card of the same dashboard.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package google

import "testing"

// TestCountryAlpha2Converts covers the join between two vocabularies. Search
// Console reports three-letter codes and every other country row in the product
// is two-letter, so a missing conversion would show one country twice under two
// different labels.
func TestCountryAlpha2Converts(t *testing.T) {
	for code, wanted := range map[string]string{
		"usa": "US",
		"USA": "US",
		"gbr": "GB",
		"deu": "DE",
		"aus": "AU",
	} {
		if got := CountryAlpha2(code); got != wanted {
			t.Errorf("CountryAlpha2(%q) = %q, want %q", code, got, wanted)
		}
	}
}

// TestUnknownCountryIsKept covers Search Console's own "zzz", which it uses for
// traffic it cannot place. A row that says so is more honest than one silently
// dropped from a total the reader is comparing against.
func TestUnknownCountryIsKept(t *testing.T) {
	if got := CountryAlpha2("zzz"); got != "zzz" {
		t.Errorf("CountryAlpha2(\"zzz\") = %q, want the input kept", got)
	}

	if got := CountryAlpha2(""); got != "" {
		t.Errorf("CountryAlpha2(\"\") = %q", got)
	}
}

// TestCodesAreDistinct guards the table against a copy-paste that would merge
// two countries into one row.
func TestCodesAreDistinct(t *testing.T) {
	seen := make(map[string]string, len(alpha2))

	for three, two := range alpha2 {
		if first, clash := seen[two]; clash {
			t.Errorf("%q and %q both map to %q", first, three, two)
		}

		seen[two] = three
	}

	if len(alpha2) < 240 {
		t.Errorf("%d codes, want the ISO list rather than a partial one", len(alpha2))
	}
}
