//
// handlers_settings.go
// Profile, password, sessions, two-factor, the team policy and deleting the account.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/i18n"
)

// showAccountSettings renders the profile and password screen.
func (h *Handler) showAccountSettings(w http.ResponseWriter, r *http.Request) {
	p := h.newPage(r, tr(r, "auth.title.account"), "settings")
	p.Data["MinLength"] = MinPasswordLength

	switch r.URL.Query().Get("saved") {
	case "profile":
		p.Flash = i18n.T(p.Lang, "auth.flash.profile_saved")
	case "password":
		p.Flash = i18n.T(p.Lang, "auth.flash.password_changed")
	}

	h.render(w, r, "settings_account", p, http.StatusOK)
}

// doUpdateProfile saves the display name and theme.
func (h *Handler) doUpdateProfile(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)

	theme := r.PostFormValue("theme")
	if theme != "light" && theme != "dark" {
		theme = "system"
	}

	if err := h.Store.UpdateProfile(r.Context(), user.ID, strings.TrimSpace(r.PostFormValue("name")), theme); err != nil {
		h.fail(w, r, err)
		return
	}

	http.Redirect(w, r, "/settings?saved=profile", http.StatusFound)
}

// doChangePassword changes the password from inside the account.
//
// The current password is required even though the browser is already signed
// in. A change that needed only a session means a stolen cookie becomes a
// permanent takeover, because the attacker can lock the owner out with it.
func (h *Handler) doChangePassword(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)
	session := sessionFrom(r)

	current := r.PostFormValue("current_password")
	next := r.PostFormValue("new_password")

	p := h.newPage(r, tr(r, "auth.title.account"), "settings")
	p.Data["MinLength"] = MinPasswordLength

	// Somebody who signed up with Google has no current password to give, so
	// the field is not demanded of them — setting the first password is not a
	// change of anything.
	if user.PasswordHash != "" && !CheckPassword(user.PasswordHash, current) {
		p.Error = i18n.T(p.Lang, "auth.error.current_password")
		h.render(w, r, "settings_account", p, http.StatusUnauthorized)

		return
	}

	if err := ValidatePassword(next); err != nil {
		p.Error = strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + "."
		h.render(w, r, "settings_account", p, http.StatusBadRequest)

		return
	}

	if err := h.Store.SetPassword(r.Context(), user.ID, next, session.ID); err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Info("password changed", "user", user.ID)

	if err := h.Mailer.SendPasswordChanged(r.Context(), user.Email, user.Name); err != nil {
		h.Log.Warn("could not send the password-changed email", "user", user.ID, "error", err)
	}

	http.Redirect(w, r, "/settings?saved=password", http.StatusFound)
}

// showSessions renders the login-management screen.
func (h *Handler) showSessions(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	session := sessionFrom(r)

	list, err := h.Store.ListSessions(r.Context(), user.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	p := h.newPage(r, tr(r, "auth.title.sessions"), "settings")
	p.Data["Sessions"] = list
	p.Data["CurrentID"] = session.ID

	if r.URL.Query().Get("revoked") == "1" {
		p.Flash = i18n.T(p.Lang, "auth.flash.device_revoked")
	}

	h.render(w, r, "settings_sessions", p, http.StatusOK)
}

// doRevokeSession signs one device out, or every device but this one.
func (h *Handler) doRevokeSession(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)
	session := sessionFrom(r)

	if r.PostFormValue("all") == "1" {
		if err := h.Store.RevokeAllSessions(r.Context(), user.ID, session.ID); err != nil {
			h.fail(w, r, err)
			return
		}

		h.Log.Info("all other sessions revoked", "user", user.ID)
		http.Redirect(w, r, "/settings/sessions?revoked=1", http.StatusFound)

		return
	}

	id, err := strconv.ParseInt(r.PostFormValue("session_id"), 10, 64)
	if err != nil {
		http.Redirect(w, r, "/settings/sessions", http.StatusFound)
		return
	}

	if err := h.Store.RevokeSession(r.Context(), user.ID, id); err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Info("session revoked", "user", user.ID, "session", id)

	// Revoking the session you are using is a sign-out, and pretending
	// otherwise leaves the browser holding a cookie for a row that is gone.
	if id == session.ID {
		ClearSessionCookie(w, h.BaseURL)
		http.Redirect(w, r, "/login", http.StatusFound)

		return
	}

	http.Redirect(w, r, "/settings/sessions?revoked=1", http.StatusFound)
}

