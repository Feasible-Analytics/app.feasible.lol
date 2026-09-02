//
// store.go
// The mirror of the payment provider's records, and the log support reads.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package billing connects the payment provider to the account lifecycle. It
// owns the webhook endpoint, the checkout and portal redirects, and the local
// mirror of what the provider believes about each account.
//
// One rule governs the whole package: nothing here decides anything from the
// event that woke it. Every handler re-reads the provider's current state and
// acts on that. Webhooks arrive out of order, arrive twice, and arrive minutes
// late; a handler that trusted the payload would eventually mark a paying
// customer as lapsed because a stale `past_due` snapshot overtook a fresh
// `active` one.
package billing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Subscription is the local mirror of one account's billing state. The payment
// provider is the source of truth; this copy exists so a page load does not
// make a network call.
type Subscription struct {
	TeamID            int64
	CustomerID        string
	SubscriptionID    string
	Status            string
	Plan              string
	PriceID           string
	CurrentPeriodEnd  time.Time
	CancelAtPeriodEnd bool
	BillingEmail      string
	PaymentState      string
	PaymentFailedAt   time.Time
	EvidenceSourceAt  int64
	EvidenceEventAt   int64
	EvidenceRank      int
	ReconciledEventAt int64
}

// Store is every read and write this package makes against system.db.
type Store struct {
	db *sql.DB

	// accountLocks serialize the provider read, mirror write, and lifecycle
	// signal for each account while allowing unrelated accounts to reconcile in
	// parallel.
	accountLocks sync.Map

	// Now is injectable so the webhook tests can assert on stored timestamps
	// without depending on when the suite runs.
	Now func() time.Time
}

// eventClaimLease allows a delivery whose process died mid-handler to be
// reclaimed. It is comfortably longer than the bounded Stripe API calls in one
// reconciliation, so a live handler cannot normally lose its claim.
const eventClaimLease = 5 * time.Minute

// outcomeProcessing is the transient event-log state held by the one delivery
// attempt allowed to execute a handler.
const outcomeProcessing = "processing"

// accountLeaseDuration is long enough for the bounded Stripe reads in one
// reconciliation. A dead process can be replaced after it expires.
const accountLeaseDuration = 5 * time.Minute

// accountLeasePoll keeps a second process responsive without busy-spinning on
// system.db while another process reconciles the same account.
const accountLeasePoll = 20 * time.Millisecond

// checkoutProviderRetryWindow stops automatic provider retries before Stripe
// may prune the idempotency result. A sessionless claim older than this fails
// closed for operator review; replacing it or retrying it after the provider's
// retention floor could create a second live Checkout Session.
const checkoutProviderRetryWindow = 23 * time.Hour

// NewStore builds a store over the system database.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// DB exposes the handle for callers that need to run their own statements
// against the same database.
func (s *Store) DB() *sql.DB {
	return s.db
}

// now returns the store's clock.
func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// LockAccount obtains this store's lock for one account and returns its
// unlock function. Keeping the lock on Store makes independently constructed
// billing services sharing that store use the same serialization boundary.
func (s *Store) LockAccount(teamID int64) func() {
	value, _ := s.accountLocks.LoadOrStore(teamID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()

	return lock.Unlock
}

// AccountLease is one process's durable ownership of an account billing
// critical section. Renew is also the fencing check used before side effects.
type AccountLease struct {
	store       *Store
	teamID      int64
	token       string
	localUnlock func()
	releaseOnce sync.Once
	done        chan struct{}
	wg          sync.WaitGroup
	mu          sync.Mutex
	lost        error
}

// Renew extends a live lease only while its token is still current and its old
// deadline has not passed. A worker that paused too long cannot resume after a
// replacement process has become eligible to acquire the account.
func (l *AccountLease) Renew(ctx context.Context) error {
	if l == nil || l.store == nil || l.token == "" {
		return nil
	}
	l.mu.Lock()
	lost := l.lost
	l.mu.Unlock()
	if lost != nil {
		return lost
	}

	now := l.store.now()
	result, err := l.store.db.ExecContext(ctx, `
		UPDATE billing_account_leases
		SET expires_at = ?, updated_at = ?
		WHERE team_id = ? AND token = ? AND expires_at > ?
	`, now.Add(accountLeaseDuration).Unix(), now.Unix(), l.teamID, l.token, now.Unix())
	if err != nil {
		return fmt.Errorf("billing: renew account %d lease: %w", l.teamID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("billing: renew account %d lease: affected rows: %w", l.teamID, err)
	}
	if rows != 1 {
		return fmt.Errorf("billing: account %d lease expired or was replaced", l.teamID)
	}

	return nil
}

// heartbeat renews a real-time critical section while provider pagination is in
// progress. Explicit Renew calls remain the fencing check before side effects.
func (l *AccountLease) heartbeat() {
	defer l.wg.Done()
	ticker := time.NewTicker(accountLeaseDuration / 3)
	defer ticker.Stop()

	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := l.Renew(ctx)
			cancel()
			if err != nil {
				l.mu.Lock()
				if l.lost == nil {
					l.lost = err
				}
				l.mu.Unlock()
				return
			}
		}
	}
}

