//
// timefmt_test.go
// The clock decision, including the two boundaries everybody gets wrong.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package timefmt

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// at builds one UTC instant on a fixed day, so a case reads as the clock time
// it is about rather than as a date.
func at(hour, minute int) time.Time {
	return time.Date(2026, 9, 4, hour, minute, 0, 0, time.UTC)
}

// TestClockRendersBothDials covers midnight and noon deliberately. Twelve-hour
// arithmetic that uses `hour % 12` without the correction prints midnight as
// "0:00 AM" and noon as "0:00 PM", and both look plausible enough to ship.
func TestClockRendersBothDials(t *testing.T) {
	cases := []struct {
		name   string
		cycle  string
		layout string
		when   time.Time
		want   string
	}{
		{"midnight on twelve", Cycle12, "15:04 MST", at(0, 0), "12:00 AM UTC"},
		{"midnight on twenty-four", Cycle24, "15:04 MST", at(0, 0), "00:00 UTC"},
		{"noon on twelve", Cycle12, "15:04 MST", at(12, 0), "12:00 PM UTC"},
		{"noon on twenty-four", Cycle24, "15:04 MST", at(12, 0), "12:00 UTC"},
		{"afternoon on twelve", Cycle12, "15:04 MST", at(13, 0), "1:00 PM UTC"},
		{"afternoon on twenty-four", Cycle24, "15:04 MST", at(13, 0), "13:00 UTC"},
		{"last minute on twelve", Cycle12, "15:04 MST", at(23, 59), "11:59 PM UTC"},
		{"last minute on twenty-four", Cycle24, "15:04 MST", at(23, 59), "23:59 UTC"},

		// The meridiem belongs with the time, not after the timezone, and the
		// date order either side of it must not move.
		{"export prepared stamp", Cycle12, "2006-01-02 15:04 MST", at(14, 30), "2026-09-04 2:30 PM UTC"},
		{"report next run", Cycle12, "Mon 2 Jan 15:04 MST", at(0, 0), "Fri 4 Sep 12:00 AM UTC"},
		{"admin stamp", Cycle12, "2 Jan 15:04 MST", at(14, 30), "4 Sep 2:30 PM UTC"},

		// A layout with no clock in it has nothing to convert, and must not
		// grow a meridiem.
		{"date only is untouched", Cycle12, "2 Jan 2006", at(14, 30), "4 Sep 2026"},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := Clock(test.cycle, test.when, test.layout); got != test.want {
				t.Fatalf("Clock(%q, %q) = %q, want %q", test.cycle, test.layout, got, test.want)
			}
		})
	}
}

// TestAnUnknownCycleReadsAsTwentyFour keeps a bad stored value out of the
// render path. Only an explicit "12" moves the dial.
func TestAnUnknownCycleReadsAsTwentyFour(t *testing.T) {
	for _, cycle := range []string{"", System, "h12", "twelve", "24"} {
		if got := Clock(cycle, at(13, 0), "15:04"); got != "13:00" {
			t.Fatalf("Clock(%q) = %q, want the 24-hour rendering", cycle, got)
		}
	}
}

// withCookie builds a request carrying one clock cookie value.
func withCookie(value string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/settings", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: value})

	return r
}

// TestResolveAsksTheBrowserOnlyForSystem checks the split this package exists
// to make: an explicit choice is the reader's own and is never second-guessed,
// and only "system" defers to the device.
func TestResolveAsksTheBrowserOnlyForSystem(t *testing.T) {
	cases := []struct {
		name       string
		preference string
		request    *http.Request
		want       string
	}{
		{"explicit twelve beats the cookie", Cycle12, withCookie(Cycle24), Cycle12},
		{"explicit twenty-four beats the cookie", Cycle24, withCookie(Cycle12), Cycle24},
		{"system follows the cookie", System, withCookie(Cycle12), Cycle12},
		{"system follows a 24 cookie", System, withCookie(Cycle24), Cycle24},

		// The one-page gap the package documents: a brand-new browser that has
		// not loaded the dashboard yet has no cookie and reads 24.
		{"system with no cookie", System, httptest.NewRequest(http.MethodGet, "/settings", nil), Cycle24},
		{"system with a junk cookie", System, withCookie("h11"), Cycle24},
		{"system with an empty cookie", System, withCookie(""), Cycle24},

		// A value from an older build, or a hand-edited row, must render rather
		// than fail.
		{"an unrecognised preference asks the browser", "hammer", withCookie(Cycle12), Cycle12},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := Resolve(test.preference, test.request); got != test.want {
				t.Fatalf("Resolve(%q) = %q, want %q", test.preference, got, test.want)
			}
		})
	}
}

// TestFromCookieToleratesNoRequest keeps a background render — a job with no
// request in hand — from panicking on the way to a timestamp.
func TestFromCookieToleratesNoRequest(t *testing.T) {
	if got := FromCookie(nil); got != Cycle24 {
		t.Fatalf("FromCookie(nil) = %q, want %q", got, Cycle24)
	}
}

// TestNormaliseKeepsJunkOutOfTheDatabase covers the form post. Anything that is
// not a dial becomes "system", so a crafted field cannot store a value the
// render path would then have to guess about.
func TestNormaliseKeepsJunkOutOfTheDatabase(t *testing.T) {
	cases := map[string]string{
		Cycle12:    Cycle12,
		Cycle24:    Cycle24,
		System:     System,
		"":         System,
		"h12":      System,
		"12; DROP": System,
	}

	for input, want := range cases {
		if got := Normalise(input); got != want {
			t.Fatalf("Normalise(%q) = %q, want %q", input, got, want)
		}
	}
}