// showSecurity renders the two-factor screen.
func (h *Handler) showSecurity(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)

	p := h.newPage(r, tr(r, "auth.title.security"), "settings")
	p.Data["Enabled"] = user.TwoFactorEnabled()
	p.Data["RecoveryLeft"] = RecoveryCodesLeft(user)
	p.Data["HasPassword"] = user.PasswordHash != ""

	// A half-finished enrolment is resumed rather than restarted. Issuing a new
	// secret on every page load would invalidate the entry somebody has already
	// added to their phone.
	if !user.TwoFactorEnabled() && user.TOTPSecret != "" {
		key, err := h.Store.TOTPKey(r.Context(), h.Sealer, user)
		if err == nil {
			p.Data["Enrolling"] = true
			p.Data["Secret"] = key.Secret()
		}
	}

	if r.URL.Query().Get("required") == "1" {
		p.Error = i18n.T(p.Lang, "auth.error.two_factor_required")
	}

	if r.URL.Query().Get("disabled") == "1" {
		p.Flash = i18n.T(p.Lang, "auth.flash.two_factor_off")
	}

	h.render(w, r, "settings_security", p, http.StatusOK)
}

// doStartTwoFactor issues a secret and shows the QR code.
func (h *Handler) doStartTwoFactor(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)

	key, err := h.Store.BeginTOTP(r.Context(), h.Sealer, user)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	p := h.newPage(r, tr(r, "auth.title.security"), "settings")
	p.Data["Enabled"] = false
	p.Data["Enrolling"] = true
	p.Data["Secret"] = key.Secret()
	p.Data["HasPassword"] = user.PasswordHash != ""

	h.render(w, r, "settings_security", p, http.StatusOK)
}

