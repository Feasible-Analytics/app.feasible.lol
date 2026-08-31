//
// usage_test.go
// The volume ladder's copy: a sales conversation, never a threat.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

// usageNotice builds one volume notice at a given level.
func usageNotice(level usage.Level, billable int64) usage.Notice {
	return usage.Notice{
		TeamID:     1,
		TeamName:   "Example Co",
		To:         "owner@example.com",
		Level:      level,
		Period:     "2026-03",
		Billable:   billable,
		Limit:      usage.MonthlyLimit,
		Projected:  usage.Projection(billable, time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)),
		Deadline:   time.Date(2026, time.April, 3, 12, 0, 0, 0, time.UTC),
		SalesEmail: "sales@feasible.lol",
		BillingURL: "https://feasible.lol/billing",
	}
}

// TestEveryRungRenders walks the four messages and checks each carries the
// numbers, the sales address and the postal address.
func TestEveryRungRenders(t *testing.T) {
	cases := map[usage.Level]int64{
		usage.LevelWarn:    700_000,
		usage.LevelNear:    850_000,
		usage.LevelReached: 1_000_000,
		"second_month":     1_400_000,
	}

	for level, billable := range cases {
		content, err := UsageContent(usageNotice(level, billable))
		if err != nil {
			t.Fatalf("%s: %v", level, err)
		}

		msg, err := content.Message("owner@example.com", "usage_"+string(level))
		if err != nil {
			t.Fatalf("%s: %v", level, err)
		}

		if strings.TrimSpace(msg.Subject) == "" {
			t.Errorf("%s has no subject", level)
		}

		if !strings.Contains(msg.HTML, "sales@feasible.lol") {
			t.Errorf("%s does not point at sales", level)
		}
		if !strings.Contains(msg.HTML, "Cloudmanic Labs, LLC") {
			t.Errorf("%s has no postal address", level)
		}

		if !strings.Contains(msg.Text, thousands(billable)) {
			t.Errorf("%s never states the number the customer is at", level)
		}

		if got := LongestLine(Wrap(msg.HTML, MaxLineLength)); got > MaxLineLength {
			t.Errorf("%s has a %d byte line", level, got)
		}
	}
}

// TestGoingOverIsNeverAThreat is the rule that separates a volume conversation
// from a dunning notice. Nothing in these four messages may mention deletion,
// data loss, or stopping collection — none of which happens for going over.
func TestGoingOverIsNeverAThreat(t *testing.T) {
	banned := []string{"delete", "deleted", "we stop collecting", "lose your data", "data will be lost", "suspend"}

	for _, level := range []usage.Level{usage.LevelWarn, usage.LevelNear, usage.LevelReached} {
		content, err := UsageContent(usageNotice(level, 900_000))
		if err != nil {
			t.Fatal(err)
		}

		body := strings.ToLower(strings.Join(append(content.Body, content.Subject, content.Heading), " "))

		for _, phrase := range banned {
			if strings.Contains(body, phrase) {
				t.Errorf("the %s email says %q — going over the plan never causes that", level, phrase)
			}
		}
	}
}

// TestTheSecondMonthEmailNamesItsDeadline is the one message in the set with a
// consequence attached, so it has to say exactly when and exactly what.
func TestTheSecondMonthEmailNamesItsDeadline(t *testing.T) {
	notice := usageNotice("second_month", 1_400_000)

	content, err := UsageContent(notice)
	if err != nil {
		t.Fatal(err)
	}

	body := strings.Join(append(content.Body, content.Subject), " ")

	if !strings.Contains(body, day(notice.Deadline)) {
		t.Errorf("the second-month email does not name %s", day(notice.Deadline))
	}

	// It must also say what stays working, because the lock is a prompt to
	// reply rather than a punishment.
	for _, promise := range []string{"keep collecting", "exports stay open", "unlocks immediately"} {
		if !strings.Contains(strings.ToLower(body), promise) {
			t.Errorf("the second-month email does not promise %q", promise)
		}
	}
}

// TestUnknownLevelIsAnError makes sure a typo fails loudly rather than sending a
// blank message.
func TestUnknownLevelIsAnError(t *testing.T) {
	if _, err := UsageContent(usageNotice("ninety_percent", 900_000)); err == nil {
		t.Fatal("an unknown level rendered")
	}
}

// TestThousandsFormatting checks the numbers these emails are entirely about.
func TestThousandsFormatting(t *testing.T) {
	cases := map[int64]string{
		0:         "0",
		999:       "999",
		1_000:     "1,000",
		700_000:   "700,000",
		1_000_000: "1,000,000",
		-4_500:    "-4,500",
	}

	for input, want := range cases {
		if got := thousands(input); got != want {
			t.Errorf("%d became %q, want %q", input, got, want)
		}
	}
}
