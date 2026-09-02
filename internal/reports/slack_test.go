//
// slack_test.go
// Where a customer-supplied chat webhook is and is not allowed to send us.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"context"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/outbound"
)

// TestAPrivateChatWebhookIsRefused is the SSRF case. The chat webhook is a URL
// typed into the reports form, and the failure it produces is rendered back on
// that same screen — so one pointed at the metadata endpoint or at a service on
// our own network would fetch it and show the answer.
func TestAPrivateChatWebhookIsRefused(t *testing.T) {
	for _, raw := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"https://10.0.0.5/services/T0/B0/x",
		"https://192.168.1.1/hooks/x",
		"https://[fd00::1]/hooks/x",
		"ftp://hooks.example.com/x",
		"hooks.example.com/x",
	} {
		if err := ValidateWebhookURL(raw); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}

	// An empty field is how a site says it wants email only, so it must not be
	// an error the form makes somebody clear.
	if err := ValidateWebhookURL("  "); err != nil {
		t.Errorf("an empty chat webhook was refused: %v", err)
	}

	if err := ValidateWebhookURL("https://hooks.example.com/services/T0/B0/x"); err != nil {
		t.Errorf("an ordinary chat webhook was refused: %v", err)
	}
}

// TestThePosterCannotDialAPrivateAddress covers the half the form check cannot:
// a hostname that resolves to a private address is only visible at connect
// time, which is where the guarded client resolves it again.
func TestThePosterCannotDialAPrivateAddress(t *testing.T) {
	err := NewSlack(outbound.Policy{AllowLoopback: true}).Post(context.Background(), "http://169.254.169.254/latest/meta-data/", "hello")
	if err == nil {
		t.Fatal("the poster reached the metadata endpoint")
	}

	if !strings.Contains(err.Error(), "private or local") {
		t.Fatalf("the poster failed for another reason: %v", err)
	}
}

// TestSavingARuleRefusesAPrivateChatWebhook proves the check is on the write
// rather than only on the screen, so the public API and the settings form
// cannot disagree about what may be stored.
func TestSavingARuleRefusesAPrivateChatWebhook(t *testing.T) {
	f := newStoreFixture(t)
	ctx := context.Background()

	err := f.store.SaveSubscription(ctx, Subscription{
		SiteID: f.siteA, Kind: KindWeekly, SlackWebhookURL: "http://169.254.169.254/", Enabled: true,
	})
	if err == nil {
		t.Fatal("a subscription pointing at the metadata endpoint was saved")
	}

	err = f.store.SaveAlertRule(ctx, AlertRule{
		SiteID: f.siteA, Kind: KindSpike, Threshold: 10, SlackWebhookURL: "https://10.0.0.5/", Enabled: true,
	})
	if err == nil {
		t.Fatal("an alert rule pointing at a private address was saved")
	}
}
