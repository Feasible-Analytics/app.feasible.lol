//
// deletion.go
// Day 90: the database file, the system rows and the payment customer, irreversibly.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/dataio"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/jobs"
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

// ErrTransferredSiteStorage means the requested purge would either destroy a
// transferred site's historical database or orphan storage owned by the team.
var ErrTransferredSiteStorage = errors.New("lifecycle: this account participates in a site transfer and cannot be purged")

// ErrAccountOwnerRequired means an immediate deletion lost its owner
// authorization before the durable control transaction committed.
var ErrAccountOwnerRequired = errors.New("lifecycle: account deletion requires the current owner")

// Purge destroys everything belonging to one account.
//
// The order is deliberate and each step is idempotent, because a crash between
// any two of them must be resumable by simply running it again:
//
//  1. Lease the account. Scheduled deletion re-reads settlement while making
//     only reversible provider changes; owner deletion records intent first.
//  2. Atomically revalidate the lifecycle/owner and mark the deletion claim
//     authoritative before any invoice, session, or customer is destroyed.
//  3. Finalize provider opportunities. A crash now resumes deletion instead of
//     recovering an account whose provider state has been irreversibly changed.
//  4. Delete team-owned control records and the team row. This reruns for a
//     legacy audit whose team already cascaded so newly indexed jobs and raw
//     payment payloads cannot survive an older build's completion marker.
//  5. Tombstone and remove the analytics directory, its WAL, and everything
//     beside it, waiting until cross-process handles are fenced.
//  6. Remove every payment-provider customer. An outage leaves the immutable
//     per-customer audit pending until a later sweep finishes this step.
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
	pending, completed, claimedAt, ownerRequested, authoritative, err := p.deletionStatus(ctx, account.TeamID)
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
		authoritative = true
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
		transitionLease.Release()
		transitionLease = nil
		account.State.DeletedAt = now.UTC()
		authoritative = true
	}

	recoverSettlement := !force && !authoritative
	if live && p.Payments != nil {
		quiescence, err = p.Payments.QuiesceForDeletion(
			ctx, lease, account.TeamID, account.CustomerID, account.State.StartedAt, recoverSettlement,
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
	if transitionLease != nil {
		transitionLease.Release()
		transitionLease = nil
	}
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
	// Scheduled deletion first made only reversible provider changes. Once its
	// exact clock has been durably claimed and marked authoritative, run the
	// irreversible finalization pass; a crash resumes in this mode and can no
	// longer convert provider payment into account recovery.
	if recoverSettlement && live && p.Payments != nil {
		quiescence, err = p.Payments.QuiesceForDeletion(
			ctx, lease, account.TeamID, account.CustomerID, account.State.StartedAt, false,
		)
		if err != nil {
			return err
		}
		if quiescence.Recovered {
			return p.cancelRecoveredDeletion(ctx, account.TeamID, now)
		}
		if err := lease.Renew(ctx); err != nil {
			return err
		}
		claimed, err = p.claim(ctx, account, now, quiescence.CustomerIDs, userID, force)
		if err != nil {
			return err
		}
		if !claimed {
			return fmt.Errorf("lifecycle: account %d authoritative deletion claim disappeared", account.TeamID)
		}
	}

	manager := p.Accounts
	if manager == nil {
		manager = accounts.NewManager(p.DataDir)
	}
	deletionGuard, err := manager.BeginDeletion(account.TeamID)
	if err != nil {
		return fmt.Errorf("lifecycle: fence account database %d: %w", account.TeamID, err)
	}
	defer deletionGuard.Release() //nolint:errcheck // an earlier deletion error is more useful

	manifest, indexed, globalRemoved, err := p.artifactState(ctx, account.TeamID)
	if err != nil {
		return err
	}
	if !indexed {
		manifest, err = p.discoverArtifacts(ctx, account.TeamID, deletionGuard)
		if err != nil {
			return err
		}
		if err := p.markArtifactsIndexed(ctx, account.TeamID, manifest, now); err != nil {
			return err
		}
	}

	// A rerun after a legacy build can find the team already gone while raw
	// payment payloads and queued import/export work still survive without a
	// foreign key. The control transaction is idempotent for an absent team and
	// deliberately retains M9's writer reservation and topology validation.
	if err := p.removeControl(ctx, account.TeamID, now); err != nil {
		return err
	}

	if err := deletionGuard.CloseAccount(); err != nil {
		return fmt.Errorf("lifecycle: close account database %d: %w", account.TeamID, err)
	}
	if err := os.RemoveAll(accounts.Dir(p.DataDir, account.TeamID)); err != nil {
		return fmt.Errorf("lifecycle: delete account database %d: %w", account.TeamID, err)
	}
	if err := p.markDeletionStep(ctx, account.TeamID, "local_removed_at", now,
		"database directory "+accounts.Dir(p.DataDir, account.TeamID)+" removed"); err != nil {
		return err
	}
	if !globalRemoved {
		if err := p.removeGlobalArtifacts(manifest, account.TeamID, os.RemoveAll); err != nil {
			return err
		}
		if err := p.markGlobalRemoved(ctx, account.TeamID, now); err != nil {
			return err
		}
	}

	// Customer removal runs only after authoritative provider finalization and
	// permanent local destruction. A provider outage therefore leaves no live
	// account that could recover with already-voided invoice state.
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
	for _, pendingCustomerID := range customers {
		if err := p.Customers.DeleteCustomer(ctx, pendingCustomerID); err != nil {
			recordErr := p.recordPaymentCustomerFailure(ctx, account.TeamID, pendingCustomerID, err)
			if p.Log != nil {
				p.Log.Error("could not delete the payment provider's customer; deletion will retry",
					"team", account.TeamID, "customer", pendingCustomerID, "error", err)
			}
			if recordErr != nil {
				return fmt.Errorf("lifecycle: delete payment customer %s: %w; could not checkpoint failure: %v", pendingCustomerID, err, recordErr)
			}
			return fmt.Errorf("lifecycle: delete payment customer %s: %w", pendingCustomerID, err)
		}
		if err := p.markPaymentCustomerRemoved(ctx, account.TeamID, pendingCustomerID, now); err != nil {
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
		SET owner_requested = 1, authoritative_at = COALESCE(authoritative_at, CAST(strftime('%s', 'now') AS INTEGER))
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
			  AND authoritative_at IS NULL
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
		  AND authoritative_at IS NULL
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
func (p *Purger) deletionStatus(ctx context.Context, teamID int64) (bool, bool, time.Time, bool, bool, error) {
	var started int64
	var completed sql.NullInt64
	var authoritative sql.NullInt64
	var ownerRequested int
	err := p.Store.DB().QueryRowContext(ctx, `
		SELECT started_at, completed_at, owner_requested, authoritative_at FROM account_deletions WHERE team_id = ?
	`, teamID).Scan(&started, &completed, &ownerRequested, &authoritative)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, time.Time{}, false, false, nil
	}
	if err != nil {
		return false, false, time.Time{}, false, false, fmt.Errorf("lifecycle: inspect deletion %d: %w", teamID, err)
	}

	return !completed.Valid, completed.Valid, time.Unix(started, 0).UTC(), ownerRequested != 0, authoritative.Valid, nil
}

// artifactState returns the durable global-file manifest and its two cleanup
// checkpoints. Once indexed, retries never depend on a shard that may already
// have been removed by an earlier process.
func (p *Purger) artifactState(ctx context.Context, teamID int64) (string, bool, bool, error) {
	var manifest string
	var indexed, removed sql.NullInt64
	if err := p.Store.DB().QueryRowContext(ctx, `
		SELECT artifact_manifest, artifacts_indexed_at, global_removed_at
		FROM account_deletions WHERE team_id = ?
	`, teamID).Scan(&manifest, &indexed, &removed); err != nil {
		return "", false, false, fmt.Errorf("lifecycle: read account %d artifact state: %w", teamID, err)
	}

	return manifest, indexed.Valid, removed.Valid, nil
}

// discoverArtifacts snapshots every global path owned by account-shard rows
// while the exclusive deletion fence is held. Account-scoped directories are
// always included, covering files created after a row was last updated. A v9
// account whose shard is already gone falls back to the durable control jobs
// that identify its old flat artifact names.
func (p *Purger) discoverArtifacts(ctx context.Context, teamID int64, guard *accounts.DeletionGuard) (string, error) {
	paths := []string{
		dataio.AccountArtifactDir(p.DataDir, dataio.UploadDir, teamID),
		dataio.AccountArtifactDir(p.DataDir, dataio.ExportDir, teamID),
	}

	account, err := guard.OpenAccount(ctx)
	if err != nil {
		return "", fmt.Errorf("lifecycle: open account %d to index artifacts: %w", teamID, err)
	}
	if account != nil {
		for _, query := range []string{
			"SELECT upload_path FROM imports WHERE upload_path <> ''",
			"SELECT path FROM exports WHERE path <> ''",
		} {
			rows, err := account.Reader().QueryContext(ctx, query)
			if err != nil {
				return "", fmt.Errorf("lifecycle: index account %d artifacts: %w", teamID, err)
			}
			for rows.Next() {
				var path string
				if err := rows.Scan(&path); err != nil {
					_ = rows.Close()
					return "", fmt.Errorf("lifecycle: index account %d artifacts: %w", teamID, err)
				}
				paths = append(paths, path)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return "", fmt.Errorf("lifecycle: index account %d artifacts: %w", teamID, err)
			}
			if err := rows.Close(); err != nil {
				return "", fmt.Errorf("lifecycle: close account %d artifact rows: %w", teamID, err)
			}
		}
	} else {
		legacy, err := p.discoverLegacyArtifacts(ctx, teamID)
		if err != nil {
			return "", err
		}
		paths = append(paths, legacy...)
	}

	unique := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		safe, err := p.safeArtifactPath(path)
		if err != nil {
			return "", err
		}
		if !seen[safe] {
			seen[safe] = true
			unique = append(unique, safe)
		}
	}
	sort.Strings(unique)

	encoded, err := json.Marshal(unique)
	if err != nil {
		return "", fmt.Errorf("lifecycle: encode account %d artifact manifest: %w", teamID, err)
	}
	return string(encoded), nil
}

// legacyArtifactIdentity is unique only within one v9 account shard. The old
// flat paths omitted the account id, so the ownership pass must detect another
// team's claim to the same kind and id before selecting a path for deletion.
type legacyArtifactIdentity struct {
	kind string
	id   int64
}

// discoverLegacyArtifacts derives v9 flat paths only after every relevant job
// has complete, mutually consistent ownership evidence. One invalid or
// conflicting row makes every account-local artifact id potentially ambiguous,
// so it fails the deletion before the manifest checkpoint is written.
func (p *Purger) discoverLegacyArtifacts(ctx context.Context, teamID int64) ([]string, error) {
	rows, err := p.Store.DB().QueryContext(ctx, `
		SELECT id, owner_team_id, kind, args
		FROM jobs
		WHERE kind IN (?, ?)
		ORDER BY id
	`, jobs.KindCSVImport, jobs.KindSiteExport)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: inspect account %d legacy artifact jobs: %w", teamID, err)
	}
	defer rows.Close() //nolint:errcheck // the explicit successful close below reports its error

	claims := make(map[legacyArtifactIdentity]map[int64]bool)
	for rows.Next() {
		var jobID int64
		var ownerTeamID sql.NullInt64
		var kind string
		var encoded string
		if err := rows.Scan(&jobID, &ownerTeamID, &kind, &encoded); err != nil {
			return nil, fmt.Errorf("lifecycle: read account %d legacy artifact job: %w", teamID, err)
		}
		if !ownerTeamID.Valid || ownerTeamID.Int64 < 1 {
			return nil, fmt.Errorf("lifecycle: legacy artifact job %d has no positive structural owner", jobID)
		}

		identity, accountID, err := decodeLegacyArtifactIdentity(kind, encoded)
		if err != nil {
			return nil, fmt.Errorf("lifecycle: legacy artifact job %d has invalid arguments: %w", jobID, err)
		}
		if accountID != ownerTeamID.Int64 {
			return nil, fmt.Errorf("lifecycle: legacy artifact job %d has conflicting structural and JSON owners", jobID)
		}
		if claims[identity] == nil {
			claims[identity] = make(map[int64]bool)
		}
		if len(claims[identity]) > 0 && !claims[identity][ownerTeamID.Int64] {
			return nil, fmt.Errorf("lifecycle: legacy %s artifact %d has conflicting account owners", identity.kind, identity.id)
		}
		claims[identity][ownerTeamID.Int64] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lifecycle: inspect account %d legacy artifact jobs: %w", teamID, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("lifecycle: close account %d legacy artifact jobs: %w", teamID, err)
	}

	var paths []string
	for identity, owners := range claims {
		if len(owners) != 1 {
			return nil, fmt.Errorf("lifecycle: account %d legacy %s artifact %d has conflicting ownership", teamID, identity.kind, identity.id)
		}
		if !owners[teamID] {
			continue
		}

		var owned []string
		switch identity.kind {
		case jobs.KindCSVImport:
			owned, err = p.legacyImportPaths(identity.id)
		case jobs.KindSiteExport:
			owned, err = p.legacyExportPaths(identity.id)
		}
		if err != nil {
			return nil, fmt.Errorf("lifecycle: discover account %d legacy artifact: %w", teamID, err)
		}
		paths = append(paths, owned...)
	}

	return paths, nil
}

// decodeLegacyArtifactIdentity requires the exact v9 argument names and JSON
// integer types for its ownership and artifact fields. Unexpected fields are
// rejected because a misspelled identity cannot safely be distinguished from
// a second claim.
func decodeLegacyArtifactIdentity(kind, encoded string) (legacyArtifactIdentity, int64, error) {
	fields, err := decodeLegacyArtifactFields(encoded)
	if err != nil {
		return legacyArtifactIdentity{}, 0, err
	}

	artifactName := "import_id"
	allowed := map[string]bool{"account_id": true, "site_id": true, artifactName: true}
	if kind == jobs.KindSiteExport {
		artifactName = "export_id"
		allowed = map[string]bool{"account_id": true, "site_id": true, artifactName: true}
	}
	for name := range fields {
		if !allowed[name] {
			return legacyArtifactIdentity{}, 0, fmt.Errorf("unexpected argument %q", name)
		}
	}

	accountID, err := decodePositiveLegacyInteger(fields, "account_id")
	if err != nil {
		return legacyArtifactIdentity{}, 0, err
	}
	artifactID, err := decodePositiveLegacyInteger(fields, artifactName)
	if err != nil {
		return legacyArtifactIdentity{}, 0, err
	}
	return legacyArtifactIdentity{kind: kind, id: artifactID}, accountID, nil
}

// decodeLegacyArtifactFields reads one JSON object while rejecting duplicate
// keys and trailing values. Standard map decoding keeps only the last duplicate,
// which could hide contradictory account ownership in an otherwise valid row.
func decodeLegacyArtifactFields(encoded string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(encoded))
	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("arguments are not a JSON object")
	}

	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("argument name is not a string")
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate argument %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	closing, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return nil, errors.New("arguments have no closing object delimiter")
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("arguments contain more than one JSON value")
	}
	return fields, nil
}

