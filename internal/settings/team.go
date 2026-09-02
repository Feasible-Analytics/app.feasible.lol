//
// team.go
// The team screen: members, guests, invitations, keys and ownership.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settings

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// teamSummary is the team as the header renders it.
type teamSummary struct {
	ID   int64
	Name string
}

// teamsMember and teamsGuest are the store's rows as the table renders them.
// They are aliases rather than the store types so a column added to the store
// does not silently appear on a customer-facing screen.
type teamsMember = teams.Member

type teamsGuest = teams.Guest

// apiKeyView is one key row.
type apiKeyView = teams.APIKey

// invitationView is one invitation with its expiry already worked out, so the
// template does no date arithmetic. A template that computes a deadline is a
// deadline computed differently from the one the store enforces.
type invitationView struct {
	ID      int64
	Email   string
	Role    teams.Role
	Guest   bool
	Expired bool
	Ago     string
}

// siteOption is one site in the guest-invitation picker.
type siteOption struct {
	ID     int64
	Domain string
}

// teamPage renders the team screen.
func (h *TeamHandler) teamPage(w http.ResponseWriter, r *http.Request, identity Identity, notice, problem string) {
	ctx := r.Context()

	role, err := h.Teams.RoleOf(ctx, identity.TeamID, identity.UserID)
	if err != nil {
		h.forbidden(w, err)

		return
	}
	if !teams.Can(role, teams.PermManageMembers) && !teams.Can(role, teams.PermCreateAPIKey) {
		h.forbidden(w, teams.ErrForbidden)

		return
	}

	var (
		members     []teams.Member
		guests      []teams.Guest
		invitations []teams.Invitation
	)
	if teams.Can(role, teams.PermManageMembers) {
		members, err = h.Teams.Members(ctx, identity.TeamID)
		if err != nil {
			h.internal(w, err)

			return
		}

		guests, err = h.Teams.Guests(ctx, identity.TeamID)
		if err != nil {
			h.internal(w, err)

			return
		}

		invitations, err = h.Teams.Invitations(ctx, identity.TeamID)
		if err != nil {
			h.internal(w, err)

			return
		}
	}

	keys, err := h.Teams.APIKeys(ctx, identity.UserID, identity.TeamID)
	if err != nil {
		h.internal(w, err)

		return
	}

	now := time.Now().UTC()

	views := make([]invitationView, 0, len(invitations))
	for _, invitation := range invitations {
		views = append(views, invitationView{
			ID:      invitation.ID,
			Email:   invitation.Email,
			Role:    invitation.Role,
			Guest:   teams.IsGuestRole(invitation.Role),
			Expired: invitation.Expired(now),
			Ago:     ago(time.Unix(invitation.ExpiresAt, 0).UTC(), now),
		})
	}

	urlNotice, urlProblem := flash(r)
	if notice == "" {
		notice = urlNotice
	}
	if problem == "" {
		problem = urlProblem
	}

	// Only a role that can create a key may be offered on the invitation form,
	// and only roles at or below the actor's rank may be assigned. Offering a
	// choice that will be refused is a form that lies about what it does.
	assignable := []teams.Role{}
	for _, candidate := range teams.TeamRoles() {
		if candidate != teams.RoleOwner && teams.Rank(candidate) <= teams.Rank(role) {
			assignable = append(assignable, candidate)
		}
	}

	invitable := append(append([]teams.Role{}, assignable...), teams.GuestRoles()...)

	h.render(w, r, "team", screen{
		TitleID:         "settings.nav.team",
		Tab:             "team",
		Message:         notice,
		Error:           problem,
		Team:            teamSummary{ID: identity.TeamID, Name: teamName(ctx, h.System, identity.TeamID)},
		Role:            role,
		Members:         members,
		Guests:          guests,
		Invitations:     views,
		APIKeys:         keys,
		NewKey:          h.takeNewKey(w, r),
		AssignableRoles: assignable,
		InvitableRoles:  invitable,
		Sites:           h.siteOptionsForRole(ctx, identity.TeamID, role),
	})
}

// siteOptionsForRole returns the guest targets only to member managers. A
// billing user opening this page for an API key has no reason to enumerate a
// team's sites.
func (h *TeamHandler) siteOptionsForRole(ctx context.Context, teamID int64, role teams.Role) []siteOption {
	if !teams.Can(role, teams.PermManageMembers) {
		return nil
	}

	return h.siteOptions(ctx, teamID)
}

