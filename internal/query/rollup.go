//
// rollup.go
// The seam where pre-aggregated days are read instead of raw events.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Source names where one slice of a query's date range is read from.
type Source int

const (
	// SourceRaw is the events and sessions tables.
	SourceRaw Source = iota

	// SourceRollup is a pre-aggregated summary table.
	SourceRollup
)

// String renders a source for a log line or a test failure.
func (s Source) String() string {
	if s == SourceRollup {
		return "rollup"
	}

	return "raw"
}

// Segment is one contiguous slice of a query's date range and where it is read
// from. A query over the last 28 days is 27 complete days out of the summary
// tables and today out of the raw events, which is two segments whose results
// are added together.
type Segment struct {
	Range  Resolved
	Source Source

	// Grain is which summary the segment reads, and is meaningless on a raw
	// segment. Daily rows serve everything except an hourly graph.
	Grain Grain
}

// Router decides which source answers each slice of a range.
//
// It is an interface with more than one implementation because "read this from
// a summary" has to be a decision the engine can be handed rather than one it
// makes: a filtered query, a query in a timezone the summary was not built in,
// or a query for a metric the summary does not carry all have to fall back to
// raw, and the check belongs in one place.
type Router interface {
	// Route splits a resolved range into the segments that answer it. It
	// returns at least one segment, or the error that stopped it reading what
	// has been built — a summary it cannot check is not one it may read from.
	Route(ctx context.Context, q *Query, r Resolved) ([]Segment, error)
}

// RawRouter reads everything from the raw tables. It stays the right answer for
// any query a roll-up cannot serve — a filtered query, or one grouped by a
// dimension the summary does not carry.
type RawRouter struct{}

// Route answers with the whole range, read raw.
func (RawRouter) Route(_ context.Context, _ *Query, r Resolved) ([]Segment, error) {
	return []Segment{{Range: r, Source: SourceRaw}}, nil
}

// splitAtToday cuts a range into the part that is finished and the part that is
// still running. It is the split the roll-up router makes — complete days come
// from the summary, the day in progress cannot — and it lives here rather than
// in the router so that the boundary arithmetic is written and tested once,
// against the same timezone handling everything else uses.
func splitAtToday(r Resolved) (complete Resolved, partial Resolved, split bool) {
	if !r.IncludesNow() {
		return r, Resolved{}, false
	}

	boundary := startOfDay(r.Now, r.Location)

	// A range that starts today has no complete part, and one that has not
	// reached today has no partial part. Neither is a split.
	if !boundary.After(r.Start) {
		return r, Resolved{}, false
	}

	complete = r
	complete.End = boundary

	partial = r
	partial.Start = boundary

	return complete, partial, true
}

// Splittable reports whether a query's metrics can be answered from more than
// one raw segment and added together. Counting rows adds up across two time
// slices; counting distinct visitors does not, because the same person can
// appear in both, and adding those two counts would invent visitors who do not
// exist.
//
// A roll-up split is exempt: its buckets carry the carry-over counts that make
// a distinct count re-aggregate exactly, and the engine subtracts them. This
// check is what stops any *other* router splitting a range it must not.
func Splittable(q *Query, p *plan) bool {
	for _, name := range q.Metrics {
		definition, ok := metricByName(name)
		if !ok {
			return false
		}

		if !definition.additive(p.MetricTable[name]) {
			return false
		}
	}

	return true
}

// Grain is a roll-up bucket width. The two are stored in the same tables behind
// a discriminator, and they are kept apart because they answer different
// questions: an hourly row exists to be drawn as an hour and a daily row exists
// to be added up.
type Grain int

const (
	// GrainDay is one local day per row, kept forever.
	GrainDay Grain = 0

	// GrainHour is one local hour per row, kept for about a fortnight because
	// nothing offers an hourly interval over a longer range.
	GrainHour Grain = 1
)

// String renders a grain for a log line, a command's output or a test failure.
func (g Grain) String() string {
	if g == GrainHour {
		return "hour"
	}

	return "day"
}