// decodePositiveLegacyInteger reads one required ownership field without JSON
// coercion. Strings, fractions, nulls, missing names, and non-positive values
// are all ambiguous in the flat v9 namespace and therefore invalid.
func decodePositiveLegacyInteger(fields map[string]json.RawMessage, name string) (int64, error) {
	raw, ok := fields[name]
	if !ok {
		return 0, fmt.Errorf("missing %s", name)
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	if value < 1 {
		return 0, fmt.Errorf("invalid %s: must be positive", name)
	}
	return value, nil
}

// legacyImportPaths returns every regular v9 upload with the exact import-id
// prefix. A non-regular match is rejected so a crafted directory or symlink
// cannot broaden deletion beyond the legacy single-file contract.
func (p *Purger) legacyImportPaths(importID int64) ([]string, error) {
	root := filepath.Join(p.DataDir, dataio.UploadDir)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	prefix := fmt.Sprintf("%06d-", importID)
	var paths []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("legacy import %s is not a regular file", entry.Name())
		}
		paths = append(paths, filepath.Join(root, entry.Name()))
	}
	return paths, nil
}

// legacyExportPaths returns the exact regular archive named by a v9 export
// job. Missing files are already clean; non-regular matches fail closed.
func (p *Purger) legacyExportPaths(exportID int64) ([]string, error) {
	path := filepath.Join(p.DataDir, dataio.ExportDir, fmt.Sprintf("export-%06d.zip", exportID))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("legacy export %s is not a regular file", filepath.Base(path))
	}
	return []string{path}, nil
}

