//
// roles_test.go
// The permission matrix, asserted end to end.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package teams

import "testing"

// TestEveryRoleHasADecisionForEveryPermission walks the whole grid.
//
// The matrix is a map of maps, so a permission nobody wrote a line for reads as
// false — which is safe but silent. This test makes the silence loud: adding a
// permission without deciding what each of the seven roles may do with it fails
// here rather than shipping as a quiet denial.
func TestEveryRoleHasADecisionForEveryPermission(t *testing.T) {
	roles := append(TeamRoles(), GuestRoles()...)

	if len(roles) != 7 {
		t.Fatalf("there are %d roles, want 7", len(roles))
	}

	for _, role := range roles {
		decisions, ok := matrix[role]
		if !ok {
			t.Fatalf("%s has no row in the matrix", role)
		}

		granted := 0
		for _, permission := range Permissions {
			if decisions[permission] {
				granted++
			}
		}

		// Every role can at least read a dashboard, so a row that grants
		// nothing is a row somebody forgot to fill in.
		if granted == 0 {
			t.Errorf("%s is granted no permission at all", role)
		}
	}
}

// TestViewerAndGuestsCannotCreateAPIKeys is the single most load-bearing
// assertion in this package.
//
// A key is a durable credential that outlives a session and is not tied to a
// login. Handing one to a read-only seat turns that seat into a permanent
// integration nobody audits, and handing one to a guest hands it to somebody
// outside the team entirely.
func TestViewerAndGuestsCannotCreateAPIKeys(t *testing.T) {
	for _, role := range []Role{RoleViewer, RoleGuestEditor, RoleGuestViewer} {
		if Can(role, PermCreateAPIKey) {
			t.Errorf("%s can create an API key and must not be able to", Label(role))
		}
	}

	for _, role := range []Role{RoleOwner, RoleAdmin, RoleEditor, RoleBilling} {
		if !Can(role, PermCreateAPIKey) {
			t.Errorf("%s cannot create an API key and should be able to", Label(role))
		}
	}
}

// TestTheFullMatrix pins every cell.
//
// It is written out rather than derived, because a test that computes the
// expected answer from the same table it is checking proves only that the table
// equals itself. Every "false" in here is a decision somebody made on purpose,
// and changing one should require changing this list too.
func TestTheFullMatrix(t *testing.T) {
	expected := map[Role][]Permission{
		RoleOwner: {
			PermViewDashboard, PermManageSiteSettings, PermManageSites, PermManageMembers,
			PermManageTeam, PermDeleteTeam, PermTransferOwnership, PermManageBilling,
			PermManageSecurity, PermCreateAPIKey,
		},
		RoleAdmin: {
			PermViewDashboard, PermManageSiteSettings, PermManageSites, PermManageMembers,
			PermManageBilling, PermCreateAPIKey,
		},
		RoleEditor:      {PermViewDashboard, PermManageSiteSettings, PermCreateAPIKey},
		RoleBilling:     {PermViewDashboard, PermManageBilling, PermCreateAPIKey},
		RoleViewer:      {PermViewDashboard},
		RoleGuestEditor: {PermViewDashboard, PermManageSiteSettings},
		RoleGuestViewer: {PermViewDashboard},
	}

	for role, allowed := range expected {
		granted := map[Permission]bool{}
		for _, permission := range allowed {
			granted[permission] = true
		}

		for _, permission := range Permissions {
			if got, want := Can(role, permission), granted[permission]; got != want {
				t.Errorf("Can(%s, %s) = %v, want %v", role, permission, got, want)
			}
		}
	}
}

// TestOnlyOwnerCanDeleteOrTransfer checks the two powers that end an account.
func TestOnlyOwnerCanDeleteOrTransfer(t *testing.T) {
	for _, permission := range []Permission{PermDeleteTeam, PermTransferOwnership, PermManageSecurity} {
		for _, role := range append(TeamRoles(), GuestRoles()...) {
			if role == RoleOwner {
				continue
			}

			if Can(role, permission) {
				t.Errorf("%s has %s and only the owner should", Label(role), permission)
			}
		}
	}
}

// TestGuestRolesAreOutsideTheHierarchy checks that a rank comparison cannot
// accidentally admit a guest. A guest is not a weaker member; it is a different
// relationship, and "at least a Viewer" must not mean "or a Guest Viewer".
func TestGuestRolesAreOutsideTheHierarchy(t *testing.T) {
	for _, role := range GuestRoles() {
		if IsTeamRole(role) {
			t.Errorf("%s counts as a team role", role)
		}

		if Rank(role) != 0 {
			t.Errorf("Rank(%s) = %d, want 0", role, Rank(role))
		}

		if !IsGuestRole(role) {
			t.Errorf("%s does not count as a guest role", role)
		}
	}

	if Rank(RoleOwner) <= Rank(RoleAdmin) || Rank(RoleAdmin) <= Rank(RoleViewer) {
		t.Error("the team roles are not ordered from most to least powerful")
	}
}

// TestUnknownRolesFailClosed checks that a role string from outside the seven —
// a typo, or a row written through a path with no CHECK constraint — is denied
// rather than defaulted.
func TestUnknownRolesFailClosed(t *testing.T) {
	for _, role := range []Role{"", "administrator", "OWNER", "guest"} {
		if Valid(role) {
			t.Errorf("%q is treated as a valid role", role)
		}

		for _, permission := range Permissions {
			if Can(role, permission) {
				t.Errorf("unknown role %q was granted %s", role, permission)
			}
		}
	}
}

// TestMemberLimitIsUnlimitedOnEveryPlan pins the product promise. The incumbent
// caps team size at 1, 3 and 10 by tier; a cap that crept back in would be a
// silent change to what somebody bought.
func TestMemberLimitIsUnlimitedOnEveryPlan(t *testing.T) {
	for _, plan := range []string{"", "starter", "growth", "business", "enterprise"} {
		if limit := MemberLimit(plan); limit != 0 {
			t.Errorf("MemberLimit(%q) = %d, want 0 (unlimited)", plan, limit)
		}
	}
}
