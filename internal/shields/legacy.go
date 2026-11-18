//
// legacy.go
// Moving hostnames allowed into the retired system-database list onto shield rules.
//
// Created: 2026-11-18
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package shields

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// LegacyHostnamesTable is the retired system-database list of allowed
// hostnames. The writer decides hostnames by shield rules alone, so `db
// migrate` carries any row left in this table onto a hostname shield rule.
const LegacyHostnamesTable = "site_allowed_hostnames"

// LegacyHostname is one row of the retired table, with the site's storage
// account and registered domain it needs to be carried.
type LegacyHostname struct {
	ID        int64
	SiteID    int64
	AccountID int64
	Domain    string
	Hostname  string
	CreatedAt int64
}

// ReadLegacyHostnames returns every row of the retired table, joined to its
// site. A database without the table returns nothing, so the carry is safe on
// an install that never had it or has already dropped it.
func ReadLegacyHostnames(ctx context.Context, system *sql.DB) ([]LegacyHostname, error) {
	var name string

	err := system.QueryRowContext(ctx,
		"SELECT name FROM sqlite_schema WHERE type = 'table' AND name = ?", LegacyHostnamesTable).Scan(&name)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("shields: look for %s: %w", LegacyHostnamesTable, err)
	}

	rows, err := system.QueryContext(ctx, `
		SELECT allowed.id, allowed.site_id, sites.account_id, sites.domain, allowed.hostname, allowed.created_at
		FROM site_allowed_hostnames AS allowed
		JOIN sites ON sites.id = allowed.site_id
		ORDER BY allowed.id
	`)
	if err != nil {
		return nil, fmt.Errorf("shields: read %s: %w", LegacyHostnamesTable, err)
	}
	defer func() { _ = rows.Close() }()

	var out []LegacyHostname

	for rows.Next() {
		var row LegacyHostname
		if err := rows.Scan(&row.ID, &row.SiteID, &row.AccountID, &row.Domain, &row.Hostname, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("shields: read %s: %w", LegacyHostnamesTable, err)
		}
		out = append(out, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shields: read %s: %w", LegacyHostnamesTable, err)
	}

	return out, nil
}

// CarryLegacyHostnames stores one account's legacy rows as hostname rules and
// reports how many rules it added.
//
// A row naming the site's own domain or one of its subdomains is skipped: the
// table holds the registered domain beside each hostname, and the shield rules
// accept the domain and its subdomains without a rule. A row
// already present as a rule is left as it is, so running the carry twice adds
// nothing. The per-site cap is not applied, because a hostname somebody
// allowed must not be lost to a limit that did not exist when they allowed it.
func CarryLegacyHostnames(ctx context.Context, account *sql.DB, rows []LegacyHostname, note string) (int, error) {
	tx, err := account.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("shields: carry hostnames: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	added := 0

	for _, row := range rows {
		value, err := Normalise(KindHostname, row.Hostname)
		if err != nil {
			continue
		}

		domain := normaliseHostname(row.Domain)
		if value == domain || strings.HasSuffix(value, "."+domain) {
			continue
		}

		result, err := tx.ExecContext(ctx, `
			INSERT INTO shield_rules (site_id, kind, value, note, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (site_id, kind, value) DO NOTHING
		`, row.SiteID, KindHostname, value, note, row.CreatedAt)
		if err != nil {
			return 0, fmt.Errorf("shields: carry hostname for site %d: %w", row.SiteID, err)
		}

		if inserted, err := result.RowsAffected(); err == nil {
			added += int(inserted)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("shields: carry hostnames: %w", err)
	}

	return added, nil
}

// ForgetLegacyHostnames deletes rows that have been carried. Only the named
// rows go, so a row whose account could not be reached stays for the next run.
func ForgetLegacyHostnames(ctx context.Context, system *sql.DB, rows []LegacyHostname) error {
	tx, err := system.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("shields: forget carried hostnames: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after commit is a no-op

	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, "DELETE FROM site_allowed_hostnames WHERE id = ?", row.ID); err != nil {
			return fmt.Errorf("shields: forget carried hostname %d: %w", row.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("shields: forget carried hostnames: %w", err)
	}

	return nil
}
