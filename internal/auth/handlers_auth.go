//
// handlers_auth.go
// Registration, sign-in, verification, password reset and the two-factor challenge.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// pendingTwoFactorCookie carries the half-finished sign-in between the password
// step and the code step. It is a signed cookie rather than a session row
// because it is not a session: it grants nothing, expires in minutes, and a
// table of abandoned half-logins is a table that only ever needs cleaning.
const pendingTwoFactorCookie = "feasible_2fa"

// showRegister renders the sign-up form.
func (h *Handler) showRegister(w http.ResponseWriter, r *http.Request) {
	if userFrom(r) != nil {
		http.Redirect(w, r, "/sites", http.StatusFound)
		return
	}

	h.render(w, r, "register", h.newPage(r, "Create your account", ""), http.StatusOK)
}

// doRegister creates an account and sends the verification email.
//
// It signs the person in immediately, before the address is proven. That is
// deliberate: everything they can reach in that state is the verification
// screen, and the alternative — a sign-up that ends at a "check your email" dead
// end — loses the people who close the tab.
func (h *Handler) doRegister(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	email := NormaliseEmail(r.PostFormValue("email"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	password := r.PostFormValue("password")

	p := h.newPage(r, "Create your account", "")
	p.Data["Email"] = email
	p.Data["Name"] = name

	fail := func(message string) {
		p.Error = message
		h.render(w, r, "register", p, http.StatusBadRequest)
	}

	if !LooksLikeEmail(email) {
		fail("That does not look like an email address.")
		return
	}

	if err := ValidatePassword(password); err != nil {
		fail(strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + ".")
		return
	}

	// Registration is rate limited per source. Without it, one script creates
	// accounts as fast as bcrypt runs and every one of them sends an email from
	// our domain to an address the script chose.
	if !h.Limiter.Allow(ClientKey(r, "register"), LoginAttempts, LoginWindow) {
		fail("Too many accounts created from here. Wait a few minutes and try again.")
		return
	}

	hash, err := HashPassword(password)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	user, team, err := h.Store.CreateUser(r.Context(), email, name, hash, "")
	if errors.Is(err, ErrEmailTaken) {
		fail("An account already uses that email address. Sign in instead, or reset your password.")
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Info("account created", "user", user.ID, "team", team.ID)

	h.sendVerification(r, user)
	h.startSession(w, r, user)

	http.Redirect(w, r, "/verify-email", http.StatusFound)
}

// sendVerification issues a code and a link and emails both.
//
// A delivery failure is logged rather than shown, and the flow continues. The
// screen offers a resend button, so a person whose first email bounced off a
// greylist has an obvious next move; failing the whole registration would leave
// them with an account they cannot reach and no way to try again.
func (h *Handler) sendVerification(r *http.Request, user *User) {
	code, linkToken, err := h.Store.CreateVerification(r.Context(), user.ID)
	if err != nil {
		h.Log.Error("could not create a verification code", "user", user.ID, "error", err)
		return
	}

	link := h.BaseURL + "/verify-email/confirm?token=" + url.QueryEscape(linkToken)

	if err := h.Mailer.SendVerification(r.Context(), user.Email, user.Name, code, link); err != nil {
		h.Log.Error("could not send the verification email", "user", user.ID, "error", err)
	}
}

// startSession creates the session row and sets the cookie. Every successful
// sign-in path goes through it so that the cookie's attributes — and in
// particular the absence of a Domain — are decided in exactly one place.
func (h *Handler) startSession(w http.ResponseWriter, r *http.Request, user *User) {
	label := DeviceLabel(r.UserAgent())

	seen, err := h.Store.HasSeenDevice(r.Context(), user.ID, label)
	if err != nil {
		h.Log.Warn("could not check known devices", "user", user.ID, "error", err)
	}

	token, session, err := h.Store.CreateSession(r.Context(), user.ID, label)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	SetSessionCookie(w, token, h.BaseURL)

	h.Log.Info("signed in", "user", user.ID, "session", session.ID, "device", label)

	// The new-device email is worth sending only when the device really is new
	// and the account is old enough for it to mean something. One on the very
	// first sign-in would just be a second email about an account somebody
	// created ten seconds ago.
	if !seen && user.Verified() && user.CreatedAt < h.Store.Now().Add(-time.Minute).Unix() {
		if err := h.Mailer.SendNewLogin(r.Context(), user.Email, user.Name, label, h.Store.Now()); err != nil {
			h.Log.Warn("could not send the new-device email", "user", user.ID, "error", err)
		}
	}
}

// showLogin renders the sign-in form.
func (h *Handler) showLogin(w http.ResponseWriter, r *http.Request) {
	if userFrom(r) != nil {
		http.Redirect(w, r, "/sites", http.StatusFound)
		return
	}

	p := h.newPage(r, "Sign in", "")
	p.Data["Next"] = safeNext(r.URL.Query().Get("next"))

	if r.URL.Query().Get("reset") == "1" {
		p.Flash = "Your password has been changed. Sign in with the new one."
	}

	h.render(w, r, "login", p, http.StatusOK)
}

// doLogin checks a password and either signs somebody in or sends them to the
// two-factor challenge.
//
// The failure message is the same whether the address is unknown or the
// password is wrong. Distinguishing them turns the sign-in form into a way to
// find out who has an account here, which for an analytics product means
// finding out which companies use us.
func (h *Handler) doLogin(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	email := NormaliseEmail(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))

	p := h.newPage(r, "Sign in", "")
	p.Data["Email"] = email
	p.Data["Next"] = next

	// Both keys are checked: by source, so one machine cannot walk through
	// every account, and by address, so a botnet cannot spread one account's
	// guesses across a thousand sources.
	sourceOK := h.Limiter.Allow(ClientKey(r, "login"), LoginAttempts, LoginWindow)
	subjectOK := h.Limiter.Allow(SubjectKey(email, "login"), LoginAttempts, LoginWindow)

	if !sourceOK || !subjectOK {
		h.Log.Warn("login rate limit reached", "email_hash", HashToken(email)[:12])

		p.Error = "Too many sign-in attempts. Wait a few minutes and try again."
		h.render(w, r, "login", p, http.StatusTooManyRequests)

		return
	}

	user, err := h.Store.UserByEmail(r.Context(), email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		h.fail(w, r, err)
		return
	}

	// The password is checked even when there is no user, against a dummy hash,
	// so the response time is the same either way and the form cannot be used
	// to enumerate addresses.
	if user == nil {
		CheckPassword("", password)

		p.Error = "That email address and password do not match an account."
		h.render(w, r, "login", p, http.StatusUnauthorized)

		return
	}

	if !CheckPassword(user.PasswordHash, password) {
		p.Error = "That email address and password do not match an account."

		if user.PasswordHash == "" {
			p.Error = "That account signs in with Google. Use the Google button above."
		}

		h.render(w, r, "login", p, http.StatusUnauthorized)

		return
	}

	h.Limiter.Reset(ClientKey(r, "login"))
	h.Limiter.Reset(SubjectKey(email, "login"))

	if user.TwoFactorEnabled() {
		h.setPendingTwoFactor(w, user.ID, next)
		http.Redirect(w, r, "/login/2fa", http.StatusFound)

		return
	}

	h.startSession(w, r, user)

	if !user.Verified() {
		http.Redirect(w, r, "/verify-email", http.StatusFound)
		return
	}

	http.Redirect(w, r, next, http.StatusFound)
}

// doLogout ends the session. It is a POST rather than a GET so that a link on
// another site, or a prefetching browser, cannot sign somebody out.
func (h *Handler) doLogout(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	if session := sessionFrom(r); session != nil {
		if err := h.Store.RevokeSession(r.Context(), session.UserID, session.ID); err != nil {
			h.Log.Warn("could not delete the session row on sign-out", "session", session.ID, "error", err)
		}
	}

	ClearSessionCookie(w, h.BaseURL)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// showVerify renders the code-entry screen.
func (h *Handler) showVerify(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if user.Verified() {
		http.Redirect(w, r, "/sites", http.StatusFound)
		return
	}

	p := h.newPage(r, "Confirm your email", "")
	p.Data["Digits"] = VerificationCodeDigits

	if r.URL.Query().Get("sent") == "1" {
		p.Flash = "We sent a new code to " + user.Email + "."
	}

	h.render(w, r, "verify", p, http.StatusOK)
}

// doVerify consumes a typed code.
func (h *Handler) doVerify(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// The code is short enough to type, which is exactly why the attempt limit
	// matters: it is what makes eight digits sufficient.
	if !h.Limiter.Allow(SubjectKey(fmt.Sprint(user.ID), "verify"), TwoFactorAttempts, TwoFactorWindow) {
		p := h.newPage(r, "Confirm your email", "")
		p.Data["Digits"] = VerificationCodeDigits
		p.Error = "Too many attempts. Wait a minute and try again."

		h.render(w, r, "verify", p, http.StatusTooManyRequests)

		return
	}

	code := strings.TrimSpace(r.PostFormValue("code"))

	err := h.Store.ConsumeVerification(r.Context(), user.ID, code, "")
	if err == nil {
		h.Log.Info("email verified", "user", user.ID)
		http.Redirect(w, r, "/sites?welcome=1", http.StatusFound)

		return
	}

	p := h.newPage(r, "Confirm your email", "")
	p.Data["Digits"] = VerificationCodeDigits

	switch {
	case errors.Is(err, ErrTokenExpired):
		p.Error = "That code has expired. Ask for a new one."
	case errors.Is(err, ErrRateLimited):
		p.Error = "Too many wrong codes — that one has been cancelled. Ask for a new one."
	case errors.Is(err, ErrNotFound):
		p.Error = "There is no code outstanding. Ask for a new one."
	case errors.Is(err, ErrBadCredentials):
		p.Error = "That code is not right."
	default:
		h.fail(w, r, err)
		return
	}

	h.render(w, r, "verify", p, http.StatusBadRequest)
}

// doResendVerify issues a fresh code.
func (h *Handler) doResendVerify(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// Resends are limited per account. An unlimited resend button is a way to
	// send someone else's mailbox one email per click.
	if !h.Limiter.Allow(SubjectKey(fmt.Sprint(user.ID), "verify-resend"), ResetAttempts, ResetWindowLimit) {
		http.Redirect(w, r, "/verify-email", http.StatusFound)
		return
	}

	h.sendVerification(r, user)
	http.Redirect(w, r, "/verify-email?sent=1", http.StatusFound)
}

// doVerifyLink consumes the one-tap link from the email.
//
// It works whether or not the browser opening it is signed in, because the
// email is very often opened on a phone that is not — which is the whole reason
// the link exists beside the code.
func (h *Handler) doVerifyLink(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")

	userID, err := h.Store.UserIDForVerificationLink(r.Context(), token)
	if err != nil {
		p := h.newPage(r, "Confirm your email", "")
		p.Error = "That link has expired or has already been used. Sign in and ask for a new code."

		h.render(w, r, "verify_failed", p, http.StatusBadRequest)

		return
	}

	if err := h.Store.ConsumeVerification(r.Context(), userID, "", token); err != nil {
		p := h.newPage(r, "Confirm your email", "")
		p.Error = "That link has expired or has already been used. Sign in and ask for a new code."

		h.render(w, r, "verify_failed", p, http.StatusBadRequest)

		return
	}

	h.Log.Info("email verified by link", "user", userID)

	// Opening the link signs that browser in. The link proves control of the
	// mailbox, which is the same proof the password reset flow accepts, and
	// making somebody sign in again on the phone they just tapped it on is
	// friction with nothing behind it.
	if userFrom(r) == nil {
		user, err := h.Store.UserByID(r.Context(), userID)
		if err == nil {
			h.startSession(w, r, user)
		}
	}

	http.Redirect(w, r, "/sites?welcome=1", http.StatusFound)
}

// showForgot renders the password reset request form.
func (h *Handler) showForgot(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "forgot", h.newPage(r, "Reset your password", ""), http.StatusOK)
}

// doForgot emails a reset link.
//
// The response is identical whether or not the address exists. Saying "no
// account with that address" turns this form into a way to test whether a
// company uses us, and it is a form anybody can submit without signing in.
func (h *Handler) doForgot(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	email := NormaliseEmail(r.PostFormValue("email"))

	p := h.newPage(r, "Reset your password", "")
	p.Flash = "If an account uses " + email + ", a reset link is on its way. It expires in an hour."

	sourceOK := h.Limiter.Allow(ClientKey(r, "reset"), ResetAttempts, ResetWindowLimit)
	subjectOK := h.Limiter.Allow(SubjectKey(email, "reset"), ResetAttempts, ResetWindowLimit)

	// A limited request still gets the same confirmation. Telling the sender
	// they have been limited tells them the address exists.
	if !sourceOK || !subjectOK {
		h.Log.Warn("password reset rate limit reached", "email_hash", HashToken(email)[:12])
		h.render(w, r, "forgot", p, http.StatusOK)

		return
	}

	user, err := h.Store.UserByEmail(r.Context(), email)
	if err == nil {
		token, err := h.Store.CreateReset(r.Context(), user.ID)
		if err != nil {
			h.fail(w, r, err)
			return
		}

		link := h.BaseURL + "/reset-password?token=" + url.QueryEscape(token)

		if err := h.Mailer.SendPasswordReset(r.Context(), user.Email, user.Name, link); err != nil {
			h.Log.Error("could not send the password reset email", "user", user.ID, "error", err)
		}
	} else if !errors.Is(err, ErrNotFound) {
		h.fail(w, r, err)
		return
	}

	h.render(w, r, "forgot", p, http.StatusOK)
}

// showReset renders the new-password form for a valid token.
func (h *Handler) showReset(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")

	p := h.newPage(r, "Choose a new password", "")
	p.Data["Token"] = token
	p.Data["MinLength"] = MinPasswordLength

	if _, err := h.Store.ResetUserID(r.Context(), token); err != nil {
		p.Error = resetErrorMessage(err)
		p.Data["Invalid"] = true

		h.render(w, r, "reset", p, http.StatusBadRequest)

		return
	}

	h.render(w, r, "reset", p, http.StatusOK)
}

// doReset consumes the token and sets the new password.
func (h *Handler) doReset(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	token := r.PostFormValue("token")
	password := r.PostFormValue("password")

	p := h.newPage(r, "Choose a new password", "")
	p.Data["Token"] = token
	p.Data["MinLength"] = MinPasswordLength

	if err := ValidatePassword(password); err != nil {
		p.Error = strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + "."
		h.render(w, r, "reset", p, http.StatusBadRequest)

		return
	}

	userID, err := h.Store.ConsumeReset(r.Context(), token)
	if err != nil {
		p.Error = resetErrorMessage(err)
		p.Data["Invalid"] = true

		h.render(w, r, "reset", p, http.StatusBadRequest)

		return
	}

	// Every session is revoked, including the one submitting this form. A reset
	// is what somebody does when they think another person is in the account,
	// and leaving that other person signed in would defeat the whole exercise.
	if err := h.Store.SetPassword(r.Context(), userID, password, 0); err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Info("password reset", "user", userID)

	if user, err := h.Store.UserByID(r.Context(), userID); err == nil {
		if err := h.Mailer.SendPasswordChanged(r.Context(), user.Email, user.Name); err != nil {
			h.Log.Warn("could not send the password-changed email", "user", userID, "error", err)
		}
	}

	ClearSessionCookie(w, h.BaseURL)
	http.Redirect(w, r, "/login?reset=1", http.StatusFound)
}

// resetErrorMessage turns a token failure into something a person can act on.
func resetErrorMessage(err error) string {
	switch {
	case errors.Is(err, ErrTokenExpired):
		return "That reset link has expired. Ask for a new one."
	case errors.Is(err, ErrTokenUsed):
		return "That reset link has already been used. Ask for a new one."
	default:
		return "That reset link is not valid. Ask for a new one."
	}
}

// setPendingTwoFactor writes the half-finished sign-in cookie.
func (h *Handler) setPendingTwoFactor(w http.ResponseWriter, userID int64, next string) {
	payload := fmt.Sprintf("%d|%d|%s", userID, h.Store.Now().Add(TwoFactorPendingWindow).Unix(), next)

	http.SetCookie(w, &http.Cookie{
		Name:     pendingTwoFactorCookie,
		Value:    h.Sealer.SignedValue(payload),
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(h.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(TwoFactorPendingWindow.Seconds()),
	})
}

// readPendingTwoFactor recovers the half-finished sign-in, or reports that
// there is not one.
func (h *Handler) readPendingTwoFactor(r *http.Request) (userID int64, next string, ok bool) {
	cookie, err := r.Cookie(pendingTwoFactorCookie)
	if err != nil {
		return 0, "", false
	}

	value, ok := h.Sealer.VerifySignedValue(cookie.Value)
	if !ok {
		return 0, "", false
	}

	parts := strings.SplitN(value, "|", 3)
	if len(parts) != 3 {
		return 0, "", false
	}

	var expires int64
	fmt.Sscanf(parts[1], "%d", &expires)

	if expires <= h.Store.Now().Unix() {
		return 0, "", false
	}

	fmt.Sscanf(parts[0], "%d", &userID)

	return userID, safeNext(parts[2]), userID > 0
}

// clearPendingTwoFactor removes the half-finished sign-in cookie.
func (h *Handler) clearPendingTwoFactor(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     pendingTwoFactorCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(h.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// showTwoFactorChallenge renders the code box after a correct password.
func (h *Handler) showTwoFactorChallenge(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.readPendingTwoFactor(r); !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	h.render(w, r, "two_factor", h.newPage(r, "Two-factor code", ""), http.StatusOK)
}

// doTwoFactorChallenge accepts either an authenticator code or a recovery code.
//
// One box takes both. Two boxes means somebody with a recovery code in hand has
// to notice a link, click it, and land on a second form — and they are already
// having the worst day this product can give them.
func (h *Handler) doTwoFactorChallenge(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	userID, next, ok := h.readPendingTwoFactor(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	p := h.newPage(r, "Two-factor code", "")

	if !h.Limiter.Allow(SubjectKey(fmt.Sprint(userID), "2fa"), TwoFactorAttempts, TwoFactorWindow) {
		h.Log.Warn("two-factor rate limit reached", "user", userID)

		p.Error = "Too many attempts. Wait a minute and try again."
		h.render(w, r, "two_factor", p, http.StatusTooManyRequests)

		return
	}

	user, err := h.Store.UserByID(r.Context(), userID)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	code := strings.TrimSpace(r.PostFormValue("code"))

	valid, err := h.Store.VerifyTOTP(h.Sealer, user, code)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	if !valid {
		used, err := h.Store.ConsumeRecoveryCode(r.Context(), user, code)
		if err != nil {
			h.fail(w, r, err)
			return
		}

		if !used {
			p.Error = "That code is not right. Check your authenticator app, or use a recovery code."
			h.render(w, r, "two_factor", p, http.StatusUnauthorized)

			return
		}

		h.Log.Warn("recovery code used", "user", user.ID, "remaining", RecoveryCodesLeft(user)-1)
	}

	h.Limiter.Reset(SubjectKey(fmt.Sprint(userID), "2fa"))
	h.clearPendingTwoFactor(w)
	h.startSession(w, r, user)

	http.Redirect(w, r, next, http.StatusFound)
}

// safeNext sanitises a post-sign-in redirect target.
//
// Only a path on this host is accepted. Taking the value as given turns the
// sign-in page into an open redirect, which is how a phishing link gets to
// carry our domain in front of somebody else's login form.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/sites"
	}

	return next
}

// fail renders the error page and logs what actually happened.
//
// The user is told something went wrong and nothing else. The detail goes to
// the log, because a database error rendered into a page is a description of
// our schema handed to whoever triggered it.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	h.Log.Error("request failed", "path", r.URL.Path, "error", err)

	p := h.newPage(r, "Something went wrong", "")
	p.Error = "Something went wrong on our side. Try again, and tell us if it keeps happening."

	h.render(w, r, "error", p, http.StatusInternalServerError)
}
