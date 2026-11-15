//
// commerce_test.go
// Mode-boundary coverage for hosted billing and self-hosted freedom.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// TestSelfHostedCommerceIsInert proves the mode flag wins over stale payment
// secrets and does not start metering, lock, lifecycle, volume or onboarding
// email workers.
func TestSelfHostedCommerceIsInert(t *testing.T) {
	dir := t.TempDir()
	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	applySystemMigrations(t, control)

	e := &env{
		cfg: &config.Config{App: config.App{
			Hosted: false, BaseURL: "http://localhost:19300", DataDir: dir,
			Stripe: config.Stripe{SecretKey: "stale", Product: "stale", PriceMonthly: "stale", PriceYearly: "stale", WebhookSecret: "stale"},
		}},
		log: logger.New(logger.Options{Level: "error"}),
	}
	manager := accounts.NewManager(dir)
	defer manager.CloseAll() //nolint:errcheck // the test has no useful cleanup recovery

	com := buildCommerce(e, control, manager, sites.New(control), nil)
	if com.Billing.Enabled() {
		t.Fatal("self-hosted mode left billing enabled")
	}
	if com.IngestRecorder() != nil {
		t.Fatal("self-hosted mode attached a billable usage recorder")
	}
	if _, locked := com.Gate.Check(1); locked {
		t.Fatal("self-hosted mode locked an account")
	}

	started := 0
	com.Start(context.Background(), func(func()) { started++ })
	if started != 0 {
		t.Fatalf("self-hosted mode started %d commercial workers", started)
	}
}

// TestHostedCommerceRunsTheOnboardingEmails is the other half: the hosted
// service starts the onboarding sweeper beside the four commercial loops.
func TestHostedCommerceRunsTheOnboardingEmails(t *testing.T) {
	dir := t.TempDir()
	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	applySystemMigrations(t, control)

	e := &env{
		cfg: &config.Config{App: config.App{Hosted: true, BaseURL: "http://localhost:19300", DataDir: dir}},
		log: logger.New(logger.Options{Level: "error"}),
	}
	manager := accounts.NewManager(dir)
	defer manager.CloseAll() //nolint:errcheck // the test has no useful cleanup recovery

	com := buildCommerce(e, control, manager, sites.New(control), nil)
	if com.Onboarding == nil || com.Onboarding.Store == nil || com.Onboarding.Outbox == nil ||
		com.Onboarding.Traffic == nil || com.Onboarding.Notify == nil {
		t.Fatalf("the onboarding sweeper is not fully assembled: %+v", com.Onboarding)
	}

	started := 0
	com.Start(context.Background(), func(func()) { started++ })
	if started != 5 {
		t.Fatalf("hosted mode started %d workers, want the four commercial loops and the onboarding emails", started)
	}
}