// Release removes only this lease token and releases the process-local lock.
func (l *AccountLease) Release() {
	if l == nil {
		return
	}

	l.releaseOnce.Do(func() {
		if l.done != nil {
			close(l.done)
			l.wg.Wait()
		}
		if l.store != nil && l.token != "" {
			_, _ = l.store.db.Exec(`DELETE FROM billing_account_leases WHERE team_id = ? AND token = ?`, l.teamID, l.token)
		}
		if l.localUnlock != nil {
			l.localUnlock()
		}
	})
}

// AcquireAccountLease serialises billing work for one account across Store
// instances and processes. The returned lease removes only its own token, so an
// expired worker cannot unlock a newer worker's lease.
func (s *Store) AcquireAccountLease(ctx context.Context, teamID int64) (*AccountLease, error) {
	localUnlock := s.LockAccount(teamID)
	token, err := randomToken()
	if err != nil {
		localUnlock()
		return nil, err
	}

	ticker := time.NewTicker(accountLeasePoll)
	defer ticker.Stop()

	for {
		nowTime := s.now()
		now := nowTime.Unix()
		expires := nowTime.Add(accountLeaseDuration).Unix()

		result, err := s.db.ExecContext(ctx, `
			INSERT INTO billing_account_leases (team_id, token, expires_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (team_id) DO UPDATE SET
				token = excluded.token,
				expires_at = excluded.expires_at,
				updated_at = excluded.updated_at
			WHERE billing_account_leases.expires_at <= ?
		`, teamID, token, expires, now, now)
		if err != nil {
			// Team deletion cascades the lease row. A reconciler that was already
			// waiting may then lose the INSERT to the foreign key; let it enter the
			// read side so DeletionStarted can observe the immutable audit instead
			// of turning a completed deletion into a perpetual webhook retry.
			if deleting, inspectErr := s.DeletionStarted(ctx, teamID); inspectErr == nil && deleting {
				return &AccountLease{localUnlock: localUnlock}, nil
			}
			localUnlock()
			return nil, fmt.Errorf("billing: acquire account %d lease: %w", teamID, err)
		}

		rows, err := result.RowsAffected()
		if err != nil {
			localUnlock()
			return nil, fmt.Errorf("billing: acquire account %d lease: affected rows: %w", teamID, err)
		}
		if rows == 1 {
			lease := &AccountLease{
				store: s, teamID: teamID, token: token, localUnlock: localUnlock,
				done: make(chan struct{}),
			}
			lease.wg.Add(1)
			go lease.heartbeat()
			return lease, nil
		}

		select {
		case <-ctx.Done():
			localUnlock()
			return nil, fmt.Errorf("billing: acquire account %d lease: %w", teamID, ctx.Err())
		case <-ticker.C:
		}
	}
}

