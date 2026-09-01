//
// apikeys_test.go
// Keys belong to a team, and stop working when their owner leaves it.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package teams

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
)

// TestOnlyRolesWithThePermissionCanCreateAKey drives the matrix through the
// store, so that the "false" in the table and the refusal in the database are
// proven to be the same thing.
func TestOnlyRolesWithThePermissionCanCreateAKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	allowed := map[int64]bool{f.owner: true, f.admin: true, f.editor: true}

	for _, actor := range []int64{f.owner, f.admin, f.editor, f.viewer} {
		_, _, err := f.store.CreateAPIKey(ctx, actor, f.teamA, "test", nil)

		if allowed[actor] && err != nil {
			t.Errorf("user %d could not create a key: %v", actor, err)
		}

		if !allowed[actor] && !errors.Is(err, ErrForbidden) {
			t.Errorf("user %d created a key and should not have been able to: %v", actor, err)
		}
	}
}

// TestAGuestCannotCreateAKey checks the other half of the rule. A guest is not
// a member of the team at all, so the refusal arrives as ErrNotFound rather
// than ErrForbidden — which is the right answer: probing for a team id should
// tell somebody outside it nothing.
func TestAGuestCannotCreateAKey(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.db.Exec(`INSERT INTO guest_memberships (site_id, user_id, role, created_at) VALUES (?, ?, 'guest_editor', ?)`,
		f.siteA1, f.outsider, f.now.Unix()); err != nil {
		t.Fatalf("insert guest: %v", err)
	}

	if _, _, err := f.store.CreateAPIKey(ctx, f.outsider, f.teamA, "guest key", nil); err == nil {
		t.Fatal("a guest editor created an API key for the team")
	}
}