// RollupDim is one thing the summary tables are keyed by. It is a registry
// rather than a switch for the same reason the dimension registry is: the
// builder, the reader and the pruner all need the same three facts — which
// table, which code, which column on each fact table — and three copies of that
// is three chances for a report to read a column the builder never wrote.
type RollupDim struct {
	// Code is the value stored in the `dimension` column. It is an explicit
	// number rather than an index so that adding a dimension never renumbers
	// the rows already on disk.
	Code int

	// Name is the query dimension this answers, empty for the whole-site
	// totals row.
	Name string

	// Table is the roll-up table the rows live in.
	Table string

	// EventColumn is the `events` column this dimension groups by, empty when
	// the dimension has no event-grain answer. SessionColumn is the same for
	// `sessions`.
	EventColumn   string
	SessionColumn string

	// Total marks the one row per bucket that describes the whole site. It has
	// no group-by column at all, which is what makes the headline numbers a
	// read of thirty rows rather than thirty thousand.
	Total bool
}

// The dimension codes. They are stable numbers written into every row, so one
// may be retired but never reused for something else.
const (
	rollupCodeTotal         = 0
	rollupCodePage          = 1
	rollupCodeHostname      = 2
	rollupCodePageTitle     = 3
	rollupCodeEventName     = 4
	rollupCodeEntryPage     = 10
	rollupCodeEntryHostname = 11
	rollupCodeExitPage      = 20
	rollupCodeSource        = 30
	rollupCodeReferrer      = 31
	rollupCodeChannel       = 32
	rollupCodeUTMSource     = 33
	rollupCodeUTMMedium     = 34
	rollupCodeUTMCampaign   = 35
	rollupCodeCountry       = 40
	rollupCodeRegion        = 41
	rollupCodeCity          = 42
	rollupCodeDevice        = 50
	rollupCodeScreen        = 51
	rollupCodeBrowser       = 60
	rollupCodeBrowserVer    = 61
	rollupCodeOS            = 70
	rollupCodeOSVersion     = 71
	rollupCodeLanguage      = 80
)

// entryHostnameDimension is the wire name of a dimension that does not exist.
// A hostname breakdown carrying a bounce rate is scoped to the visits that
// entered on that hostname, exactly as a page breakdown is, so the summary
// needs a keying by entry hostname even though nobody can ask for one directly.
const entryHostnameDimension = "visit:entry_hostname"

