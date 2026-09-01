//
// records.go
// The import and export rows a customer watches, with progress and a reason.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dataio

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Import sources.
const (
	SourceCSV           = "csv"
	SourceGA4           = "ga4"
	SourceSearchConsole = "search_console"
)

// Import and export statuses.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// ExportWindow is how long a prepared export can be downloaded for. A prepared
// export is a full copy of a customer's traffic sitting on disk behind a URL,
// so it expires whether or not anybody fetched it.
const ExportWindow = 24 * time.Hour

// Import is one import run as the customer sees it.
type Import struct {
	ID            int64
	SiteID        int64
	Source        string
	Label         string
	Status        string
	ProgressDone  int
	ProgressTotal int
	RowsWritten   int64
	Dimensions    []string
	Cursor        string
	Failure       string
	UploadPath    string
	RangeStart    int64
	RangeEnd      int64
	CreatedAt     int64
	StartedAt     int64
	CompletedAt   int64
}

// Progress renders the two counters as a percentage for a progress bar. It
// answers zero rather than dividing by zero for a job that has not worked out
// how much there is to do yet.
func (i Import) Progress() int {
	if i.ProgressTotal <= 0 {
		return 0
	}

	percent := i.ProgressDone * 100 / i.ProgressTotal
	if percent > 100 {
		return 100
	}

	return percent
}

