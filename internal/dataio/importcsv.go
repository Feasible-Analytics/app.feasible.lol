//
// importcsv.go
// Reading the ten CSV formats, one file at a time, with a readable failure.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package dataio

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// MaxCSVRowsPerFile bounds one file. Ten years of daily rows broken down by
// browser version is comfortably under this; anything above it is a raw event
// log somebody has mislabelled, and reading it as roll-ups would produce
// millions of one-pageview rows.
const MaxCSVRowsPerFile = 5_000_000

// DateLayouts are the date spellings a file may use. Only unambiguous layouts
// are accepted: 03/04/2026 is the fourth of March in one country and the third
// of April in another, and guessing wrong shifts a customer's whole history by
// months with nothing to indicate it.
var DateLayouts = []string{"2006-01-02", "2006/01/02", time.RFC3339}

// CSVSource is one file to import: its name, and how to open it. A zip entry
// and a loose file both become one of these, so the importer has a single path
// through the work.
type CSVSource struct {
	Name string
	Open func() (io.ReadCloser, error)
}

// ImportCSV reads every file in a source list into imported roll-up rows.
//
// It writes progress after each file rather than at the end. An import that
// takes four minutes and shows nothing for the first three and a half is an
// import somebody reloads the page on and then reports as stuck, and "never
// fail silently" applies just as much to work in progress as it does to work
// that failed.
func ImportCSV(ctx context.Context, db *sql.DB, cache *intern.Cache, record *Import, sources []CSVSource, location *time.Location, now func() time.Time) error {
	if len(sources) == 0 {
		return fmt.Errorf("that upload contains no CSV files — an import expects the files named %s", strings.Join(SheetNames(), ", "))
	}

	if err := StartImport(ctx, db, record.ID, len(sources), now()); err != nil {
		return err
	}

	covered := map[string]bool{}
	var rowsWritten int64
	var earliest, latest int64

	for index, source := range sources {
		dimensions, written, first, last, err := importOneFile(ctx, db, cache, record, source, location)
		if err != nil {
			return err
		}

		for _, name := range dimensions {
			covered[name] = true
		}

		rowsWritten += written

		if first != 0 && (earliest == 0 || first < earliest) {
			earliest = first
		}
		if last > latest {
			latest = last
		}

		if err := SetProgress(ctx, db, record.ID, index+1, rowsWritten, source.Name); err != nil {
			return err
		}
	}

	names := make([]string, 0, len(covered))
	for name := range covered {
		names = append(names, name)
	}

	return CompleteImport(ctx, db, record.ID, names, earliest, latest, rowsWritten, now())
}

// importOneFile reads a single CSV into roll-up rows and reports what it
// carried. Every error names the file and the line, because "your import
// failed" without either is a message the customer cannot act on and we cannot
// answer a ticket about.
func importOneFile(ctx context.Context, db *sql.DB, cache *intern.Cache, record *Import, source CSVSource, location *time.Location) (result []string, firstResult, lastResult, rowsResult int64, err error) {
	handle, err := source.Open()
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("%s: could not be opened: %w", source.Name, err)
	}
	defer closeResource(handle, &err, source.Name)

	reader := csv.NewReader(handle)
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if errors.Is(err, io.EOF) {
		return nil, 0, 0, 0, nil
	}
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("%s: could not read the header row: %w", source.Name, err)
	}

	layout, err := planColumns(source.Name, header)
	if err != nil {
		return nil, 0, 0, 0, err
	}

	writer, err := NewWriter(db, cache, record.ID, record.SiteID, layout.dimensions)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("%s: %w", source.Name, err)
	}

	var earliest, latest int64
	line := 1

	for {
		fields, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, 0, 0, 0, fmt.Errorf("%s line %d: %w", source.Name, line+1, err)
		}

		line++

		if line-1 > MaxCSVRowsPerFile {
			return nil, 0, 0, 0, fmt.Errorf("%s has more than %d rows — that is a raw event log rather than a daily roll-up",
				source.Name, MaxCSVRowsPerFile)
		}

		// A trailing blank line is normal in a hand-edited export and is not
		// worth failing an import over.
		if len(fields) == 1 && strings.TrimSpace(fields[0]) == "" {
			continue
		}

		row, err := layout.row(fields, location)
		if err != nil {
			return nil, 0, 0, 0, fmt.Errorf("%s line %d: %w", source.Name, line, err)
		}

		if earliest == 0 || row.Timestamp < earliest {
			earliest = row.Timestamp
		}
		if row.Timestamp > latest {
			latest = row.Timestamp
		}

		if err := writer.Add(ctx, row); err != nil {
			return nil, 0, 0, 0, fmt.Errorf("%s line %d: %w", source.Name, line, err)
		}
	}

	if err := writer.Flush(ctx); err != nil {
		return nil, 0, 0, 0, fmt.Errorf("%s: %w", source.Name, err)
	}

	return writer.Dimensions(), writer.Written(), earliest, latest, nil
}

// columnPlan is how one file's columns map onto a roll-up row: which field each
// position fills. It is worked out once from the header rather than per line,
// because a per-line map lookup over a million rows is most of an import.
type columnPlan struct {
	dateIndex int

	// dimensionAt and metricAt are indexed by column position. An empty string
	// means the column is ignored.
	dimensionAt []string
	metricAt    []string

	dimensions []string
}

