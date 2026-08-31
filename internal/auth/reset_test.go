//
// reset_test.go
// Single-use, time-limited password reset tokens.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"testing"
	"time"
)

// TestResetIsSingleUse checks the difference between a reset token and a
// password: a reset link works once and then never again.
func TestResetIsSingleUse(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	token, err := s.CreateReset(ctx, user.ID)
	if err != nil {
		t.Fatalf("create reset: %v", err)
	}

	found, err := s.ConsumeReset(ctx, token)
	if err != nil {
		t.Fatalf("consume reset: %v", err)
	}

	if found != user.ID {
		t.Errorf("want user %d, got %d", user.ID, found)
	}

	if _, err := s.ConsumeReset(ctx, token); err != ErrTokenUsed {
		t.Errorf("a spent reset token must not work twice, got %v", err)
	}
}

// TestResetExpires checks the hour limit is real rather than decorative.
func TestResetExpires(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Now()
	s.SetClock(func() time.Time { return now })

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	token, err := s.CreateReset(ctx, user.ID)
	if err != nil {
		t.Fatalf("create reset: %v", err)
	}

	now = now.Add(ResetWindow + time.Minute)

	if _, err := s.ConsumeReset(ctx, token); err != ErrTokenExpired {
		t.Errorf("want ErrTokenExpired, got %v", err)
	}
}

// TestAskingForANewResetCancelsTheOld checks that an old email somebody else
// already read stops working the moment a new link is requested.
func TestAskingForANewResetCancelsTheOld(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	first, err := s.CreateReset(ctx, user.ID)
	if err != nil {
		t.Fatalf("create reset: %v", err)
	}

	second, err := s.CreateReset(ctx, user.ID)
	if err != nil {
		t.Fatalf("create reset: %v", err)
	}

	if _, err := s.ResetUserID(ctx, first); err != ErrNotFound {
		t.Errorf("the first link should be gone, got %v", err)
	}

	if _, err := s.ResetUserID(ctx, second); err != nil {
		t.Errorf("the newest link should work: %v", err)
	}
}

// TestResetUserIDDoesNotConsume checks that rendering the form does not spend
// the token, which is what makes the back button survivable.
func TestResetUserIDDoesNotConsume(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	token, err := s.CreateReset(ctx, user.ID)
	if err != nil {
		t.Fatalf("create reset: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := s.ResetUserID(ctx, token); err != nil {
			t.Fatalf("reading the token should not spend it: %v", err)
		}
	}

	if _, err := s.ConsumeReset(ctx, token); err != nil {
		t.Errorf("the token should still be spendable: %v", err)
	}
}

// TestPruneResetsClearsSpentTokens checks the sweep. Nothing depends on it, but
// a table of dead reset tokens is a table that only becomes interesting to
// somebody who has stolen the file.
func TestPruneResetsClearsSpentTokens(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	token, err := s.CreateReset(ctx, user.ID)
	if err != nil {
		t.Fatalf("create reset: %v", err)
	}

	if _, err := s.ConsumeReset(ctx, token); err != nil {
		t.Fatalf("consume reset: %v", err)
	}

	if _, err := s.PruneResets(ctx); err != nil {
		t.Fatalf("prune resets: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM password_reset_tokens").Scan(&count); err != nil {
		t.Fatalf("count tokens: %v", err)
	}

	if count != 0 {
		t.Errorf("want no tokens left, got %d", count)
	}
}