// CreateImport records an import before any work starts. The row exists first
// so that an upload which fails halfway still leaves something the customer can
// see and delete, rather than a file on disk nothing points at.
func CreateImport(ctx context.Context, db *sql.DB, siteID int64, source, label string, now time.Time) (*Import, error) {
	result, err := db.ExecContext(ctx, `
		INSERT INTO imports (site_id, source, label, status, created_at)
		VALUES (?, ?, ?, 'pending', ?)`, siteID, source, label, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("dataio: create import: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("dataio: create import: %w", err)
	}

	return &Import{ID: id, SiteID: siteID, Source: source, Label: label, Status: StatusPending, CreatedAt: now.Unix()}, nil
}

// GetImport reads one import, scoped to its site so an id from another account
// cannot be used to read somebody else's.
func GetImport(ctx context.Context, db *sql.DB, siteID, id int64) (*Import, error) {
	row := db.QueryRowContext(ctx, importSelect+" WHERE site_id = ? AND id = ?", siteID, id)

	return scanImport(row)
}

// GetImportByID reads one import without a site, for the background job that
// already knows which one it is running.
func GetImportByID(ctx context.Context, db *sql.DB, id int64) (*Import, error) {
	row := db.QueryRowContext(ctx, importSelect+" WHERE id = ?", id)

	return scanImport(row)
}

// importSelect is the column list every read uses, kept in one place so a new
// column cannot be added to one reader and forgotten in another.
const importSelect = `
	SELECT id, site_id, source, label, status, progress_done, progress_total, rows_written,
	       dimensions, cursor, failure, upload_path,
	       COALESCE(range_start, 0), COALESCE(range_end, 0),
	       created_at, COALESCE(started_at, 0), COALESCE(completed_at, 0)
	FROM imports`

// scanner is whatever a row came back on, so one scan function serves both the
// single-row and the list reads.
type scanner interface {
	Scan(dest ...any) error
}

// scanImport reads one row into an Import.
func scanImport(row scanner) (*Import, error) {
	var record Import
	var dimensions string

	err := row.Scan(&record.ID, &record.SiteID, &record.Source, &record.Label, &record.Status,
		&record.ProgressDone, &record.ProgressTotal, &record.RowsWritten,
		&dimensions, &record.Cursor, &record.Failure, &record.UploadPath,
		&record.RangeStart, &record.RangeEnd,
		&record.CreatedAt, &record.StartedAt, &record.CompletedAt)
	if err != nil {
		return nil, fmt.Errorf("dataio: read import: %w", err)
	}

	// A dimension list that will not parse is treated as empty rather than
	// fatal: it is a display field, and refusing to show an import because its
	// label is malformed helps nobody.
	_ = json.Unmarshal([]byte(dimensions), &record.Dimensions)

	return &record, nil
}

// ListImports reads a site's imports, newest first.
func ListImports(ctx context.Context, db *sql.DB, siteID int64, limit int) ([]Import, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := db.QueryContext(ctx, importSelect+" WHERE site_id = ? ORDER BY created_at DESC, id DESC LIMIT ?", siteID, limit)
	if err != nil {
		return nil, fmt.Errorf("dataio: list imports: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var imports []Import

	for rows.Next() {
		record, err := scanImport(rows)
		if err != nil {
			return nil, err
		}

		imports = append(imports, *record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dataio: list imports: %w", err)
	}

	return imports, nil
}

// StartImport moves a pending import into running and stamps the time. It also
// clears any previous failure, so a retried import does not show the reason the
// last attempt failed while it is succeeding.
func StartImport(ctx context.Context, db *sql.DB, id int64, total int, now time.Time) error {
	_, err := db.ExecContext(ctx, `
		UPDATE imports SET status = 'running', started_at = ?, progress_total = ?, failure = ''
		WHERE id = ?`, now.Unix(), total, id)
	if err != nil {
		return fmt.Errorf("dataio: start import %d: %w", id, err)
	}

	return nil
}

// SetProgress updates the counters a progress bar reads. It is deliberately a
// separate write per unit of work rather than one at the end: an import that
// takes four minutes and shows nothing for the first three and a half is an
// import the customer reloads the page on, then reports as stuck.
func SetProgress(ctx context.Context, db *sql.DB, id int64, done int, rowsWritten int64, cursor string) error {
	_, err := db.ExecContext(ctx,
		"UPDATE imports SET progress_done = ?, rows_written = ?, cursor = ? WHERE id = ?",
		done, rowsWritten, cursor, id)
	if err != nil {
		return fmt.Errorf("dataio: import %d progress: %w", id, err)
	}

	return nil
}

// CompleteImport marks an import finished and records what it covers. The
// dimension list is what the query layer's gap reporting reads back, so it is
// written here rather than derived later from the rows.
func CompleteImport(ctx context.Context, db *sql.DB, id int64, dimensions []string, rangeStart, rangeEnd, rowsWritten int64, now time.Time) error {
	encoded, err := json.Marshal(dimensions)
	if err != nil {
		return fmt.Errorf("dataio: complete import %d: %w", id, err)
	}

	_, err = db.ExecContext(ctx, `
		UPDATE imports
		SET status = 'completed', completed_at = ?, dimensions = ?, rows_written = ?,
		    range_start = ?, range_end = ?, progress_done = progress_total, failure = ''
		WHERE id = ?`, now.Unix(), string(encoded), rowsWritten, rangeStart, rangeEnd, id)
	if err != nil {
		return fmt.Errorf("dataio: complete import %d: %w", id, err)
	}

	return nil
}

// FailImport records why an import stopped. The message is shown to the
// customer verbatim, so callers write it for them: which file, which row, and
// what was wrong with it.
func FailImport(ctx context.Context, db *sql.DB, id int64, reason string, now time.Time) error {
	_, err := db.ExecContext(ctx,
		"UPDATE imports SET status = 'failed', completed_at = ?, failure = ? WHERE id = ?",
		now.Unix(), reason, id)
	if err != nil {
		return fmt.Errorf("dataio: fail import %d: %w", id, err)
	}

	return nil
}

// SetUploadPath records where an import's file was copied to.
func SetUploadPath(ctx context.Context, db *sql.DB, id int64, path string) error {
	if _, err := db.ExecContext(ctx, "UPDATE imports SET upload_path = ? WHERE id = ?", path, id); err != nil {
		return fmt.Errorf("dataio: import %d upload path: %w", id, err)
	}

	return nil
}

// DeleteImport removes an import and, through the foreign key, every roll-up
// row it brought in. Deleting the import is the only way to take imported
// history back out, which is why it is scoped to one import id rather than to a
// date range: a range delete could not tell imported rows apart from each other.
func DeleteImport(ctx context.Context, db *sql.DB, siteID, id int64) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM imported_rollups WHERE import_id = ? AND site_id = ?", id, siteID); err != nil {
		return fmt.Errorf("dataio: delete imported rows: %w", err)
	}

	if _, err := db.ExecContext(ctx, "DELETE FROM imports WHERE id = ? AND site_id = ?", id, siteID); err != nil {
		return fmt.Errorf("dataio: delete import: %w", err)
	}

	return nil
}

// Export is one prepared export.
type Export struct {
	ID          int64
	SiteID      int64
	Path        string
	Status      string
	Bytes       int64
	Failure     string
	CreatedAt   int64
	CompletedAt int64
	ExpiresAt   int64
}

// Expired reports whether the download window has closed.
func (e Export) Expired(now time.Time) bool {
	return now.Unix() >= e.ExpiresAt
}

// CreateExport records an export and returns the download token. The token is
// returned once and stored hashed, for the same reason every other token in
// this system is: a link that leaks out of a browser history or a mail spool
// must not be replayable against somebody's whole traffic history.
func CreateExport(ctx context.Context, db *sql.DB, siteID int64, now time.Time) (*Export, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", fmt.Errorf("dataio: create export: %w", err)
	}

	token := hex.EncodeToString(raw)

	result, err := db.ExecContext(ctx, `
		INSERT INTO exports (site_id, token_hash, status, created_at, expires_at)
		VALUES (?, ?, 'pending', ?, ?)`,
		siteID, HashToken(token), now.Unix(), now.Add(ExportWindow).Unix())
	if err != nil {
		return nil, "", fmt.Errorf("dataio: create export: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, "", fmt.Errorf("dataio: create export: %w", err)
	}

	return &Export{
		ID: id, SiteID: siteID, Status: StatusPending,
		CreatedAt: now.Unix(), ExpiresAt: now.Add(ExportWindow).Unix(),
	}, token, nil
}

// HashToken renders a download token the way it is stored.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// exportSelect is the column list every export read uses.
const exportSelect = `
	SELECT id, site_id, path, status, bytes, failure, created_at,
	       COALESCE(completed_at, 0), expires_at
	FROM exports`

// scanExport reads one row into an Export.
func scanExport(row scanner) (*Export, error) {
	var record Export

	err := row.Scan(&record.ID, &record.SiteID, &record.Path, &record.Status, &record.Bytes,
		&record.Failure, &record.CreatedAt, &record.CompletedAt, &record.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("dataio: read export: %w", err)
	}

	return &record, nil
}

// GetExport reads one export by id.
func GetExport(ctx context.Context, db *sql.DB, id int64) (*Export, error) {
	return scanExport(db.QueryRowContext(ctx, exportSelect+" WHERE id = ?", id))
}

// ExportByToken resolves a download link. It is the only way to reach an export
// file, and it is why the token is long and random rather than the row id.
func ExportByToken(ctx context.Context, db *sql.DB, token string) (*Export, error) {
	return scanExport(db.QueryRowContext(ctx, exportSelect+" WHERE token_hash = ?", HashToken(token)))
}

// ListExports reads a site's exports, newest first.
func ListExports(ctx context.Context, db *sql.DB, siteID int64, limit int) ([]Export, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := db.QueryContext(ctx, exportSelect+" WHERE site_id = ? ORDER BY created_at DESC, id DESC LIMIT ?", siteID, limit)
	if err != nil {
		return nil, fmt.Errorf("dataio: list exports: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var exports []Export

	for rows.Next() {
		record, err := scanExport(rows)
		if err != nil {
			return nil, err
		}

		exports = append(exports, *record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dataio: list exports: %w", err)
	}

	return exports, nil
}

// CompleteExport records a finished archive.
func CompleteExport(ctx context.Context, db *sql.DB, id int64, path string, bytes int64, now time.Time) error {
	_, err := db.ExecContext(ctx,
		"UPDATE exports SET status = 'completed', path = ?, bytes = ?, completed_at = ? WHERE id = ?",
		path, bytes, now.Unix(), id)
	if err != nil {
		return fmt.Errorf("dataio: complete export %d: %w", id, err)
	}

	return nil
}

// FailExport records why an export could not be built.
func FailExport(ctx context.Context, db *sql.DB, id int64, reason string, now time.Time) error {
	_, err := db.ExecContext(ctx,
		"UPDATE exports SET status = 'failed', failure = ?, completed_at = ? WHERE id = ?",
		reason, now.Unix(), id)
	if err != nil {
		return fmt.Errorf("dataio: fail export %d: %w", id, err)
	}

	return nil
}
