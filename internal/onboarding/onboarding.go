//
// onboarding.go
// Which onboarding email a new account is owed, as a pure function of its age.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package onboarding sends a new hosted account its welcome, and then help
// with the install for as long as no site on the account has counted anything.
//
// This file is pure. The clock is a parameter and nothing touches a database;
// the reads, the sweep and the stop link live in store.go, service.go and
// unsubscribe.go.
package onboarding

import "time"

// The templates. The name is what the sent marker stores, so renaming one
// would send it again to every account inside the window. Add, never rename.
const (
	TemplateWelcome    = "onboarding_welcome"
	TemplateNoDataDay1 = "onboarding_no_data_day_1"
	TemplateNoDataDay3 = "onboarding_no_data_day_3"
	TemplateNoDataDay7 = "onboarding_no_data_day_7"
)

// Day is one day on the onboarding clock, measured from sign-up.
const Day = 24 * time.Hour

// WindowDays is how long after sign-up an account can still be sent anything.
// Past it, a process that was down for a month does not write to somebody
// about an install they gave up on weeks ago.
const WindowDays = 14

// WelcomeGap is the least time between the welcome and the first install-help
// email, so an owner who verifies their address a day late is not sent both in
// the same minute.
const WelcomeGap = 12 * time.Hour

// Step is one install-help email and the day it becomes due.
type Step struct {
	Day      int
	Template string
}

// NoData is the install-help sequence, in order. Each is sent only while no
// site on the account has counted an event.
var NoData = []Step{
	{Day: 1, Template: TemplateNoDataDay1},
	{Day: 3, Template: TemplateNoDataDay3},
	{Day: 7, Template: TemplateNoDataDay7},
}

// Templates lists every onboarding template, welcome first.
func Templates() []string {
	templates := []string{TemplateWelcome}
	for _, step := range NoData {
		templates = append(templates, step.Template)
	}

	return templates
}

// Progress is everything Next needs to know about one account.
type Progress struct {
	// CreatedAt is the sign-up instant, day zero of the clock.
	CreatedAt time.Time

	// Verified is whether the owner has proved their address. Nothing is sent
	// before that, which is also what keeps the welcome from arriving in the
	// same minute as the verification code.
	Verified bool

	// Sent maps each template already settled for this account onto when it
	// was settled. A step skipped because data arrived is in here too.
	Sent map[string]time.Time
}

// Next names the one email an account is owed at this instant, or "" for none.
// noData reports that the email may go out only if the account still has no
// events, which the caller has to check because this function reads nothing.
//
// Only the latest due step is ever returned. An account whose day-1 email was
// missed is sent the day-3 one on day 3, not both.
func Next(p Progress, now time.Time) (template string, noData bool) {
	age := now.Sub(p.CreatedAt)
	if !p.Verified || age < 0 || age >= WindowDays*Day {
		return "", false
	}

	welcomed, ok := p.Sent[TemplateWelcome]
	if !ok {
		return TemplateWelcome, false
	}

	var due *Step
	for i := range NoData {
		if time.Duration(NoData[i].Day)*Day <= age {
			due = &NoData[i]
		}
	}
	if due == nil {
		return "", false
	}

	if _, settled := p.Sent[due.Template]; settled {
		return "", false
	}

	if now.Sub(welcomed) < WelcomeGap {
		return "", false
	}

	return due.Template, true
}
