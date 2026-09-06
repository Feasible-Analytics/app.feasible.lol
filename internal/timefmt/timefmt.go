//
// timefmt.go
// Whether a clock time is printed on a 12-hour or a 24-hour dial.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package timefmt owns the one decision behind every clock time the product
// prints: twelve hours or twenty-four.
//
// It is deliberately not part of internal/i18n. The choice is personal, not
// linguistic — an American who wants a 24-hour clock and a German who wants a
// 12-hour one are both ordinary people, and deriving the dial from the
// negotiated language gets each of them wrong on every screen they open. The
// two live in separate packages so that link cannot be made by accident.
package timefmt

import (
	"net/http"
	"strings"
	"time"
)

// The three values a stored preference can hold.
//
// System means "nobody has pinned a dial yet" and is what a new account starts
// on. It is never offered on the settings form: nobody can predict what their
// own device is set to, so a "follow my device" row names an outcome the reader
// cannot picture. The form shows the two dials with the one they are on already
// selected, and saving writes that dial explicitly.
const (
	System  = "system"
	Cycle12 = "12"
	Cycle24 = "24"
)

// CookieName carries the browser's own hour cycle to the server.
//
// It exists because nothing in an HTTP request says what clock the reader set
// on their device. Only the browser knows, through
// Intl.DateTimeFormat().resolvedOptions().hourCycle, so the dashboard bundle
// writes what it finds and every later request can read it.
const CookieName = "feasible_clock"

// CookieMaxAge is how long the browser's answer is trusted, in seconds. A year,
// matching the language cookie: a clock preference does not go stale, and the
// dashboard rewrites it whenever the device's answer changes.
const CookieMaxAge = 365 * 24 * 60 * 60

// Resolve turns a stored preference into the dial to actually print on.
//
// An explicit 12 or 24 is the reader's own answer and is returned untouched. A
// stored "system" — and anything unrecognised, which is the same situation from
// the reader's point of view — asks the browser through the cookie.
//
// A request that carries no cookie yet gets 24. That happens exactly once, on
// the first server-rendered screen a brand-new browser reaches before it has
// ever loaded the dashboard; the next load has the cookie and it then stays put
// for a year. Closing that one-page gap would cost a blocking round trip on
// every load, which is a much worse trade than one page on a 24-hour clock.
func Resolve(preference string, r *http.Request) string {
	switch strings.TrimSpace(preference) {
	case Cycle12:
		return Cycle12
	case Cycle24:
		return Cycle24
	}

	return FromCookie(r)
}

// FromCookie reads the browser's own hour cycle, or answers 24 when the cookie
// is absent, empty or holds anything else. Junk is treated as absence rather
// than as an error: the value is written by a script in a browser we do not
// control, and a malformed one is not worth failing a page render over.
func FromCookie(r *http.Request) string {
	if r == nil {
		return Cycle24
	}

	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return Cycle24
	}

	if strings.TrimSpace(cookie.Value) == Cycle12 {
		return Cycle12
	}

	return Cycle24
}

// Normalise reduces a posted or stored value to one this package understands,
// so an unrecognised string can never reach the database. Anything that is not
// an explicit dial becomes "system" — the unpinned state, which defers to the
// device rather than picking a dial on somebody's behalf.
func Normalise(value string) string {
	switch strings.TrimSpace(value) {
	case Cycle12:
		return Cycle12
	case Cycle24:
		return Cycle24
	}

	return System
}

// Clock renders one instant's time-of-day on the given dial, in the layout the
// caller asks for.
//
// The layout is a Go reference layout written for a 24-hour clock — "15:04
// MST", "Mon 2 Jan 15:04 MST". On a 12-hour dial the hour and the meridiem are
// substituted into it, so a caller writes one layout and both dials keep the
// same date order, the same separators and the same timezone abbreviation.
// Three call sites formatting inline with their own if statement is how the
// exports table and the reports badge end up disagreeing about midnight.
func Clock(cycle string, t time.Time, layout string) string {
	if cycle != Cycle12 {
		return t.Format(layout)
	}

	return t.Format(twelveHour(layout))
}

// twelveHour rewrites a 24-hour reference layout into its 12-hour equivalent.
//
// "15" is Go's 24-hour hour, "3" its 12-hour one, "04" the minute, "05" the
// second and "PM" the meridiem. The meridiem is inserted directly after the
// last clock token rather than appended to the layout, because appending puts
// it after the timezone and prints "2:30 UTC PM". A layout with no "15" in it
// has no 24-hour clock to convert and is returned untouched, so calling this
// on a date-only layout is harmless and calling it twice cannot double the
// meridiem.
func twelveHour(layout string) string {
	hour := strings.Index(layout, "15")
	if hour < 0 {
		return layout
	}

	// Walk past the minute and then the second, so the meridiem lands after
	// the whole time and not in the middle of it.
	end := hour + len("15")
	for _, token := range []string{":04", ":05"} {
		if !strings.HasPrefix(layout[end:], token) {
			break
		}

		end += len(token)
	}

	return layout[:hour] + "3" + layout[hour+len("15"):end] + " PM" + layout[end:]
}