// rollupDims is the registry. Every table in the migration appears here, and a
// dimension missing from it is simply one the summary cannot answer — the
// router sends that query to raw rather than guessing.
var rollupDims = []RollupDim{
	{Code: rollupCodeTotal, Name: "", Table: "rollup_visitors", Total: true},

	{Code: rollupCodePage, Name: "event:page", Table: "rollup_pages", EventColumn: "pathname_id"},
	{Code: rollupCodeHostname, Name: "event:hostname", Table: "rollup_pages", EventColumn: "hostname_id"},
	{Code: rollupCodePageTitle, Name: "event:page_title", Table: "rollup_pages", EventColumn: "page_title_id"},

	{Code: rollupCodeEventName, Name: "event:name", Table: "rollup_custom_events", EventColumn: "name_id"},

	{Code: rollupCodeEntryPage, Name: "visit:entry_page", Table: "rollup_entry_pages", SessionColumn: "entry_page_id"},
	{Code: rollupCodeEntryHostname, Name: entryHostnameDimension, Table: "rollup_entry_pages", SessionColumn: "entry_hostname_id"},

	{Code: rollupCodeExitPage, Name: "visit:exit_page", Table: "rollup_exit_pages", SessionColumn: "exit_page_id"},

	{Code: rollupCodeSource, Name: "visit:source", Table: "rollup_sources", EventColumn: "source_id", SessionColumn: "source_id"},
	{Code: rollupCodeReferrer, Name: "visit:referrer", Table: "rollup_sources", EventColumn: "referrer_id", SessionColumn: "referrer_id"},
	{Code: rollupCodeChannel, Name: "visit:channel", Table: "rollup_sources", EventColumn: "channel_id", SessionColumn: "channel_id"},
	{Code: rollupCodeUTMSource, Name: "visit:utm_source", Table: "rollup_sources", EventColumn: "utm_source_id", SessionColumn: "utm_source_id"},
	{Code: rollupCodeUTMMedium, Name: "visit:utm_medium", Table: "rollup_sources", EventColumn: "utm_medium_id", SessionColumn: "utm_medium_id"},
	{Code: rollupCodeUTMCampaign, Name: "visit:utm_campaign", Table: "rollup_sources", EventColumn: "utm_campaign_id", SessionColumn: "utm_campaign_id"},

	{Code: rollupCodeCountry, Name: "visit:country", Table: "rollup_locations", EventColumn: "country_id", SessionColumn: "country_id"},
	{Code: rollupCodeRegion, Name: "visit:region", Table: "rollup_locations", EventColumn: "region_id", SessionColumn: "region_id"},
	{Code: rollupCodeCity, Name: "visit:city", Table: "rollup_locations", EventColumn: "city_id", SessionColumn: "city_id"},

	{Code: rollupCodeDevice, Name: "visit:device", Table: "rollup_devices", EventColumn: "device_type_id", SessionColumn: "device_type_id"},
	{Code: rollupCodeScreen, Name: "visit:screen", Table: "rollup_devices", EventColumn: "screen_size_id", SessionColumn: "screen_size_id"},

	{Code: rollupCodeBrowser, Name: "visit:browser", Table: "rollup_browsers", EventColumn: "browser_id", SessionColumn: "browser_id"},
	{Code: rollupCodeBrowserVer, Name: "visit:browser_version", Table: "rollup_browsers", EventColumn: "browser_version_id", SessionColumn: "browser_version_id"},

	{Code: rollupCodeOS, Name: "visit:os", Table: "rollup_operating_systems", EventColumn: "os_id", SessionColumn: "os_id"},
	{Code: rollupCodeOSVersion, Name: "visit:os_version", Table: "rollup_operating_systems", EventColumn: "os_version_id", SessionColumn: "os_version_id"},

	{Code: rollupCodeLanguage, Name: "visit:language", Table: "rollup_languages", EventColumn: "language_id", SessionColumn: "language_id"},
}

// RollupDims returns the registry. The builder walks it to decide what to
// aggregate, so a dimension added here is a dimension that starts being built
// without any other change.
func RollupDims() []RollupDim {
	out := make([]RollupDim, len(rollupDims))
	copy(out, rollupDims)

	return out
}

// RollupTables lists the summary tables, sorted. The pruner and the rebuild
// command clear every one of them, and reading the list from the registry means
// a new table cannot be left behind by a job that does not know about it.
func RollupTables() []string {
	seen := map[string]bool{}
	var names []string

	for _, d := range rollupDims {
		if seen[d.Table] {
			continue
		}
		seen[d.Table] = true
		names = append(names, d.Table)
	}

	sort.Strings(names)

	return names
}

// BuildsFromEvents reports whether this dimension has an event-grain
// aggregation. The whole-site row has one; a dimension that only exists on
// `sessions`, such as an entry page, does not.
func (d RollupDim) BuildsFromEvents() bool {
	return d.Total || d.EventColumn != ""
}

// BuildsFromSessions reports whether this dimension has a visit-grain
// aggregation.
func (d RollupDim) BuildsFromSessions() bool {
	return d.Total || d.SessionColumn != ""
}

// EventGroupSQL is the events-table expression the builder groups by. The
// whole-site row groups by a literal zero, so the builder writes one statement
// shape rather than two.
func (d RollupDim) EventGroupSQL() string {
	if d.Total {
		return "0"
	}

	return "e." + d.EventColumn
}

// SessionGroupSQL is the sessions-table expression the builder groups by.
func (d RollupDim) SessionGroupSQL() string {
	if d.Total {
		return "0"
	}

	return "s." + d.SessionColumn
}

// rollupDimByName finds the registry entry for a query dimension name.
func rollupDimByName(name string) (RollupDim, bool) {
	for _, d := range rollupDims {
		if d.Name == name {
			return d, true
		}
	}

	return RollupDim{}, false
}

// rollupTotals is the whole-site entry, which every query with no keyed
// dimension reads.
func rollupTotals() RollupDim {
	found, _ := rollupDimByName("")

	return found
}

