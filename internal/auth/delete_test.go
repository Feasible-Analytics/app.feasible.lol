//
// delete_test.go
// Deleting an account through the durable lifecycle purger.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// TestDeleteAccountRemovesTheDatabaseFile checks the half of deletion that is
// not a SQL statement. A privacy product whose deletion leaves an orphaned
// database file on disk has no honest answer to "what do you still hold".
func TestDeleteAccountRemovesTheDatabaseFile(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	manager := accounts.NewManager(dataDir)

	t.Cleanup(func() { checkClose(t, "account manager", manager.CloseAll) })

	user, team, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Opening the account creates the file and brings it up to schema, which is
	// what an incoming event would have done.
	if _, err := manager.Open(ctx, team.ID); err != nil {
		t.Fatalf("open account: %v", err)
	}

	path := accounts.Path(dataDir, team.ID)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the account database should exist: %v", err)
	}

	now := time.Unix(1_788_192_000, 0).UTC()
	lifecycleStore := lifecycle.NewStore(db)
	if err := lifecycleStore.Save(ctx, team.ID, lifecycle.State{Trigger: lifecycle.TriggerTrial, StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	purger := &lifecycle.Purger{Store: lifecycleStore, Accounts: manager, DataDir: dataDir}
	deleter := NewDeleter(purger, logger.New(logger.Options{Output: os.Stderr}))
	deleter.Now = func() time.Time { return now }

	if err := deleter.DeleteAccount(ctx, user.ID, team.ID); err != nil {
		t.Fatalf("delete account: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the account database file should be gone, got %v", err)
	}

	if _, err := os.Stat(accounts.Dir(dataDir, team.ID)); !os.IsNotExist(err) {
		t.Error("the account directory should be gone too, along with the WAL")
	}
	if _, err := os.Stat(accounts.DeletedMarker(dataDir, team.ID)); err != nil {
		t.Fatalf("the permanent deletion marker is missing: %v", err)
	}
	if _, err := manager.Open(ctx, team.ID); err == nil {
		t.Fatal("the settings deletion path recreated the deleted account database")
	}
	if _, err := s.UserByID(ctx, user.ID); err != ErrNotFound {
		t.Errorf("the user should be gone, got %v", err)
	}
	_, replacement, err := s.CreateUser(ctx, "replacement@example.com", "", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID <= team.ID {
		t.Fatalf("settings deletion reused team id %d for replacement %d", team.ID, replacement.ID)
	}
}
