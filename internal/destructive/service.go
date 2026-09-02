//
// service.go
// Durable claims around analytics resets, site deletion and account purges.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package destructive coordinates operations that span system.db and an
// account analytics database. SQLite cannot transact across those files, so a
// durable control-row claim is the lock transfers consult while analytics are
// erased and the system mutation is committed.
package destructive

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
)

// LeaseDuration is how soon a crashed operation may be retried. The durable
// row blocks transfers even after this lease; expiry only permits another
// destructive worker to resume the same idempotent workflow.
const LeaseDuration = time.Minute

// Operation kinds persisted in the destructive_operations table.
const (
	KindSiteReset  = "site_reset"
	KindSiteDelete = "site_delete"
)

// Errors callers map to a conflict or an ownership refusal.
var (
	ErrNotFound = errors.New("destructive: resource not found for this owner")
	ErrBusy     = errors.New("destructive: another operation is in progress")
)

// Service coordinates control state with per-account analytics storage.
type Service struct {
	DB       *sql.DB
	Accounts *accounts.Manager
	Now      func() time.Time
	Lease    time.Duration
}

// claim is the durable identity and ownership snapshot for one operation.
type claim struct {
	Kind      string
	SiteID    int64
	OwnerID   int64
	AccountID int64
	Token     string
}

// now reads the service clock in UTC.
func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// leaseDuration returns the configured test lease or the production default.
func (s *Service) leaseDuration() time.Duration {
	if s.Lease <= 0 {
		return LeaseDuration
	}

	return s.Lease
}

// ResetSite erases one site's analytics while preserving its control row.
func (s *Service) ResetSite(ctx context.Context, ownerTeamID, siteID int64) error {
	operation, err := s.claimSite(ctx, ownerTeamID, siteID, KindSiteReset)
	if err != nil {
		return err
	}

	if err := s.eraseSite(ctx, operation); err != nil {
		return err
	}
	if err := s.markAnalyticsDeleted(ctx, operation); err != nil {
		return err
	}

	return s.finishReset(ctx, operation)
}

// DeleteSite erases analytics and then removes the site's control row. A crash
// after either step is safe to retry because erasure is idempotent and the
// tombstone records which step committed.
func (s *Service) DeleteSite(ctx context.Context, ownerTeamID, siteID int64) error {
	operation, err := s.claimSite(ctx, ownerTeamID, siteID, KindSiteDelete)
	if err != nil {
		return err
	}

	if err := s.eraseSite(ctx, operation); err != nil {
		return err
	}
	if err := s.markAnalyticsDeleted(ctx, operation); err != nil {
		return err
	}

	return s.finishDelete(ctx, operation)
}