// RollupLocalUnix renders an instant as the local seconds the `bucket` column
// stores: the wall-clock reading in the site's timezone, counted from the
// epoch. It is the one conversion between the two clocks in this package, so a
// bucket bound and a bucket value can never be computed two different ways.
func RollupLocalUnix(at time.Time, loc *time.Location) int64 {
	if loc == nil {
		loc = time.UTC
	}

	_, offset := at.In(loc).Zone()

	return at.Unix() + int64(offset)
}

// RollupBucketExpr renders the SQL that turns a UTC timestamp column into the
// local-seconds bucket the row is stored under. The offsets in force across the
// window travel as bind parameters because SQLite has no timezone database, and
// they are computed span by span so that a daylight saving change puts events
// in the local day a reader actually had rather than shifting an hour of
// traffic into the wrong one.
//
// The truncation is integer arithmetic rather than a call to date(), and that
// is not a micro-optimisation: a build evaluates this expression once per event
// per dimension, and SQLite's date functions format and re-parse a string every
// time. Reading a local day off the wall clock is exact here because a local
// day is always 86,400 *local* seconds from midnight to midnight — the clocks
// going back makes a day longer in real time, not in wall-clock time.
func RollupBucketExpr(column string, grain Grain, loc *time.Location, from, to time.Time) (string, []any) {
	local := localExpr(column, zoneOffsets(loc, from, to))

	period := int64(86400)
	if grain == GrainHour {
		period = 3600
	}

	// A half-hour-offset zone still starts its hours on the local minute zero,
	// which falls out of truncating the wall clock rather than the instant.
	return fmt.Sprintf("((%s) / %d * %d)", local.SQL, period, period), local.Args
}

// RollupPreviousBucketSQL renders the bucket immediately before another, which
// is what the carry-over counts are defined against. Both grains are a constant
// number of wall-clock seconds wide, so this is subtraction.
func RollupPreviousBucketSQL(column string, grain Grain) string {
	if grain == GrainHour {
		return "(" + column + " - 3600)"
	}

	return "(" + column + " - 86400)"
}

// RollupBucketStart snaps an instant back to the start of its local bucket.
func RollupBucketStart(at time.Time, grain Grain, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}

	at = at.In(loc)

	if grain == GrainHour {
		return time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), 0, 0, 0, loc)
	}

	return time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, loc)
}

// RollupNextBucket steps one bucket forward, through the calendar for days so a
// daylight saving change does not shift every later bucket by an hour.
func RollupNextBucket(at time.Time, grain Grain, loc *time.Location) time.Time {
	if grain == GrainHour {
		return at.Add(time.Hour)
	}

	return startOfDay(at.AddDate(0, 0, 1), loc)
}

// RollupCoverage is what one site's summary actually holds, as the router needs
// to see it. The window is in local seconds and the timezone is the one the
// buckets were cut in — a query in another timezone is asking about different
// days and cannot read these rows.
type RollupCoverage struct {
	Timezone string
	From     int64
	Through  int64
}

// RollupState reads what has been built. It is an interface so the engine can
// be handed a fake in a test, and so that the query package does not have to
// know how the worker records its progress.
type RollupState interface {
	// Coverage returns what is built for one site and grain. The boolean is
	// false when nothing is; the error is a read that failed, which is never
	// the same thing as nothing being built.
	Coverage(ctx context.Context, siteID int64, grain Grain) (RollupCoverage, bool, error)
}

// databaseState reads rollup_state. An engine lives for one request, so a read
// here happens a couple of times per query against a table with one row per
// site and grain; that is cheaper than a cache that would have to be
// invalidated.
type databaseState struct {
	db *sql.DB
}

// Coverage answers what is built. A missing row is "nothing built"; anything
// else is returned, because a summary that cannot be checked must fail loudly
// rather than quietly put every dashboard on the slow path.
func (s databaseState) Coverage(ctx context.Context, siteID int64, grain Grain) (RollupCoverage, bool, error) {
	var coverage RollupCoverage

	err := s.db.QueryRowContext(ctx,
		"SELECT timezone, covered_from, covered_through FROM rollup_state WHERE site_id = ? AND grain = ?",
		siteID, int(grain),
	).Scan(&coverage.Timezone, &coverage.From, &coverage.Through)

	if errors.Is(err, sql.ErrNoRows) {
		return RollupCoverage{}, false, nil
	}
	if err != nil {
		return RollupCoverage{}, false, fmt.Errorf("query: read roll-up coverage: %w", err)
	}

	return coverage, true, nil
}