// randomToken returns an opaque claim value with enough entropy to make a
// stale claimant matching a replacement impossible in practice.
func randomToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("billing: generate claim token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// QuiescenceObject is a reversible Stripe mutation recorded before the provider
// call. The row survives a process crash and is removed only after restoration
// or the account's team cascade completes deletion.
type QuiescenceObject struct {
	CustomerID string
	Type       string
	ID         string
}

// RememberQuiescence durably records a provider object before mutating it.
func (s *Store) RememberQuiescence(ctx context.Context, teamID int64, customerID, objectType, id string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO billing_quiescence_objects (team_id, customer_id, object_type, stripe_id, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (team_id, object_type, stripe_id) DO NOTHING
	`, teamID, customerID, objectType, id, s.now().Unix())
	if err != nil {
		return fmt.Errorf("billing: remember %s %s quiescence for %d: %w", objectType, id, teamID, err)
	}

	return nil
}

// QuiescenceObjects lists every reversible mutation still owned by an account.
func (s *Store) QuiescenceObjects(ctx context.Context, teamID int64) ([]QuiescenceObject, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT customer_id, object_type, stripe_id
		FROM billing_quiescence_objects
		WHERE team_id = ?
		ORDER BY object_type, stripe_id
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("billing: list quiescence for %d: %w", teamID, err)
	}
	defer func() { _ = rows.Close() }()

	var objects []QuiescenceObject
	for rows.Next() {
		var object QuiescenceObject
		if err := rows.Scan(&object.CustomerID, &object.Type, &object.ID); err != nil {
			return nil, fmt.Errorf("billing: list quiescence for %d: %w", teamID, err)
		}
		objects = append(objects, object)
	}

	return objects, rows.Err()
}

// ForgetQuiescenceObject removes exactly one successfully restored provider
// object. Failed and not-yet-visited rows remain durable for the next retry.
func (s *Store) ForgetQuiescenceObject(ctx context.Context, teamID int64, object QuiescenceObject) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM billing_quiescence_objects
		WHERE team_id = ? AND customer_id = ? AND object_type = ? AND stripe_id = ?
	`, teamID, object.CustomerID, object.Type, object.ID)
	if err != nil {
		return fmt.Errorf("billing: clear %s %s quiescence for %d: %w", object.Type, object.ID, teamID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("billing: clear %s %s quiescence for %d: affected rows: %w", object.Type, object.ID, teamID, err)
	}
	if rows != 1 {
		return fmt.Errorf("billing: quiescence object %s %s for %d was not pending", object.Type, object.ID, teamID)
	}

	return nil
}

// ForgetQuiescence clears the durable restoration list after every provider
// object has been restored successfully.
func (s *Store) ForgetQuiescence(ctx context.Context, teamID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM billing_quiescence_objects WHERE team_id = ?`, teamID); err != nil {
		return fmt.Errorf("billing: clear quiescence for %d: %w", teamID, err)
	}

	return nil
}

// RememberAccountCustomer records one provider identity as belonging to an
// account without allowing a conflicting webhook to transfer it to another.
func (s *Store) RememberAccountCustomer(ctx context.Context, teamID int64, customerID string) error {
	if customerID == "" {
		return nil
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO billing_account_customers (customer_id, team_id, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (customer_id) DO UPDATE SET updated_at = excluded.updated_at
		WHERE billing_account_customers.team_id = excluded.team_id
	`, customerID, teamID, s.now().Unix(), s.now().Unix())
	if err != nil {
		return fmt.Errorf("billing: remember customer %s for %d: %w", customerID, teamID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("billing: remember customer %s for %d: affected rows: %w", customerID, teamID, err)
	}
	if rows != 1 {
		return fmt.Errorf("billing: customer %s is already owned by another account", customerID)
	}

	return nil
}

// AccountCustomers returns every durable provider identity tied to one team in
// deterministic order.
func (s *Store) AccountCustomers(ctx context.Context, teamID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT customer_id FROM billing_account_customers
		WHERE team_id = ? ORDER BY customer_id
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("billing: list customers for %d: %w", teamID, err)
	}
	defer func() { _ = rows.Close() }()

	var customers []string
	for rows.Next() {
		var customerID string
		if err := rows.Scan(&customerID); err != nil {
			return nil, fmt.Errorf("billing: list customers for %d: %w", teamID, err)
		}
		customers = append(customers, customerID)
	}

	return customers, rows.Err()
}

// Load reads one account's mirrored billing state. A team with no row has never
// been to checkout, which is the normal state of a trial and not an error.
func (s *Store) Load(ctx context.Context, teamID int64) (Subscription, error) {
	var (
		out             Subscription
		customer        sql.NullString
		subID           sql.NullString
		periodEnd       sql.NullInt64
		paymentFailedAt sql.NullInt64
		cancelAtEnd     int64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT stripe_customer_id, stripe_subscription_id, status, plan,
		       stripe_price_id, current_period_end, cancel_at_period_end, billing_email,
		       payment_state, payment_failed_at, evidence_source_created,
		       evidence_event_created, evidence_rank, reconciled_event_created
		FROM subscriptions WHERE team_id = ?
	`, teamID).Scan(&customer, &subID, &out.Status, &out.Plan, &out.PriceID, &periodEnd,
		&cancelAtEnd, &out.BillingEmail, &out.PaymentState, &paymentFailedAt,
		&out.EvidenceSourceAt, &out.EvidenceEventAt, &out.EvidenceRank, &out.ReconciledEventAt)

	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{TeamID: teamID, Status: "none"}, nil
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("billing: load %d: %w", teamID, err)
	}

	out.TeamID = teamID
	out.CustomerID = customer.String
	out.SubscriptionID = subID.String
	out.CancelAtPeriodEnd = cancelAtEnd != 0

	if periodEnd.Valid {
		out.CurrentPeriodEnd = time.Unix(periodEnd.Int64, 0).UTC()
	}
	if paymentFailedAt.Valid {
		out.PaymentFailedAt = time.Unix(paymentFailedAt.Int64, 0).UTC()
	}

	return out, nil
}

// DeletionStarted reports whether day-90 destruction has become terminal for
// an account. A late webhook may be older than local processing time, but once
// the immutable deletion audit exists it must never recreate billing state.
func (s *Store) DeletionStarted(ctx context.Context, teamID int64) (bool, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM account_deletions WHERE team_id = ?)
	`, teamID).Scan(&exists); err != nil {
		return false, fmt.Errorf("billing: inspect deletion %d: %w", teamID, err)
	}

	return exists == 1, nil
}

// RecoverableScheduledDeletion reports whether the only deletion intent is an
// unfinished scheduled claim. Settlement recovery may repair billing through
// this narrow state; an explicit owner intent remains terminal.
func (s *Store) RecoverableScheduledDeletion(ctx context.Context, teamID int64) (bool, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM account_deletions
			JOIN teams ON teams.id = account_deletions.team_id
			WHERE account_deletions.team_id = ?
			  AND account_deletions.completed_at IS NULL
			  AND account_deletions.owner_requested = 0
			  AND account_deletions.authoritative_at IS NULL
		)
	`, teamID).Scan(&exists); err != nil {
		return false, fmt.Errorf("billing: inspect recoverable deletion %d: %w", teamID, err)
	}

	return exists == 1, nil
}

// ReopenScheduledDeletionForRecovery removes the authoritative fence only when
// no irreversible deletion checkpoint exists. It is used when Stripe rejects
// the first finalizing operation because the invoice settled instead.
func (s *Store) ReopenScheduledDeletionForRecovery(ctx context.Context, teamID int64) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE account_deletions
		SET authoritative_at = NULL
		WHERE team_id = ? AND completed_at IS NULL AND owner_requested = 0
		  AND authoritative_at IS NOT NULL
		  AND local_removed_at IS NULL AND provider_removed_at IS NULL
		  AND control_removed_at IS NULL
	`, teamID)
	if err != nil {
		return fmt.Errorf("billing: reopen deletion %d for provider settlement: %w", teamID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("billing: reopen deletion %d for provider settlement: affected rows: %w", teamID, err)
	}
	if rows != 1 {
		return fmt.Errorf("billing: deletion %d is not safely recoverable", teamID)
	}

	return nil
}

