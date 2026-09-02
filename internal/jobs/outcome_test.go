//
// outcome_test.go
// A handler that did nothing and will not say why is a failure, not a success.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package jobs

import (
	"context"
	"errors"
	"testing"
)

// TestASilentSuccessIsRecordedAsAFailure is the whole point of the Outcome
// type. It is the shape of a notifier that returned an empty list and sent
// nothing for months while every job row said completed.
func TestASilentSuccessIsRecordedAsAFailure(t *testing.T) {
	ctx := context.Background()
	client := NewClient(newSystem(t))

	id, err := client.EnqueueOwned(ctx, 0, "notifications", "reports.schedule", struct{}{}, "")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(client)
	runner.Register("notifications", "reports.schedule",
		Reporting(nil, func(context.Context, Job) (Outcome, error) {
			return Outcome{}, nil
		}))

	if _, err := runner.Once(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	job, err := client.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if job.State == StateCompleted {
		t.Fatal("a run that did nothing and said nothing was recorded as completed")
	}

	if job.LastError == "" {
		t.Fatal("the row carries no reason for the failure")
	}
}

// TestNothingWithAReasonSucceeds is the other half: a handler that correctly
// had no work to do is not a failure, as long as it says so. Without this the
// check would be unusable and somebody would return a fake count to get past it.
func TestNothingWithAReasonSucceeds(t *testing.T) {
	ctx := context.Background()
	client := NewClient(newSystem(t))

	id, err := client.EnqueueOwned(ctx, 0, "notifications", "reports.schedule", struct{}{}, "")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(client)
	runner.Register("notifications", "reports.schedule",
		Reporting(nil, func(context.Context, Job) (Outcome, error) {
			return Nothing("no sites have a report configured"), nil
		}))

	if _, err := runner.Once(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	job, err := client.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if job.State != StateCompleted {
		t.Fatalf("a run that explained itself was recorded as %s", job.State)
	}
}

// TestValidateNamesTheRule covers the check on its own, so a handler's own
// tests can assert against it without a database.
func TestValidateNamesTheRule(t *testing.T) {
	if err := (Outcome{}).Validate(); !errors.Is(err, ErrSilentSuccess) {
		t.Fatalf("an empty outcome validated as %v", err)
	}

	if err := (Outcome{Handled: 1}).Validate(); err != nil {
		t.Fatalf("work done was rejected: %v", err)
	}

	if err := Nothing("nothing was due").Validate(); err != nil {
		t.Fatalf("a stated reason was rejected: %v", err)
	}

	// Whitespace is not a reason. Without this, a handler passes the check by
	// returning a space and the guarantee is worth nothing.
	if err := Nothing("   ").Validate(); !errors.Is(err, ErrSilentSuccess) {
		t.Fatalf("a blank reason validated as %v", err)
	}
}

// TestAnErrorFromAReportingHandlerStillFails checks the adapter does not eat a
// real failure on its way to the row.
func TestAnErrorFromAReportingHandlerStillFails(t *testing.T) {
	ctx := context.Background()
	client := NewClient(newSystem(t))

	id, err := client.EnqueueOwned(ctx, 0, "notifications", "reports.alerts", struct{}{}, "")
	if err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(client)
	runner.Register("notifications", "reports.alerts",
		Reporting(nil, func(context.Context, Job) (Outcome, error) {
			return Outcome{}, errors.New("the relay refused the message")
		}))

	if _, err := runner.Once(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	job, err := client.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}

	if job.LastError != "the relay refused the message" {
		t.Fatalf("the row says %q", job.LastError)
	}
}
