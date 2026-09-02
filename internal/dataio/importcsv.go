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
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/referrer"
)

// MaxCSVRowsPerFile bounds one file. Ten years of daily rows broken down by
// browser version is comfortably under this; anything above it is a raw event
// log somebody has mislabelled, and reading it as roll-ups would produce
// millions of one-pageview rows.
const MaxCSVRowsPerFile = 5_000_000

const (
	// MaxArchiveCSVFiles rejects accidental directory backups and adversarial
	// archives before they can turn one migration job into thousands of opens.
	MaxArchiveCSVFiles = 100

	// MaxArchiveUncompressedBytes bounds ZIP expansion independently from the
	// compressed upload cap, closing the classic small-upload ZIP-bomb path.
	// The upload cap applies to the compressed file, deflate expands a
	// well-chosen input a thousandfold, and encoding/csv holds a whole record
	// in memory, so without this one crafted line is allocated in full before
	// any row cap or parse error can fire.
	MaxArchiveUncompressedBytes = 1 << 30

	// MaxArchiveCompressionRatio permits ordinary spreadsheet compression while
	// refusing entries whose expansion is implausibly large for analytics CSV.
	MaxArchiveCompressionRatio = 1000
)

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
	dateIndex          int
	propertyKeyIndex   int
	propertyValueIndex int
	linkURLIndex       int

	// dimensionAt and metricAt are indexed by column position. An empty string
	// means the column is ignored.
	dimensionAt []string
	metricAt    []string
	metricScale []int64

	dimensions []string

	// deriveChannel marks a sources sheet that carries the inputs to Feasible's
	// acquisition classifier but no channel column of its own. Plausible exports
	// exactly that shape, so deriving it restores the default Sources-card tab.
	deriveChannel bool
}

// planColumns reads a header row. An unrecognised column is an error naming the
// column and listing what is understood: a file that silently drops a column
// imports numbers that are quietly too small, and nobody finds out.
func planColumns(filename string, header []string) (*columnPlan, error) {
	plan := &columnPlan{
		dateIndex:          -1,
		propertyKeyIndex:   -1,
		propertyValueIndex: -1,
		linkURLIndex:       -1,
		dimensionAt:        make([]string, len(header)),
		metricAt:           make([]string, len(header)),
		metricScale:        make([]int64, len(header)),
	}

	for i := range plan.metricScale {
		plan.metricScale[i] = 1
	}

	table := plausibleTable(filename)

	seen := map[string]bool{}

	for i, raw := range header {
		name := normaliseHeader(raw)

		switch name {
		case "":
			continue

		case DateHeader:
			plan.dateIndex = i

		case "property":
			if table != "imported_custom_props" {
				return nil, unknownColumnError(filename, raw)
			}
			plan.propertyKeyIndex = i

		case "value":
			if table != "imported_custom_props" {
				return nil, unknownColumnError(filename, raw)
			}
			plan.propertyValueIndex = i

		case "link_url":
			if table != "imported_custom_events" {
				return nil, unknownColumnError(filename, raw)
			}
			plan.linkURLIndex = i

		case "utm_content", "utm_term":
			if table != "imported_sources" {
				return nil, unknownColumnError(filename, raw)
			}
			// Feasible retains these values on native event details, but they
			// are not query dimensions. The source, medium and campaign beside
			// them remain fully filterable after migration.

		case "total_scroll_depth":
			if table != "imported_pages" {
				return nil, unknownColumnError(filename, raw)
			}
			plan.metricAt[i] = FieldScrollDepth

		case "total_scroll_depth_visits":
			if table != "imported_pages" {
				return nil, unknownColumnError(filename, raw)
			}
			plan.metricAt[i] = FieldScrollVisits

		case "total_time_on_page":
			if table != "imported_pages" {
				return nil, unknownColumnError(filename, raw)
			}
			plan.metricAt[i] = FieldEngagement
			// Plausible exports seconds; Feasible stores engagement totals in
			// milliseconds before calculating the average.
			plan.metricScale[i] = 1000

		case "total_time_on_page_visits":
			if table != "imported_pages" {
				return nil, unknownColumnError(filename, raw)
			}
			plan.metricAt[i] = FieldEngagementVisits

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

			return nil, unknownColumnError(filename, raw)
		}
	}

	if plan.dateIndex < 0 {
		return nil, fmt.Errorf("%s: there is no date column — every imported table is a day at a time", filename)
	}

	if table == "imported_custom_props" {
		if plan.propertyKeyIndex < 0 || plan.propertyValueIndex < 0 {
			return nil, fmt.Errorf("%s: Plausible custom properties need both property and value columns", filename)
		}
		plan.dimensions = append(plan.dimensions, query.ImportedPropertyDimension)
	}

	if table == "imported_custom_events" && plan.linkURLIndex >= 0 {
		plan.dimensions = append(plan.dimensions, query.ImportedPropertyDimension)
	}

	if table == "imported_sources" && !seen["visit:channel"] {
		plan.dimensions = append(plan.dimensions, "visit:channel")
		plan.deriveChannel = true
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

	if p.propertyKeyIndex >= 0 {
		if p.propertyKeyIndex >= len(fields) || p.propertyValueIndex >= len(fields) {
			return row, fmt.Errorf("this row is missing its Plausible property or value")
		}
		row.PropertyKey = strings.TrimSpace(fields[p.propertyKeyIndex])
		row.PropertyValue = fields[p.propertyValueIndex]
	}

	if p.linkURLIndex >= 0 {
		if p.linkURLIndex >= len(fields) {
			return row, fmt.Errorf("this row is missing its Plausible link URL")
		}
		row.PropertyKey = "url"
		row.PropertyValue = fields[p.linkURLIndex]
	}

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

			scale := p.metricScale[i]
			if scale > 1 && number > math.MaxInt64/scale {
				return row, fmt.Errorf("%s is too large", field)
			}
			row.Metrics[field] += number * scale
		}
	}

	if p.deriveChannel {
		deriveImportedChannel(&row)
	}

	return row, nil
}

