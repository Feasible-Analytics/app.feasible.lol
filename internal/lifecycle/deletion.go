//
// deletion.go
// Day 90: the database file, the control rows and the payment customer, irreversibly.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package lifecycle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// CustomerRemover deletes an account's record at the payment provider. It is an
// interface so this package does not depend on the Stripe client, and so a test
// can assert the call was made without a network.
type CustomerRemover interface {
	DeleteCustomer(ctx context.Context, customerID string) error
}

// Purger performs the day-90 deletion. It is a type rather than a function
// because the order of its five steps matters, and because that order is the
// part somebody reading this in a year needs to understand.
type Purger struct {
	Store *Store

	// Accounts owns the open database handles. The file cannot be removed while
	// this process still holds a writer on it, and on some filesystems removing
	// it anyway leaves a handle writing to an unlinked inode until the process
	// exits.
	Accounts *accounts.Manager

	// DataDir is where the account directories live.
	DataDir string

	// Customers removes the payment provider's record. Nil is allowed: a
	// self-hosted install has no payment provider, and deletion must still work.
	Customers CustomerRemover

	Log *logger.Logger
}

// Purge destroys everything belonging to one account.
//
// The order is deliberate and each step is idempotent, because a crash between
// any two of them must be resumable by simply running it again:
//
//  1. Record the deletion in a table with no foreign key, capturing the contact
//     address. Everything else about the account is about to cease to exist,
//     including every row that could tell us who to write to.
//  2. Mark the clock deleted. From this moment the sweeper skips the account,
//     so a failure below cannot leave it half-deleted and still ticking.
//  3. Remove the payment provider's customer, which takes the stored card with
//     it.
//  4. Close the database handle and remove the account directory: the analytics
//     database, its WAL, and everything beside it.
//  5. Delete the team row, whose cascade removes the sites, memberships, API
//     keys, shared links, usage counters and lifecycle rows.
//
// There is no soft delete and no recycle bin. Keeping one would mean the
// deletion we spent ninety days warning somebody about was not real.
func (p *Purger) Purge(ctx context.Context, account Account, now time.Time) error {
	db := p.Store.DB()

	claimed, err := p.claim(ctx, account, now)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	if account.State.DeletedAt.IsZero() {
		account.State.DeletedAt = now.UTC()
	}

	// A rerun after a crash finds the team already gone. Everything below is
	// idempotent, but the clock row cascaded away with the team, so writing it
	// again would fail on a foreign key and stop the resume dead.
	live, err := p.teamExists(ctx, account.TeamID)
	if err != nil {
		return err
	}

	var steps []string

	if live && p.Customers != nil && account.CustomerID != "" {
		if err := p.Customers.DeleteCustomer(ctx, account.CustomerID); err != nil {
			// A payment provider that will not answer must not stop us
			// destroying the data we hold. The failure is recorded so somebody
			// can finish the job by hand rather than discovering years later
			// that a card is still on file.
			steps = append(steps, "payment customer NOT removed: "+err.Error())

			if p.Log != nil {
				p.Log.Error("could not delete the payment provider's customer — remove it by hand",
					"team", account.TeamID, "customer", account.CustomerID, "error", err)
			}
		} else {
			steps = append(steps, "payment customer "+account.CustomerID+" removed")
		}
	}

	if p.Accounts != nil {
		if err := p.Accounts.Close(account.TeamID); err != nil {
			return fmt.Errorf("lifecycle: close account %d before deleting it: %w", account.TeamID, err)
		}
	}

	dir := accounts.Dir(p.DataDir, account.TeamID)

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("lifecycle: remove %s: %w", dir, err)
	}
	steps = append(steps, "database directory "+dir+" removed")

	// Deleting the team cascades to every other table that references it. The
	// cascade is why this is one statement rather than fifteen, and why a table
	// added later without a foreign key would silently survive a deletion.
	if _, err := db.ExecContext(ctx, `DELETE FROM teams WHERE id = ?`, account.TeamID); err != nil {
		return fmt.Errorf("lifecycle: delete team %d: %w", account.TeamID, err)
	}
	steps = append(steps, "control rows removed")

	// Only the run that did the work writes the notes. A resume after a crash
	// would otherwise replace the record of what the first attempt managed with
	// the much shorter list of what the second one found left to do.
	if _, err := db.ExecContext(ctx, `
		UPDATE account_deletions
		SET completed_at = COALESCE(completed_at, ?),
		    notes        = CASE WHEN notes = '' THEN ? ELSE notes END
		WHERE team_id = ?
	`, now.UTC().Unix(), strings.Join(steps, "; "), account.TeamID); err != nil {
		return fmt.Errorf("lifecycle: finish deletion record %d: %w", account.TeamID, err)
	}

	if p.Log != nil {
		p.Log.Warn("account permanently deleted",
			"team", account.TeamID, "name", account.TeamName,
			"clock_started", account.State.StartedAt.Format(time.RFC3339), "steps", strings.Join(steps, "; "))
	}

	return nil
}