// planColumns reads a header row. An unrecognised column is an error naming the
// column and listing what is understood: a file that silently drops a column
// imports numbers that are quietly too small, and nobody finds out.
func planColumns(filename string, header []string) (*columnPlan, error) {
	plan := &columnPlan{
		dateIndex:   -1,
		dimensionAt: make([]string, len(header)),
		metricAt:    make([]string, len(header)),
	}

	seen := map[string]bool{}

	for i, raw := range header {
		name := normaliseHeader(raw)

		switch name {
		case "":
			continue

		case DateHeader:
			plan.dateIndex = i

		default:
			if dimension, ok := dimensionHeaders[name]; ok {
				plan.dimensionAt[i] = dimension

				if !seen[dimension] {
					seen[dimension] = true
					plan.dimensions = append(plan.dimensions, dimension)
				}

				continue
			}

			if field, ok := metricHeaders[name]; ok {
				plan.metricAt[i] = field
				continue
			}

			return nil, fmt.Errorf("%s: column %q is not one we recognise — the columns an import understands are %s",
				filename, raw, strings.Join(KnownHeaders(), ", "))
		}
	}

	if plan.dateIndex < 0 {
		return nil, fmt.Errorf("%s: there is no date column — every imported table is a day at a time", filename)
	}

	return plan, nil
}

// row turns one CSV line into a parsed row.
func (p *columnPlan) row(fields []string, location *time.Location) (Row, error) {
	row := Row{Dimensions: map[string]string{}, Metrics: map[string]int64{}}

	if p.dateIndex >= len(fields) {
		return row, fmt.Errorf("this row has %d columns and the header has more", len(fields))
	}

	timestamp, err := parseDay(fields[p.dateIndex], location)
	if err != nil {
		return row, err
	}
	row.Timestamp = timestamp

	for i, value := range fields {
		if i >= len(p.dimensionAt) {
			continue
		}

		if dimension := p.dimensionAt[i]; dimension != "" {
			row.Dimensions[dimension] = value
			continue
		}

		if field := p.metricAt[i]; field != "" {
			number, err := parseCount(value)
			if err != nil {
				return row, fmt.Errorf("%s is %q, which is not a number", field, value)
			}

			row.Metrics[field] += number
		}
	}

	return row, nil
}

// parseDay turns a date cell into unix seconds at the site's local midnight.
// Local rather than UTC, because a report buckets by the site's own day and an
// imported row stored at UTC midnight would land in yesterday for every site
// west of Greenwich.
func parseDay(value string, location *time.Location) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("the date is empty")
	}

	if location == nil {
		location = time.UTC
	}

	for _, layout := range DateLayouts {
		parsed, err := time.ParseInLocation(layout, value, location)
		if err != nil {
			continue
		}

		day := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, location)

		return day.Unix(), nil
	}

	return 0, fmt.Errorf("%q is not a date we can read — use YYYY-MM-DD", value)
}

// parseCount reads a metric cell. Blank is zero, because an export leaves a
// column empty where the number was zero, and a float is truncated because
// every counter here is a whole thing that happened.
func parseCount(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}

	// Thousands separators appear in files that have been through a
	// spreadsheet, and failing over one helps nobody.
	value = strings.ReplaceAll(value, ",", "")

	if number, err := strconv.ParseInt(value, 10, 64); err == nil {
		return number, nil
	}

	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}

	return int64(number), nil
}

// SourcesFromUpload turns an uploaded file into the list of CSVs to read. A
// single .csv is one source; a .zip is every .csv inside it, which is what an
// export directory arrives as.
func SourcesFromUpload(path string) ([]CSVSource, func() error, error) {
	if strings.EqualFold(filepath.Ext(path), ".zip") {
		return sourcesFromZip(path)
	}

	name := filepath.Base(path)

	return []CSVSource{{
		Name: name,
		Open: func() (io.ReadCloser, error) { return os.Open(path) },
	}}, func() error { return nil }, nil
}

// sourcesFromZip lists the CSV entries inside an archive. Directories and
// anything that is not a .csv are skipped rather than refused: an export
// unzipped and re-zipped on a Mac carries a __MACOSX directory that nobody
// should have to know about.
func sourcesFromZip(path string) ([]CSVSource, func() error, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, nil, fmt.Errorf("that zip file could not be read: %w", err)
	}

	var sources []CSVSource

	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(filepath.Ext(entry.Name), ".csv") {
			continue
		}

		if strings.HasPrefix(filepath.Base(entry.Name), ".") {
			continue
		}

		// Our own export carries the raw events alongside the ten roll-up
		// tables. Reading it back as a roll-up would turn every event into a
		// one-pageview row and double the site's history, so a re-imported
		// export deliberately brings in the roll-ups only.
		if strings.EqualFold(filepath.Base(entry.Name), RawEventsSheet+".csv") {
			continue
		}

		file := entry

		sources = append(sources, CSVSource{
			Name: filepath.Base(file.Name),
			Open: func() (io.ReadCloser, error) { return file.Open() },
		})
	}

	if len(sources) == 0 {
		emptyErr := fmt.Errorf("that zip file has no CSVs in it — an import expects the files named %s", strings.Join(SheetNames(), ", "))
		if closeErr := archive.Close(); closeErr != nil {
			emptyErr = errors.Join(emptyErr, fmt.Errorf("dataio: close empty upload archive: %w", closeErr))
		}
		return nil, nil, emptyErr
	}

	return sources, archive.Close, nil
}