// Save writes the mirror back. It is a full overwrite rather than a set of
// partial updates because the whole row is read from the provider in one go —
// updating some columns from a fresh read and leaving others from an older one
// is how a row ends up describing a state that never existed.
func (s *Store) Save(ctx context.Context, sub Subscription) error {
	_, err := s.save(ctx, sub, false)

	return err
}

// SaveReconciled writes a provider snapshot only when its event watermark is
// not older than the row already committed. The boolean is false when another
// process won with newer Stripe evidence.
func (s *Store) SaveReconciled(ctx context.Context, sub Subscription) (bool, error) {
	return s.save(ctx, sub, true)
}

// save performs the shared subscription upsert and optionally applies the
// durable event-timestamp compare-and-swap guard.
func (s *Store) save(ctx context.Context, sub Subscription, ordered bool) (bool, error) {
	now := s.now().Unix()

	var periodEnd any
	if !sub.CurrentPeriodEnd.IsZero() {
		periodEnd = sub.CurrentPeriodEnd.UTC().Unix()
	}
	var paymentFailedAt any
	if !sub.PaymentFailedAt.IsZero() {
		paymentFailedAt = sub.PaymentFailedAt.UTC().Unix()
	}

	cancelAtEnd := 0
	if sub.CancelAtPeriodEnd {
		cancelAtEnd = 1
	}

	guard := ""
	if ordered {
		guard = `
			WHERE excluded.stripe_subscription_id IS NOT subscriptions.stripe_subscription_id
			   OR excluded.reconciled_event_created > subscriptions.reconciled_event_created
			   OR (excluded.reconciled_event_created = subscriptions.reconciled_event_created
			       AND (excluded.evidence_source_created > subscriptions.evidence_source_created
			            OR (excluded.evidence_source_created = subscriptions.evidence_source_created
			                AND (excluded.evidence_event_created > subscriptions.evidence_event_created
			                     OR (excluded.evidence_event_created = subscriptions.evidence_event_created
			                         AND excluded.evidence_rank >= subscriptions.evidence_rank)))))`
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO subscriptions
			(team_id, stripe_customer_id, stripe_subscription_id, status, plan,
			 stripe_price_id, current_period_end, cancel_at_period_end, billing_email,
			 payment_state, payment_failed_at, evidence_source_created,
			 evidence_event_created, evidence_rank, reconciled_event_created,
			 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (team_id) DO UPDATE SET
			stripe_customer_id     = excluded.stripe_customer_id,
			stripe_subscription_id = excluded.stripe_subscription_id,
			status                 = excluded.status,
			plan                   = excluded.plan,
			stripe_price_id        = excluded.stripe_price_id,
			current_period_end     = excluded.current_period_end,
			cancel_at_period_end   = excluded.cancel_at_period_end,
			billing_email          = CASE WHEN excluded.billing_email <> ''
			                              THEN excluded.billing_email
			                              ELSE subscriptions.billing_email END,
			payment_state          = excluded.payment_state,
			payment_failed_at      = excluded.payment_failed_at,
			evidence_source_created = excluded.evidence_source_created,
			evidence_event_created  = excluded.evidence_event_created,
			evidence_rank           = excluded.evidence_rank,
			reconciled_event_created = excluded.reconciled_event_created,
			updated_at             = excluded.updated_at
	`+guard, sub.TeamID, nullIfEmpty(sub.CustomerID), nullIfEmpty(sub.SubscriptionID), sub.Status, sub.Plan,
		sub.PriceID, periodEnd, cancelAtEnd, sub.BillingEmail, sub.PaymentState, paymentFailedAt,
		sub.EvidenceSourceAt, sub.EvidenceEventAt, sub.EvidenceRank, sub.ReconciledEventAt, now, now)
	if err != nil {
		return false, fmt.Errorf("billing: save %d: %w", sub.TeamID, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("billing: save %d: affected rows: %w", sub.TeamID, err)
	}

	return rows == 1, nil
}

// CheckoutClaim is the durable intent written before a hosted checkout is
// created. Its idempotency key remains stable across retries and restarts.
type CheckoutClaim struct {
	TeamID         int64
	Plan           string
	PriceID        string
	IdempotencyKey string
	SessionID      string
	SessionURL     string
	Status         string
	ClaimToken     string
	ExpiresAt      time.Time
	CustomerID     string
	BillingEmail   string
}

// Expired reports whether a sessionless provider result is now indeterminate.
// It must not be replaced automatically: after Stripe's documented 24-hour
// idempotency floor, neither reusing nor changing the key can prove uniqueness.
func (c CheckoutClaim) Expired(now time.Time) bool {
	return c.Status == "creating" && c.SessionID == "" && !c.ExpiresAt.After(now.UTC())
}

// CheckoutClaimForAccount loads an existing checkout intent, returning false
// when the account has never started one.
func (s *Store) CheckoutClaimForAccount(ctx context.Context, teamID int64) (CheckoutClaim, bool, error) {
	var claim CheckoutClaim

	var expiresAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT team_id, plan, stripe_price_id, idempotency_key, session_id,
		       session_url, status, claim_token, claim_expires_at,
		       customer_id, billing_email
		FROM billing_checkouts WHERE team_id = ?
	`, teamID).Scan(&claim.TeamID, &claim.Plan, &claim.PriceID, &claim.IdempotencyKey,
		&claim.SessionID, &claim.SessionURL, &claim.Status, &claim.ClaimToken, &expiresAt,
		&claim.CustomerID, &claim.BillingEmail)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckoutClaim{}, false, nil
	}
	if err != nil {
		return CheckoutClaim{}, false, fmt.Errorf("billing: load checkout claim %d: %w", teamID, err)
	}

	claim.ExpiresAt = time.Unix(expiresAt, 0).UTC()

	return claim, true, nil
}

