//
// tags.go
// Every message the product can send, named once.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

// The tags for the messages built in this package. A tag is what delivery
// reporting groups on, so it is stable: add one, never rename one.
const (
	TagVerifyEmail          = "verify_email"
	TagPasswordReset        = "password_reset"
	TagPasswordChanged      = "password_changed"
	TagNewLogin             = "new_login"
	TagSettingsConfirmation = "settings_confirmation"
	TagTeamInvitation       = "team_invitation"
)

// The tags for the messages built in internal/reports. They are named here
// rather than there because the guard below has to know the whole set, and
// internal/mail must not import internal/reports.
const (
	TagReportWeekly  = "report_weekly"
	TagReportMonthly = "report_monthly"
	TagReportPreview = "report_preview"
	TagAlertSpike    = "alert_spike"
	TagAlertDrop     = "alert_drop"
)

// Tags is every message the product can send.
//
// The eleven one-off messages are written out, because the list's job is to be
// the thing a new sender has to be added to. The lifecycle sequence and the
// volume ladder are read from the same source their senders read, so a rung
// added there cannot be missing here.
//
// internal/mailsample builds one of each and its test refuses a message with no
// postal address on it.
func Tags() []string {
	tags := []string{
		TagVerifyEmail,
		TagPasswordReset,
		TagPasswordChanged,
		TagNewLogin,
		TagSettingsConfirmation,
		TagTeamInvitation,
		TagReportWeekly,
		TagReportMonthly,
		TagReportPreview,
		TagAlertSpike,
		TagAlertDrop,
	}

	// The lifecycle sequence and the volume ladder build their tags from the
	// state that triggered them, so they are read from the same source the
	// senders read rather than copied.
	for _, scheduled := range lifecycle.Sequence {
		tags = append(tags, scheduled.Template)
	}

	for _, level := range []usage.Level{usage.LevelWarn, usage.LevelNear, usage.LevelReached} {
		tags = append(tags, usageTag(level))
	}

	return tags
}

// usageTag is the tag for one rung of the volume ladder.
func usageTag(level usage.Level) string {
	return "usage_" + string(level)
}