// deriveImportedChannel reconstructs the acquisition channel from the source
// and UTM fields Plausible exports. Click identifiers cannot participate
// because aggregate CSVs intentionally do not contain per-click identifiers;
// source category and campaign tags still recover the meaningful taxonomy.
func deriveImportedChannel(row *Row) {
	source := strings.TrimSpace(row.Dimensions["visit:source"])
	utmSource := strings.TrimSpace(row.Dimensions["visit:utm_source"])
	category := referrer.CategoryForSource(source)

	if utmSource != "" {
		// A known campaign source is stronger evidence than the display source,
		// but an unknown tag must not erase a category Plausible already encoded
		// in its canonical source label.
		if campaignCategory := referrer.CategoryForSource(utmSource); campaignCategory != referrer.CategoryUnknown {
			category = campaignCategory
		}
		if source == "" {
			source = utmSource
		}
	}

	if source == "" || strings.EqualFold(source, "Direct") || strings.EqualFold(source, referrer.Direct) {
		source = referrer.Direct
	}

	row.Dimensions["visit:channel"] = referrer.Channel(referrer.Input{
		Source:         source,
		Category:       category,
		Medium:         row.Dimensions["visit:utm_medium"],
		Campaign:       row.Dimensions["visit:utm_campaign"],
		CampaignSource: utmSource,
	})
}

// plausibleTable identifies one table in Plausible's dated export naming
// scheme while still accepting the undated names used by fixtures and older
// self-hosted versions.
func plausibleTable(filename string) string {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(filename)), filepath.Ext(filename))

	tables := make([]string, 0, len(Sheets)+1)
	for _, sheet := range Sheets {
		tables = append(tables, sheet.Name)
	}
	tables = append(tables, "imported_custom_props")

	for _, table := range tables {
		if base == table || plausibleDatedName(base, table) {
			return table
		}
	}

	return ""
}

// plausibleDatedName validates Plausible's table_YYYYMMDD_YYYYMMDD filename
// suffix without accepting arbitrary lookalikes that happen to share a prefix.
func plausibleDatedName(base, table string) bool {
	suffix := strings.TrimPrefix(base, table+"_")
	parts := strings.Split(suffix, "_")
	if len(parts) != 2 {
		return false
	}

	for _, part := range parts {
		if len(part) != 8 {
			return false
		}
		if _, err := time.Parse("20060102", part); err != nil {
			return false
		}
	}

	return true
}

// unknownColumnError keeps strict generic-import validation while giving every
// failure the exact file, column and supported vocabulary needed to fix it.
func unknownColumnError(filename, column string) error {
	return fmt.Errorf("%s: column %q is not one we recognise — the columns an import understands are %s",
		filename, column, strings.Join(KnownHeaders(), ", "))
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
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".zip" {
		return sourcesFromZip(path)
	}
	if ext != ".csv" {
		return nil, nil, fmt.Errorf("choose a .zip Plausible export or a .csv analytics file")
	}

	name := filepath.Base(path)

	return []CSVSource{{
		Name: name,
		Open: func() (io.ReadCloser, error) { return os.Open(path) },
	}}, func() error { return nil }, nil
}