// NewCheckoutClaim replaces an absent or explicitly retired checkout with a
// fresh intent. Sessionless claims are never replaced merely because time
// passed; Checkout must recover them with their original idempotency key or
// fail closed before Stripe may prune that key.
func (s *Store) NewCheckoutClaim(ctx context.Context, teamID int64, plan, priceID, customerID, billingEmail string) (CheckoutClaim, error) {
	token, err := randomToken()
	if err != nil {
		return CheckoutClaim{}, err
	}

	now := s.now().Unix()
	claim := CheckoutClaim{
		TeamID:         teamID,
		Plan:           plan,
		PriceID:        priceID,
		IdempotencyKey: fmt.Sprintf("checkout-%d-%s", teamID, token),
		Status:         "creating",
		ClaimToken:     token,
		ExpiresAt:      s.now().Add(checkoutProviderRetryWindow),
		CustomerID:     customerID,
		BillingEmail:   billingEmail,
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO billing_checkouts
			(team_id, plan, stripe_price_id, idempotency_key, session_id,
			 session_url, status, claim_token, claim_expires_at, customer_id,
			 billing_email, created_at, updated_at)
		VALUES (?, ?, ?, ?, '', '', 'creating', ?, ?, ?, ?, ?, ?)
		ON CONFLICT (team_id) DO UPDATE SET
			plan = excluded.plan,
			stripe_price_id = excluded.stripe_price_id,
			idempotency_key = excluded.idempotency_key,
			session_id = '',
			session_url = '',
			status = 'creating',
			claim_token = excluded.claim_token,
			claim_expires_at = excluded.claim_expires_at,
			customer_id = excluded.customer_id,
			billing_email = excluded.billing_email,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at
		WHERE billing_checkouts.status = 'expired'
	`, teamID, plan, priceID, claim.IdempotencyKey, token,
		claim.ExpiresAt.Unix(), customerID, billingEmail, now, now)
	if err != nil {
		return CheckoutClaim{}, fmt.Errorf("billing: create checkout claim %d: %w", teamID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return CheckoutClaim{}, fmt.Errorf("billing: create checkout claim %d: affected rows: %w", teamID, err)
	}
	if affected != 1 {
		return CheckoutClaim{}, fmt.Errorf("billing: create checkout claim %d: an unexpired claim already exists", teamID)
	}

	return claim, nil
}

// ErrCheckoutClaimReplaced reports that a late provider response belongs to an
// expired claim. SaveCheckoutSession records the session for durable cleanup
// before returning this error.
var ErrCheckoutClaimReplaced = errors.New("billing: checkout claim was replaced")

// SaveCheckoutSession attaches Stripe's response to the pre-existing claim.
// Matching the token means a stale retry cannot overwrite a replacement claim;
// a mismatch atomically records the late session for cleanup.
func (s *Store) SaveCheckoutSession(ctx context.Context, claim CheckoutClaim, sessionID, sessionURL, status string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("billing: save checkout session %d: %w", claim.TeamID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	result, err := tx.ExecContext(ctx, `
		UPDATE billing_checkouts
		SET session_id = ?, session_url = ?, status = ?, updated_at = ?
		WHERE team_id = ? AND claim_token = ?
	`, sessionID, sessionURL, status, s.now().Unix(), claim.TeamID, claim.ClaimToken)
	if err != nil {
		return fmt.Errorf("billing: save checkout session %d: %w", claim.TeamID, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("billing: save checkout session %d: affected rows: %w", claim.TeamID, err)
	}
	if rows != 1 {
		now := s.now().Unix()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO billing_checkout_cleanup (session_id, team_id, created_at, updated_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (session_id) DO UPDATE SET updated_at = excluded.updated_at
		`, sessionID, claim.TeamID, now, now); err != nil {
			return fmt.Errorf("billing: remember late checkout session %s: %w", sessionID, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("billing: remember late checkout session %s: %w", sessionID, err)
		}
		return ErrCheckoutClaimReplaced
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("billing: save checkout session %d: %w", claim.TeamID, err)
	}

	return nil
}

