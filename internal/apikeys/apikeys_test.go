//
// apikeys_test.go
// Minting, presenting and revoking the one credential this API has.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package apikeys

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// now is the clock every test in this file runs against.
var now = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// testStore builds a migrated system database with a team and a user in it.
func testStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatal(err)
	}

	stamp := now.Unix()

	if _, err := db.Exec(`INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Test', ?, ?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`INSERT INTO users (id, email, created_at, updated_at) VALUES (1, 'a@example.test', ?, ?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (1, 1, 'owner', ?)`, stamp); err != nil {
		t.Fatal(err)
	}

	keys := NewStore(db)
	keys.Now = func() time.Time { return now }

	return keys, db
}

// TestKeyRoundTrips covers the whole life of a key: minted once, presented, and
// never recoverable afterwards.
func TestKeyRoundTrips(t *testing.T) {
	keys, db := testStore(t)
	ctx := context.Background()

	key, plaintext, err := keys.Create(ctx, 1, 1, "deploy", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(plaintext, Prefix) {
		t.Errorf("key %q does not carry the marker that makes it recognisable in a log", plaintext)
	}

	// The plaintext must not be anywhere in the database. A key found in a
	// stolen copy of system.db is a key somebody can replay against the
	// running service.
	var stored string
	if err := db.QueryRow(`SELECT key_hash FROM api_keys WHERE id = ?`, key.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}

	if stored == plaintext {
		t.Fatal("the key was stored in the clear")
	}

	if stored != Hash(plaintext) {
		t.Fatal("the stored hash is not the key's hash")
	}

	authenticated, err := keys.Authenticate(ctx, plaintext)
	if err != nil {
		t.Fatalf("a freshly minted key did not authenticate: %v", err)
	}

	if authenticated.TeamID != 1 || authenticated.ID != key.ID {
		t.Errorf("authenticated as %+v", authenticated)
	}
	if authenticated.Role != "owner" {
		t.Errorf("authenticated role = %q, want owner", authenticated.Role)
	}
}

// TestAuthenticateRefusesEverythingElse walks the ways a presented credential
// can be wrong.
func TestAuthenticateRefusesEverythingElse(t *testing.T) {
	keys, _ := testStore(t)
	ctx := context.Background()

	_, plaintext, err := keys.Create(ctx, 1, 1, "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		presented string
		want      error
	}{
		{"nothing at all", "", ErrMalformed},
		{"somebody else's format", "sk-live-abcdef", ErrMalformed},
		{"the marker and nothing else", Prefix, ErrMalformed},
		{"the right shape, wrong value", Prefix + strings.Repeat("a", 43), ErrNotFound},
		{"one character off", plaintext[:len(plaintext)-1] + "X", ErrNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := keys.Authenticate(ctx, tc.presented)

			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestRevokedKeyLooksLikeItNeverExisted checks that revocation is
// indistinguishable from a key that was never issued. Telling whoever holds a
// stolen key that it was once real is information they can use.
func TestRevokedKeyLooksLikeItNeverExisted(t *testing.T) {
	keys, _ := testStore(t)
	ctx := context.Background()

	key, plaintext, err := keys.Create(ctx, 1, 1, "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	if err := keys.Revoke(ctx, 1, key.ID); err != nil {
		t.Fatal(err)
	}

	_, revokedErr := keys.Authenticate(ctx, plaintext)
	_, unknownErr := keys.Authenticate(ctx, Prefix+strings.Repeat("b", 43))

	if !errors.Is(revokedErr, ErrNotFound) || !errors.Is(unknownErr, ErrNotFound) {
		t.Fatalf("revoked = %v, unknown = %v — both must be the same error", revokedErr, unknownErr)
	}

	// The row survives revocation, because "what was this key and when did it
	// stop working" is a question somebody asks after an incident.
	list, err := keys.List(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(list) != 1 || list[0].RevokedAt.IsZero() {
		t.Fatalf("list = %+v, want the revoked key with its timestamp", list)
	}
}

// TestKeyStopsAuthenticatingWhenItsOwnerLeavesTheTeam proves the canonical
// authenticator enforces live membership. Every public API and MCP transport
// calls this store, so this direct check covers the boundary they share.
func TestKeyStopsAuthenticatingWhenItsOwnerLeavesTheTeam(t *testing.T) {
	keys, db := testStore(t)
	ctx := context.Background()

	key, plaintext, err := keys.Create(ctx, 1, 1, "departing member", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := keys.Authenticate(ctx, plaintext); err != nil {
		t.Fatalf("key did not authenticate before membership removal: %v", err)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM team_memberships WHERE team_id = 1 AND user_id = 1`); err != nil {
		t.Fatal(err)
	}

	if _, err := keys.Authenticate(ctx, plaintext); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key authenticated after its owner left the team: %v", err)
	}
	if err := keys.Validate(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolved key remained valid after its owner left the team: %v", err)
	}
}