// siteOptions lists a team's sites for the guest-invitation picker.
//
// A failure here empties the picker rather than failing the page, because the
// rest of the team screen is still usable — but it is logged, because a picker
// that is empty for a team that has sites looks like a permissions bug and
// would otherwise leave nothing anywhere to say what happened.
func (h *TeamHandler) siteOptions(ctx context.Context, teamID int64) []siteOption {
	rows, err := h.System.QueryContext(ctx, `
		SELECT id, domain FROM sites
		WHERE COALESCE(owner_team_id, account_id) = ?
		ORDER BY domain
	`, teamID)
	if err != nil {
		h.logSiteOptions(teamID, err)

		return nil
	}
	defer func() { _ = rows.Close() }()

	var options []siteOption

	for rows.Next() {
		var option siteOption
		if err := rows.Scan(&option.ID, &option.Domain); err != nil {
			h.logSiteOptions(teamID, err)

			return options
		}

		options = append(options, option)
	}

	if err := rows.Err(); err != nil {
		h.logSiteOptions(teamID, err)
	}

	return options
}

// logSiteOptions records why the guest-invitation picker is short.
func (h *TeamHandler) logSiteOptions(teamID int64, err error) {
	if h.Log != nil {
		h.Log.Error("the guest site picker could not be built", "team", teamID, "error", err)
	}
}

// teamAction handles every form on the team screen.
func (h *TeamHandler) teamAction(w http.ResponseWriter, r *http.Request, identity Identity, action string) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)

		return
	}

	if err := r.ParseForm(); err != nil {
		h.redirect(w, r, "/settings/members", "", tr(r, "settings.flash.form_unreadable"))

		return
	}

	ctx := r.Context()

	switch action {
	case "role":
		h.setRole(w, r, identity, ctx)

	case "remove":
		userID, _ := strconv.ParseInt(r.PostFormValue("user_id"), 10, 64)

		if err := h.Teams.RemoveMember(ctx, identity.UserID, identity.TeamID, userID); err != nil {
			h.redirect(w, r, "/settings/members", "", explain(err))

			return
		}

		h.redirect(w, r, "/settings/members", tr(r, "settings.flash.member_removed"), "")

	case "invite":
		h.invite(w, r, identity, ctx)

	case "invite/revoke":
		invitationID, _ := strconv.ParseInt(r.PostFormValue("invitation_id"), 10, 64)

		if err := h.Teams.RevokeInvitation(ctx, identity.UserID, identity.TeamID, invitationID); err != nil {
			h.redirect(w, r, "/settings/members", "", explain(err))

			return
		}

		h.redirect(w, r, "/settings/members", tr(r, "settings.flash.invitation_revoked"), "")

	case "api-keys":
		h.createKey(w, r, identity, ctx)

	case "api-keys/revoke":
		keyID, _ := strconv.ParseInt(r.PostFormValue("key_id"), 10, 64)

		if err := h.Teams.RevokeAPIKey(ctx, identity.UserID, identity.TeamID, keyID); err != nil {
			h.redirect(w, r, "/settings/members", "", explain(err))

			return
		}

		h.redirect(w, r, "/settings/members", tr(r, "settings.flash.key_revoked"), "")

	case "transfer":
		userID, _ := strconv.ParseInt(r.PostFormValue("user_id"), 10, 64)

		if err := h.Teams.TransferOwnership(ctx, identity.UserID, identity.TeamID, userID); err != nil {
			h.redirect(w, r, "/settings/members", "", explain(err))

			return
		}

		h.redirect(w, r, "/settings/members", tr(r, "settings.flash.ownership_transferred"), "")

	default:
		http.NotFound(w, r)
	}
}

// setRole changes one member's role.
func (h *TeamHandler) setRole(w http.ResponseWriter, r *http.Request, identity Identity, ctx context.Context) {
	userID, _ := strconv.ParseInt(r.PostFormValue("user_id"), 10, 64)
	role := teams.Role(r.PostFormValue("role"))

	if err := h.Teams.SetRole(ctx, identity.UserID, identity.TeamID, userID, role); err != nil {
		h.redirect(w, r, "/settings/members", "", explain(err))

		return
	}

	message := tr(r, "settings.flash.role_changed", "role", teams.Label(role))
	if !teams.Can(role, teams.PermCreateAPIKey) {
		message += " " + tr(r, "settings.flash.role_has_no_keys", "role", teams.Label(role))
	}

	h.redirect(w, r, "/settings/members", message, "")
}

