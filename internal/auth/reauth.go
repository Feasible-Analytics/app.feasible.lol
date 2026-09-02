//
// reauth.go
// Proving it is really you before a sensitive settings change.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
)

// The three ways a signed-in person can prove the request is theirs. A live
// session alone is never enough for the changes that guard the account — a
// stolen cookie must not be able to set a password, remove the second factor
// or delete everything — so each account gets whichever proof it can give.
const (
	// reauthPassword is the ordinary case: the account has a password.
	reauthPassword = "password"

	// reauthTOTP is a Google-only account with two-factor turned on. The
	// authenticator is the one secret the browser does not hold.
	reauthTOTP = "totp"

	// reauthEmail is a Google-only account with nothing else. A one-time code
	// is emailed, because the mailbox is the one place a stolen session
	// cannot read.
	reauthEmail = "email"
)

// confirmCookie carries the hash of the emailed code between the request that
// sent it and the change that spends it. It is a signed cookie rather than a
// row because it is worthless ten minutes later and proves nothing on its
// own: the code is in the email, not the cookie.
const confirmCookie = "feasible_confirm"

// ConfirmWindow is how long an emailed confirmation code stays usable. Long
// enough to open the inbox, short enough that a code left in it is not a
// standing key to the settings.
const ConfirmWindow = 10 * time.Minute

// ConfirmDigits is the length of the emailed code. Six digits with the attempt
// limit below is one guess in two hundred thousand per window.
const ConfirmDigits = 6

// reauthMode reports which proof the settings forms ask this person for.
func reauthMode(user *User) string {
	switch {
	case user.PasswordHash != "":
		return reauthPassword
	case user.TwoFactorEnabled():
		return reauthTOTP
	default:
		return reauthEmail
	}
}

// reauthenticate checks the proof a settings form carries, and reports the
// catalogue id of the message to show when it does not hold up.
//
// The password field is read under either of the two names the forms use, so
// the change-password form can keep calling it the current password. A
// successful email confirmation clears its cookie, which is what makes the
// code single-use.
func (h *Handler) reauthenticate(w http.ResponseWriter, r *http.Request, user *User) (bool, string) {
	switch reauthMode(user) {
	case reauthPassword:
		password := r.PostFormValue("current_password")
		if password == "" {
			password = r.PostFormValue("password")
		}

		if !CheckPassword(user.PasswordHash, password) {
			return false, "auth.error.password"
		}

		return true, ""

	case reauthTOTP:
		if !h.Limiter.Allow(SubjectKey(strconv.FormatInt(user.ID, 10), "reauth"), TwoFactorAttempts, TwoFactorWindow) {
			return false, "auth.error.too_many_attempts"
		}

		valid, err := h.Store.VerifyTOTP(r.Context(), h.Sealer, user, r.PostFormValue("code"))
		if err != nil || !valid {
			return false, "auth.error.totp_code"
		}

		return true, ""

	default:
		if !h.Limiter.Allow(SubjectKey(strconv.FormatInt(user.ID, 10), "reauth"), TwoFactorAttempts, TwoFactorWindow) {
			return false, "auth.error.too_many_attempts"
		}

		if !h.checkConfirmation(r, user.ID, r.PostFormValue("code")) {
			return false, "auth.error.confirm_code"
		}

		h.clearConfirmation(w)

		return true, ""
	}
}

// doSendConfirmation emails a one-time code to the signed-in address and
// remembers its hash in a signed cookie, then returns to the form that asked.
//
// Sends are limited per account. The button is on a page only the account
// holder can see, but an unlimited one is still a way to fill your own inbox
// by accident and, with a stolen session, somebody else's on purpose.
func (h *Handler) doSendConfirmation(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	user := userFrom(r)
	next := safeNext(r.PostFormValue("next"))

	if !h.Limiter.Allow(SubjectKey(strconv.FormatInt(user.ID, 10), "confirm-send"), ResetAttempts, ResetWindowLimit) {
		http.Redirect(w, r, withQuery(next, "confirm", "limited"), http.StatusFound)
		return
	}

	code, err := numericCode(ConfirmDigits)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	if err := h.sendConfirmationCode(r, user, code); err != nil {
		h.fail(w, r, err)
		return
	}

	expires := h.Store.Now().Add(ConfirmWindow).Unix()

	http.SetCookie(w, &http.Cookie{
		Name:     confirmCookie,
		Value:    h.Sealer.SignedValue(fmt.Sprintf("%d|%d|%s", user.ID, expires, HashToken(code))),
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(h.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ConfirmWindow.Seconds()),
	})

	http.Redirect(w, r, withQuery(next, "confirm", "sent"), http.StatusFound)
}

// sendConfirmationCode writes and sends the email. The code goes ahead of the
// expiry so a reader, or a mail client that highlights numbers, sees it first.
func (h *Handler) sendConfirmationCode(r *http.Request, user *User, code string) error {
	content := mail.Content{
		Subject: "Your feasible.lol confirmation code",
		Heading: "Confirm it is you",
		Body: []string{
			"Somebody signed in to your feasible.lol account asked to change a security setting. Enter this code to confirm it was you:",
			code,
			"The code expires in ten minutes. If you did not ask for it, sign out of every other device from the sessions screen.",
		},
		Closing: "You are receiving this because your account signs in with Google and has no password to ask for.",
	}

	message, err := content.Message(user.Email, "settings_confirmation")
	if err != nil {
		return err
	}

	_, err = h.Mailer.Send(r.Context(), message)

	return err
}

// checkConfirmation compares a typed code against the cookie's hash.
func (h *Handler) checkConfirmation(r *http.Request, userID int64, code string) bool {
	cookie, err := r.Cookie(confirmCookie)
	if err != nil {
		return false
	}

	value, ok := h.Sealer.VerifySignedValue(cookie.Value)
	if !ok {
		return false
	}

	parts := strings.SplitN(value, "|", 3)
	if len(parts) != 3 {
		return false
	}

	owner, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || owner != userID {
		return false
	}

	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || expires <= h.Store.Now().Unix() {
		return false
	}

	code = strings.TrimSpace(strings.ReplaceAll(code, " ", ""))

	return code != "" && constantTimeEqual(parts[2], HashToken(code))
}

// clearConfirmation removes the cookie once its code has been spent.
func (h *Handler) clearConfirmation(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     confirmCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(h.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// withQuery adds one parameter to a local path that may already carry a query.
func withQuery(path, key, value string) string {
	parsed, err := url.Parse(path)
	if err != nil {
		return path
	}

	query := parsed.Query()
	query.Set(key, value)
	parsed.RawQuery = query.Encode()

	return parsed.String()
}