// safeArtifactPath canonicalizes a path and rejects anything outside the two
// global artifact roots. A corrupt shard row must fail closed instead of
// turning account deletion into an arbitrary filesystem removal primitive.
func (p *Purger) safeArtifactPath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("lifecycle: resolve artifact path: %w", err)
	}

	for _, kind := range []string{dataio.UploadDir, dataio.ExportDir} {
		root, err := filepath.Abs(filepath.Join(p.DataDir, kind))
		if err != nil {
			return "", fmt.Errorf("lifecycle: resolve artifact root: %w", err)
		}
		rel, err := filepath.Rel(root, abs)
		if err == nil && rel != "." && rel != "" && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return abs, nil
		}
	}

	return "", fmt.Errorf("lifecycle: refuse artifact path outside import/export roots: %s", path)
}

// markArtifactsIndexed persists the ownership manifest before either the shard
// or its system rows can be removed.
func (p *Purger) markArtifactsIndexed(ctx context.Context, teamID int64, manifest string, now time.Time) error {
	if _, err := p.Store.DB().ExecContext(ctx, `
		UPDATE account_deletions
		SET artifact_manifest = ?, artifacts_indexed_at = COALESCE(artifacts_indexed_at, ?)
		WHERE team_id = ?
	`, manifest, now.UTC().Unix(), teamID); err != nil {
		return fmt.Errorf("lifecycle: checkpoint account %d artifacts: %w", teamID, err)
	}
	return nil
}

