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

	"github.com/Feasible-Analytics/app.feasible.lol/internal/slack"
)

// SignupMethodPassword and SignupMethodGoogle name the two ways an account can
// come into existence. They are on the notice because the two funnels convert
// differently and the difference is invisible once the account exists.
const (
	SignupMethodPassword = "password"
	SignupMethodGoogle   = "google"
)

// announceSignup posts a new account to Slack.
//
// The referral is passed in rather than read here, because the caller has
// already stored it and the message must say the same thing the row does.
func (h *Handler) announceSignup(user *User, teamID int64, method string, referral Referral) {
	if h == nil || !h.Slack.Enabled() || user == nil {
		return
	}

	h.Slack.SignedUp(slack.Signup{
		Name:     user.Name,
		Email:    user.Email,
		TeamID:   teamID,
		Method:   method,
		Referral: slackReferral(referral),
	})
}

// announceGoogleSignup posts an account created through federated sign-in.
//
// The team is looked up rather than passed because the federated path resolves
// a profile into a user and never sees the team that was created alongside it.
// A failed lookup still sends the notice: knowing somebody signed up matters
// more than the link, and a signup that vanishes because of a second query is
// the wrong trade.
func (h *Handler) announceGoogleSignup(ctx context.Context, user *User, referral Referral) {
	if h == nil || !h.Slack.Enabled() || user == nil {
		return
	}

	var teamID int64
	if h.Store != nil {
		if team, err := h.Store.TeamForUser(ctx, user.ID); err == nil && team != nil {
			teamID = team.ID
		}
	}

	h.announceSignup(user, teamID, SignupMethodGoogle, referral)
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

// slackReferral turns a stored first touch into the shape the chat notice
// speaks. The two are separate types because one is a database row and the
// other is a message, and a change to either must not silently reshape the
// other.
func slackReferral(referral Referral) slack.Referral {
	return slack.Referral{
		Referrer: referral.Referrer,
		Source:   referral.Source,
		Medium:   referral.Medium,
		Campaign: referral.Campaign,
		Landing:  referral.LandingPage,
	}
}