// TestValidateRefreshesRole proves a long-lived transport cannot retain the
// permissions its owner had before an administrator demoted them.
func TestValidateRefreshesRole(t *testing.T) {
	keys, db := testStore(t)
	ctx := context.Background()

	key, _, err := keys.Create(ctx, 1, 1, "role changes", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		UPDATE team_memberships SET role = 'billing' WHERE team_id = 1 AND user_id = 1
	`); err != nil {
		t.Fatal(err)
	}

	if err := keys.Validate(ctx, key); err != nil {
		t.Fatal(err)
	}
	if key.Role != "billing" {
		t.Fatalf("role = %q, want billing", key.Role)
	}
}

// TestRevokingSomebodyElsesKeyFails checks the team predicate on the write path,
// which is the one place a missing predicate would be a real breach rather than
// a leak.
func TestRevokingSomebodyElsesKeyFails(t *testing.T) {
	keys, db := testStore(t)
	ctx := context.Background()

	stamp := now.Unix()
	if _, err := db.Exec(`INSERT INTO teams (id, name, created_at, updated_at) VALUES (2, 'Other', ?, ?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	key, _, err := keys.Create(ctx, 1, 1, "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	if err := keys.Revoke(ctx, 2, key.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another team revoked our key: %v", err)
	}
}

// TestLastUsedIsThrottled checks that the bookkeeping write does not happen on
// every request. Without the throttle, every authenticated call would take the
// single system.db write lock — the one lock the whole deployment shares — and
// a busy integration would serialise itself behind its own bookkeeping.
func TestLastUsedIsThrottled(t *testing.T) {
	keys, db := testStore(t)
	ctx := context.Background()

	clock := now
	keys.Now = func() time.Time { return clock }

	_, plaintext, err := keys.Create(ctx, 1, 1, "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := keys.Authenticate(ctx, plaintext); err != nil {
		t.Fatal(err)
	}

	first := lastUsed(t, db)
	if first == 0 {
		t.Fatal("the first use was not recorded")
	}

	clock = clock.Add(10 * time.Second)

	if _, err := keys.Authenticate(ctx, plaintext); err != nil {
		t.Fatal(err)
	}

	if lastUsed(t, db) != first {
		t.Fatal("a second use ten seconds later wrote again")
	}

	clock = clock.Add(2 * time.Minute)

	if _, err := keys.Authenticate(ctx, plaintext); err != nil {
		t.Fatal(err)
	}

	if lastUsed(t, db) == first {
		t.Fatal("a use two minutes later did not refresh the timestamp")
	}
}

// lastUsed reads the recorded timestamp.
func lastUsed(t *testing.T, db *sql.DB) int64 {
	t.Helper()

	var at sql.NullInt64
	if err := db.QueryRow(`SELECT last_used_at FROM api_keys WHERE id = 1`).Scan(&at); err != nil {
		t.Fatal(err)
	}

	return at.Int64
}

// TestScopesDefaultToEverything is the "one key type, self-serve" rule as a
// test. A key created with no scopes has to work for stats and for
// provisioning, because making integrators mint a second key is exactly the
// friction this package exists to remove.
func TestScopesDefaultToEverything(t *testing.T) {
	unscoped := &Key{}

	for _, scope := range []string{ScopeStatsRead, ScopeSitesRead, ScopeSitesProvision, ScopeWebhooks} {
		if !unscoped.Allows(scope) {
			t.Errorf("an unscoped key was refused %s", scope)
		}
	}

	narrow := &Key{Scopes: []string{ScopeStatsRead}}

	if !narrow.Allows(ScopeStatsRead) {
		t.Error("a stats:read key was refused stats:read")
	}

	if narrow.Allows(ScopeSitesProvision) {
		t.Error("a stats:read key was allowed to provision sites")
	}
}

// TestKeysAreDistinct guards against a generator that is not actually random.
func TestKeysAreDistinct(t *testing.T) {
	seen := map[string]bool{}

	for i := 0; i < 200; i++ {
		key, err := Generate()
		if err != nil {
			t.Fatal(err)
		}

		if seen[key] {
			t.Fatalf("Generate repeated itself after %d calls", i)
		}

		seen[key] = true
	}
}

// TestByIDAnswersLikeAuthenticate checks that a key read by id carries the same
// fields, and is refused on the same terms, as the key presented in a header.
// An OAuth token stands for a key by id, so a difference here would be a token
// that outlives the key it was issued for.
func TestByIDAnswersLikeAuthenticate(t *testing.T) {
	keys, _ := testStore(t)
	ctx := context.Background()

	created, plaintext, err := keys.Create(ctx, 1, 1, "by-id", []string{ScopeStatsRead}, 250)
	if err != nil {
		t.Fatal(err)
	}

	presented, err := keys.Authenticate(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}

	byID, err := keys.ByID(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	if byID.ID != presented.ID || byID.TeamID != presented.TeamID || byID.UserID != presented.UserID ||
		byID.Role != presented.Role || byID.Name != presented.Name || byID.Prefix != presented.Prefix ||
		byID.HourlyLimit != presented.HourlyLimit || strings.Join(byID.Scopes, ",") != strings.Join(presented.Scopes, ",") ||
		!byID.CreatedAt.Equal(presented.CreatedAt) {
		t.Fatalf("ByID = %+v, Authenticate = %+v", byID, presented)
	}

	if err := keys.Revoke(ctx, 1, created.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := keys.ByID(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked key read by id returned %v, want ErrNotFound", err)
	}

	if _, err := keys.ByID(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing key read by id returned %v, want ErrNotFound", err)
	}
}

// TestListRefusesAnUnreadableScopeList checks that a key the API would reject
// is not listed as if it had every scope. The row can only get that way by
// hand, and the answer has to name the key rather than hide it.
func TestListRefusesAnUnreadableScopeList(t *testing.T) {
	keys, db := testStore(t)
	ctx := context.Background()

	created, _, err := keys.Create(ctx, 1, 1, "broken", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`UPDATE api_keys SET scopes = 'not json' WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}

	_, err = keys.List(ctx, 1)
	if err == nil || !strings.Contains(err.Error(), "unreadable scope list") {
		t.Fatalf("List returned %v, want an error naming the unreadable scope list", err)
	}
}
