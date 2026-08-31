//
// engine.go
// Compiling a query into statements, running them, and joining the pieces back together.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// Engine answers queries against one account's analytics database. It holds a
// read-only handle: a report must never be able to write, and a query bug that
// tries to should fail loudly rather than take the write lock away from
// ingestion.
type Engine struct {
	db *sql.DB

	// Now is the clock every date range is resolved against. It is injectable
	// because "today" is the single hardest thing to test in an analytics
	// product, and a suite that waits for real midnight is a suite nobody runs.
	Now func() time.Time

	// Router decides which source answers each slice of the range. Today that
	// is always the raw tables; see rollup.go for why the seam exists now.
	Router Router

	// MaxGroups bounds how many groups are pulled into memory when the
	// ordering cannot be pushed into SQL.
	MaxGroups int
}

// New builds an engine over an account's reader handle.
//
// It routes through the roll-up tables by default. That is safe on a database
// where nothing has been built yet: the router asks what is covered before it
// splits anything, finds nothing, and answers the whole range from raw.
func New(db *sql.DB) *Engine {
	return &Engine{
		db:        db,
		Now:       func() time.Time { return time.Now().UTC() },
		Router:    NewRollupRouter(db),
		MaxGroups: MaxGroups,
	}
}

// now reads the engine's clock.
func (e *Engine) now() time.Time {
	if e.Now == nil {
		return time.Now().UTC()
	}

	return e.Now()
}

// router returns the configured router, defaulting to raw.
func (e *Engine) router() Router {
	if e.Router == nil {
		return RawRouter{}
	}

	return e.Router
}

// maxGroups returns the configured group ceiling.
func (e *Engine) maxGroups() int {
	if e.MaxGroups <= 0 {
		return MaxGroups
	}

	return e.MaxGroups
}

// Run answers one query. Everything a caller got wrong comes back as *Error,
// which the HTTP layer turns into a 400 with the message attached; anything
// else is ours and is a 500.
func (e *Engine) Run(ctx context.Context, q Query) (*Result, error) {
	q.Normalise()

	if err := q.Validate(); err != nil {
		return nil, err
	}

	location, err := time.LoadLocation(q.Timezone)
	if err != nil {
		return nil, invalid("unknown timezone %q", q.Timezone)
	}

	resolved, err := e.resolveRange(ctx, &q, location)
	if err != nil {
		return nil, err
	}

	// The declared property scopes are read before anything is planned,
	// because whether a property describes a hit or a whole visit changes
	// which table can answer it and which denominator a rate divides by.
	scopes, err := e.propertyScopes(ctx, q.SiteIDs)
	if err != nil {
		return nil, err
	}

	blueprint, err := decideScoped(&q, scopes)
	if err != nil {
		return nil, err
	}

	compile, err := e.compileContext(ctx, &q)
	if err != nil {
		return nil, err
	}

	warnings := &warningSet{}

	primary := &executor{
		engine:   e,
		query:    &q,
		plan:     blueprint,
		resolved: resolved,
		compile:  compile,
		warnings: warnings,
	}

	// A breakdown that forced the session half of the query onto entry pages
	// says so on every session-scoped metric. The filter compiler raises the
	// same warning for a filter; this is the dimension's half of it.
	if blueprint.SessionsEntryScoped {
		primary.warnSessionMetrics(WarnEntryScoped, entryScopeWarning)
	}

	groups, err := primary.execute(ctx, nil)
	if err != nil {
		return nil, err
	}

	rows, total, err := primary.finalise(ctx, groups)
	if err != nil {
		return nil, err
	}

	result := &Result{
		Results: rows,
		Query:   resolvedQuery(&q, resolved),
		Meta: Meta{
			PresentIndex: resolved.PresentIndex(),
			Interval:     resolved.Interval,
			SampleRate:   q.SampleRate,

			// Named from the segments the query actually read, not from a
			// second call to the router: a router that answered differently the
			// second time would label the numbers with the wrong provenance.
			Sources: sourceNames(primary.segments),
		},
	}

	if hasTimeDimension(&q) || q.Include.TimeLabels {
		result.Meta.TimeLabels = resolved.Labels()
	}

	if q.Include.TotalRows {
		result.Meta.TotalRows = &total
	}

	// The gaps are attached whether or not any rows came back. "Your imported
	// history does not carry country, so none of it is in this number" is most
	// of the answer when a filtered breakdown comes back looking thin.
	result.Meta.ImportGaps = primary.importGaps()

	if q.SampleRate < 1 {
		for _, name := range q.Metrics {
			warnings.add(name, WarnSampled,
				fmt.Sprintf("read from %g%% of visitors and scaled back up — totals are estimates", q.SampleRate*100))
		}
	}

	if q.Include.Comparisons != nil {
		if err := e.attachComparison(ctx, &q, blueprint, resolved, compile, primary.keyRestriction(groups), result); err != nil {
			return nil, err
		}
	}

	result.Meta.MetricWarnings = warnings.all()

	return result, nil
}