// CheckoutCleanupSessions lists late sessions whose provider expiration has not
// yet been acknowledged.
func (s *Store) CheckoutCleanupSessions(ctx context.Context, teamID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id FROM billing_checkout_cleanup WHERE team_id = ? ORDER BY session_id
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("billing: list checkout cleanup for %d: %w", teamID, err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("billing: list checkout cleanup for %d: %w", teamID, err)
		}
		sessions = append(sessions, id)
	}

	return sessions, rows.Err()
}

// RememberCheckoutCleanup records a provider session before attempting to
// expire it, including sessions discovered only through provider metadata.
func (s *Store) RememberCheckoutCleanup(ctx context.Context, teamID int64, sessionID string) error {
	now := s.now().Unix()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO billing_checkout_cleanup (session_id, team_id, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (session_id) DO UPDATE SET updated_at = excluded.updated_at
	`, sessionID, teamID, now, now); err != nil {
		return fmt.Errorf("billing: remember checkout cleanup %s: %w", sessionID, err)
	}

	return nil
}

// FinishCheckoutCleanup forgets a session only after Stripe confirms it is no
// longer usable.
func (s *Store) FinishCheckoutCleanup(ctx context.Context, sessionID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM billing_checkout_cleanup WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("billing: finish checkout cleanup %s: %w", sessionID, err)
	}

	return nil
}

// MarkCheckoutStatus advances a checkout intent without changing its stable
// identity. Complete intents block every later attempt for the account.
func (s *Store) MarkCheckoutStatus(ctx context.Context, teamID int64, status string) error {
	if status != "open" && status != "complete" && status != "expired" {
		return fmt.Errorf("billing: invalid checkout status %q", status)
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE billing_checkouts SET status = ?, updated_at = ? WHERE team_id = ?
	`, status, s.now().Unix(), teamID)
	if err != nil {
		return fmt.Errorf("billing: mark checkout %d %s: %w", teamID, status, err)
	}

	return nil
}

