//
// notices.go
// The commercial events this package announces to our own Slack.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/slack"
)

// SignupMethodPassword and SignupMethodGoogle name the two ways an account can
// come into existence. They are on the notice because the two funnels convert
// differently and the difference is invisible once the account exists.
const (
	SignupMethodPassword = "password"
	SignupMethodGoogle   = "google"
)

// maxReferralField bounds what we copy out of a request into a chat message. A
// referrer is attacker-controlled text on a public form, and a megabyte of it
// in Slack helps nobody.
const maxReferralField = 200

// announceSignup posts a new account to Slack.
//
// It takes the request because everything we know about where somebody came
// from is on it and nowhere else: we persist no attribution today, so the
// notice is the only place this ever appears.
func (h *Handler) announceSignup(r *http.Request, user *User, teamID int64, method string) {
	if h == nil || !h.Slack.Enabled() || user == nil {
		return
	}

	h.Slack.SignedUp(slack.Signup{
		Name:     user.Name,
		Email:    user.Email,
		TeamID:   teamID,
		Method:   method,
		Referral: signupReferral(r),
	})
}

// announceGoogleSignup posts an account created through federated sign-in.
//
// The team is looked up rather than passed because the federated path resolves
// a profile into a user and never sees the team that was created alongside it.
// A failed lookup still sends the notice: knowing somebody signed up matters
// more than the link, and a signup that vanishes because of a second query is
// the wrong trade.
func (h *Handler) announceGoogleSignup(ctx context.Context, r *http.Request, user *User) {
	if h == nil || !h.Slack.Enabled() || user == nil {
		return
	}

	var teamID int64
	if h.Store != nil {
		if team, err := h.Store.TeamForUser(ctx, user.ID); err == nil && team != nil {
			teamID = team.ID
		}
	}

	h.announceSignup(r, user, teamID, SignupMethodGoogle)
}

// announceClosure posts an owner deleting their account. It is called before
// the deletion runs, because afterwards there is no name or address left to
// put in the message.
func (h *Handler) announceClosure(user *User, teamID int64) {
	if h == nil || !h.Slack.Enabled() || user == nil {
		return
	}

	h.Slack.AccountClosed(slack.Closure{
		TeamID: teamID,
		Name:   user.Name,
		Email:  user.Email,
	})
}

// signupReferral reads what the signup request itself carried.
//
// This is thin on purpose rather than by oversight. Nothing in this product
// stores where an account came from, so there is no first-touch source to look
// up — only the header the browser sent and whatever campaign parameters
// survived onto the URL that was posted. It reports what it found and nothing
// more, so an empty result reads as "we do not know" rather than as "direct".
func signupReferral(r *http.Request) slack.Referral {
	if r == nil {
		return slack.Referral{}
	}

	referral := slack.Referral{Referrer: clipField(r.Referer())}

	query := r.URL.Query()

	// The form is posted to a URL that may have carried the parameters through,
	// and a hidden field is the other place they can arrive. Neither is
	// guaranteed, so both are read and the first non-empty one wins.
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

	// Our own pages are not a referral. Somebody arriving at the form from the
	// pricing page came from wherever they were before that, and reporting the
	// last hop instead would make every signup look self-referred.
	if sameHost(referral.Referrer, r.Host) {
		referral.Referrer = ""
	}

	return referral
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

// clipField bounds one copied value and strips the control characters that
// would let a crafted referrer forge extra lines in the chat message.
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