// resolveRange turns the request's date range into absolute bounds, reading the
// site's first event only when the range is "all".
func (e *Engine) resolveRange(ctx context.Context, q *Query, location *time.Location) (Resolved, error) {
	var earliest time.Time

	if q.DateRange.NeedsEarliest() {
		found, err := e.earliestEvent(ctx, q.SiteIDs)
		if err != nil {
			return Resolved{}, err
		}
		earliest = found
	}

	resolved, err := q.DateRange.Resolve(e.now().In(location), location, earliest)
	if err != nil {
		return Resolved{}, err
	}

	return resolved.WithInterval(explicitInterval(q)), nil
}

// attachComparison answers the same query over the earlier period and hangs the
// numbers off the rows already computed. It runs after the primary query so
// that it can be restricted to the groups that actually came back, rather than
// paginating a second, differently-ordered result set.
func (e *Engine) attachComparison(ctx context.Context, q *Query, blueprint *plan, resolved Resolved, compile compileContext, keys map[int][]any, result *Result) error {
	comparison, err := resolved.Compare(q.Include.Comparisons)
	if err != nil {
		return err
	}

	result.Meta.ComparisonDateRange = []string{
		comparison.Start.In(comparison.Location).Format(time.RFC3339),
		comparison.End.In(comparison.Location).Format(time.RFC3339),
	}

	previous := &executor{
		engine:   e,
		query:    q,
		plan:     blueprint,
		resolved: comparison,
		compile:  compile,
		warnings: &warningSet{},

		// The earlier period is never paginated. Its rows are looked up by the
		// key of a row that is already on the page, and a LIMIT here would cut
		// the earlier period by its own ordering — which is how a comparison
		// ends up attached to the wrong row.
		comparison: true,
	}

	// A time series cannot be restricted to the primary's keys, because the two
	// periods have different dates by definition.
	if hasTimeDimension(q) {
		keys = nil
	}

	groups, err := previous.execute(ctx, keys)
	if err != nil {
		return err
	}

	// The earlier period's rows are keyed by their labels rather than by their
	// interned ids, because that is what the rows already on the page carry.
	// Keying by id would look up "United States" under the number 5 and find
	// nothing, and a comparison that silently reads zero looks exactly like a
	// period in which nothing happened.
	earlierRows := groups.list()

	labels, err := previous.resolveLabels(ctx, earlierRows)
	if err != nil {
		return err
	}

	values := map[string][]float64{}
	for i, row := range earlierRows {
		values[strings.Join(labels[i], keySeparator)] = previous.metricValues(row)
	}

	// A time series is matched by position rather than by label, because the
	// two periods have different dates by definition: bucket three of last week
	// is what bucket three of this week is compared against.
	byIndex := timeOnly(q)
	var currentLabels, previousLabels []string

	if byIndex {
		currentLabels = resolved.Labels()
		previousLabels = comparison.Labels()
	}

	for i := range result.Results {
		row := &result.Results[i]

		key := strings.Join(row.Dimensions, keySeparator)

		if byIndex && len(row.Dimensions) == 1 {
			position := indexOf(currentLabels, row.Dimensions[0])
			if position < 0 || position >= len(previousLabels) {
				continue
			}
			key = previousLabels[position]
		}

		earlier, ok := values[key]
		if !ok {
			earlier = make([]float64, len(q.Metrics))
		}

		changes := make([]*float64, len(q.Metrics))
		for j := range q.Metrics {
			changes[j] = change(row.Metrics[j], earlier[j])
		}

		row.Comparison = &ComparisonRow{Metrics: earlier, Change: changes}
	}

	return nil
}

// compileContext reads the two interned event names every metric definition
// keys off. They are read once per query rather than per expression, and a name
// this account has never recorded resolves to -1 so it matches no row — id 0 is
// the empty string, and matching that would count every event with no name.
func (e *Engine) compileContext(ctx context.Context, q *Query) (compileContext, error) {
	compile := compileContext{pageviewNameID: -1, engagementNameID: -1, sampleRate: q.SampleRate}

	// Path cleaning is one indexed existence check per query rather than a
	// decision taken per row, and it is taken here so that every statement the
	// query goes on to build reads the page dimension the same way.
	cleaned, err := hasPathCleaning(ctx, e, q.SiteIDs)
	if err != nil {
		return compile, err
	}
	compile.pathClean = cleaned

	rows, err := e.db.QueryContext(ctx,
		"SELECT id, value FROM "+intern.EventName.Table()+" WHERE value IN (?, ?)",
		ingest.EventPageview, ingest.EventEngagement)
	if err != nil {
		return compile, fmt.Errorf("query: read event names: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id    int64
			value string
		)

		if err := rows.Scan(&id, &value); err != nil {
			return compile, fmt.Errorf("query: read event names: %w", err)
		}

		switch value {
		case ingest.EventPageview:
			compile.pageviewNameID = id
		case ingest.EventEngagement:
			compile.engagementNameID = id
		}
	}

	if err := rows.Err(); err != nil {
		return compile, fmt.Errorf("query: read event names: %w", err)
	}

	return compile, nil
}