// RollupRouter answers the complete days of a range from the summary tables and
// leaves today to the raw ones.
//
// Everything it refuses, it refuses by returning one raw segment, because a
// summary is a cache: being unable to use it has to be slow, never wrong.
type RollupRouter struct {
	State RollupState
}

// NewRollupRouter builds a router over an account's read handle.
func NewRollupRouter(db *sql.DB) *RollupRouter {
	return &RollupRouter{State: databaseState{db: db}}
}

// Route splits the range. The order of the checks is the order they get
// cheaper: the ones that need no database come first, so a filtered query costs
// nothing to refuse.
func (r *RollupRouter) Route(ctx context.Context, q *Query, resolved Resolved) ([]Segment, error) {
	raw := []Segment{{Range: resolved, Source: SourceRaw}}

	if r == nil || r.State == nil {
		return raw, nil
	}

	read, ok := planRollupRead(q, resolved)
	if !ok {
		return raw, nil
	}

	complete, partial, split := splitAtToday(resolved)

	// A range that is entirely in the future, or entirely today, has no
	// complete day in it at all.
	if !complete.End.After(complete.Start) {
		return raw, nil
	}

	coverage, ok, err := r.State.Coverage(ctx, q.SiteIDs[0], read.grain)
	if err != nil {
		return nil, err
	}
	if !ok || coverage.Timezone != q.Timezone {
		return raw, nil
	}

	from := RollupLocalUnix(complete.Start, resolved.Location)
	through := RollupLocalUnix(complete.End, resolved.Location)

	if from < coverage.From || through > coverage.Through {
		return raw, nil
	}

	segments := []Segment{{Range: complete, Source: SourceRollup, Grain: read.grain}}
	if split {
		segments = append(segments, Segment{Range: partial, Source: SourceRaw})
	}

	return segments, nil
}

// rollupRead is everything the reader needs once a query has been accepted:
// which grain answers it, which keyed dimension it groups by, and whether the
// carry-over correction has to be applied.
type rollupRead struct {
	grain Grain

	// keyed is the summary dimension the breakdown groups by, per fact table.
	// A page breakdown keys the event half by the page that was viewed and the
	// visit half by the page the visit entered on, which is why there are two.
	eventDim   RollupDim
	sessionDim RollupDim

	hasEvent   bool
	hasSession bool

	// timeIndex is where the time bucket sits in the request's dimension list,
	// or -1 when the query does not group by time.
	timeIndex int

	// keyIndex is where the keyed dimension sits, or -1.
	keyIndex int

	// perBucket is true when every output group is exactly one summary bucket,
	// which is when the carry-over correction is not needed at all.
	perBucket bool
}

// planRollupRead decides whether the summary tables can answer a query exactly,
// and how. Everything it refuses is something whose answer from a summary would
// differ from the answer from raw, which is the one outcome worse than a slow
// query.
func planRollupRead(q *Query, resolved Resolved) (rollupRead, bool) {
	read := rollupRead{timeIndex: -1, keyIndex: -1}

	// One site. Roll-up rows are keyed per site and their counts add across
	// sites, but a visitor seen on two sites of the same account would be added
	// twice, and there is no cheap way to tell that apart from two visitors.
	if len(q.SiteIDs) != 1 {
		return read, false
	}

	// A filter narrows the rows a summary has already collapsed, so there is
	// nothing left to filter. Sampling and including bots change which rows
	// were counted, and the summary counted the default set.
	//
	// Including imported history does not. The builder writes native, non-bot
	// rows only, which is exactly the set the raw native pass reads, and
	// imported rows are added afterwards by a statement over their own table.
	// The summary is therefore a valid substitute for the native half either
	// way — and since the dashboard asks for imports on every report, treating
	// it as a refusal put every dashboard query in the product on a raw scan.
	if len(q.Filters) > 0 || q.SampleRate != 1 || q.Include.Bots {
		return read, false
	}

	blueprint, err := decide(q)
	if err != nil {
		return read, false
	}

	// A composite needs a second query of its own shape against raw rows.
	if len(blueprint.Specials) > 0 {
		return read, false
	}

	for _, name := range q.Metrics {
		if _, ok := rollupComponents(name, blueprint.MetricTable[name]); !ok {
			return read, false
		}
	}

	if !readDimensions(&read, q, blueprint, resolved) {
		return read, false
	}

	read.hasEvent = blueprint.Primary == tableEvents || (blueprint.HasSecondary && blueprint.Secondary == tableEvents)
	read.hasSession = blueprint.Primary == tableSessions || (blueprint.HasSecondary && blueprint.Secondary == tableSessions)

	if read.hasEvent && !read.eventDim.BuildsFromEvents() {
		return read, false
	}

	if read.hasSession && !read.sessionDim.BuildsFromSessions() {
		return read, false
	}

	return read, true
}