// claimSite validates current ownership after taking system.db's writer lock
// and creates or reclaims the durable tombstone. A different operation cannot
// replace a tombstone because analytics may already have been erased.
func (s *Service) claimSite(ctx context.Context, ownerTeamID, siteID int64, kind string) (claim, error) {
	token, err := operationToken()
	if err != nil {
		return claim{}, err
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return claim{}, fmt.Errorf("destructive: claim site: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	operation := claim{Kind: kind, SiteID: siteID, OwnerID: ownerTeamID, Token: token}
	var currentOwner int64
	err = tx.QueryRowContext(ctx, `
		SELECT account_id, COALESCE(owner_team_id, account_id)
		FROM sites WHERE id = ?`, siteID).Scan(&operation.AccountID, &currentOwner)
	if errors.Is(err, sql.ErrNoRows) || currentOwner != ownerTeamID {
		return claim{}, ErrNotFound
	}
	if err != nil {
		return claim{}, fmt.Errorf("destructive: claim site: %w", err)
	}

	now := s.now()
	var existingKind string
	var leaseUntil int64
	err = tx.QueryRowContext(ctx, `
		SELECT kind, lease_until FROM destructive_operations
		WHERE resource_type = 'site' AND resource_id = ?`, siteID).Scan(&existingKind, &leaseUntil)
	switch {
	case err == nil && (existingKind != kind || leaseUntil > now.Unix()):
		return claim{}, ErrBusy
	case err == nil:
		result, updateErr := tx.ExecContext(ctx, `
			UPDATE destructive_operations
			SET lease_token = ?, lease_until = ?, updated_at = ?
			WHERE resource_type = 'site' AND resource_id = ?
			  AND kind = ? AND owner_team_id = ? AND storage_account_id = ?
		`, token, now.Add(s.leaseDuration()).Unix(), now.Unix(), siteID, kind, ownerTeamID, operation.AccountID)
		if updateErr != nil {
			return claim{}, fmt.Errorf("destructive: reclaim site: %w", updateErr)
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return claim{}, ErrBusy
		}
	case errors.Is(err, sql.ErrNoRows):
		var teamBusy bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM destructive_operations
			WHERE resource_type = 'team' AND resource_id = ?)
		`, ownerTeamID).Scan(&teamBusy); err != nil {
			return claim{}, fmt.Errorf("destructive: claim site: %w", err)
		}
		if teamBusy {
			return claim{}, ErrBusy
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO destructive_operations
				(resource_type, resource_id, kind, owner_team_id, storage_account_id,
				 state, lease_token, lease_until, created_at, updated_at)
			VALUES ('site', ?, ?, ?, ?, 'claimed', ?, ?, ?, ?)
		`, siteID, kind, ownerTeamID, operation.AccountID, token,
			now.Add(s.leaseDuration()).Unix(), now.Unix(), now.Unix())
		if err != nil {
			return claim{}, fmt.Errorf("destructive: claim site: %w", err)
		}
	default:
		return claim{}, fmt.Errorf("destructive: claim site: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return claim{}, fmt.Errorf("destructive: claim site: %w", err)
	}

	return operation, nil
}

// resetDisposition classifies every site-scoped table as either analytics that
// a reset erases or durable configuration that it preserves.
type resetDisposition bool

const (
	preserveOnReset resetDisposition = false
	eraseOnReset    resetDisposition = true
)

// accountResetDisposition is deliberately exhaustive. A new site-scoped table
// makes reset fail closed until its owner decides whether it is fact data or
// configuration instead of silently treating configuration as analytics.
var accountResetDisposition = map[string]resetDisposition{
	"events":                    eraseOnReset,
	"event_details":             eraseOnReset,
	"event_sampling":            eraseOnReset,
	"sessions":                  eraseOnReset,
	"session_sampling":          eraseOnReset,
	"sampling_strata":           eraseOnReset,
	"sampling_daily_counts":     eraseOnReset,
	"ingest_session_state":      eraseOnReset,
	"ingest_orphan_engagements": eraseOnReset,
	"hostname_rejections":       eraseOnReset,
	"rollup_visitors":           eraseOnReset,
	"rollup_sources":            eraseOnReset,
	"rollup_pages":              eraseOnReset,
	"rollup_entry_pages":        eraseOnReset,
	"rollup_exit_pages":         eraseOnReset,
	"rollup_locations":          eraseOnReset,
	"rollup_devices":            eraseOnReset,
	"rollup_browsers":           eraseOnReset,
	"rollup_operating_systems":  eraseOnReset,
	"rollup_languages":          eraseOnReset,
	"rollup_custom_events":      eraseOnReset,
	"rollup_state":              eraseOnReset,
	"imported_rollups":          eraseOnReset,
	"search_console_daily":      eraseOnReset,
	"exports":                   eraseOnReset,
	"path_clean_map":            eraseOnReset,
	"ingest_health":             eraseOnReset,
	"ingest_last_request":       eraseOnReset,
	"ingest_observations":       eraseOnReset,
	"goals":                     preserveOnReset,
	"goal_properties":           preserveOnReset,
	"allowed_properties":        preserveOnReset,
	"funnels":                   preserveOnReset,
	"funnel_steps":              preserveOnReset,
	"imports":                   preserveOnReset,
	"shield_rules":              preserveOnReset,
	"path_clean_rules":          preserveOnReset,
	"google_connections":        preserveOnReset,
	"annotations":               preserveOnReset,
}

// controlResetDisposition separates delivery history and queued work from the
// settings that define a site. Guests, publication settings, reports, alerts,
// allow-lists and integrations all survive an analytics reset.
var controlResetDisposition = map[string]resetDisposition{
	"guest_memberships":         preserveOnReset,
	"team_invitations":          preserveOnReset,
	"shared_links":              preserveOnReset,
	"share_password_attempts":   preserveOnReset,
	"site_tracker_config":       preserveOnReset,
	"site_custom_properties":    preserveOnReset,
	"webhook_endpoints":         preserveOnReset,
	"webhook_deliveries":        eraseOnReset,
	"site_allowed_hostnames":    preserveOnReset,
	"saved_segments":            preserveOnReset,
	"report_subscriptions":      preserveOnReset,
	"alert_rules":               preserveOnReset,
	"notifications_sent":        eraseOnReset,
	"notification_claims":       eraseOnReset,
	"notification_destinations": eraseOnReset,
	"jobs":                      eraseOnReset,
	"cron_slots":                eraseOnReset,
}

// eraseSite removes a site's analytics facts inside the immutable storage
// account selected by its claim. Reset uses the explicit classification above;
// full deletion still discovers and removes every site-scoped row.
func (s *Service) eraseSite(ctx context.Context, operation claim) error {
	account, err := s.Accounts.Open(ctx, operation.AccountID)
	if err != nil {
		return fmt.Errorf("destructive: open analytics: %w", err)
	}

	tx, err := account.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("destructive: erase analytics: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	paths, err := siteFilePaths(ctx, tx, operation.SiteID, operation.Kind == KindSiteDelete)
	if err != nil {
		return fmt.Errorf("destructive: read site files: %w", err)
	}

	deferredFiles := map[string]bool{"exports": true}
	if operation.Kind == KindSiteDelete {
		deferredFiles["imports"] = true
		err = eraseSiteRows(ctx, tx, operation.SiteID, deferredFiles)
	} else {
		err = eraseClassifiedSiteRows(ctx, tx, operation.SiteID, accountResetDisposition, deferredFiles)
	}
	if err != nil {
		return fmt.Errorf("destructive: erase analytics: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("destructive: erase analytics: %w", err)
	}

	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("destructive: remove site file %s: %w", path, err)
		}
	}

	fileTx, err := account.Writer().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("destructive: finish site files: %w", err)
	}
	defer fileTx.Rollback() //nolint:errcheck // rollback after commit is harmless

	if _, err := fileTx.ExecContext(ctx, `DELETE FROM exports WHERE site_id = ?`, operation.SiteID); err != nil {
		return fmt.Errorf("destructive: delete export records: %w", err)
	}
	if operation.Kind == KindSiteDelete {
		if _, err := fileTx.ExecContext(ctx, `DELETE FROM imports WHERE site_id = ?`, operation.SiteID); err != nil {
			return fmt.Errorf("destructive: delete import records: %w", err)
		}
	}
	if err := fileTx.Commit(); err != nil {
		return fmt.Errorf("destructive: finish site files: %w", err)
	}

	return nil
}

