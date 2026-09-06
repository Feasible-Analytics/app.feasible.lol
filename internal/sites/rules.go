//
// rules.go
// When each account last changed a rule the ingest path consults.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package sites

import (
	"context"
	"fmt"
	"time"
)

// StampRules records that an account's shield or path-cleaning rules changed.
//
// It is one row in system.db rather than anything in the account database,
// because the point is to let a refresh decide whether to open that account at
// all — and a marker inside it could only be read by opening it.
//
// Both rule kinds share one marker. A shield edit therefore also re-reads path
// rules, which costs one extra small query on an account that was being opened
// anyway, and is a great deal simpler than two columns that can disagree.
func (c *Cache) StampRules(ctx context.Context, accountID int64, now time.Time) error {
	if c == nil || c.db == nil || accountID == 0 {
		return nil
	}

	_, err := c.db.ExecContext(ctx, `
		INSERT INTO account_rule_versions (account_id, changed_at) VALUES (?, ?)
		ON CONFLICT (account_id) DO UPDATE SET changed_at = excluded.changed_at
	`, accountID, now.UTC().UnixNano())
	if err != nil {
		return fmt.Errorf("sites: stamp rule change for account %d: %w", accountID, err)
	}

	return nil
}

// RuleVersions reads every account's marker in one query.
//
// Nanoseconds, not seconds: two edits inside the same second are two edits, and
// a refresh that compared seconds would miss the second one for an hour.
func (c *Cache) RuleVersions(ctx context.Context) (map[int64]int64, error) {
	versions := map[int64]int64{}

	if c.db == nil {
		return versions, nil
	}

	rows, err := c.db.QueryContext(ctx, "SELECT account_id, changed_at FROM account_rule_versions")
	if err != nil {
		return nil, fmt.Errorf("sites: read rule versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var accountID, changedAt int64

		if err := rows.Scan(&accountID, &changedAt); err != nil {
			return nil, fmt.Errorf("sites: read rule versions: %w", err)
		}

		versions[accountID] = changedAt
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sites: read rule versions: %w", err)
	}

	return versions, nil
}
