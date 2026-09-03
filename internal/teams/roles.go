//
// roles.go
// The seven roles and exactly what each one may do.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package teams owns membership and authorisation: who is in a team, what
// their role lets them do, how somebody is invited, and how a site or a whole
// account changes hands.
//
// The permission matrix is a table in this file rather than a scattering of
// `if role == "admin"` checks, because an authorisation rule that lives at its
// call site is a rule nobody can review as a whole. Every question the rest of
// the product asks about permission goes through Can, and the matrix is
// asserted end to end by a test.
//
// Single sign-on is deliberately absent. SAML 2.0 is a later addition and will
// be built from the specification when it lands; nothing in this package
// assumes a password is the only way somebody proved who they are.
package teams

import "sort"

// Role is a person's standing in a team, or on one site.
type Role string

// The five team roles, ordered from most to least powerful. The order is
// meaningful: Rank reads it, so a check for "at least an admin" is a
// comparison rather than a list of role names somebody will forget to extend.
const (
	// RoleOwner manages the team, its sites, the subscription and the security
	// policy, and is the only role that can delete the team or hand it on.
	RoleOwner Role = "owner"

	// RoleAdmin manages members, sites and the subscription, but not the team
	// itself. The split is about what cannot be undone: an admin is somebody
	// trusted to run the account day to day, and paying for it is part of that,
	// while deleting the team or handing it to somebody else is not.
	RoleAdmin Role = "admin"

	// RoleEditor reads every dashboard and changes site settings.
	RoleEditor Role = "editor"

	// RoleBilling lets an accounts department pay an invoice without being
	// given the ability to reconfigure anybody's tracking.
	RoleBilling Role = "billing"

	// RoleViewer reads dashboards and nothing else. It is the one team role
	// that cannot create an API key: a key is a durable credential that
	// outlives a session, and handing one to a read-only seat is how a
	// read-only seat becomes a permanent unaudited integration.
	RoleViewer Role = "viewer"
)

// The two guest roles. A guest is invited to a single site and can see nothing
// else about the team that owns it, which is what makes an agency handing a
// client a login possible at all. Neither can create an API key, for the same
// reason a Viewer cannot and more so: a guest is by definition outside the team.
const (
	RoleGuestEditor Role = "guest_editor"
	RoleGuestViewer Role = "guest_viewer"
)

// Permission is one thing somebody may or may not do. They are strings rather
// than an enum of ints so a permission in a log line or a forbidden response is
// readable without a lookup table.
type Permission string

const (
	// PermViewDashboard is reading the numbers. Every role has it — a seat that
	// cannot see the product is not a seat.
	PermViewDashboard Permission = "view_dashboard"

	// PermManageSiteSettings covers one site's own configuration: its timezone,
	// its hostname allow-list, its shared links, its scheduled reports, its
	// alerts and its annotations.
	PermManageSiteSettings Permission = "manage_site_settings"

	// PermManageSites is adding, removing and transferring sites, which is a
	// team-level power rather than a site-level one.
	PermManageSites Permission = "manage_sites"

	// PermManageMembers is inviting, removing and re-roling people.
	PermManageMembers Permission = "manage_members"

	// PermManageTeam is the team's own settings, including its name.
	PermManageTeam Permission = "manage_team"

	// PermDeleteTeam destroys the account and every site in it.
	PermDeleteTeam Permission = "delete_team"

	// PermTransferOwnership hands the account to another member.
	PermTransferOwnership Permission = "transfer_ownership"

	// PermManageBilling is the subscription, the card and the invoices.
	PermManageBilling Permission = "manage_billing"

	// PermManageSecurity is the team-wide security policy — today two-factor
	// enforcement, and single sign-on when that is built.
	PermManageSecurity Permission = "manage_security"

	// PermCreateAPIKey is issuing a key for the stats API. Viewer, Guest Editor
	// and Guest Viewer do not have it, and that is the single most load-bearing
	// "false" in this table.
	PermCreateAPIKey Permission = "create_api_key"
)

// Permissions is every permission, in a stable order. The settings screen and
// the matrix test both walk it, so a permission added without a decision for
// every role fails a test rather than defaulting to denied in silence.
var Permissions = []Permission{
	PermViewDashboard,
	PermManageSiteSettings,
	PermManageSites,
	PermManageMembers,
	PermManageTeam,
	PermDeleteTeam,
	PermTransferOwnership,
	PermManageBilling,
	PermManageSecurity,
	PermCreateAPIKey,
}