// siteFilePaths snapshots file names while their rows remain durable. A reset
// removes generated exports only; retained imports are configuration/source
// material and survive exactly like the import records that name them.
func siteFilePaths(ctx context.Context, tx *sql.Tx, siteID int64, includeImports bool) ([]string, error) {
	query := `SELECT path FROM exports WHERE site_id = ? AND path <> ''`
	args := []any{siteID}
	if includeImports {
		query = `
			SELECT upload_path FROM imports WHERE site_id = ? AND upload_path <> ''
			UNION ALL
			SELECT path FROM exports WHERE site_id = ? AND path <> ''
		`
		args = append(args, siteID)
	}

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read site file paths: %w", err)
	}

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read site file paths: %w", err)
		}
		paths = append(paths, path)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("read site file paths: %w", err)
	}

	return paths, nil
}

// markAnalyticsDeleted durably records the cross-database boundary before a
// control row is removed.
func (s *Service) markAnalyticsDeleted(ctx context.Context, operation claim) error {
	now := s.now()
	result, err := s.DB.ExecContext(ctx, `
		UPDATE destructive_operations
		SET state = 'analytics_deleted', lease_until = ?, updated_at = ?
		WHERE resource_type = 'site' AND resource_id = ? AND kind = ? AND lease_token = ?
	`, now.Add(s.leaseDuration()).Unix(), now.Unix(), operation.SiteID, operation.Kind, operation.Token)
	if err != nil {
		return fmt.Errorf("destructive: mark analytics deleted: %w", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return ErrBusy
	}

	return nil
}

// finishReset revalidates ownership at the destructive commit boundary and
// releases the tombstone without changing the site row.
func (s *Service) finishReset(ctx context.Context, operation claim) error {
	return s.finishSite(ctx, operation, false)
}

// finishDelete revalidates ownership and removes the control row while the
// tombstone still blocks transfers.
func (s *Service) finishDelete(ctx context.Context, operation claim) error {
	return s.finishSite(ctx, operation, true)
}

// finishSite commits the final system mutation and tombstone removal in one
// transaction so no transfer can enter between them.
func (s *Service) finishSite(ctx context.Context, operation claim, remove bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("destructive: finish site: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	var ownerID, accountID int64
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(owner_team_id, account_id), account_id FROM sites WHERE id = ?
	`, operation.SiteID).Scan(&ownerID, &accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("destructive: finish site: %w", err)
	}
	if ownerID != operation.OwnerID || accountID != operation.AccountID {
		return ErrNotFound
	}

	var state string
	err = tx.QueryRowContext(ctx, `
		SELECT state FROM destructive_operations
		WHERE resource_type = 'site' AND resource_id = ? AND kind = ? AND lease_token = ?
	`, operation.SiteID, operation.Kind, operation.Token).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || state != "analytics_deleted" {
		return ErrBusy
	}
	if err != nil {
		return fmt.Errorf("destructive: finish site: %w", err)
	}

	if remove {
		// A deletion removes every site-scoped control row before the routing row.
		if err := eraseSiteRows(ctx, tx, operation.SiteID, map[string]bool{"sites": true}); err != nil {
			return fmt.Errorf("destructive: erase control state: %w", err)
		}
	} else {
		// A reset clears only operational facts. The exhaustive classification
		// makes a newly added site table block the reset until its behavior is
		// deliberately selected.
		if err := eraseClassifiedSiteRows(ctx, tx, operation.SiteID, controlResetDisposition,
			map[string]bool{"sites": true}); err != nil {
			return fmt.Errorf("destructive: erase control facts: %w", err)
		}
	}

	if remove {
		result, err := tx.ExecContext(ctx, `
			DELETE FROM sites
			WHERE id = ? AND account_id = ? AND COALESCE(owner_team_id, account_id) = ?
		`, operation.SiteID, operation.AccountID, operation.OwnerID)
		if err != nil {
			return fmt.Errorf("destructive: finish site: %w", err)
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return ErrNotFound
		}
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM destructive_operations
		WHERE resource_type = 'site' AND resource_id = ? AND lease_token = ?
	`, operation.SiteID, operation.Token); err != nil {
		return fmt.Errorf("destructive: finish site: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("destructive: finish site: %w", err)
	}

	return nil
}

// schemaTable is one SQLite table and the foreign keys that can make its rows
// inherit a site's scope from a parent table.
type schemaTable struct {
	name       string
	primaryKey string
	hasSiteID  bool
	foreign    []schemaForeignKey
}

// schemaForeignKey is one single-column relationship. All current account and
// control relationships are single-column; composite relationships are still
// represented as separate predicates and therefore fail closed at commit if a
// future schema needs stronger handling.
type schemaForeignKey struct {
	from   string
	parent string
	to     string
}

// eraseSiteRows discovers every table directly carrying site_id plus dependent
// tables reachable through foreign keys, then deletes children before parents.
// The numeric id is rendered into generated SQL only after strconv has reduced
// it to digits; table and column identifiers come from SQLite and are quoted.
func eraseSiteRows(ctx context.Context, tx *sql.Tx, siteID int64, excluded map[string]bool) error {
	tables, err := readSchemaTables(ctx, tx)
	if err != nil {
		return err
	}

	expressions := siteExpressions(tables, siteID, excluded)

	return eraseExpressions(ctx, tx, tables, expressions)
}

// eraseClassifiedSiteRows validates the complete site-scoped schema and then
// deletes only tables explicitly classified as analytics or operational facts.
func eraseClassifiedSiteRows(ctx context.Context, tx *sql.Tx, siteID int64,
	dispositions map[string]resetDisposition, excluded map[string]bool) error {
	tables, err := readSchemaTables(ctx, tx)
	if err != nil {
		return err
	}
	expressions := siteExpressions(tables, siteID, excluded)
	deleteExpressions := make(map[string]string, len(expressions))
	for name, expression := range expressions {
		disposition, classified := dispositions[name]
		if !classified {
			return fmt.Errorf("unclassified site-scoped table %s", name)
		}
		if disposition == eraseOnReset {
			deleteExpressions[name] = expression
		}
	}

	return eraseExpressions(ctx, tx, tables, deleteExpressions)
}

// siteExpressions derives every direct and foreign-key-inherited site scope.
func siteExpressions(tables map[string]*schemaTable, siteID int64, excluded map[string]bool) map[string]string {
	expressions := map[string]string{}
	known := map[string]string{}
	visiting := map[string]bool{}
	for name := range tables {
		if excluded[name] {
			continue
		}
		if expression := sitePredicate(name, tables, excluded, known, visiting,
			strconv.FormatInt(siteID, 10)); expression != "" {
			expressions[name] = expression
		}
	}

	return expressions
}

// eraseExpressions orders scoped children before their parents and executes
// the generated deletion predicates inside the caller's transaction.
func eraseExpressions(ctx context.Context, tx *sql.Tx, tables map[string]*schemaTable,
	expressions map[string]string) error {

	children := map[string][]string{}
	for name, table := range tables {
		if expressions[name] == "" {
			continue
		}
		for _, foreign := range table.foreign {
			if expressions[foreign.parent] != "" {
				children[foreign.parent] = append(children[foreign.parent], name)
			}
		}
	}

	names := make([]string, 0, len(expressions))
	for name := range expressions {
		names = append(names, name)
	}
	sort.Strings(names)

	ordered := make([]string, 0, len(names))
	seen := map[string]bool{}
	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		sort.Strings(children[name])
		for _, child := range children[name] {
			visit(child)
		}
		ordered = append(ordered, name)
	}
	for _, name := range names {
		visit(name)
	}

	for _, name := range ordered {
		query := "DELETE FROM " + quoteIdentifier(name) + " WHERE " + expressions[name]
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("delete from %s: %w", name, err)
		}
	}

	return nil
}