// invite sends an invitation to any address.
func (h *TeamHandler) invite(w http.ResponseWriter, r *http.Request, identity Identity, ctx context.Context) {
	siteID, _ := strconv.ParseInt(r.PostFormValue("site_id"), 10, 64)
	role := teams.Role(r.PostFormValue("role"))

	// A guest role needs a site and a team role must not have one. Correcting
	// it here rather than refusing means the form can offer both kinds in one
	// pair of dropdowns without producing an error nobody can act on.
	if !teams.IsGuestRole(role) {
		siteID = 0
	}

	if teams.IsGuestRole(role) && siteID == 0 {
		h.redirect(w, r, "/settings/members", "", tr(r, "settings.flash.guest_needs_site"))

		return
	}

	token, invitation, err := h.Teams.Invite(ctx, identity.UserID, teams.Invitation{
		TeamID: identity.TeamID,
		SiteID: siteID,
		Email:  r.PostFormValue("email"),
		Role:   role,
	})
	if err != nil {
		h.redirect(w, r, "/settings/members", "", explain(err))

		return
	}

	// The inviter's name only decorates the email. A user row that will not
	// read is worth a line in the log, but not worth refusing to send an
	// invitation that is otherwise complete.
	var inviterName string
	if err := h.System.QueryRowContext(ctx, `SELECT name FROM users WHERE id = ?`, identity.UserID).Scan(&inviterName); err != nil && h.Log != nil {
		h.Log.Warn("the inviter's name could not be read", "user", identity.UserID, "error", err)
	}

	if h.Mail == nil {
		err = errors.New("settings: invitation mail is unavailable")
	} else {
		link := h.BaseURL + "/invitations/" + url.PathEscape(token)
		err = h.Mail.SendInvitation(ctx, invitation.Email, teamName(ctx, h.System, identity.TeamID),
			inviterName, teams.Label(invitation.Role), link, time.Unix(invitation.ExpiresAt, 0))
	}
	if err != nil {
		// The invitation is revoked because nobody received its link, and a
		// revoke that itself fails leaves a live token behind — which is a
		// credential, so it is logged rather than dropped.
		if revokeErr := h.Teams.RevokeInvitation(ctx, identity.UserID, identity.TeamID, invitation.ID); revokeErr != nil && h.Log != nil {
			h.Log.Error("an undelivered invitation could not be revoked", "team", identity.TeamID,
				"invitation", invitation.ID, "error", revokeErr)
		}

		if h.Log != nil {
			h.Log.Error("an invitation could not be delivered", "team", identity.TeamID,
				"invitation", invitation.ID, "email", invitation.Email, "error", err)
		}

		h.redirect(w, r, "/settings/members", "", "The invitation email could not be delivered.")

		return
	}

	h.redirect(w, r, "/settings/members",
		tr(r, "settings.flash.invitation_sent", "email", invitation.Email), "")
}

// createKey issues an API key scoped to this team.
func (h *TeamHandler) createKey(w http.ResponseWriter, r *http.Request, identity Identity, ctx context.Context) {
	secret, _, err := h.Teams.CreateAPIKey(ctx, identity.UserID, identity.TeamID, r.PostFormValue("name"), nil)
	if err != nil {
		h.redirect(w, r, "/settings/members", "", explain(err))

		return
	}

	// The secret travels in a cookie rather than in the redirect's query
	// string, so a live credential does not end up in browser history, the
	// proxy's access log, or a Referer header on the next request.
	http.SetCookie(w, &http.Cookie{
		Name:     keyCookie,
		Value:    secret,
		Path:     PathPrefix,
		HttpOnly: true,
		Secure:   strings.HasPrefix(h.BaseURL, "https://"),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   60,
	})

	h.redirect(w, r, "/settings/members", tr(r, "settings.flash.key_created"), "")
}

// takeNewKey reads a freshly-created key out of its cookie and clears it, so
// the secret is shown exactly once.
func (h *TeamHandler) takeNewKey(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie(keyCookie)
	if err != nil || cookie.Value == "" {
		return ""
	}

	http.SetCookie(w, &http.Cookie{Name: keyCookie, Value: "", Path: PathPrefix, MaxAge: -1})

	return cookie.Value
}

// explain turns a store error into a sentence for the person who caused it.
func explain(err error) string {
	switch {
	case errors.Is(err, teams.ErrLastOwner):
		return "A team always needs an owner. Transfer ownership first, then change this."

	case errors.Is(err, teams.ErrForbidden):
		return err.Error()

	case errors.Is(err, teams.ErrNotFound):
		return "That person or invitation is not part of this team."

	case errors.Is(err, teams.ErrInvalidRole):
		return err.Error()
	}

	return err.Error()
}

// internal answers our own failure.
func (h *TeamHandler) internal(w http.ResponseWriter, err error) {
	if h.Log != nil {
		h.Log.Error("a settings page failed", "error", err)
	}

	http.Error(w, "The page could not be built.", http.StatusInternalServerError)
}