// MarkCheckoutClaimStatus advances only the checkout intent identified by its
// unguessable claim token. Account leases serialize healthy workers, while this
// second fence stops a paused worker whose lease expired from mutating the
// replacement claim after it resumes.
func (s *Store) MarkCheckoutClaimStatus(ctx context.Context, claim CheckoutClaim, status string) error {
	if status != "open" && status != "complete" && status != "expired" {
		return fmt.Errorf("billing: invalid checkout status %q", status)
	}
	if claim.ClaimToken == "" {
		return fmt.Errorf("billing: checkout claim %d has no claim token", claim.TeamID)
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE billing_checkouts SET status = ?, updated_at = ?
		WHERE team_id = ? AND claim_token = ?
	`, status, s.now().Unix(), claim.TeamID, claim.ClaimToken)
	if err != nil {
		return fmt.Errorf("billing: mark checkout claim %d %s: %w", claim.TeamID, status, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("billing: mark checkout claim %d %s: affected rows: %w", claim.TeamID, status, err)
	}
	if rows != 1 {
		return fmt.Errorf("%w: team %d", ErrCheckoutClaimReplaced, claim.TeamID)
	}

	return nil
}

// TeamForCustomer maps a payment-provider customer back to an account. It is
// the fallback when an event carries no metadata — which is every event created
// by somebody clicking around the provider's own dashboard.
func (s *Store) TeamForCustomer(ctx context.Context, customerID string) (int64, error) {
	if customerID == "" {
		return 0, nil
	}

	var teamID int64

	err := s.db.QueryRowContext(ctx, `
		SELECT team_id FROM billing_account_customers WHERE customer_id = ?
	`, customerID).Scan(&teamID)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.db.QueryRowContext(ctx, `
			SELECT team_id FROM subscriptions WHERE stripe_customer_id = ?
		`, customerID).Scan(&teamID)
	}

	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("billing: team for customer %s: %w", customerID, err)
	}

	return teamID, nil
}

// EventStatus is what happened to one webhook delivery.
const (
	// OutcomeApplied means the delivery changed something.
	OutcomeApplied = "applied"

	// OutcomeIgnored means we recorded it and did nothing, because it is a type
	// this product does not act on.
	OutcomeIgnored = "ignored"

	// OutcomeDuplicate means we had already applied this event id.
	OutcomeDuplicate = "duplicate"

	// OutcomeError means the handler failed. The delivery is left unhandled so
	// the provider's own retry can run it again, which is safe because the
	// handler reconciles from current state rather than replaying the event.
	OutcomeError = "error"
)

// EventClaim identifies the atomic processing lease held for one event.
type EventClaim struct {
	Claimed    bool
	Processing bool
	StartedAt  int64
}

// ClaimEvent records a delivery and atomically claims its processing lease.
//
// A brand-new event or a failed prior attempt is claimed and handled. An event
// already applied or currently handled is a duplicate and skipped. A processing
// claim older than the lease can be reclaimed after a process crash.
func (s *Store) ClaimEvent(ctx context.Context, eventID, eventType string, teamID int64, payload []byte) (EventClaim, error) {
	now := s.now().Unix()
	staleBefore := s.now().Add(-eventClaimLease).Unix()

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO stripe_events
			(event_id, type, team_id, payload, received_at, handled_at, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (event_id) DO UPDATE SET
			team_id    = COALESCE(excluded.team_id, stripe_events.team_id),
			handled_at = excluded.handled_at,
			outcome    = excluded.outcome,
			error      = ''
		WHERE stripe_events.outcome IN ('', ?)
		   OR (stripe_events.outcome = ? AND stripe_events.handled_at <= ?)
	`, eventID, eventType, nullIfZero(teamID), string(payload), now, now, outcomeProcessing,
		OutcomeError, outcomeProcessing, staleBefore)
	if err != nil {
		return EventClaim{}, fmt.Errorf("billing: claim event %s: %w", eventID, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return EventClaim{}, fmt.Errorf("billing: claim event %s: affected rows: %w", eventID, err)
	}

	if rows == 1 {
		return EventClaim{Claimed: true, StartedAt: now}, nil
	}
	// A prior delivery may have arrived before its customer could be routed.
	// Persist ownership learned now even if that delivery is already handled so
	// account deletion can erase its verbatim payload.
	if teamID > 0 {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE stripe_events SET team_id = COALESCE(team_id, ?) WHERE event_id = ?
		`, teamID, eventID); err != nil {
			return EventClaim{}, fmt.Errorf("billing: bind event %s to team: %w", eventID, err)
		}
	}

	var outcome string
	if err := s.db.QueryRowContext(ctx, `SELECT outcome FROM stripe_events WHERE event_id = ?`, eventID).Scan(&outcome); err != nil {
		return EventClaim{}, fmt.Errorf("billing: inspect event claim %s: %w", eventID, err)
	}

	return EventClaim{Processing: outcome == outcomeProcessing}, nil
}

// FinishEvent records what the handler decided.
func (s *Store) FinishEvent(ctx context.Context, eventID string, claim EventClaim, outcome string, teamID int64, handlerErr error) error {
	message := ""
	if handlerErr != nil {
		message = handlerErr.Error()
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE stripe_events
		SET handled_at = ?, outcome = ?, error = ?, team_id = COALESCE(?, team_id)
		WHERE event_id = ? AND outcome = ? AND handled_at = ?
	`, s.now().Unix(), outcome, message, nullIfZero(teamID), eventID, outcomeProcessing, claim.StartedAt)
	if err != nil {
		return fmt.Errorf("billing: finish event %s: %w", eventID, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("billing: finish event %s: affected rows: %w", eventID, err)
	}
	if rows != 1 {
		return fmt.Errorf("billing: finish event %s: processing claim was lost", eventID)
	}

	return nil
}

// EventPayloads returns authenticated event bodies for one account and set of
// types. Callers decode them with the provider model instead of extracting JSON
// fields with string matching.
func (s *Store) EventPayloads(ctx context.Context, teamID int64, eventTypes []string) ([][]byte, error) {
	if teamID < 1 || len(eventTypes) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(eventTypes))
	args := make([]any, 0, len(eventTypes)+1)
	args = append(args, teamID)

	for i, eventType := range eventTypes {
		placeholders[i] = "?"
		args = append(args, eventType)
	}

	query := `
		SELECT payload
		FROM stripe_events
		WHERE team_id = ? AND outcome <> 'error' AND type IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY id DESC
	`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("billing: event payloads for account %d: %w", teamID, err)
	}
	defer func() { _ = rows.Close() }()

	var payloads [][]byte
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("billing: event payloads for account %d: %w", teamID, err)
		}

		payloads = append(payloads, payload)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("billing: event payloads for account %d: %w", teamID, err)
	}

	return payloads, nil
}