// readSchemaTables reads table columns and foreign keys through SQLite's
// structured PRAGMA interfaces instead of parsing CREATE TABLE text.
func readSchemaTables(ctx context.Context, tx *sql.Tx) (map[string]*schemaTable, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT name FROM sqlite_schema
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("read table list: %w", err)
	}

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read table list: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("read table list: %w", err)
	}

	tables := make(map[string]*schemaTable, len(names))
	for _, name := range names {
		table := &schemaTable{name: name}
		columnRows, err := tx.QueryContext(ctx, "PRAGMA table_info("+quoteIdentifier(name)+")")
		if err != nil {
			return nil, fmt.Errorf("read %s columns: %w", name, err)
		}
		for columnRows.Next() {
			var cid, notNull, primary int
			var column, columnType string
			var defaultValue any
			if err := columnRows.Scan(&cid, &column, &columnType, &notNull, &defaultValue, &primary); err != nil {
				_ = columnRows.Close()
				return nil, fmt.Errorf("read %s columns: %w", name, err)
			}
			if column == "site_id" {
				table.hasSiteID = true
			}
			if primary == 1 {
				table.primaryKey = column
			}
		}
		if err := columnRows.Close(); err != nil {
			return nil, fmt.Errorf("read %s columns: %w", name, err)
		}
		tables[name] = table
	}

	for _, name := range names {
		foreignRows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_list("+quoteIdentifier(name)+")")
		if err != nil {
			return nil, fmt.Errorf("read %s foreign keys: %w", name, err)
		}
		for foreignRows.Next() {
			var id, sequence int
			var parent, from, to, onUpdate, onDelete, match string
			if err := foreignRows.Scan(&id, &sequence, &parent, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				_ = foreignRows.Close()
				return nil, fmt.Errorf("read %s foreign keys: %w", name, err)
			}
			if to == "" && tables[parent] != nil {
				to = tables[parent].primaryKey
			}
			if from != "" && to != "" {
				tables[name].foreign = append(tables[name].foreign, schemaForeignKey{from: from, parent: parent, to: to})
			}
		}
		if err := foreignRows.Close(); err != nil {
			return nil, fmt.Errorf("read %s foreign keys: %w", name, err)
		}
	}

	return tables, nil
}