// readDimensions works out the grain and the keyed dimension, refusing any
// grouping the summary is not keyed by.
func readDimensions(read *rollupRead, q *Query, blueprint *plan, resolved Resolved) bool {
	read.eventDim = rollupTotals()
	read.sessionDim = rollupTotals()

	for i, d := range blueprint.Dimensions {
		switch {
		case d.Time:
			if read.timeIndex >= 0 {
				return false
			}
			read.timeIndex = i

		default:
			if read.keyIndex >= 0 {
				return false
			}
			read.keyIndex = i

			eventDim, sessionDim, ok := rollupDimsFor(d, blueprint)
			if !ok {
				return false
			}

			read.eventDim, read.sessionDim = eventDim, sessionDim
		}
	}

	switch resolved.Interval {
	case IntervalMinute:
		// Realtime is a thirty-minute window of raw rows and is already fast.
		return false

	case IntervalHour:
		// Hourly rows are only ever correct read one at a time: within a day a
		// visitor keeps the same id, so adding two hours together counts them
		// twice, and the hourly rows carry no correction for it.
		if read.timeIndex < 0 {
			return false
		}

		read.grain = GrainHour
		read.perBucket = true

	default:
		read.grain = GrainDay
		read.perBucket = read.timeIndex >= 0 && resolved.Interval == IntervalDay

		// Daily rows start at local midnight, so a range that starts anywhere
		// else cannot be assembled from them without splitting a bucket.
		if !resolved.Start.Equal(startOfDay(resolved.Start, resolved.Location)) {
			return false
		}
	}

	return true
}

// rollupDimsFor maps one query dimension onto the summary keying for each fact
// table. The two are not always the same: a bounce rate beside a page is
// measured over the visits that entered on it, so the event half is keyed by
// the page viewed and the visit half by the entry page — which is exactly what
// the raw path does, and the reason the answers match.
func rollupDimsFor(d dimension, blueprint *plan) (RollupDim, RollupDim, bool) {
	var (
		eventDim   RollupDim
		sessionDim RollupDim
	)

	if found, ok := rollupDimByName(d.Name); ok {
		eventDim, sessionDim = found, found
	} else {
		return eventDim, sessionDim, false
	}

	needsEvents := blueprint.Primary == tableEvents || (blueprint.HasSecondary && blueprint.Secondary == tableEvents)
	needsSessions := blueprint.Primary == tableSessions || (blueprint.HasSecondary && blueprint.Secondary == tableSessions)

	if needsEvents && eventDim.EventColumn == "" {
		// An entry or exit page has no event column; the raw path reaches it
		// by joining sessions, and the summary carries no such rows.
		return eventDim, sessionDim, false
	}

	if needsSessions && sessionDim.SessionColumn == "" {
		// The event-scoped dimensions that do compose with a visit metric do so
		// through their entry analogue, and that is a different keying.
		entry := ""

		switch d.Name {
		case "event:page":
			entry = "visit:entry_page"
		case "event:hostname":
			entry = entryHostnameDimension
		default:
			return eventDim, sessionDim, false
		}

		found, ok := rollupDimByName(entry)
		if !ok {
			return eventDim, sessionDim, false
		}

		sessionDim = found
	}

	return eventDim, sessionDim, true
}