// LoggedEvent is one delivery as the support screen shows it.
type LoggedEvent struct {
	EventID    string
	Type       string
	TeamID     int64
	ReceivedAt time.Time
	HandledAt  time.Time
	Outcome    string
	Error      string
}

// RecentEvents lists the last deliveries, newest first, optionally for one
// account. This is what "logged where support can read it" means in practice:
// a person answering "they say they paid" needs the events, their order, and
// what each one did.
func (s *Store) RecentEvents(ctx context.Context, teamID int64, limit int) ([]LoggedEvent, error) {
	query := `
		SELECT event_id, type, COALESCE(team_id, 0), received_at, handled_at, outcome, error
		FROM stripe_events
	`
	args := []any{}

	if teamID > 0 {
		query += " WHERE team_id = ?"
		args = append(args, teamID)
	}

	query += " ORDER BY received_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("billing: recent events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []LoggedEvent

	for rows.Next() {
		var (
			entry    LoggedEvent
			received int64
			handled  sql.NullInt64
		)

		if err := rows.Scan(&entry.EventID, &entry.Type, &entry.TeamID, &received, &handled, &entry.Outcome, &entry.Error); err != nil {
			return nil, fmt.Errorf("billing: recent events: %w", err)
		}

		entry.ReceivedAt = time.Unix(received, 0).UTC()
		if handled.Valid {
			entry.HandledAt = time.Unix(handled.Int64, 0).UTC()
		}

		out = append(out, entry)
	}

	return out, rows.Err()
}

// nullIfEmpty writes NULL instead of an empty string, so that a unique index on
// a provider id does not collide across every account that has none.
func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}

	return value
}

// nullIfZero writes NULL instead of a zero id, so an event we could not route
// is visibly unrouted rather than attributed to account zero.
func nullIfZero(id int64) any {
	if id < 1 {
		return nil
	}

	return id
}