// sitePredicate recursively builds the row predicate that ties one table to a
// site, either directly through site_id or through a scoped parent foreign key.
func sitePredicate(name string, tables map[string]*schemaTable, excluded map[string]bool,
	known map[string]string, visiting map[string]bool, siteID string) string {
	if excluded[name] {
		return ""
	}
	if predicate, ok := known[name]; ok {
		return predicate
	}
	table := tables[name]
	if table == nil || visiting[name] {
		return ""
	}
	if table.hasSiteID {
		predicate := quoteIdentifier("site_id") + " = " + siteID
		known[name] = predicate
		return predicate
	}

	visiting[name] = true
	var predicates []string
	for _, foreign := range table.foreign {
		parentPredicate := sitePredicate(foreign.parent, tables, excluded, known, visiting, siteID)
		if parentPredicate == "" {
			continue
		}
		predicates = append(predicates,
			quoteIdentifier(foreign.from)+" IN (SELECT "+quoteIdentifier(foreign.to)+" FROM "+
				quoteIdentifier(foreign.parent)+" WHERE "+parentPredicate+")")
	}
	delete(visiting, name)

	predicate := strings.Join(predicates, " OR ")
	known[name] = predicate

	return predicate
}

// quoteIdentifier quotes a schema-owned SQLite identifier. Doubling quotes is
// SQLite's identifier escaping rule and keeps generated deletion SQL structural.
func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

// operationToken creates an internal lease identity. It is not exposed, but
// unpredictability prevents one worker from accidentally completing another
// worker's reclaimed operation with a reused process-local identifier.
func operationToken() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("destructive: create lease token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}
