//
// sheets.go
// The ten CSV table formats, and the one vocabulary both halves read them with.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package dataio moves analytics data in and out: CSV import covering the ten
// table formats an established export produces, and a full-site export as a ZIP
// of the same ten files plus the raw events.
//
// The important decision is what an import becomes once it is in. Imported
// history is written as roll-up rows carrying the full dimension block and a
// record of which dimensions the source actually reported, so a filter on a
// dimension the data has narrows it exactly like native traffic, and a filter
// on a dimension the data lacks is reported as a labelled gap. The incumbent
// stores per-dimension marginals instead, which is why applying any filter to
// their imported data zeroes it out and why a customer with sixty million
// pageviews called the feature useless in public.
//
// Raw event export is included here, not held behind a plan. Data somebody
// generated on their own site is theirs.
package dataio

import (
	"sort"
	"strings"
)

// The metric fields a CSV column can carry. They are totals, never averages: an
// average of averages is wrong the moment two rows are added together, and
// every roll-up row is added to another one.
const (
	FieldVisitors   = "visitors"
	FieldVisits     = "visits"
	FieldPageviews  = "pageviews"
	FieldEvents     = "events"
	FieldBounces    = "bounces"
	FieldExits      = "exits"
	FieldDuration   = "visit_duration"
	FieldEngagement = "time_on_page"
)

// dimensionHeaders maps a CSV column name to the query dimension it holds. The
// spellings are the ones an established export uses, because matching them is
// what makes a competitor's export directory import here without editing.
var dimensionHeaders = map[string]string{
	"page":                     "event:page",
	"path":                     "event:page",
	"hostname":                 "event:hostname",
	"page_title":               "event:page_title",
	"name":                     "event:name",
	"event_name":               "event:name",
	"entry_page":               "visit:entry_page",
	"exit_page":                "visit:exit_page",
	"referrer":                 "visit:referrer",
	"source":                   "visit:source",
	"channel":                  "visit:channel",
	"utm_source":               "visit:utm_source",
	"utm_medium":               "visit:utm_medium",
	"utm_campaign":             "visit:utm_campaign",
	"country":                  "visit:country",
	"region":                   "visit:region",
	"city":                     "visit:city",
	"device":                   "visit:device",
	"screen_size":              "visit:screen",
	"browser":                  "visit:browser",
	"browser_version":          "visit:browser_version",
	"operating_system":         "visit:os",
	"operating_system_version": "visit:os_version",
	"language":                 "visit:language",
}

// metricHeaders maps a CSV column name to the roll-up field it fills. Entrances
// and exits are both visit counts under different names, and keeping both
// spellings is what lets an entry-page file and an exit-page file share one
// parser.
var metricHeaders = map[string]string{
	"visitors":       FieldVisitors,
	"visits":         FieldVisits,
	"entrances":      FieldVisits,
	"pageviews":      FieldPageviews,
	"events":         FieldEvents,
	"bounces":        FieldBounces,
	"exits":          FieldExits,
	"visit_duration": FieldDuration,
	"time_on_page":   FieldEngagement,
}

// DateHeader is the one column every format has.
const DateHeader = "date"

// Sheet is one of the ten table formats. It is declared rather than inferred so
// that the exporter and the importer agree about what a file is called and what
// is in it, and so that "which formats do you support" has an answer that is
// code rather than documentation.
type Sheet struct {
	// Name is the file's base name inside the export, without the extension.
	Name string

	// Dimensions are the query dimensions this format breaks the day down by.
	// An empty list is the totals sheet: one row per day and nothing else.
	Dimensions []string

	// Columns are the dimension columns in file order, as CSV header names.
	Columns []string

	// Metrics are the metric columns in file order.
	Metrics []string

	// Grain says which fact tables the exporter reads to build it. A sheet
	// broken down by page cannot carry a bounce rate, because a bounce is a
	// property of a whole visit and a page is not — the same rule the query
	// engine refuses that combination under.
	Grain grain
}

// grain names which fact tables a sheet is built from.
type grain int

const (
	// grainEvent counts hits: visitors, visits, pageviews, events, engagement.
	grainEvent grain = 1 << iota

	// grainSession counts visits: visits, bounces, visit duration.
	grainSession
)