// twoFactorQR renders the enrolment QR code as a PNG.
//
// It is a separate request rather than a data URI in the page so that the
// secret never appears in the HTML the browser caches, and so the image is
// never written into a page that something else might store.
func (h *Handler) twoFactorQR(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)

	key, err := h.Store.TOTPKey(r.Context(), h.Sealer, user)
	if err != nil {
		http.Error(w, "no enrolment in progress", http.StatusNotFound)
		return
	}

	png, err := QRCodePNG(key)
	if err != nil {
		h.Log.Error("could not render the two-factor qr code", "user", user.ID, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// doEnableTwoFactor finishes enrolment once a code from the app checks out, and
// shows the recovery codes.
func (h *Handler) doEnableTwoFactor(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)

	if !h.Limiter.Allow(SubjectKey(strconv.FormatInt(user.ID, 10), "2fa-setup"), TwoFactorAttempts, TwoFactorWindow) {
		p := h.newPage(r, tr(r, "auth.title.security"), "settings")
		p.Data["Enrolling"] = true
		p.Error = i18n.T(p.Lang, "auth.error.too_many_attempts")

		h.render(w, r, "settings_security", p, http.StatusTooManyRequests)

		return
	}

	valid, err := h.Store.VerifyTOTP(h.Sealer, user, r.PostFormValue("code"))
	if err != nil {
		h.fail(w, r, err)
		return
	}

	if !valid {
		key, keyErr := h.Store.TOTPKey(r.Context(), h.Sealer, user)

		p := h.newPage(r, tr(r, "auth.title.security"), "settings")
		p.Data["Enrolling"] = true
		p.Data["HasPassword"] = user.PasswordHash != ""
		p.Error = i18n.T(p.Lang, "auth.error.totp_code")

		if keyErr == nil {
			p.Data["Secret"] = key.Secret()
		}

		h.render(w, r, "settings_security", p, http.StatusUnauthorized)

		return
	}

	codes, err := h.Store.EnableTOTP(r.Context(), user.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Info("two-factor enabled", "user", user.ID)

	// The codes are shown here and nowhere else, ever. Storing a readable copy
	// so they could be shown again would defeat the point of hashing them.
	p := h.newPage(r, tr(r, "auth.title.recovery_codes"), "settings")
	p.Data["Codes"] = codes

	h.render(w, r, "recovery_codes", p, http.StatusOK)
}

// doRegenerateRecovery issues a fresh set of recovery codes.
func (h *Handler) doRegenerateRecovery(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)

	if user.PasswordHash != "" && !CheckPassword(user.PasswordHash, r.PostFormValue("password")) {
		p := h.newPage(r, tr(r, "auth.title.security"), "settings")
		p.Data["Enabled"] = user.TwoFactorEnabled()
		p.Data["RecoveryLeft"] = RecoveryCodesLeft(user)
		p.Data["HasPassword"] = true
		p.Error = i18n.T(p.Lang, "auth.error.password")

		h.render(w, r, "settings_security", p, http.StatusUnauthorized)

		return
	}

	codes, err := h.Store.RegenerateRecoveryCodes(r.Context(), user.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Info("recovery codes regenerated", "user", user.ID)

	p := h.newPage(r, tr(r, "auth.title.recovery_codes"), "settings")
	p.Data["Codes"] = codes
	p.Flash = i18n.T(p.Lang, "auth.flash.recovery_regenerated")

	h.render(w, r, "recovery_codes", p, http.StatusOK)
}

// doDisableTwoFactor turns two-factor off after re-authentication.
//
// The password is required, and the team policy overrides everything: a member
// of a team that requires two-factor cannot switch it off, because that would
// make the policy advisory.
func (h *Handler) doDisableTwoFactor(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)

	p := h.newPage(r, tr(r, "auth.title.security"), "settings")
	p.Data["Enabled"] = user.TwoFactorEnabled()
	p.Data["RecoveryLeft"] = RecoveryCodesLeft(user)
	p.Data["HasPassword"] = user.PasswordHash != ""

	if team, err := h.Store.TeamForUser(r.Context(), user.ID); err == nil && team.Require2FA {
		p.Error = i18n.T(p.Lang, "auth.error.two_factor_locked")
		h.render(w, r, "settings_security", p, http.StatusForbidden)

		return
	}

	if user.PasswordHash != "" && !CheckPassword(user.PasswordHash, r.PostFormValue("password")) {
		p.Error = i18n.T(p.Lang, "auth.error.password")
		h.render(w, r, "settings_security", p, http.StatusUnauthorized)

		return
	}

	if err := h.Store.DisableTOTP(r.Context(), user.ID); err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Info("two-factor disabled", "user", user.ID)
	http.Redirect(w, r, "/settings/security?disabled=1", http.StatusFound)
}

// showTeamSettings renders the team name and the two-factor policy.
func (h *Handler) showTeamSettings(w http.ResponseWriter, r *http.Request) {
	p := h.newPage(r, tr(r, "auth.title.team"), "settings")

	if r.URL.Query().Get("saved") == "1" {
		p.Flash = i18n.T(p.Lang, "auth.flash.team_saved")
	}

	h.render(w, r, "settings_team", p, http.StatusOK)
}

// doTeamSettings saves the team name and the two-factor policy.
func (h *Handler) doTeamSettings(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)

	team, err := h.Store.TeamForUser(r.Context(), user.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	if name := strings.TrimSpace(r.PostFormValue("name")); name != "" {
		if err := h.Store.UpdateTeamName(r.Context(), team.ID, name); err != nil {
			h.fail(w, r, err)
			return
		}
	}

	require := r.PostFormValue("require_2fa") == "1"

	if err := h.Store.SetRequire2FA(r.Context(), team.ID, require); err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Info("team settings saved", "team", team.ID, "require_2fa", require)
	http.Redirect(w, r, "/settings/team?saved=1", http.StatusFound)
}

// doDeleteAccount deletes everything.
//
// It demands the password and the typed word, because there is nothing after
// this: the account database file is unlinked, the control rows are gone and
// the payment provider's customer record is deleted. No support process can put
// any of it back.
func (h *Handler) doDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)

	team, err := h.Store.TeamForUser(r.Context(), user.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	p := h.newPage(r, tr(r, "auth.title.account"), "settings")
	p.Data["MinLength"] = MinPasswordLength

	if strings.TrimSpace(strings.ToUpper(r.PostFormValue("confirm"))) != "DELETE" {
		p.Error = i18n.T(p.Lang, "auth.error.type_delete")
		h.render(w, r, "settings_account", p, http.StatusBadRequest)

		return
	}

	if user.PasswordHash != "" && !CheckPassword(user.PasswordHash, r.PostFormValue("password")) {
		p.Error = i18n.T(p.Lang, "auth.error.password")
		h.render(w, r, "settings_account", p, http.StatusUnauthorized)

		return
	}

	if err := h.Deleter.DeleteAccount(r.Context(), user.ID, team.ID); err != nil {
		h.fail(w, r, err)
		return
	}

	// The routing map is rebuilt so the deleted domains stop being accepted
	// now, rather than at the end of the next refresh interval.
	if h.SiteCache != nil {
		if err := h.SiteCache.Refresh(r.Context()); err != nil {
			h.Log.Warn("could not refresh the routing map after deleting an account", "error", err)
		}
	}

	ClearSessionCookie(w, h.BaseURL)

	p = h.newPage(r, tr(r, "auth.title.account_deleted"), "")
	p.Flash = i18n.T(p.Lang, "auth.flash.account_deleted")

	h.render(w, r, "deleted", p, http.StatusOK)
}

