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

// PaymentQuiescence is the provider state held while deletion claims the local
// account. Restore is called if the local claim loses or fails; a successful
// claim deliberately leaves collection paused until the customer is removed.
type PaymentQuiescence struct {
	Recovered   bool
	CustomerIDs []string
	Restore     func(context.Context) error
}

// AccountLease is the durable ownership fence shared by billing and deletion.
// Renew must succeed before an irreversible side effect; an expired worker is
// not allowed to continue merely because it still has an in-memory handle.
type AccountLease interface {
	Renew(context.Context) error
	Release()
}

// PaymentCoordinator shares billing's durable account lease with deletion and
// closes provider-side settlement opportunities before local destruction.
type PaymentCoordinator interface {
	AcquireAccountLease(ctx context.Context, teamID int64) (AccountLease, error)
	QuiesceForDeletion(ctx context.Context, lease AccountLease, teamID int64, customerID string, lapseStarted time.Time, recoverSettlement bool) (PaymentQuiescence, error)
}

// Purger performs the day-90 deletion. It is a type rather than a function
// because the order of its steps matters, and because that order is the
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

	// Payments coordinates provider truth and the same durable lease used by
	// checkout and webhook reconciliation. Nil is valid for self-hosted installs.
	Payments PaymentCoordinator

	Log *logger.Logger
}

// Purge destroys everything belonging to one account.
//
// The order is deliberate and each step is idempotent, because a crash between
// any two of them must be resumable by simply running it again:
//
//  1. Lease the account. An explicit owner request records its authorized intent
//     before provider mutation; a scheduled deletion first re-reads settlement.
//  2. Suspend provider-side checkout, subscription, and invoice collection, then
//     record every discovered customer in an audit with no foreign key.
//  3. Mark the clock deleted. From this moment the sweeper skips the account,
//     so a failure below cannot leave it half-deleted and still ticking.
//  4. Delete the team row, whose cascade prevents new authorized account work.
//  5. Tombstone and remove the analytics directory, its WAL, and everything
//     beside it.
//  6. Remove the payment provider's customer and stored card. A provider outage
//     leaves the immutable audit pending until a later sweep finishes this step.
//
// There is no soft delete and no recycle bin. Keeping one would mean the
// deletion we spent ninety days warning somebody about was not real.
func (p *Purger) Purge(ctx context.Context, account Account, now time.Time) error {
	return p.purge(ctx, account, now, 0, false)
}

// DeleteNow runs the same durable, resumable deletion workflow for an explicit
// owner request. The owner id is revalidated in the deletion-claim transaction,
// so a stale settings page cannot delete an account after ownership changes.
func (p *Purger) DeleteNow(ctx context.Context, userID, teamID int64, now time.Time) error {
	account, err := p.Store.AccountForDeletion(ctx, teamID)
	if err != nil {
		return err
	}

	return p.purge(ctx, account, now, userID, true)
}