// propertyScopes reads the account's property allow-list for the sites in a
// query. It is one small query against a table with a row per registered
// property, and it runs per query rather than being cached because a scope
// somebody just corrected has to take effect on the next refresh rather than
// after a process restart.
func (e *Engine) propertyScopes(ctx context.Context, sites []int64) (map[string]string, error) {
	condition := inInt64("site_id", sites)

	rows, err := e.db.QueryContext(ctx,
		"SELECT name, scope FROM allowed_properties WHERE "+condition.SQL, condition.Args...)
	if err != nil {
		return nil, fmt.Errorf("query: read property scopes: %w", err)
	}
	defer rows.Close()

	scopes := map[string]string{}

	for rows.Next() {
		var name, scope string

		if err := rows.Scan(&name, &scope); err != nil {
			return nil, fmt.Errorf("query: read property scopes: %w", err)
		}

		// Two sites of one account disagreeing about a name resolves to event
		// scope, which is the conservative reading: an event-scoped
		// denominator counts everybody, where a session-scoped one narrows the
		// set a rate is measured over.
		if existing, ok := scopes[name]; ok && existing != scope {
			scopes[name] = propScopeEvent
			continue
		}

		scopes[name] = scope
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read property scopes: %w", err)
	}

	return scopes, nil
}

// earliestEvent finds the site's first event, which is what "all time" starts
// from. A site with no events at all answers with the zero time, and the range
// resolver turns that into today rather than into 1970.
func (e *Engine) earliestEvent(ctx context.Context, sites []int64) (time.Time, error) {
	condition := inInt64("site_id", sites)

	var earliest sql.NullInt64
	if err := e.db.QueryRowContext(ctx, "SELECT MIN(timestamp) FROM events WHERE "+condition.SQL, condition.Args...).Scan(&earliest); err != nil {
		return time.Time{}, fmt.Errorf("query: read first event: %w", err)
	}

	if !earliest.Valid {
		return time.Time{}, nil
	}

	return time.Unix(earliest.Int64, 0).UTC(), nil
}

// keySeparator joins a row's dimension values into one map key. It is a unit
// separator because it cannot appear in a URL path, a country name or a bucket
// label, so two different rows can never collide into one key.
const keySeparator = "\x1f"

// groupRow is one group's raw components, before any metric is computed.
type groupRow struct {
	key string

	// raw holds the dimension values exactly as the database returned them:
	// an integer id for an interned dimension, a string for a time bucket or a
	// property. Labels are resolved later and only for the rows that survive
	// pagination.
	raw []any

	// components holds each metric's aggregate parts, keyed by metric name.
	components map[string][]float64
}

// groupSet is the accumulating result of every statement a query runs, keyed so
// that a second statement's numbers land on the row the first one created.
type groupSet struct {
	order []string
	rows  map[string]*groupRow
}

// newGroupSet builds an empty set.
func newGroupSet() *groupSet {
	return &groupSet{rows: map[string]*groupRow{}}
}

// upsert returns the row for a key, creating it in insertion order. Insertion
// order is the database's order, which is what makes the SQL fast path's ORDER
// BY survive all the way to the response.
func (g *groupSet) upsert(key string, raw []any) *groupRow {
	if row, ok := g.rows[key]; ok {
		return row
	}

	row := &groupRow{key: key, raw: raw, components: map[string][]float64{}}
	g.rows[key] = row
	g.order = append(g.order, key)

	return row
}

// list returns the rows in insertion order.
func (g *groupSet) list() []*groupRow {
	rows := make([]*groupRow, 0, len(g.order))
	for _, key := range g.order {
		rows = append(rows, g.rows[key])
	}

	return rows
}

// rowKey renders a row's dimension values as a map key.
func rowKey(raw []any) string {
	parts := make([]string, 0, len(raw))
	for _, value := range raw {
		parts = append(parts, valueString(value))
	}

	return strings.Join(parts, keySeparator)
}

// valueString renders one raw dimension value.
func valueString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case int64:
		return strconv.FormatInt(typed, 10)
	case string:
		return typed
	case []byte:
		return string(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return fmt.Sprint(typed)
	}
}

// indexOf finds a label's position, or -1.
func indexOf(values []string, target string) int {
	for i, value := range values {
		if value == target {
			return i
		}
	}

	return -1
}

// hasTimeDimension reports whether the query groups by time.
func hasTimeDimension(q *Query) bool {
	for _, name := range q.Dimensions {
		if isTimeDimension(name) {
			return true
		}
	}

	return false
}

// timeOnly reports whether time is the query's only dimension, which is the
// shape a graph asks for and the only one worth gap-filling.
func timeOnly(q *Query) bool {
	return len(q.Dimensions) == 1 && isTimeDimension(q.Dimensions[0])
}

// explicitInterval returns the bucket width an explicit time dimension asks
// for, or empty when the range's own width should stand.
func explicitInterval(q *Query) string {
	for _, name := range q.Dimensions {
		resolved, err := resolveDimension(name)
		if err == nil && resolved.Time && resolved.Interval != "" {
			return resolved.Interval
		}
	}

	return ""
}

// sourceNames renders the segments a query was routed to.
func sourceNames(segments []Segment) []string {
	seen := map[string]bool{}
	var names []string

	for _, segment := range segments {
		name := segment.Source.String()
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}
