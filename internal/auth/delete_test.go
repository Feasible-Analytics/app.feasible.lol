//
// delete_test.go
// Deleting an account: the file, the rows, and the payment provider's customer.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// TestDeleteAccountRemovesTheDatabaseFile checks the half of deletion that is
// not a SQL statement. A privacy product whose deletion leaves an orphaned
// database file on disk has no honest answer to "what do you still hold".
func TestDeleteAccountRemovesTheDatabaseFile(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	manager := accounts.NewManager(dataDir)

	t.Cleanup(func() { manager.CloseAll() })

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

	deleter := NewDeleter(s, manager, dataDir, NewStripe("", nil), logger.New(logger.Options{Output: os.Stderr}))

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

// TestStripeDeleteTolerates404 checks that an already-deleted customer does not
// block the account deletion the user actually asked for. The goal is "this
// customer does not exist", and it already does not.
func TestStripeDeleteTolerates404(t *testing.T) {
	var seen string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	stripe := NewStripe("sk_test", nil)
	stripe.HTTPClient = &http.Client{Transport: rewriteTo(server.URL)}

	if err := stripe.DeleteCustomer(context.Background(), "cus_123"); err != nil {
		t.Fatalf("a missing customer should not fail the deletion: %v", err)
	}

	if seen != "DELETE /v1/customers/cus_123" {
		t.Errorf("unexpected request: %q", seen)
	}
}

// TestStripeDeleteReportsARealFailure checks the one case worth stopping on: a
// customer record we cannot delete is one that can still be charged.
func TestStripeDeleteReportsARealFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Invalid API Key"}}`))
	}))
	defer server.Close()

	stripe := NewStripe("sk_bad", nil)
	stripe.HTTPClient = &http.Client{Transport: rewriteTo(server.URL)}

	if err := stripe.DeleteCustomer(context.Background(), "cus_123"); err == nil {
		t.Error("a rejected delete should be reported rather than swallowed")
	}
}

// TestStripeIsOptional checks that a self-hosted install with no payment
// provider deletes accounts without complaint.
func TestStripeIsOptional(t *testing.T) {
	stripe := NewStripe("", nil)

	if stripe.Configured() {
		t.Error("an empty key should mean not configured")
	}

	if err := stripe.DeleteCustomer(context.Background(), "cus_123"); err != nil {
		t.Errorf("an unconfigured provider should be skipped: %v", err)
	}
}