// purge owns the shared scheduled and owner-requested deletion sequence.
// Force changes only the claim policy: provider quiescence and every durable
// cleanup checkpoint remain identical in both paths.
func (p *Purger) purge(ctx context.Context, account Account, now time.Time, userID int64, force bool) error {
	requestedNow := force
	pending, completed, claimedAt, ownerRequested, err := p.deletionStatus(ctx, account.TeamID)
	if err != nil {
		return err
	}
	if completed {
		return nil
	}
	if pending {
		account.State.DeletedAt = claimedAt
		force = ownerRequested
	}
	live, err := p.teamExists(ctx, account.TeamID)
	if err != nil {
		return err
	}

	var quiescence PaymentQuiescence
	var lease AccountLease
	if live && p.Payments != nil {
		lease, err = p.Payments.AcquireAccountLease(ctx, account.TeamID)
		if err != nil {
			return err
		}
		defer lease.Release()
	}
	if pending && requestedNow && !ownerRequested {
		authorized, err := p.promotePendingOwnerIntent(ctx, userID, account.TeamID)
		if err != nil {
			return err
		}
		if !authorized {
			return fmt.Errorf("lifecycle: account %d deletion requester is no longer owner", account.TeamID)
		}
		force = true
	}

	var transitionLease *TransitionLease
	if force && !account.State.Deleted() {
		transitionLease, err = p.Store.AcquireTransitionLease(ctx, account.TeamID)
		if err != nil {
			return err
		}
		defer transitionLease.Release()

		if lease != nil {
			if err := lease.Renew(ctx); err != nil {
				return err
			}
		}
		claimed, err := p.claim(ctx, account, now, nil, userID, true)
		if err != nil {
			return err
		}
		if !claimed {
			return fmt.Errorf("lifecycle: account %d deletion claim lost or requester is no longer owner", account.TeamID)
		}
		account.State.DeletedAt = now.UTC()
	}

	if live && p.Payments != nil {
		quiescence, err = p.Payments.QuiesceForDeletion(
			ctx, lease, account.TeamID, account.CustomerID, account.State.StartedAt, !force,
		)
		if err != nil {
			return err
		}
		if quiescence.Recovered {
			if pending {
				return p.cancelRecoveredDeletion(ctx, account.TeamID, now)
			}
			return nil
		}
	}
	if lease != nil {
		if err := lease.Renew(ctx); err != nil {
			return err
		}
	}
	if !account.State.Deleted() {
		transitionLease, err = p.Store.AcquireTransitionLease(ctx, account.TeamID)
		if err != nil {
			if quiescence.Restore != nil {
				_ = quiescence.Restore(ctx)
			}
			return err
		}
		defer transitionLease.Release()
	}

	claimed, err := p.claim(ctx, account, now, quiescence.CustomerIDs, userID, force)
	if err != nil {
		if quiescence.Restore != nil {
			_ = quiescence.Restore(ctx)
		}
		return err
	}
	if !claimed {
		if quiescence.Restore != nil {
			if err := quiescence.Restore(ctx); err != nil {
				return err
			}
		}
		if force {
			return fmt.Errorf("lifecycle: account %d deletion claim lost or requester is no longer owner", account.TeamID)
		}
		return nil
	}
	if account.State.DeletedAt.IsZero() {
		account.State.DeletedAt = now.UTC()
	}

	// A rerun after a crash can find the team already gone. Everything below is
	// idempotent, but the clock row cascaded away with the team, so writing it
	// again would fail on a foreign key and stop the resume dead.
	live, err = p.teamExists(ctx, account.TeamID)
	if err != nil {
		return err
	}

	if live {
		if err := p.removeControl(ctx, account.TeamID, now); err != nil {
			return err
		}
	} else if err := p.markDeletionStep(ctx, account.TeamID, "control_removed_at", now, "control rows removed"); err != nil {
		return err
	}

	manager := p.Accounts
	if manager == nil {
		manager = accounts.NewManager(p.DataDir)
	}
	if err := manager.Delete(account.TeamID); err != nil {
		return fmt.Errorf("lifecycle: delete account database %d: %w", account.TeamID, err)
	}
	if err := p.markDeletionStep(ctx, account.TeamID, "local_removed_at", now,
		"database directory "+accounts.Dir(p.DataDir, account.TeamID)+" removed"); err != nil {
		return err
	}

	customers, err := p.pendingPaymentCustomers(ctx, account.TeamID)
	if err != nil {
		return err
	}
	customerCount, err := p.paymentCustomerCount(ctx, account.TeamID)
	if err != nil {
		return err
	}
	if len(customers) > 0 && p.Customers == nil {
		return fmt.Errorf("lifecycle: account %d still has %d payment customers but no customer remover is configured", account.TeamID, len(customers))
	}
	for _, customerID := range customers {
		if err := p.Customers.DeleteCustomer(ctx, customerID); err != nil {
			recordErr := p.recordPaymentCustomerFailure(ctx, account.TeamID, customerID, err)
			if p.Log != nil {
				p.Log.Error("could not delete the payment provider's customer; deletion will retry",
					"team", account.TeamID, "customer", customerID, "error", err)
			}
			if recordErr != nil {
				return fmt.Errorf("lifecycle: delete payment customer %s: %w; could not checkpoint failure: %v", customerID, err, recordErr)
			}
			return fmt.Errorf("lifecycle: delete payment customer %s: %w", customerID, err)
		}
		if err := p.markPaymentCustomerRemoved(ctx, account.TeamID, customerID, now); err != nil {
			return err
		}
	}
	providerNote := "no payment customer existed"
	if customerCount > 0 {
		providerNote = fmt.Sprintf("%d payment customer(s) removed", customerCount)
	}
	if err := p.markDeletionStep(ctx, account.TeamID, "provider_removed_at", now, providerNote); err != nil {
		return err
	}

	if err := p.finishControl(ctx, account.TeamID, now); err != nil {
		return err
	}

	if p.Log != nil {
		p.Log.Warn("account permanently deleted",
			"team", account.TeamID, "name", account.TeamName,
			"clock_started", account.State.StartedAt.Format(time.RFC3339))
	}

	return nil
}

