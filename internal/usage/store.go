//
// store.go
// The counters, the notices already sent, and the state of the overage conversation.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Store is every read and write this package makes against system.db. It is a
// type rather than loose functions so a caller holds one thing, and so the
// clock can be injected: the two-week reply window and the month boundaries are
// both time arithmetic that has to be testable without waiting.
type Store struct {
	db *sql.DB

	// Now is injectable so a test can drive a three-month sequence in
	// microseconds. It defaults to the system clock.
	Now func() time.Time
}

// NewStore builds a store over the system database.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// now returns the store's clock.
func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// Add increments an account's counters for a period. It is an upsert rather
// than a read-modify-write because several shards may be flushing the same
// account at once, and the arithmetic has to happen inside SQLite where it is
// atomic rather than in Go where it is a race.
func (s *Store) Add(ctx context.Context, teamID int64, period string, counts Counts) error {
	if counts.Pageviews == 0 && counts.CustomEvents == 0 {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_counters (team_id, period, pageviews, custom_events, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (team_id, period) DO UPDATE SET
			pageviews     = pageviews + excluded.pageviews,
			custom_events = custom_events + excluded.custom_events,
			updated_at    = excluded.updated_at
	`, teamID, period, counts.Pageviews, counts.CustomEvents, s.now().Unix())
	if err != nil {
		return fmt.Errorf("usage: add %d/%s: %w", teamID, period, err)
	}

	return nil
}

// Get reads one account's counts for one period. A period with no row is zero
// rather than an error: an account that has sent nothing this month is the
// normal state of a new account, not a missing record.
func (s *Store) Get(ctx context.Context, teamID int64, period string) (Counts, error) {
	var counts Counts

	err := s.db.QueryRowContext(ctx, `
		SELECT pageviews, custom_events FROM usage_counters WHERE team_id = ? AND period = ?
	`, teamID, period).Scan(&counts.Pageviews, &counts.CustomEvents)

	if errors.Is(err, sql.ErrNoRows) {
		return Counts{}, nil
	}
	if err != nil {
		return Counts{}, fmt.Errorf("usage: read %d/%s: %w", teamID, period, err)
	}

	return counts, nil
}

// History reads an account's last n periods, newest first. The billing screen
// draws it, and the consecutive-months rule counts it.
func (s *Store) History(ctx context.Context, teamID int64, limit int) ([]PeriodCounts, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT period, pageviews, custom_events
		FROM usage_counters
		WHERE team_id = ?
		ORDER BY period DESC
		LIMIT ?
	`, teamID, limit)
	if err != nil {
		return nil, fmt.Errorf("usage: history %d: %w", teamID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []PeriodCounts

	for rows.Next() {
		var entry PeriodCounts
		if err := rows.Scan(&entry.Period, &entry.Counts.Pageviews, &entry.Counts.CustomEvents); err != nil {
			return nil, fmt.Errorf("usage: history %d: %w", teamID, err)
		}

		out = append(out, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: history %d: %w", teamID, err)
	}

	return out, nil
}

// PeriodCounts is one month of one account's usage.
type PeriodCounts struct {
	Period string
	Counts Counts
}

// Teams lists the accounts with any usage in a period. The ladder sweeps this
// rather than every team, because an account that has sent nothing cannot
// possibly be over its limit and reading it would be pure cost.
func (s *Store) Teams(ctx context.Context, period string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT team_id FROM usage_counters
		WHERE period = ?
		  AND NOT EXISTS (SELECT 1 FROM account_comps WHERE account_comps.team_id = usage_counters.team_id)
		ORDER BY team_id
	`, period)
	if err != nil {
		return nil, fmt.Errorf("usage: teams for %s: %w", period, err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("usage: teams for %s: %w", period, err)
		}

		ids = append(ids, id)
	}

	return ids, rows.Err()
}

// OverageTeams lists the accounts with a conversation in progress. The sweeper
// unions it with the accounts that have usage this month, because an account
// that stopped sending traffic entirely still has a reply deadline running and
// still has to be unlocked when it comes back into range — and it would never be
// looked at again if the walk were driven by this month's counters alone.
func (s *Store) OverageTeams(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT team_id FROM usage_overages
		WHERE NOT EXISTS (SELECT 1 FROM account_comps WHERE account_comps.team_id = usage_overages.team_id)
		ORDER BY team_id
	`)
	if err != nil {
		return nil, fmt.Errorf("usage: overage teams: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("usage: overage teams: %w", err)
		}

		ids = append(ids, id)
	}

	return ids, rows.Err()
}

// ConsecutiveOver counts how many complete months in a row, ending with the one
// before `period`, the account was over the limit. The month in progress is
// excluded deliberately: a month is not "over" until it has finished, and
// counting a partial one would lock somebody on the third of the month.
func (s *Store) ConsecutiveOver(ctx context.Context, teamID int64, period string) (int, error) {
	count := 0
	cursor := period

	for i := 0; i < 24; i++ {
		previous, err := PreviousPeriod(cursor)
		if err != nil {
			return 0, err
		}

		counts, err := s.Get(ctx, teamID, previous)
		if err != nil {
			return 0, err
		}

		if counts.Billable() <= MonthlyLimit {
			return count, nil
		}

		count++
		cursor = previous
	}

	return count, nil
}

// NoticeSent reports whether one rung's email has already gone out this month.
// It is a read in front of a unique constraint rather than instead of one: the
// constraint is the guarantee, and this only avoids rendering a message that
// would be rejected on insert.
func (s *Store) NoticeSent(ctx context.Context, teamID int64, period string, level Level) (bool, error) {
	var one int

	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM usage_notices WHERE team_id = ? AND period = ? AND threshold = ?
	`, teamID, period, string(level)).Scan(&one)

	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("usage: notice check %d/%s: %w", teamID, period, err)
	}

	return true, nil
}

// RecordNotice marks one rung's email as sent. A conflict means another sweep
// beat us to it, which is a success rather than an error — the customer got the
// message exactly once, which is the whole point.
func (s *Store) RecordNotice(ctx context.Context, teamID int64, period string, level Level) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_notices (team_id, period, threshold, sent_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (team_id, period, threshold) DO NOTHING
	`, teamID, period, string(level), s.now().Unix())
	if err != nil {
		return false, fmt.Errorf("usage: record notice %d/%s: %w", teamID, period, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("usage: record notice %d/%s: %w", teamID, period, err)
	}

	return affected > 0, nil
}

// Overage reads the state of the conversation with one account. A missing row
// is the zero value, which means no conversation is in progress.
func (s *Store) Overage(ctx context.Context, teamID int64) (Overage, error) {
	var (
		out                              Overage
		asked, deadline, replied, locked sql.NullInt64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT period, asked_at, reply_deadline, replied_at, locked_at
		FROM usage_overages WHERE team_id = ?
	`, teamID).Scan(&out.Period, &asked, &deadline, &replied, &locked)

	if errors.Is(err, sql.ErrNoRows) {
		return Overage{}, nil
	}
	if err != nil {
		return Overage{}, fmt.Errorf("usage: overage %d: %w", teamID, err)
	}

	out.AskedAt = fromUnix(asked)
	out.ReplyDeadline = fromUnix(deadline)
	out.RepliedAt = fromUnix(replied)
	out.LockedAt = fromUnix(locked)

	return out, nil
}

// SaveOverage writes the conversation state back.
func (s *Store) SaveOverage(ctx context.Context, teamID int64, overage Overage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO usage_overages (team_id, period, asked_at, reply_deadline, replied_at, locked_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (team_id) DO UPDATE SET
			period         = excluded.period,
			asked_at       = excluded.asked_at,
			reply_deadline = excluded.reply_deadline,
			replied_at     = excluded.replied_at,
			locked_at      = excluded.locked_at,
			updated_at     = excluded.updated_at
	`, teamID, overage.Period, toUnix(overage.AskedAt), toUnix(overage.ReplyDeadline),
		toUnix(overage.RepliedAt), toUnix(overage.LockedAt), s.now().Unix())
	if err != nil {
		return fmt.Errorf("usage: save overage %d: %w", teamID, err)
	}

	return nil
}

// ClearOverage ends the conversation, which is what coming back into range
// does. The row is deleted rather than flagged so that a later overage starts a
// fresh conversation with a fresh two-week window.
func (s *Store) ClearOverage(ctx context.Context, teamID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM usage_overages WHERE team_id = ?`, teamID); err != nil {
		return fmt.Errorf("usage: clear overage %d: %w", teamID, err)
	}

	return nil
}

// MarkReplied records that a person answered. It is called from the support
// command rather than from anything automatic, because there is no automatic
// signal that a conversation happened.
func (s *Store) MarkReplied(ctx context.Context, teamID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE usage_overages SET replied_at = ?, locked_at = NULL, updated_at = ? WHERE team_id = ?
	`, s.now().Unix(), s.now().Unix(), teamID)
	if err != nil {
		return fmt.Errorf("usage: mark replied %d: %w", teamID, err)
	}

	return nil
}

// LockedTeams lists the accounts whose dashboard is locked for volume. The
// access gate loads it into memory rather than querying per request, for the
// same reason the routing map is a snapshot.
func (s *Store) LockedTeams(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT team_id FROM usage_overages
		WHERE locked_at IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM account_comps WHERE account_comps.team_id = usage_overages.team_id)
		ORDER BY team_id
	`)
	if err != nil {
		return nil, fmt.Errorf("usage: locked teams: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("usage: locked teams: %w", err)
		}

		ids = append(ids, id)
	}

	return ids, rows.Err()
}

// toUnix converts a time to the nullable integer the schema stores. A zero time
// is NULL rather than 1970, so "never replied" and "replied at the epoch" can
// never be confused.
func toUnix(at time.Time) any {
	if at.IsZero() {
		return nil
	}

	return at.UTC().Unix()
}

// fromUnix converts a nullable column back, mapping NULL to the zero time.
func fromUnix(value sql.NullInt64) time.Time {
	if !value.Valid {
		return time.Time{}
	}

	return time.Unix(value.Int64, 0).UTC()
}