// matrix is the whole authorisation model.
//
// Two decisions in here are worth stating rather than leaving to be inferred.
//
// Billing can read dashboards. The role exists to keep an accounts department
// out of a site's configuration, not out of the product; a seat that can pay for
// something it cannot see is a seat nobody buys.
//
// Guest Editor has PermManageSiteSettings but not PermManageSites. It can
// configure the one site it was invited to and cannot add, remove or move any
// site — exactly the boundary between a client and the agency managing them.
var matrix = map[Role]map[Permission]bool{
	RoleOwner: {
		PermViewDashboard:      true,
		PermManageSiteSettings: true,
		PermManageSites:        true,
		PermManageMembers:      true,
		PermManageTeam:         true,
		PermDeleteTeam:         true,
		PermTransferOwnership:  true,
		PermManageBilling:      true,
		PermManageSecurity:     true,
		PermCreateAPIKey:       true,
	},
	RoleAdmin: {
		PermViewDashboard:      true,
		PermManageSiteSettings: true,
		PermManageSites:        true,
		PermManageMembers:      true,
		PermManageBilling:      true,
		PermCreateAPIKey:       true,
	},
	RoleEditor: {
		PermViewDashboard:      true,
		PermManageSiteSettings: true,
		PermCreateAPIKey:       true,
	},
	RoleBilling: {
		PermViewDashboard: true,
		PermManageBilling: true,
		PermCreateAPIKey:  true,
	},
	RoleViewer: {
		PermViewDashboard: true,
	},
	RoleGuestEditor: {
		PermViewDashboard:      true,
		PermManageSiteSettings: true,
	},
	RoleGuestViewer: {
		PermViewDashboard: true,
	},
}

// rank orders the team roles for comparisons such as "at least an admin". The
// guest roles are deliberately outside the ordering: a guest is not a weaker
// member, it is a different kind of relationship, and pretending otherwise
// would let a rank comparison accidentally admit one.
var rank = map[Role]int{
	RoleOwner:   5,
	RoleAdmin:   4,
	RoleEditor:  3,
	RoleBilling: 2,
	RoleViewer:  1,
}

// Can answers whether a role may do something. An unknown role is denied rather
// than defaulted, so a role string that reached the database through a path
// without a CHECK constraint fails closed.
func Can(role Role, permission Permission) bool {
	return matrix[role][permission]
}

// IsTeamRole reports whether a role is one of the five held inside a team, as
// opposed to a per-site guest role.
func IsTeamRole(role Role) bool {
	_, ok := rank[role]

	return ok
}

// IsGuestRole reports whether a role is granted per site.
func IsGuestRole(role Role) bool {
	return role == RoleGuestEditor || role == RoleGuestViewer
}

// Valid reports whether a role string is one this product knows.
func Valid(role Role) bool {
	_, ok := matrix[role]

	return ok
}

// Rank returns a team role's position in the hierarchy, zero for anything that
// is not a team role. It exists so that "an admin may not re-role an owner" is
// a comparison rather than a chain of equality tests.
func Rank(role Role) int {
	return rank[role]
}

// TeamRoles lists the five team roles, most powerful first. The invitation form
// renders it, so the order it is offered in is the order defined here.
func TeamRoles() []Role {
	out := make([]Role, 0, len(rank))
	for role := range rank {
		out = append(out, role)
	}

	sort.Slice(out, func(i, j int) bool { return rank[out[i]] > rank[out[j]] })

	return out
}

// GuestRoles lists the two per-site roles.
func GuestRoles() []Role {
	return []Role{RoleGuestEditor, RoleGuestViewer}
}

// Label is the role's name as a person reads it on a settings screen.
func Label(role Role) string {
	switch role {
	case RoleOwner:
		return "Owner"
	case RoleAdmin:
		return "Admin"
	case RoleEditor:
		return "Editor"
	case RoleBilling:
		return "Billing"
	case RoleGuestEditor:
		return "Guest Editor"
	case RoleGuestViewer:
		return "Guest Viewer"
	case RoleViewer:
		return "Viewer"
	}

	return string(role)
}

// MemberLimit is how many people may be in a team, and always returns zero,
// meaning no limit. It is a function rather than an absent check because
// "unlimited team members on every plan" is a product promise, and a promise
// with no code behind it is one somebody quietly caps next quarter. The
// incumbent caps this at 1, 3 and 10 by tier; we do not cap it at all.
func MemberLimit(plan string) int {
	return 0
}
