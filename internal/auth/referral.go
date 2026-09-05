//
// referral.go
// First touch: where somebody came from, carried to the day they sign up.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// referralCookie carries first touch until the account exists.
//
// It is first-party, holds nothing about a person, and never leaves this
// deployment. The alternative is recording the referrer of the registration
// POST, which is always our own form — every account would look self-referred
// and the real answer would be lost.
const referralCookie = "fs_ref"

// ReferralWindow is how long a first touch is still the reason somebody signed
// up. Long enough for the read-about-it-then-think-about-it path this product
// actually gets, short enough that a visit last quarter is not credited for a
// decision made today.
const ReferralWindow = 30 * 24 * time.Hour

// maxReferralField bounds every stored and reported value. These arrive on a
// public URL, so they are attacker-controlled text that ends up in a database
// row and a chat message.
const maxReferralField = 200

// Referral is where one person came from. Every field is optional: most
// browsers send no referrer, and most visits carry no campaign.
type Referral struct {
	Referrer    string `json:"r,omitempty"`
	Source      string `json:"s,omitempty"`
	Medium      string `json:"m,omitempty"`
	Campaign    string `json:"c,omitempty"`
	LandingPage string `json:"l,omitempty"`

	// FirstSeenAt is when they first arrived, in unix seconds. The gap between
	// it and the account's creation is how long they thought about it.
	FirstSeenAt int64 `json:"t,omitempty"`
}

// Empty reports whether we learned nothing at all. A recorded landing page
// counts: it is the difference between "they came directly" and "we do not
// know", and those are not the same answer.
func (r Referral) Empty() bool {
	return r.Referrer == "" && r.Source == "" && r.Medium == "" &&
		r.Campaign == "" && r.LandingPage == ""
}

// recordFirstTouch remembers where this visitor came from, once.
//
// It runs on the first page anybody loads and never again, because first touch
// is the question. A visitor who already carries the cookie, or who is already
// signed in, has either been answered or is not a prospect.
func (h *Handler) recordFirstTouch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		return
	}

	// A page load, not an asset or a beacon. Anything else would overwrite
	// nothing but would set a cookie on requests that are not a person
	// arriving.
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		return
	}

	if _, err := r.Cookie(referralCookie); err == nil {
		return
	}

	// Somebody signed in is not arriving for the first time. Their attribution,
	// if we ever captured it, was written when they registered.
	if _, err := r.Cookie(SessionCookieName); err == nil {
		return
	}

	touch := requestReferral(r)
	touch.LandingPage = clipField(r.URL.Path)
	touch.FirstSeenAt = time.Now().UTC().Unix()

	encoded, err := encodeReferral(touch)
	if err != nil {
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     referralCookie,
		Value:    encoded,
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(h.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ReferralWindow.Seconds()),
	})
}

