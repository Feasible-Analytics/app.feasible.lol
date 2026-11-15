//
// unsubscribe.go
// The one-click way out of the onboarding emails.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package onboarding

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// StopPath is where a token is redeemed. It is the same page a report
// recipient unsubscribes on, so there is one way out of every email.
const StopPath = "/unsubscribe/"

// ListTips is what the unsubscribe page is told is being stopped.
const ListTips = "onboarding_tips"

// claimPrefix marks a token as one of ours, so it can be told apart from a
// report or alert token sealed with the same key.
const claimPrefix = "onboarding"

// Sealer encrypts and reads a token. The implementation is *auth.Sealer.
type Sealer interface {
	SealToken(plaintext string) (string, error)
	OpenToken(token string) (string, error)
}

// Stopper mints and redeems the stop links. The token carries the team, sealed
// with the application key, so nothing is stored until somebody clicks.
type Stopper struct {
	Store   *Store
	Sealer  Sealer
	BaseURL string
	Log     *logger.Logger

	// Now is injectable for tests.
	Now func() time.Time
}

// Link is where one team stops these emails, or empty when no link can be
// made. The sweeper refuses to send without one.
func (u *Stopper) Link(teamID int64) string {
	if u == nil || u.Sealer == nil || teamID < 1 || strings.TrimSpace(u.BaseURL) == "" {
		return ""
	}

	token, err := u.Sealer.SealToken(claimPrefix + "\x00" + strconv.FormatInt(teamID, 10))
	if err != nil {
		if u.Log != nil {
			u.Log.Error("could not mint an onboarding stop link", "team", teamID, "error", err)
		}

		return ""
	}

	return strings.TrimRight(u.BaseURL, "/") + StopPath + url.PathEscape(token)
}

// Owns reports whether a token is an onboarding one, so the caller knows
// whether to answer it here or hand it to the report unsubscriber.
func (u *Stopper) Owns(token string) bool {
	_, ok := u.read(token)

	return ok
}

// Describe says what a token would stop, without stopping it. There is no site
// to name: the emails are about the whole account.
func (u *Stopper) Describe(_ context.Context, token string) (site, list string, ok bool) {
	if _, opened := u.read(token); !opened {
		return "", "", false
	}

	return "", ListTips, true
}

// Remove stops the emails for the token's team. A token that cannot be read
// stops nothing and is not an error, so a second click still sees success.
func (u *Stopper) Remove(ctx context.Context, token string) error {
	teamID, opened := u.read(token)
	if !opened || u.Store == nil {
		return nil
	}

	now := time.Now().UTC()
	if u.Now != nil {
		now = u.Now().UTC()
	}

	stopped, err := u.Store.OptOut(ctx, teamID, now)
	if err != nil {
		return err
	}

	if u.Log != nil && stopped {
		u.Log.Info("an account stopped the onboarding emails", "team", teamID)
	}

	return nil
}

// read opens a token into the team it names.
func (u *Stopper) read(token string) (int64, bool) {
	if u == nil || u.Sealer == nil {
		return 0, false
	}

	plaintext, err := u.Sealer.OpenToken(strings.TrimSpace(token))
	if err != nil {
		return 0, false
	}

	parts := strings.Split(plaintext, "\x00")
	if len(parts) != 2 || parts[0] != claimPrefix {
		return 0, false
	}

	teamID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || teamID < 1 {
		return 0, false
	}

	return teamID, true
}