// removeGlobalArtifacts deletes only canonical paths from the durable manifest.
// Missing paths are success, making every restart and repeated attempt safe.
func (p *Purger) removeGlobalArtifacts(manifest string, teamID int64, removeAll func(string) error) error {
	var paths []string
	if err := json.Unmarshal([]byte(manifest), &paths); err != nil {
		return fmt.Errorf("lifecycle: decode account %d artifact manifest: %w", teamID, err)
	}
	for _, path := range paths {
		safe, err := p.safeArtifactPath(path)
		if err != nil {
			return err
		}
		if err := removeAll(safe); err != nil {
			return fmt.Errorf("lifecycle: remove account %d artifact %s: %w", teamID, safe, err)
		}
	}
	return nil
}

// markGlobalRemoved records that uploaded imports and prepared archives have
// been removed from their global filesystem roots.
func (p *Purger) markGlobalRemoved(ctx context.Context, teamID int64, now time.Time) error {
	if _, err := p.Store.DB().ExecContext(ctx, `
		UPDATE account_deletions
		SET global_removed_at = COALESCE(global_removed_at, ?)
		WHERE team_id = ?
	`, now.UTC().Unix(), teamID); err != nil {
		return fmt.Errorf("lifecycle: checkpoint global removal %d: %w", teamID, err)
	}
	return nil
}