// rollupComponent is one aggregate a metric reads out of a summary row: the
// column to sum, and the column recording how much of it was already counted in
// the bucket before.
type rollupComponent struct {
	column string

	// carried is the column that makes this component re-aggregate across
	// buckets, empty for a component that is a plain sum and needs no
	// correction. Counting rows adds up; counting distinct things does not.
	carried string
}

// rollupComponents returns the summary columns that reproduce a metric's
// components, in the order the metric defines them. The order is the contract:
// bounce_rate is bounces over visits, and getting the two the wrong way round
// produces a number that still looks like a percentage.
func rollupComponents(name string, t table) ([]rollupComponent, bool) {
	if t == tableSessions {
		switch name {
		case "visitors":
			return []rollupComponent{{column: "visitors", carried: "visitors_carried"}}, true
		case "visits":
			return []rollupComponent{{column: "visits"}}, true
		case "bounce_rate":
			return []rollupComponent{{column: "bounces"}, {column: "visits"}}, true
		case "visit_duration":
			return []rollupComponent{{column: "visit_duration"}, {column: "visits"}}, true
		case "views_per_visit":
			return []rollupComponent{{column: "session_pageviews"}, {column: "visits"}}, true
		}

		return nil, false
	}

	switch name {
	case "visitors":
		return []rollupComponent{{column: "event_visitors", carried: "event_visitors_carried"}}, true
	case "visits":
		return []rollupComponent{{column: "event_visits", carried: "event_visits_carried"}}, true
	case "pageviews":
		return []rollupComponent{{column: "pageviews"}}, true
	case "events":
		return []rollupComponent{{column: "events"}}, true
	}

	// time_on_page and scroll_depth are averaged over the visits whose tracker
	// reported a measurement, which is a distinct count of sessions that cannot
	// be corrected the way a visitor count can — a visitor id lasts a day and a
	// visit can last as long as somebody keeps clicking.
	return nil, false
}

// utcSpans is the offset list for a column that is already local. The `bucket`
// column stores local seconds, so rendering its label needs no conversion at
// all — but it goes through the same expression builder the raw path uses, so
// the two produce identical strings by construction rather than by agreement.
var utcSpans = []offsetSpan{{Until: math.MaxInt64, Offset: 0}}

// rollupColumnExpr renders one component as SQL over the summary rows,
// including the carry-over correction when the output group spans more than one
// bucket.
func rollupColumnExpr(component rollupComponent, alias string, perBucket bool, firsts []any) expr {
	sum := "SUM(" + alias + "." + component.column + ")"

	if component.carried == "" || perBucket || len(firsts) == 0 {
		return expr{SQL: sum}
	}

	carried := "SUM(CASE WHEN " + alias + ".bucket IN (" + placeholders(len(firsts)) + ") THEN 0 ELSE " +
		alias + "." + component.carried + " END)"

	return expr{SQL: "(" + sum + " - " + carried + ")", Args: append([]any{}, firsts...)}
}

// rollupWindow is the half-open bucket range a segment reads.
func rollupWindow(r Resolved) (int64, int64) {
	return RollupLocalUnix(r.Start, r.Location), RollupLocalUnix(r.End, r.Location)
}

// String renders a coverage window for a command's output or a log line.
func (c RollupCoverage) String() string {
	return fmt.Sprintf("%s..%s (%s)",
		time.Unix(c.From, 0).UTC().Format(time.RFC3339),
		time.Unix(c.Through, 0).UTC().Format(time.RFC3339),
		c.Timezone)
}

// RollupDimensionNames lists the dimensions the summary can group by, sorted.
// The `rollup status` command prints it, because "why is this report still
// slow" is nearly always "that dimension is not summarised".
func RollupDimensionNames() []string {
	names := make([]string, 0, len(rollupDims))

	for _, d := range rollupDims {
		if d.Name == "" || d.Name == entryHostnameDimension {
			continue
		}
		names = append(names, d.Name)
	}

	sort.Strings(names)

	return names
}