// TestAKeyIsScopedToTheTeamItWasCreatedAgainst is the acceptance criterion.
//
// The owner belongs to both teams here. A key made against team A must read
// team A's sites and nothing of team B's, which is precisely what the incumbent
// gets wrong: their keys inherit every site their owner can see, so a key made
// for one client silently reads every other client on the same login.
func TestAKeyIsScopedToTheTeamItWasCreatedAgainst(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	secret, _, err := f.store.CreateAPIKey(ctx, f.owner, f.teamA, "team A only", nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	if !strings.HasPrefix(secret, KeyPrefix) {
		t.Fatalf("the key %q does not carry the %q prefix a scanner looks for", secret, KeyPrefix)
	}

	auth, err := f.store.AuthenticateAPIKey(ctx, secret)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	if auth.TeamID != f.teamA {
		t.Fatalf("the key resolved to team %d, want %d", auth.TeamID, f.teamA)
	}

	for _, site := range []int64{f.siteA1, f.siteA2} {
		reads, err := f.store.KeyReadsSite(ctx, auth, site)
		if err != nil || !reads {
			t.Errorf("the key cannot read its own team's site %d (%v)", site, err)
		}
	}

	reads, err := f.store.KeyReadsSite(ctx, auth, f.siteB1)
	if err != nil {
		t.Fatalf("check other team's site: %v", err)
	}

	if reads {
		t.Fatal("a key made against team A read a site belonging to team B")
	}
}

// TestAKeyStopsWorkingWhenItsOwnerLeavesTheTeam is the other half of scoping,
// and the reason authentication joins onto the live membership rather than
// trusting the row that created the key.
func TestAKeyStopsWorkingWhenItsOwnerLeavesTheTeam(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	secret, _, err := f.store.CreateAPIKey(ctx, f.editor, f.teamA, "editor's key", nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	if _, err := f.store.AuthenticateAPIKey(ctx, secret); err != nil {
		t.Fatalf("the key does not work before the editor leaves: %v", err)
	}

	if err := f.store.RemoveMember(ctx, f.owner, f.teamA, f.editor); err != nil {
		t.Fatalf("remove member: %v", err)
	}

	if _, err := f.store.AuthenticateAPIKey(ctx, secret); !errors.Is(err, ErrKeyNotAuthorised) {
		t.Fatalf("the key still works after its owner left: %v", err)
	}
}

// TestAKeyLosesReadAccessWhenItsOwnerIsDemoted checks that a key is exactly as
// powerful as its owner is right now, rather than as powerful as they were when
// they made it.
func TestAKeyLosesReadAccessWhenItsOwnerIsDemoted(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	secret, _, err := f.store.CreateAPIKey(ctx, f.editor, f.teamA, "editor's key", nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	if err := f.store.SetRole(ctx, f.owner, f.teamA, f.editor, RoleViewer); err != nil {
		t.Fatalf("demote: %v", err)
	}

	auth, err := f.store.AuthenticateAPIKey(ctx, secret)
	if err != nil {
		t.Fatalf("authenticate after demotion: %v", err)
	}

	if auth.Role != RoleViewer {
		t.Fatalf("the key resolved to role %q, want viewer — the role is stale", auth.Role)
	}

	// A viewer can still read, so the key still reads. The point of the check
	// is that the role travelled with the demotion rather than being frozen.
	if reads, _ := f.store.KeyReadsSite(ctx, auth, f.siteA1); !reads {
		t.Fatal("a viewer's key cannot read the dashboard")
	}
}

// TestARevokedKeyStopsWorking checks the manual remedy.
func TestARevokedKeyStopsWorking(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	secret, key, err := f.store.CreateAPIKey(ctx, f.admin, f.teamA, "temporary", nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	if err := f.store.RevokeAPIKey(ctx, f.admin, f.teamA, key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := f.store.AuthenticateAPIKey(ctx, secret); !errors.Is(err, ErrKeyNotAuthorised) {
		t.Fatalf("a revoked key still authenticates: %v", err)
	}

	// A revoked key stays in the list, because "did we turn that one off?" is a
	// question with a real answer and a list that hides them cannot answer it.
	keys, err := f.store.APIKeys(ctx, f.admin, f.teamA)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}

	if len(keys) != 1 || keys[0].Active() {
		t.Fatalf("the revoked key is not listed as revoked: %+v", keys)
	}
}

// TestSomebodyElsesKeyNeedsMemberManagement checks who may revoke what.
func TestSomebodyElsesKeyNeedsMemberManagement(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	_, key, err := f.store.CreateAPIKey(ctx, f.editor, f.teamA, "editor's own", nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// A viewer manages nothing and owns nothing here.
	if err := f.store.RevokeAPIKey(ctx, f.viewer, f.teamA, key.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a viewer revoked somebody else's key: %v", err)
	}

	// An admin manages members, so it manages their keys too.
	if err := f.store.RevokeAPIKey(ctx, f.admin, f.teamA, key.ID); err != nil {
		t.Fatalf("an admin could not revoke a member's key: %v", err)
	}
}

// TestKeyListingFollowsTheSamePermissionAsKeyCreation checks that editors and
// billing users see their own credentials while member managers see the team.
func TestKeyListingFollowsTheSamePermissionAsKeyCreation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, _, err := f.store.CreateAPIKey(ctx, f.owner, f.teamA, "owner", nil); err != nil {
		t.Fatalf("create owner key: %v", err)
	}
	if _, _, err := f.store.CreateAPIKey(ctx, f.editor, f.teamA, "editor", nil); err != nil {
		t.Fatalf("create editor key: %v", err)
	}

	keys, err := f.store.APIKeys(ctx, f.editor, f.teamA)
	if err != nil {
		t.Fatalf("editor list: %v", err)
	}
	if len(keys) != 1 || keys[0].UserID != f.editor {
		t.Fatalf("editor saw another member's keys: %+v", keys)
	}

	keys, err = f.store.APIKeys(ctx, f.admin, f.teamA)
	if err != nil || len(keys) != 2 {
		t.Fatalf("admin should see all team keys: %+v, %v", keys, err)
	}

	if _, err := f.store.APIKeys(ctx, f.viewer, f.teamA); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer listed API keys: %v", err)
	}
}

// TestAnUnknownKeyIsRefusedWithoutSayingWhy checks that the four different
// reasons a key can fail all answer the same way, so the endpoint cannot be
// used to probe.
func TestAnUnknownKeyIsRefusedWithoutSayingWhy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for _, secret := range []string{"", "   ", "fsbl_nope", "not-even-close"} {
		if _, err := f.store.AuthenticateAPIKey(ctx, secret); !errors.Is(err, ErrKeyNotAuthorised) {
			t.Errorf("AuthenticateAPIKey(%q) = %v, want ErrKeyNotAuthorised", secret, err)
		}
	}
}

// TestAKeyFromTheTeamScreenAuthenticatesAgainstThePublicAPI is the integration
// this package's keys exist inside.
//
// There is one api_keys table and one authenticator. A key minted here with a
// different marker, a different hash or no readable head would be a credential
// this product hands somebody and then refuses — which is worse than not
// offering the button, because the failure lands on the integrator.
func TestAKeyFromTheTeamScreenAuthenticatesAgainstThePublicAPI(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	secret, _, err := f.store.CreateAPIKey(ctx, f.owner, f.teamA, "for the API", nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	key, err := apikeys.NewStore(f.db).Authenticate(ctx, secret)
	if err != nil {
		t.Fatalf("the public API refused a key issued from the team screen: %v", err)
	}

	if key.TeamID != f.teamA {
		t.Fatalf("the key authenticated as team %d, want %d", key.TeamID, f.teamA)
	}

	// The readable head is what a key list shows. An empty one is two keys that
	// cannot be told apart in the list somebody revokes from.
	if key.Prefix == "" || !strings.HasPrefix(secret, key.Prefix) {
		t.Fatalf("the stored prefix %q is not the head of the key", key.Prefix)
	}
}