// claim atomically revalidates the exact running clock and writes the durable
// audit row before destruction starts. A concurrent payment either clears the
// clock first and makes this a no-op, or observes deleted_at and cannot revive
// an account whose deletion has begun.
func (p *Purger) claim(ctx context.Context, account Account, now time.Time) (bool, error) {
	db := p.Store.DB()

	// A clock already marked deleted with an unfinished audit row is a crash
	// recovery, not a new claim. The remaining idempotent steps must resume.
	if account.State.Deleted() {
		var pending int
		err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM account_deletions
			WHERE team_id = ? AND completed_at IS NULL
		`, account.TeamID).Scan(&pending)
		if err != nil {
			return false, fmt.Errorf("lifecycle: inspect deletion %d: %w", account.TeamID, err)
		}

		return pending == 1, nil
	}

	if !account.State.DueForDeletion(now) {
		return false, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("lifecycle: claim deletion %d: %w", account.TeamID, err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	result, err := tx.ExecContext(ctx, `
		UPDATE account_lifecycle
		SET deleted_at = ?, updated_at = ?
		WHERE team_id = ? AND trigger = ? AND started_at IS ? AND deleted_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM subscriptions
		      WHERE subscriptions.team_id = account_lifecycle.team_id
		        AND subscriptions.payment_state = 'paid'
		  )
	`, now.UTC().Unix(), now.UTC().Unix(), account.TeamID,
		string(account.State.Trigger), toUnix(account.State.StartedAt))
	if err != nil {
		return false, fmt.Errorf("lifecycle: claim deletion %d: %w", account.TeamID, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("lifecycle: claim deletion %d: affected rows: %w", account.TeamID, err)
	}
	if rows != 1 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO account_deletions
			(team_id, team_name, contact_email, stripe_customer_id, clock_started_at, started_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (team_id) DO NOTHING
	`, account.TeamID, account.TeamName, account.Email, account.CustomerID,
		account.State.StartedAt.UTC().Unix(), now.UTC().Unix()); err != nil {
		return false, fmt.Errorf("lifecycle: record deletion %d: %w", account.TeamID, err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("lifecycle: claim deletion %d: %w", account.TeamID, err)
	}

	return true, nil
}

// teamExists reports whether the account's control rows are still there. It is
// how a resumed deletion tells "nothing has happened yet" from "the cascade
// already ran", which decides whether the clock row can still be written.
func (p *Purger) teamExists(ctx context.Context, teamID int64) (bool, error) {
	var one int

	err := p.Store.DB().QueryRowContext(ctx, `SELECT 1 FROM teams WHERE id = ?`, teamID).Scan(&one)

	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lifecycle: check team %d: %w", teamID, err)
	}

	return true, nil
}

// Deletion is a completed destruction, read back for the confirmation email and
// for anybody asking what happened to an account that is no longer there.
type Deletion struct {
	TeamID     int64
	TeamName   string
	Email      string
	StartedAt  time.Time
	DeletedAt  time.Time
	Notified   bool
	Notes      string
	CustomerID string
}

// PendingConfirmations lists deletions whose confirmation email has not gone
// out. It is a separate step from the deletion itself so that a mail relay being
// down cannot stop the data being destroyed on the day we promised.
func (p *Purger) PendingConfirmations(ctx context.Context) ([]Deletion, error) {
	rows, err := p.Store.DB().QueryContext(ctx, `
		SELECT team_id, team_name, contact_email, stripe_customer_id, clock_started_at, completed_at, notes
		FROM account_deletions
		WHERE completed_at IS NOT NULL AND notified_at IS NULL AND contact_email <> ''
		ORDER BY team_id
	`)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: pending confirmations: %w", err)
	}
	defer rows.Close()

	var out []Deletion

	for rows.Next() {
		var (
			entry     Deletion
			started   int64
			completed sql.NullInt64
		)

		if err := rows.Scan(&entry.TeamID, &entry.TeamName, &entry.Email, &entry.CustomerID, &started, &completed, &entry.Notes); err != nil {
			return nil, fmt.Errorf("lifecycle: pending confirmations: %w", err)
		}

		entry.StartedAt = time.Unix(started, 0).UTC()
		entry.DeletedAt = fromUnix(completed)

		out = append(out, entry)
	}

	return out, rows.Err()
}

// MarkNotified records that the confirmation went out, which is what makes that
// last email idempotent once every other row for the account has gone.
func (p *Purger) MarkNotified(ctx context.Context, teamID int64, at time.Time, outcome string) error {
	_, err := p.Store.DB().ExecContext(ctx, `
		UPDATE account_deletions
		SET notified_at = ?, notes = notes || '; confirmation ' || ?
		WHERE team_id = ? AND notified_at IS NULL
	`, at.UTC().Unix(), outcome, teamID)
	if err != nil {
		return fmt.Errorf("lifecycle: mark notified %d: %w", teamID, err)
	}

	return nil
}