// removeControl atomically removes the live account rows and checkpoints that
// phase on the immutable audit. A crash on either side is discoverable.
func (p *Purger) removeControl(ctx context.Context, teamID int64, now time.Time) error {
	tx, err := p.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lifecycle: remove system rows %d: %w", teamID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	// Reserve system.db's writer before validating the topology. Transfers
	// consult the same operation row, so one side wins before either can change
	// ownership or remove the team.
	if _, err := tx.ExecContext(ctx, `
		UPDATE destructive_operations SET updated_at = updated_at
		WHERE resource_type = 'team' AND resource_id = ?
	`, teamID); err != nil {
		return fmt.Errorf("lifecycle: fence control deletion %d: %w", teamID, err)
	}
	if err := validateTransferTopologyTx(ctx, tx, teamID); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM stripe_events WHERE team_id = ?`, teamID); err != nil {
		return fmt.Errorf("lifecycle: delete payment payloads for team %d: %w", teamID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE owner_team_id = ?`, teamID); err != nil {
		return fmt.Errorf("lifecycle: delete jobs for team %d: %w", teamID, err)
	}

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
		DELETE FROM destructive_operations
		WHERE resource_type = 'team' AND resource_id = ?
	`, teamID); err != nil {
		return fmt.Errorf("lifecycle: clear deletion fence %d: %w", teamID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE account_deletions
		SET control_removed_at = COALESCE(control_removed_at, ?),
		    notes = CASE WHEN instr(notes, 'system rows removed') > 0 THEN notes
		                 WHEN notes = '' THEN 'system rows removed'
		                 ELSE notes || '; system rows removed' END
		WHERE team_id = ?
	`, now.UTC().Unix(), teamID); err != nil {
		return fmt.Errorf("lifecycle: checkpoint control removal %d: %w", teamID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lifecycle: remove system rows %d: %w", teamID, err)
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
	defer func() { _ = rows.Close() }()

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

	note := "payment customer removal pending"
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
		SET completed_at = COALESCE(completed_at, ?),
		    stripe_customer_id = '',
		    notes = 'live account data removed; deletion confirmation pending'
		WHERE team_id = ? AND control_removed_at IS NOT NULL
		  AND local_removed_at IS NOT NULL AND artifacts_indexed_at IS NOT NULL
		  AND global_removed_at IS NOT NULL AND provider_removed_at IS NOT NULL
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_deletion_customers WHERE team_id = ?`, teamID); err != nil {
		return fmt.Errorf("lifecycle: minimise payment customer audit %d: %w", teamID, err)
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

	// Acquire SQLite's writer reservation before topology validation. A site
	// transfer and a deletion claim therefore serialize on system.db instead of
	// both authorizing from stale reads.
	if _, err := tx.ExecContext(ctx, `UPDATE destructive_operations SET updated_at = updated_at WHERE resource_id = -1`); err != nil {
		return false, fmt.Errorf("lifecycle: fence deletion %d: %w", account.TeamID, err)
	}
	if err := validateTransferTopologyTx(ctx, tx, account.TeamID); err != nil {
		return false, err
	}

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
		if err := claimTeamOperationTx(ctx, tx, account.TeamID, force, now); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE account_deletions
			SET authoritative_at = COALESCE(authoritative_at, ?)
			WHERE team_id = ? AND completed_at IS NULL
		`, now.UTC().Unix(), account.TeamID); err != nil {
			return false, fmt.Errorf("lifecycle: finalize deletion claim %d: %w", account.TeamID, err)
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
		if force {
			var owner int
			if err := tx.QueryRowContext(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM team_memberships
					WHERE team_id = ? AND user_id = ? AND role = 'owner'
				)
			`, account.TeamID, userID).Scan(&owner); err != nil {
				return false, fmt.Errorf("lifecycle: revalidate deletion owner %d: %w", account.TeamID, err)
			}
			if owner == 0 {
				return false, ErrAccountOwnerRequired
			}
		}
		return false, nil
	}
	if err := claimTeamOperationTx(ctx, tx, account.TeamID, force, now); err != nil {
		return false, err
	}

	ownerRequested := 0
	if force {
		ownerRequested = 1
	}
	if _, err := tx.ExecContext(ctx, `
			INSERT INTO account_deletions
				(team_id, team_name, contact_email, stripe_customer_id, clock_started_at, started_at, owner_requested, authoritative_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (team_id) DO NOTHING
		`, account.TeamID, account.TeamName, account.Email, account.CustomerID,
		account.State.StartedAt.UTC().Unix(), now.UTC().Unix(), ownerRequested, now.UTC().Unix()); err != nil {
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

// claimTeamOperationTx publishes the durable team fence in the same transaction
// that makes deletion authoritative. Transfers see this row before they can
// change ownership, including after a process crash and during provider work.
func claimTeamOperationTx(ctx context.Context, tx *sql.Tx, teamID int64, ownerRequested bool, now time.Time) error {
	kind := "account_purge"
	if ownerRequested {
		kind = "account_delete"
	}
	stamp := now.UTC().Unix()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO destructive_operations
			(resource_type, resource_id, kind, owner_team_id, storage_account_id,
			 state, lease_token, lease_until, created_at, updated_at)
		VALUES ('team', ?, ?, ?, ?, 'claimed', ?, ?, ?, ?)
		ON CONFLICT (resource_type, resource_id) DO UPDATE SET
			kind = excluded.kind,
			owner_team_id = excluded.owner_team_id,
			storage_account_id = excluded.storage_account_id,
			state = 'claimed',
			lease_token = excluded.lease_token,
			lease_until = excluded.lease_until,
			updated_at = excluded.updated_at
	`, teamID, kind, teamID, teamID, uuid.NewString(), now.Add(5*time.Minute).Unix(), stamp, stamp)
	if err != nil {
		return fmt.Errorf("lifecycle: claim destructive operation %d: %w", teamID, err)
	}

	return nil
}

// validateTransferTopologyTx refuses either side of a cross-account site
// transfer while the caller holds system.db's writer reservation.
func validateTransferTopologyTx(ctx context.Context, tx *sql.Tx, teamID int64) error {
	var transferred int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sites
		WHERE account_id <> COALESCE(owner_team_id, account_id)
		  AND (account_id = ? OR owner_team_id = ?)
	`, teamID, teamID).Scan(&transferred); err != nil {
		return fmt.Errorf("lifecycle: check transferred site storage: %w", err)
	}
	if transferred > 0 {
		return ErrTransferredSiteStorage
	}

	return nil
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

// teamExists reports whether the account's system rows are still there. It is
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
	defer func() { _ = rows.Close() }()

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
	defer func() { _ = rows.Close() }()

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