// clearFirstTouch drops the cookie once its contents have been stored. Keeping
// it would credit a second account created from the same browser to the same
// first touch, which is a different visit.
func clearFirstTouch(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     referralCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// signupReferral is what to store against a new account.
//
// The cookie is preferred because it is the visit that actually sent them. The
// live request is the fallback for somebody who registered on the same page
// load that brought them, or whose browser refused the cookie.
func signupReferral(r *http.Request) Referral {
	if r == nil {
		return Referral{}
	}

	if cookie, err := r.Cookie(referralCookie); err == nil {
		if touch, err := decodeReferral(cookie.Value); err == nil && !touch.Empty() {
			if touch.FirstSeenAt == 0 ||
				time.Since(time.Unix(touch.FirstSeenAt, 0)) <= ReferralWindow {
				return touch
			}
		}
	}

	touch := requestReferral(r)
	if touch.Empty() {
		return Referral{}
	}

	touch.LandingPage = clipField(r.URL.Path)
	touch.FirstSeenAt = time.Now().UTC().Unix()

	return touch
}

// requestReferral reads what one request carries about where it came from.
func requestReferral(r *http.Request) Referral {
	referral := Referral{Referrer: clipField(r.Referer())}

	query := r.URL.Query()

	// A tagged link puts the parameters on the URL; a form can carry them
	// forward in a hidden field. Both are read, and the first non-empty wins.
	pick := func(names ...string) string {
		for _, name := range names {
			if value := strings.TrimSpace(query.Get(name)); value != "" {
				return clipField(value)
			}
			if value := strings.TrimSpace(r.PostFormValue(name)); value != "" {
				return clipField(value)
			}
		}

		return ""
	}

	referral.Source = pick("utm_source", "ref", "via")
	referral.Medium = pick("utm_medium")
	referral.Campaign = pick("utm_campaign")

	// Our own pages are not a referral. Reporting the last hop would make every
	// signup look self-referred and hide whoever actually sent them.
	if sameHost(referral.Referrer, r.Host) {
		referral.Referrer = ""
	}

	return referral
}

// encodeReferral packs a first touch into a cookie value.
func encodeReferral(referral Referral) (string, error) {
	payload, err := json.Marshal(referral)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(payload), nil
}

// decodeReferral reads a cookie value back.
//
// The value is not signed. There is nothing to forge: the only thing a visitor
// can do by editing it is lie about where they came from, which they can do
// anyway by sending any referrer they like. Every field is clipped again on the
// way out, because the length limit is what protects the row and the chat
// message and this is the path that skipped it.
func decodeReferral(value string) (Referral, error) {
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return Referral{}, err
	}

	var referral Referral
	if err := json.Unmarshal(payload, &referral); err != nil {
		return Referral{}, err
	}

	referral.Referrer = clipField(referral.Referrer)
	referral.Source = clipField(referral.Source)
	referral.Medium = clipField(referral.Medium)
	referral.Campaign = clipField(referral.Campaign)
	referral.LandingPage = clipField(referral.LandingPage)

	return referral, nil
}

// sameHost reports whether a referrer points back at this deployment.
func sameHost(referrer, host string) bool {
	if referrer == "" || host == "" {
		return false
	}

	parsed, err := url.Parse(referrer)
	if err != nil {
		return false
	}

	return strings.EqualFold(parsed.Host, host)
}

// clipField bounds one value and strips the control characters that would let a
// crafted referrer forge extra lines in a chat message.
func clipField(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}

		return r
	}, strings.TrimSpace(value))

	if len(value) > maxReferralField {
		return value[:maxReferralField] + "…"
	}

	return value
}

// SaveReferral records where one account came from.
//
// It is a separate write rather than part of the account transaction. Losing an
// attribution row is a gap in a report; failing a registration over one would
// cost the customer the thing they were trying to do.
func (s *Store) SaveReferral(ctx context.Context, userID int64, referral Referral) error {
	if referral.Empty() {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_referrals
			(user_id, referrer, source, medium, campaign, landing_page, first_seen_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO NOTHING`,
		userID, referral.Referrer, referral.Source, referral.Medium,
		referral.Campaign, referral.LandingPage, referral.FirstSeenAt,
		s.Now().UTC().Unix(),
	)

	return err
}

// Referral reads back where one account came from. It is here for the reports
// this table exists to make answerable, and for the tests.
func (s *Store) Referral(ctx context.Context, userID int64) (Referral, bool, error) {
	var referral Referral

	err := s.db.QueryRowContext(ctx, `
		SELECT referrer, source, medium, campaign, landing_page, first_seen_at
		FROM user_referrals WHERE user_id = ?`, userID).
		Scan(&referral.Referrer, &referral.Source, &referral.Medium,
			&referral.Campaign, &referral.LandingPage, &referral.FirstSeenAt)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Referral{}, false, nil
		}

		return Referral{}, false, err
	}

	return referral, true, nil
}

// captureReferral stores where a brand-new account came from and returns what
// it stored, so the chat notice and the row can never disagree.
//
// A failed write is logged and the registration continues. Losing an
// attribution row is a gap in a report; failing a signup over one would cost
// somebody the thing they came here to do.
func (h *Handler) captureReferral(w http.ResponseWriter, r *http.Request, userID int64) Referral {
	referral := signupReferral(r)

	clearFirstTouch(w, strings.HasPrefix(h.BaseURL, "https://"))

	if referral.Empty() || h.Store == nil {
		return referral
	}

	if err := h.Store.SaveReferral(r.Context(), userID, referral); err != nil && h.Log != nil {
		h.Log.Error("could not store where an account came from", "user", userID, "error", err)
	}

	return referral
}