// Sheets is every format, in the order an export writes them. Ten formats,
// which is what an established export produces, so a directory of their CSVs
// imports here file for file.
var Sheets = []Sheet{
	{
		Name: "imported_visitors", Grain: grainEvent | grainSession,
		Metrics: []string{FieldVisitors, FieldPageviews, FieldEvents, FieldVisits, FieldBounces, FieldDuration},
	},
	{
		Name: "imported_sources", Grain: grainEvent | grainSession,
		Dimensions: []string{"visit:source", "visit:referrer", "visit:utm_source", "visit:utm_medium", "visit:utm_campaign"},
		Columns:    []string{"source", "referrer", "utm_source", "utm_medium", "utm_campaign"},
		Metrics:    []string{FieldVisitors, FieldVisits, FieldPageviews, FieldBounces, FieldDuration},
	},
	{
		Name: "imported_pages", Grain: grainEvent,
		Dimensions: []string{"event:hostname", "event:page"},
		Columns:    []string{"hostname", "page"},
		Metrics:    []string{FieldVisitors, FieldVisits, FieldPageviews, FieldEngagement},
	},
	{
		Name: "imported_entry_pages", Grain: grainSession,
		Dimensions: []string{"visit:entry_page"},
		Columns:    []string{"entry_page"},
		Metrics:    []string{FieldVisitors, FieldVisits, FieldPageviews, FieldBounces, FieldDuration},
	},
	{
		Name: "imported_exit_pages", Grain: grainSession,
		Dimensions: []string{"visit:exit_page"},
		Columns:    []string{"exit_page"},
		Metrics:    []string{FieldVisitors, FieldExits, FieldPageviews},
	},
	{
		Name: "imported_locations", Grain: grainEvent | grainSession,
		Dimensions: []string{"visit:country", "visit:region", "visit:city"},
		Columns:    []string{"country", "region", "city"},
		Metrics:    []string{FieldVisitors, FieldVisits, FieldPageviews, FieldBounces, FieldDuration},
	},
	{
		Name: "imported_devices", Grain: grainEvent | grainSession,
		Dimensions: []string{"visit:device", "visit:screen"},
		Columns:    []string{"device", "screen_size"},
		Metrics:    []string{FieldVisitors, FieldVisits, FieldPageviews, FieldBounces, FieldDuration},
	},
	{
		Name: "imported_browsers", Grain: grainEvent | grainSession,
		Dimensions: []string{"visit:browser", "visit:browser_version"},
		Columns:    []string{"browser", "browser_version"},
		Metrics:    []string{FieldVisitors, FieldVisits, FieldPageviews, FieldBounces, FieldDuration},
	},
	{
		Name: "imported_operating_systems", Grain: grainEvent | grainSession,
		Dimensions: []string{"visit:os", "visit:os_version"},
		Columns:    []string{"operating_system", "operating_system_version"},
		Metrics:    []string{FieldVisitors, FieldVisits, FieldPageviews, FieldBounces, FieldDuration},
	},
	{
		Name: "imported_custom_events", Grain: grainEvent,
		Dimensions: []string{"event:name"},
		Columns:    []string{"name"},
		Metrics:    []string{FieldVisitors, FieldEvents},
	},
}

// RawEventsSheet is the raw event export. It is not one of the ten roll-up
// formats and is not importable as one — it is the customer's own events, one
// row each, included in every plan because data somebody generated is theirs.
const RawEventsSheet = "raw_events"

// Header renders one sheet's CSV header row.
func (s Sheet) Header() []string {
	header := make([]string, 0, 1+len(s.Columns)+len(s.Metrics))
	header = append(header, DateHeader)
	header = append(header, s.Columns...)
	header = append(header, s.Metrics...)

	return header
}

// SheetNames lists the formats, for an error message that has to say what is
// supported rather than merely that something is not.
func SheetNames() []string {
	names := make([]string, 0, len(Sheets))
	for _, sheet := range Sheets {
		names = append(names, sheet.Name+".csv")
	}

	return names
}

// KnownHeaders lists every column name the importer understands, sorted. It is
// what the error for an unrecognised column prints, because "unknown column"
// without the alternatives is a round trip to the documentation over a typo.
func KnownHeaders() []string {
	names := []string{DateHeader}

	for name := range dimensionHeaders {
		names = append(names, name)
	}
	for name := range metricHeaders {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// normaliseHeader folds a CSV column name into the form the maps are keyed by.
// Exports in the wild differ on case and on spaces versus underscores, and a
// file that fails to import over a capital letter is a file somebody edits by
// hand rather than a bug they report.
func normaliseHeader(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, "\ufeff") // a UTF-8 byte order mark, which spreadsheets add
	name = strings.ReplaceAll(name, " ", "_")
	name = strings.ReplaceAll(name, "-", "_")

	return name
}