// ClassifyUpload validates an upload's container and identifies a Plausible
// native archive from its canonical imported_* table names. Detection happens
// before the job is queued so the import history and UI can name the source
// accurately even while work is still pending.
func ClassifyUpload(path string) (string, error) {
	if !strings.EqualFold(filepath.Ext(path), ".zip") {
		if !strings.EqualFold(filepath.Ext(path), ".csv") {
			return "", fmt.Errorf("choose a .zip Plausible export or a .csv analytics file")
		}
		return SourceCSV, nil
	}

	sources, closeArchive, err := sourcesFromZip(path)
	if err != nil {
		return "", err
	}
	if err := closeArchive(); err != nil {
		return "", fmt.Errorf("dataio: close upload archive: %w", err)
	}

	plausible, visitors := 0, false
	for _, source := range sources {
		table := plausibleTable(source.Name)
		if table == "" {
			continue
		}
		plausible++
		visitors = visitors || table == "imported_visitors"
	}

	if plausible > 0 {
		if !visitors {
			return "", fmt.Errorf("that looks like a Plausible export but is missing imported_visitors CSV")
		}
		if plausible != len(sources) {
			return "", fmt.Errorf("that Plausible archive contains CSV files whose table names are not recognised")
		}
		return SourcePlausible, nil
	}

	return SourceCSV, nil
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
	seen := map[string]bool{}
	var expanded uint64

	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(filepath.Ext(entry.Name), ".csv") {
			continue
		}

		if strings.HasPrefix(filepath.Base(entry.Name), ".") {
			continue
		}

		name := strings.ToLower(filepath.Base(entry.Name))
		if seen[name] {
			return nil, nil, closeArchiveWith(archive,
				fmt.Errorf("that zip file contains more than one CSV named %s", filepath.Base(entry.Name)))
		}
		seen[name] = true

		if entry.Flags&0x1 != 0 {
			return nil, nil, closeArchiveWith(archive,
				fmt.Errorf("%s is encrypted — export it again without a password", filepath.Base(entry.Name)))
		}

		if entry.UncompressedSize64 > MaxArchiveUncompressedBytes {
			return nil, nil, closeArchiveWith(archive, tooLarge(filepath.Base(entry.Name)))
		}

		expanded += entry.UncompressedSize64
		if expanded > MaxArchiveUncompressedBytes {
			return nil, nil, closeArchiveWith(archive,
				fmt.Errorf("that zip expands beyond the %d MB import limit", MaxArchiveUncompressedBytes>>20))
		}

		if entry.CompressedSize64 > 0 && entry.UncompressedSize64/entry.CompressedSize64 > MaxArchiveCompressionRatio {
			return nil, nil, closeArchiveWith(archive,
				fmt.Errorf("%s expands more than %d times and was refused as an unsafe archive",
					filepath.Base(entry.Name), MaxArchiveCompressionRatio))
		}

		// Our own export carries the raw events alongside the ten roll-up
		// tables. Reading it back as a roll-up would turn every event into a
		// one-pageview row and double the site's history, so a re-imported
		// export deliberately brings in the roll-ups only.
		if strings.EqualFold(filepath.Base(entry.Name), RawEventsSheet+".csv") {
			continue
		}

		if len(sources) >= MaxArchiveCSVFiles {
			return nil, nil, closeArchiveWith(archive,
				fmt.Errorf("that zip contains more than %d CSV files", MaxArchiveCSVFiles))
		}

		file := entry

		sources = append(sources, CSVSource{
			Name: filepath.Base(file.Name),
			Open: func() (io.ReadCloser, error) {
				reader, err := file.Open()
				if err != nil {
					return nil, err
				}

				return &limitedEntry{ReadCloser: reader, name: filepath.Base(file.Name), remaining: MaxArchiveUncompressedBytes + 1}, nil
			},
		})

	}

	if len(sources) == 0 {
		return nil, nil, closeArchiveWith(archive,
			fmt.Errorf("that zip file has no CSVs in it — an import expects the files named %s", strings.Join(SheetNames(), ", ")))
	}

	return sources, archive.Close, nil
}

// closeArchiveWith closes an archive that is being refused and keeps the
// reason alongside any close failure, so neither is lost.
func closeArchiveWith(archive *zip.ReadCloser, cause error) error {
	if closeErr := archive.Close(); closeErr != nil {
		cause = errors.Join(cause, fmt.Errorf("dataio: close refused upload archive: %w", closeErr))
	}

	return cause
}

// tooLarge is the message for an entry that inflates past the cap.
func tooLarge(name string) error {
	return fmt.Errorf("%s inflates to more than %d MB — that is a raw event log rather than a daily roll-up",
		name, MaxArchiveUncompressedBytes>>20)
}

// limitedEntry stops reading a zip entry that has grown past the cap. The
// central directory can claim any size it likes, so the check on the header is
// not enough on its own; this is what holds when the header lies.
type limitedEntry struct {
	io.ReadCloser
	name      string
	remaining int64
}

// Read hands back at most the bytes still allowed and fails once they are gone.
func (l *limitedEntry) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, tooLarge(l.name)
	}

	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}

	n, err := l.ReadCloser.Read(p)
	l.remaining -= int64(n)

	return n, err
}