// promotePendingOwnerIntent transactionally converts an older scheduled claim
// into an explicit owner request before provider work. Membership revocation and
// this update serialize in SQLite, so exactly one authorization state wins.
func (p *Purger) promotePendingOwnerIntent(ctx context.Context, userID, teamID int64) (bool, error) {
	lease, err := p.Store.AcquireTransitionLease(ctx, teamID)
	if err != nil {
		return false, err
	}
	defer lease.Release()

	result, err := p.Store.DB().ExecContext(ctx, `
		UPDATE account_deletions
		SET owner_requested = 1
		WHERE team_id = ? AND completed_at IS NULL AND owner_requested = 0
		  AND EXISTS (
		      SELECT 1 FROM team_memberships
		      WHERE team_memberships.team_id = account_deletions.team_id
		        AND team_memberships.user_id = ?
		        AND team_memberships.role = 'owner'
		  )
	`, teamID, userID)
	if err != nil {
		return false, fmt.Errorf("lifecycle: authorize pending owner deletion %d: %w", teamID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("lifecycle: authorize pending owner deletion %d: affected rows: %w", teamID, err)
	}
	if rows == 1 {
		return true, nil
	}

	var alreadyAuthorized int
	if err := p.Store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM account_deletions
		WHERE team_id = ? AND completed_at IS NULL AND owner_requested = 1
	`, teamID).Scan(&alreadyAuthorized); err != nil {
		return false, fmt.Errorf("lifecycle: inspect pending owner deletion %d: %w", teamID, err)
	}

	return alreadyAuthorized == 1, nil
}

// cancelRecoveredDeletion atomically retires a scheduled legacy claim when
// provider evidence proves the live account paid before destruction completed.
// Owner-requested intents are never eligible for payment-based cancellation.
func (p *Purger) cancelRecoveredDeletion(ctx context.Context, teamID int64, now time.Time) error {
	lease, err := p.Store.AcquireTransitionLease(ctx, teamID)
	if err != nil {
		return err
	}
	defer lease.Release()

	tx, err := p.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lifecycle: cancel recovered deletion %d: %w", teamID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	result, err := tx.ExecContext(ctx, `
		UPDATE account_lifecycle
		SET trigger = '', started_at = NULL, deleted_at = NULL, updated_at = ?
		WHERE team_id = ?
		  AND EXISTS (
		      SELECT 1 FROM account_deletions
		      WHERE team_id = ? AND completed_at IS NULL AND owner_requested = 0
		  )
	`, now.UTC().Unix(), teamID, teamID)
	if err != nil {
		return fmt.Errorf("lifecycle: restore recovered account %d: %w", teamID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("lifecycle: restore recovered account %d: affected rows: %w", teamID, err)
	}
	if rows != 1 {
		return fmt.Errorf("lifecycle: recovered deletion %d is missing or owner-requested", teamID)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE teams
		SET trial_ends_at = NULL, accept_traffic_until = NULL, updated_at = ?
		WHERE id = ?
	`, now.UTC().Unix(), teamID); err != nil {
		return fmt.Errorf("lifecycle: restore recovered account mirrors %d: %w", teamID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM lifecycle_outbox WHERE team_id = ? AND completed_at IS NULL
	`, teamID); err != nil {
		return fmt.Errorf("lifecycle: cancel recovered deletion notices %d: %w", teamID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE collection_gaps SET ended_at = ? WHERE team_id = ? AND ended_at IS NULL
	`, now.UTC().Unix(), teamID); err != nil {
		return fmt.Errorf("lifecycle: close recovered deletion gaps %d: %w", teamID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_deletion_customers WHERE team_id = ?`, teamID); err != nil {
		return fmt.Errorf("lifecycle: clear recovered payment customers %d: %w", teamID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM account_deletions
		WHERE team_id = ? AND completed_at IS NULL AND owner_requested = 0
	`, teamID); err != nil {
		return fmt.Errorf("lifecycle: clear recovered deletion audit %d: %w", teamID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lifecycle: cancel recovered deletion %d: %w", teamID, err)
	}

	return nil
}

// deletionStatus distinguishes a new claim, a crash recovery, and an already
// completed idempotent retry before any team-scoped lease is acquired.
func (p *Purger) deletionStatus(ctx context.Context, teamID int64) (bool, bool, time.Time, bool, error) {
	var started int64
	var completed sql.NullInt64
	var ownerRequested int
	err := p.Store.DB().QueryRowContext(ctx, `
		SELECT started_at, completed_at, owner_requested FROM account_deletions WHERE team_id = ?
	`, teamID).Scan(&started, &completed, &ownerRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, time.Time{}, false, nil
	}
	if err != nil {
		return false, false, time.Time{}, false, fmt.Errorf("lifecycle: inspect deletion %d: %w", teamID, err)
	}

	return !completed.Valid, completed.Valid, time.Unix(started, 0).UTC(), ownerRequested != 0, nil
}

// removeControl atomically removes the live account rows and checkpoints that
// phase on the immutable audit. A crash on either side is discoverable.
func (p *Purger) removeControl(ctx context.Context, teamID int64, now time.Time) error {
	tx, err := p.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lifecycle: remove control rows %d: %w", teamID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	// Remove identities whose only remaining membership is this account. Users
	// who belong to or are guests of another team survive with that access intact.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM users
		WHERE id IN (
		    SELECT user_id FROM team_memberships WHERE team_id = ?
		)
		AND NOT EXISTS (
		    SELECT 1 FROM team_memberships other
		    WHERE other.user_id = users.id AND other.team_id <> ?
		)
		AND NOT EXISTS (
		    SELECT 1 FROM guest_memberships guest
		    JOIN sites guest_site ON guest_site.id = guest.site_id
		    WHERE guest.user_id = users.id AND guest_site.account_id <> ?
		)
	`, teamID, teamID, teamID); err != nil {
		return fmt.Errorf("lifecycle: delete orphan users for team %d: %w", teamID, err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM teams WHERE id = ?`, teamID); err != nil {
		return fmt.Errorf("lifecycle: delete team %d: %w", teamID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE account_deletions
		SET control_removed_at = COALESCE(control_removed_at, ?),
		    notes = CASE WHEN instr(notes, 'control rows removed') > 0 THEN notes
		                 WHEN notes = '' THEN 'control rows removed'
		                 ELSE notes || '; control rows removed' END
		WHERE team_id = ?
	`, now.UTC().Unix(), teamID); err != nil {
		return fmt.Errorf("lifecycle: checkpoint control removal %d: %w", teamID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lifecycle: remove control rows %d: %w", teamID, err)
	}

	return nil
}

// markDeletionStep records one completed external step. Column is selected from
// internal constants only; values never come from a request.
func (p *Purger) markDeletionStep(ctx context.Context, teamID int64, column string, now time.Time, note string) error {
	if column != "local_removed_at" && column != "provider_removed_at" && column != "control_removed_at" {
		return fmt.Errorf("lifecycle: invalid deletion checkpoint %q", column)
	}
	query := `UPDATE account_deletions SET ` + column + ` = COALESCE(` + column + `, ?),
		notes = CASE WHEN instr(notes, ?) > 0 THEN notes
		             WHEN notes = '' THEN ? ELSE notes || '; ' || ? END
		WHERE team_id = ?`
	if _, err := p.Store.DB().ExecContext(ctx, query, now.UTC().Unix(), note, note, note, teamID); err != nil {
		return fmt.Errorf("lifecycle: checkpoint %s for %d: %w", column, teamID, err)
	}

	return nil
}

// pendingPaymentCustomers returns every provider customer discovered before
// the team cascade, excluding customers already confirmed deleted on an earlier
// retry. The table deliberately survives team deletion with the audit row.
func (p *Purger) pendingPaymentCustomers(ctx context.Context, teamID int64) ([]string, error) {
	rows, err := p.Store.DB().QueryContext(ctx, `
		SELECT customer_id FROM account_deletion_customers
		WHERE team_id = ? AND removed_at IS NULL
		ORDER BY customer_id
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: list payment customers for deletion %d: %w", teamID, err)
	}
	defer rows.Close()

	var customers []string
	for rows.Next() {
		var customerID string
		if err := rows.Scan(&customerID); err != nil {
			return nil, fmt.Errorf("lifecycle: read payment customer for deletion %d: %w", teamID, err)
		}
		customers = append(customers, customerID)
	}

	return customers, rows.Err()
}

// paymentCustomerCount returns the complete durable provider set, including
// customers removed on an earlier attempt. It keeps the final audit note
// accurate across a crash after the last per-customer checkpoint.
func (p *Purger) paymentCustomerCount(ctx context.Context, teamID int64) (int, error) {
	var count int
	if err := p.Store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM account_deletion_customers WHERE team_id = ?
	`, teamID).Scan(&count); err != nil {
		return 0, fmt.Errorf("lifecycle: count payment customers for deletion %d: %w", teamID, err)
	}

	return count, nil
}

// markPaymentCustomerRemoved checkpoints one provider record independently.
// A later provider failure leaves completed customers out of the next retry and
// continues from the durable unfinished set.
func (p *Purger) markPaymentCustomerRemoved(ctx context.Context, teamID int64, customerID string, now time.Time) error {
	result, err := p.Store.DB().ExecContext(ctx, `
		UPDATE account_deletion_customers
		SET removed_at = COALESCE(removed_at, ?), last_error = ''
		WHERE team_id = ? AND customer_id = ?
	`, now.UTC().Unix(), teamID, customerID)
	if err != nil {
		return fmt.Errorf("lifecycle: checkpoint payment customer %s for %d: %w", customerID, teamID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("lifecycle: checkpoint payment customer %s for %d: affected rows: %w", customerID, teamID, err)
	}
	if rows != 1 {
		return fmt.Errorf("lifecycle: payment customer %s was not recorded for deletion %d", customerID, teamID)
	}

	return nil
}

// recordPaymentCustomerFailure preserves the latest provider error beside the
// exact customer that failed. The aggregate deletion note remains useful for a
// support timeline, while the per-customer field drives precise retry audits.
func (p *Purger) recordPaymentCustomerFailure(ctx context.Context, teamID int64, customerID string, providerErr error) error {
	tx, err := p.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lifecycle: record payment customer failure %s for %d: %w", customerID, teamID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	result, err := tx.ExecContext(ctx, `
		UPDATE account_deletion_customers
		SET last_error = ?
		WHERE team_id = ? AND customer_id = ? AND removed_at IS NULL
	`, providerErr.Error(), teamID, customerID)
	if err != nil {
		return fmt.Errorf("lifecycle: record payment customer failure %s for %d: %w", customerID, teamID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("lifecycle: record payment customer failure %s for %d: affected rows: %w", customerID, teamID, err)
	}
	if rows != 1 {
		return fmt.Errorf("lifecycle: failed payment customer %s was not pending for deletion %d", customerID, teamID)
	}

	note := "payment customer " + customerID + " NOT removed: " + providerErr.Error()
	if _, err := tx.ExecContext(ctx, `
		UPDATE account_deletions
		SET notes = CASE WHEN notes = '' THEN ? ELSE notes || '; ' || ? END
		WHERE team_id = ?
	`, note, note, teamID); err != nil {
		return fmt.Errorf("lifecycle: append payment customer failure note %d: %w", teamID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lifecycle: commit payment customer failure %s for %d: %w", customerID, teamID, err)
	}

	return nil
}

// finishControl completes the immutable audit and creates the confirmation only
// after local data and the provider customer have both been removed.
func (p *Purger) finishControl(ctx context.Context, teamID int64, now time.Time) error {
	tx, err := p.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lifecycle: finish deletion %d: %w", teamID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	// Warning rows no longer serve an operational purpose once the account is
	// gone. The confirmation row is inserted from immutable audit data and has
	// no team foreign key, so it survives the cascade below.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM lifecycle_outbox WHERE team_id = ? AND template <> ?
	`, teamID, TemplateAccountDeleted); err != nil {
		return fmt.Errorf("lifecycle: clear warning outbox %d: %w", teamID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO lifecycle_outbox
			(team_id, started_at, template, recipient, message_key,
			 created_at, updated_at)
		SELECT team_id, clock_started_at, ?, contact_email,
		       'lifecycle-' || team_id || '-' || clock_started_at || '-' || ?,
		       ?, ?
		FROM account_deletions WHERE team_id = ? AND contact_email <> ''
		ON CONFLICT (team_id, started_at, template) DO NOTHING
	`, TemplateAccountDeleted, TemplateAccountDeleted, now.UTC().Unix(), now.UTC().Unix(), teamID); err != nil {
		return fmt.Errorf("lifecycle: create confirmation outbox %d: %w", teamID, err)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE account_deletions
		SET completed_at = COALESCE(completed_at, ?)
		WHERE team_id = ? AND control_removed_at IS NOT NULL
		  AND local_removed_at IS NOT NULL AND provider_removed_at IS NOT NULL
	`, now.UTC().Unix(), teamID)
	if err != nil {
		return fmt.Errorf("lifecycle: finish deletion record %d: %w", teamID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("lifecycle: finish deletion record %d: %w", teamID, err)
	}
	if rows != 1 {
		return fmt.Errorf("lifecycle: deletion %d has unfinished cleanup steps", teamID)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lifecycle: finish deletion %d: %w", teamID, err)
	}

	return nil
}

// claim atomically revalidates the exact running clock and writes the durable
// audit row before destruction starts. A concurrent payment either clears the
// clock first and makes this a no-op, or observes deleted_at and cannot revive
// an account whose deletion has begun.
func (p *Purger) claim(ctx context.Context, account Account, now time.Time, customerIDs []string, userID int64, force bool) (bool, error) {
	db := p.Store.DB()

	if !account.State.Deleted() && !force && !account.State.DueForDeletion(now) {
		return false, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("lifecycle: claim deletion %d: %w", account.TeamID, err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	// A clock already marked deleted with an unfinished audit row is a crash
	// recovery, not a new claim. Provider discovery may have happened after an
	// explicit intent was recorded, so merge those customer IDs before resuming.
	if account.State.Deleted() {
		var pending int
		err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM account_deletions
			WHERE team_id = ? AND completed_at IS NULL
		`, account.TeamID).Scan(&pending)
		if err != nil {
			return false, fmt.Errorf("lifecycle: inspect deletion %d: %w", account.TeamID, err)
		}
		if pending != 1 {
			return false, nil
		}
		if err := p.recordPaymentCustomers(ctx, tx, account, customerIDs, now); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("lifecycle: resume deletion claim %d: %w", account.TeamID, err)
		}

		return true, nil
	}

	query := `
		UPDATE account_lifecycle
		SET deleted_at = ?, updated_at = ?
		WHERE team_id = ? AND trigger = ? AND started_at IS ? AND deleted_at IS NULL
	`
	args := []any{now.UTC().Unix(), now.UTC().Unix(), account.TeamID,
		string(account.State.Trigger), toUnix(account.State.StartedAt)}
	if force {
		query += ` AND EXISTS (
			SELECT 1 FROM team_memberships
			WHERE team_memberships.team_id = account_lifecycle.team_id
			  AND team_memberships.user_id = ?
			  AND team_memberships.role = 'owner'
		)`
		args = append(args, userID)
	} else {
		query += ` AND NOT EXISTS (
			SELECT 1 FROM subscriptions
			WHERE subscriptions.team_id = account_lifecycle.team_id
			  AND subscriptions.payment_state = 'paid'
		)`
	}
	result, err := tx.ExecContext(ctx, query, args...)
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

	ownerRequested := 0
	if force {
		ownerRequested = 1
	}
	if _, err := tx.ExecContext(ctx, `
			INSERT INTO account_deletions
				(team_id, team_name, contact_email, stripe_customer_id, clock_started_at, started_at, owner_requested)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (team_id) DO NOTHING
		`, account.TeamID, account.TeamName, account.Email, account.CustomerID,
		account.State.StartedAt.UTC().Unix(), now.UTC().Unix(), ownerRequested); err != nil {
		return false, fmt.Errorf("lifecycle: record deletion %d: %w", account.TeamID, err)
	}

	if err := p.recordPaymentCustomers(ctx, tx, account, customerIDs, now); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("lifecycle: claim deletion %d: %w", account.TeamID, err)
	}

	return true, nil
}

// recordPaymentCustomers merges every provider identity discovered before or
// after an authorized deletion intent into the no-FK retry audit transaction.
func (p *Purger) recordPaymentCustomers(ctx context.Context, tx *sql.Tx, account Account, customerIDs []string, now time.Time) error {
	uniqueCustomers := make(map[string]bool, len(customerIDs)+1)
	if account.CustomerID != "" {
		uniqueCustomers[account.CustomerID] = true
	}
	for _, customerID := range customerIDs {
		if customerID != "" {
			uniqueCustomers[customerID] = true
		}
	}
	for customerID := range uniqueCustomers {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO account_deletion_customers
				(team_id, customer_id, created_at)
			VALUES (?, ?, ?)
			ON CONFLICT (team_id, customer_id) DO NOTHING
			`, account.TeamID, customerID, now.UTC().Unix()); err != nil {
			return fmt.Errorf("lifecycle: record payment customer %s for deletion %d: %w", customerID, account.TeamID, err)
		}
	}

	return nil
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

// PendingDeletions returns every claimed deletion that has not completed. The
// audit has no team foreign key, so this remains discoverable even when an old
// build crashed after removing the team but before marking completion.
func (p *Purger) PendingDeletions(ctx context.Context) ([]Account, error) {
	rows, err := p.Store.DB().QueryContext(ctx, `
		SELECT team_id, team_name, contact_email, stripe_customer_id,
		       clock_started_at, started_at
		FROM account_deletions
		WHERE completed_at IS NULL
		ORDER BY team_id
	`)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: pending deletions: %w", err)
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		var account Account
		var clockStarted, claimedAt int64
		if err := rows.Scan(&account.TeamID, &account.TeamName, &account.Email,
			&account.CustomerID, &clockStarted, &claimedAt); err != nil {
			return nil, fmt.Errorf("lifecycle: pending deletions: %w", err)
		}
		account.State = State{
			Trigger:   TriggerLapse,
			StartedAt: time.Unix(clockStarted, 0).UTC(),
			DeletedAt: time.Unix(claimedAt, 0).UTC(),
		}
		out = append(out, account)
	}

	return out, rows.Err()
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