// requireOwner reports whether somebody may change team-wide settings. It is
// separate from the team read so that a caller cannot accidentally treat "is in
// the team" as "may change the team".
func (h *Handler) requireOwner(r *http.Request, teamID int64) bool {
	user := userFrom(r)
	if user == nil {
		return false
	}

	ok, err := exists(r.Context(), h.Store.DB(),
		"SELECT 1 FROM team_memberships WHERE team_id = ? AND user_id = ? AND role IN ('owner', 'admin')",
		teamID, user.ID)
	if err != nil {
		h.Log.Warn("could not read the team role", "user", user.ID, "team", teamID, "error", err)
		return false
	}

	return ok
}

// notFound renders the 404 page. It is used wherever a site id in a URL does
// not belong to the signed-in team, so a guessed id is indistinguishable from
// one that does not exist.
func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	p := h.newPage(r, tr(r, "auth.title.not_found"), "")
	p.Error = i18n.T(p.Lang, "auth.error.not_found")

	h.render(w, r, "error", p, http.StatusNotFound)
}

// siteOr404 loads a site scoped to the signed-in team, rendering the 404 page
// and reporting false when it is not theirs.
func (h *Handler) siteOr404(w http.ResponseWriter, r *http.Request) (*Site, *Team, bool) {
	user := userFrom(r)

	team, err := h.Store.TeamForUser(r.Context(), user.ID)
	if err != nil {
		h.notFound(w, r)
		return nil, nil, false
	}

	site, err := h.Store.SiteByID(r.Context(), team.ID, pathID(r, "id"))
	if errors.Is(err, ErrNotFound) {
		h.notFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		h.fail(w, r, err)
		return nil, nil, false
	}

	return site, team, true
}
