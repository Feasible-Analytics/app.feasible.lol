//
// delete.go
// Owner-requested entrypoint into the durable permanent-deletion workflow.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// PermanentAccountDeleter is the durable lifecycle purger used by both the
// scheduled day-90 path and an owner's immediate settings request.
type PermanentAccountDeleter interface {
	DeleteNow(ctx context.Context, userID, teamID int64, now time.Time) error
}

// Deleter removes an account and everything belonging to it.
type Deleter struct {
	purger PermanentAccountDeleter
	log    *logger.Logger

	// Now is injectable so authorization/deletion race tests can assert the
	// exact durable audit timestamp without sleeping.
	Now func() time.Time
}

// NewDeleter wires up the pieces account deletion touches.
func NewDeleter(purger PermanentAccountDeleter, log *logger.Logger) *Deleter {
	return &Deleter{purger: purger, log: log}
}

// DeleteAccount removes a team, its owner and every trace of both.
//
// It really deletes. A privacy product whose "delete my account" leaves a
// hidden row and an orphaned database file has no honest answer to "what do you
// still hold about me", and the answer has to be "nothing" rather than "nothing
// you can see".
func (d *Deleter) DeleteAccount(ctx context.Context, userID, teamID int64) error {
	if d == nil || d.purger == nil {
		return fmt.Errorf("auth: permanent deletion is not configured")
	}
	now := time.Now().UTC()
	if d.Now != nil {
		now = d.Now().UTC()
	}
	if err := d.purger.DeleteNow(ctx, userID, teamID, now); err != nil {
		return err
	}

	if d.log != nil {
		d.log.Info("account deleted through durable lifecycle workflow", "team", teamID, "user", userID)
	}

	return nil
}
