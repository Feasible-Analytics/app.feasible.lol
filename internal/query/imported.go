//
// imported.go
// Reading imported history as first-class rows, and labelling the gaps honestly.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"fmt"
	"math/bits"
	"sort"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// ImportedTable is where imported history lives. It is a roll-up: one row per
// day per combination of the dimensions the source actually reported, with a
// counter per metric. Storing one row per imported pageview is not an option
// for a site bringing in sixty million of them.
const ImportedTable = "imported_rollups"

// importedAlias is the table's alias in every statement built here.
const importedAlias = "ir"

// importedDimension is one dimension an imported row can carry: the column that
// holds it, and the bit that records whether this particular row has it.
type importedDimension struct {
	Column string
	Bit    uint64
}

// importedOrder is the bit assignment, and it is append-only. The bits are
// stored in the `covered` column of every imported row, so re-ordering this
// list would silently re-interpret every import that has already run.
var importedOrder = []string{
	"event:page",
	"event:hostname",
	"event:page_title",
	"event:name",
	"visit:entry_page",
	"visit:exit_page",
	"visit:referrer",
	"visit:source",
	"visit:channel",
	"visit:utm_source",
	"visit:utm_medium",
	"visit:utm_campaign",
	"visit:country",
	"visit:region",
	"visit:city",
	"visit:device",
	"visit:screen",
	"visit:browser",
	"visit:browser_version",
	"visit:os",
	"visit:os_version",
	"visit:language",
}

// importedColumns maps each of those dimensions to its column on the roll-up
// table. Entry and exit page are session columns on the fact tables and plain
// columns here, because a roll-up row is neither an event nor a visit — it is a
// day's worth of both.
var importedColumns = map[string]string{
	"event:page":            "pathname_id",
	"event:hostname":        "hostname_id",
	"event:page_title":      "page_title_id",
	"event:name":            "name_id",
	"visit:entry_page":      "entry_page_id",
	"visit:exit_page":       "exit_page_id",
	"visit:referrer":        "referrer_id",
	"visit:source":          "source_id",
	"visit:channel":         "channel_id",
	"visit:utm_source":      "utm_source_id",
	"visit:utm_medium":      "utm_medium_id",
	"visit:utm_campaign":    "utm_campaign_id",
	"visit:country":         "country_id",
	"visit:region":          "region_id",
	"visit:city":            "city_id",
	"visit:device":          "device_type_id",
	"visit:screen":          "screen_size_id",
	"visit:browser":         "browser_id",
	"visit:browser_version": "browser_version_id",
	"visit:os":              "os_id",
	"visit:os_version":      "os_version_id",
	"visit:language":        "language_id",
}

// importedDimensions is the resolved registry, built once from the two lists
// above so the bit and the column for a dimension can never disagree.
var importedDimensions = buildImportedDimensions()

// buildImportedDimensions assigns one bit per dimension, in list order.
func buildImportedDimensions() map[string]importedDimension {
	out := make(map[string]importedDimension, len(importedOrder))

	for i, name := range importedOrder {
		out[name] = importedDimension{Column: importedColumns[name], Bit: 1 << uint(i)}
	}

	return out
}

// ImportedDimensionNames lists every dimension an import can carry, sorted. The
// importer prints it in the error for a column it does not recognise.
func ImportedDimensionNames() []string {
	names := append([]string(nil), importedOrder...)
	sort.Strings(names)

	return names
}

// ImportedColumn returns the roll-up column a dimension is stored in.
func ImportedColumn(name string) (string, bool) {
	found, ok := importedDimensions[name]

	return found.Column, ok
}

// ImportedCoverage turns a list of dimension names into the bitmask an imported
// row stores. It is exported because the importer writes the mask and the query
// layer reads it, and two copies of this arithmetic would eventually disagree
// about which bit meant which dimension.
func ImportedCoverage(names []string) (uint64, error) {
	var mask uint64

	for _, name := range names {
		found, ok := importedDimensions[name]
		if !ok {
			return 0, fmt.Errorf("%q is not a dimension imported data can carry — known ones are %s",
				name, strings.Join(ImportedDimensionNames(), ", "))
		}

		mask |= found.Bit
	}

	return mask, nil
}

// ImportedMetricNames lists the metrics imported roll-ups can answer. The three
// composites are absent because none of them has an aggregate a daily total
// could carry: a scroll depth is a distribution and an exit rate needs the
// pageviews of a specific page in a specific visit.
func ImportedMetricNames() []string {
	return []string{"visitors", "visits", "pageviews", "events", "bounce_rate", "visit_duration", "views_per_visit", "time_on_page"}
}

// importedComponents returns the aggregate expressions that feed one metric
// from the roll-up table. The component order matches the metric definition
// exactly, because Combine reads them positionally.
func importedComponents(name string) ([]expr, bool) {
	sum := func(column string) expr { return expr{SQL: "SUM(" + importedAlias + "." + column + ")"} }

	switch name {
	case "visitors":
		// A daily visitor count cannot be de-duplicated against the native
		// tables, and adding it to a distinct count is the standard treatment
		// for historical imports: the two sources cover different days. Where
		// they overlap the total is an upper bound, and the response says so.
		return []expr{sum("visitors")}, true
	case "visits":
		return []expr{sum("visits")}, true
	case "pageviews":
		return []expr{sum("pageviews")}, true
	case "events":
		return []expr{sum("events")}, true
	case "bounce_rate":
		return []expr{sum("bounces"), sum("visits")}, true
	case "visit_duration":
		return []expr{sum("duration_total"), sum("visits")}, true
	case "views_per_visit":
		return []expr{sum("pageviews"), sum("visits")}, true
	case "time_on_page":
		return []expr{sum("engagement_total"), sum("visits")}, true
	}

	return nil, false
}

// ImportGap is imported data that could not answer a question, and how much of
// it there was. It is the whole difference between this implementation and the
// incumbent's: they import per-dimension marginals, so applying any filter
// makes the imported half silently read zero and a customer with sixty million
// pageviews concludes the feature does not work. Here the rows that cannot
// answer are counted and named, so the reader is told what is missing from the
// number rather than shown a number that is quietly wrong.
type ImportGap struct {
	// Dimension is the thing the imported rows do not carry.
	Dimension string `json:"dimension"`

	// Pageviews is how much imported traffic is excluded because of it.
	Pageviews float64 `json:"pageviews"`

	// Reason is the sentence to show the reader.
	Reason string `json:"reason"`
}

// importSelection is one import's chosen roll-up shape for this query: the
// `covered` value whose rows will be read, and nothing else from that import.
//
// Choosing exactly one shape per import is what stops the same traffic being
// counted twice. A full export brings in a totals sheet, a sources sheet and a
// pages sheet, and every one of them describes the same days: adding all three
// together would triple a customer's history the moment they finished an
// import. The shape chosen is the least detailed one that still carries every
// dimension the query needs, because a less detailed sheet aggregates fewer
// rows and therefore over-states distinct visitors least.
type importSelection struct {
	ImportID int64
	Covered  uint64
}

// importCandidate is one (import, shape) pair found in range, with how much
// traffic it accounts for.
type importCandidate struct {
	ImportID  int64
	Covered   uint64
	Pageviews float64
}

// importedPass adds imported history to the groups a query has already found,
// and records what the imported rows could not answer.
//
// It creates groups as well as merging into them. A page that exists only in
// imported history is a real row of the answer, and a pass that could only add
// numbers to rows the native tables already produced would drop every one of
// them — which is the same failure as importing marginals, arrived at from the
// other direction.
func (x *executor) importedPass(ctx context.Context, r Resolved, groups *groupSet, restrict map[int][]any) error {
	if !x.query.Include.Imports {
		return nil
	}

	// Sampling picks fact rows out of the raw tables and scales additive totals
	// back up. There are no fact rows to pick from in a pre-aggregated row, so an
	// imported total added to a sampled one would be scaled by a rate it was
	// never reduced by.
	if x.query.SampleRate > 0 && x.query.SampleRate < 1 {
		x.addGap(ImportGap{
			Dimension: "sample_rate",
			Reason:    "imported history is stored as daily totals and cannot be sampled, so it is left out of a sampled query",
		})

		return nil
	}

	required, unanswerable := x.importedRequirements()

	for _, gap := range unanswerable {
		x.addGap(gap)
	}

	candidates, err := x.importCandidates(ctx, r)
	if err != nil {
		return err
	}

	if len(candidates) == 0 {
		return nil
	}

	// A query that imported rows cannot express at all — a custom property, or
	// a goal that has to look inside a visit — excludes every one of them. The
	// volume is reported so the reader knows how much is missing.
	if len(unanswerable) > 0 {
		var total float64
		for _, candidate := range mostAggregated(candidates) {
			total += candidate.Pageviews
		}

		x.setGapVolume(total)

		return nil
	}

	mask, err := ImportedCoverage(required)
	if err != nil {
		return &Error{Message: err.Error()}
	}

	selected, missed := selectImports(candidates, mask)

	for _, gap := range describeMisses(missed, required) {
		x.addGap(gap)
	}

	if len(selected) == 0 {
		return nil
	}

	dims, err := x.importedDimensionColumns()
	if err != nil {
		return err
	}

	conditions := []expr{
		inInt64(importedAlias+".site_id", x.query.SiteIDs),
		{
			SQL:  importedAlias + ".timestamp >= ? AND " + importedAlias + ".timestamp < ?",
			Args: []any{r.Start.Unix(), r.End.Unix()},
		},
		selectionCondition(selected),
	}

	filters, err := x.importedFilters()
	if err != nil {
		return err
	}
	conditions = append(conditions, filters...)
	conditions = append(conditions, x.restrictions(dims, restrict)...)

	columns, targets := x.importedColumnsFor()
	if len(columns) == 0 {
		// Every metric in the query is a composite. There is nothing an
		// imported roll-up can add, and saying so is better than adding zero.
		x.addGap(ImportGap{
			Dimension: "metrics",
			Reason: "imported history carries daily totals, so it cannot answer scroll depth, exit rate or conversion rate — " +
				"those figures cover natively-collected traffic only",
		})

		return nil
	}

	st := statement{
		table: tableEvents, alias: importedAlias,
		dims: dims, columns: columns, conditions: conditions,
	}

	sqlText, args := x.renderStatement(st)

	// The rendered FROM names the fact table, so it is swapped for the roll-up
	// table here. Rendering is shared on purpose: the SELECT list, GROUP BY and
	// argument ordering are the parts that must not diverge from every other
	// statement this package builds.
	sqlText = strings.Replace(sqlText, " FROM events "+importedAlias, " FROM "+ImportedTable+" "+importedAlias, 1)

	if _, err := x.readRows(ctx, sqlText, args, len(dims), len(columns), groups, targets, true); err != nil {
		return err
	}

	return nil
}

// importCandidates lists every (import, shape) pair with data in range, and how
// much traffic each holds. One grouped read answers both halves of the job:
// which shape to read, and how much is being left out when none of them fits.
func (x *executor) importCandidates(ctx context.Context, r Resolved) ([]importCandidate, error) {
	sites := inInt64(importedAlias+".site_id", x.query.SiteIDs)

	args := append([]any{}, sites.Args...)
	args = append(args, r.Start.Unix(), r.End.Unix())

	rows, err := x.engine.db.QueryContext(ctx,
		"SELECT "+importedAlias+".import_id, "+importedAlias+".covered, COALESCE(SUM("+importedAlias+".pageviews), 0)"+
			" FROM "+ImportedTable+" "+importedAlias+
			" WHERE "+sites.SQL+" AND "+importedAlias+".timestamp >= ? AND "+importedAlias+".timestamp < ?"+
			" GROUP BY "+importedAlias+".import_id, "+importedAlias+".covered", args...)
	if err != nil {
		return nil, fmt.Errorf("query: read imported shapes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []importCandidate

	for rows.Next() {
		var candidate importCandidate
		var covered int64

		if err := rows.Scan(&candidate.ImportID, &covered, &candidate.Pageviews); err != nil {
			return nil, fmt.Errorf("query: read imported shapes: %w", err)
		}

		candidate.Covered = uint64(covered)
		candidates = append(candidates, candidate)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query: read imported shapes: %w", err)
	}

	return candidates, nil
}

// selectImports picks one shape per import and reports the imports that have
// none. The least detailed shape that still carries everything the query needs
// wins, because it aggregates the fewest rows and therefore over-states
// distinct visitors least.
func selectImports(candidates []importCandidate, mask uint64) ([]importSelection, []importCandidate) {
	best := map[int64]importCandidate{}

	for _, candidate := range candidates {
		if candidate.Covered&mask != mask {
			continue
		}

		current, ok := best[candidate.ImportID]
		if !ok || betterShape(candidate.Covered, current.Covered) {
			best[candidate.ImportID] = candidate
		}
	}

	selected := make([]importSelection, 0, len(best))
	for _, candidate := range best {
		selected = append(selected, importSelection{ImportID: candidate.ImportID, Covered: candidate.Covered})
	}

	sort.Slice(selected, func(i, j int) bool { return selected[i].ImportID < selected[j].ImportID })

	var missed []importCandidate
	for _, candidate := range mostAggregated(candidates) {
		if _, ok := best[candidate.ImportID]; ok {
			continue
		}

		missed = append(missed, candidate)
	}

	return selected, missed
}

// betterShape reports whether one coverage mask is a better read than another:
// fewer dimensions first, then the lower value so the choice is deterministic
// and two runs of the same query cannot disagree.
func betterShape(candidate, current uint64) bool {
	left, right := bits.OnesCount64(candidate), bits.OnesCount64(current)

	if left != right {
		return left < right
	}

	return candidate < current
}

// mostAggregated reduces the candidate list to one entry per import — the least
// detailed shape it holds, which is the one whose totals describe the import as
// a whole rather than a breakdown of it.
func mostAggregated(candidates []importCandidate) []importCandidate {
	best := map[int64]importCandidate{}

	for _, candidate := range candidates {
		current, ok := best[candidate.ImportID]
		if !ok || betterShape(candidate.Covered, current.Covered) {
			best[candidate.ImportID] = candidate
		}
	}

	out := make([]importCandidate, 0, len(best))
	for _, candidate := range best {
		out = append(out, candidate)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ImportID < out[j].ImportID })

	return out
}

// selectionCondition renders the chosen (import, shape) pairs as one predicate.
func selectionCondition(selected []importSelection) expr {
	parts := make([]expr, 0, len(selected))

	for _, choice := range selected {
		parts = append(parts, expr{
			SQL:  "(" + importedAlias + ".import_id = ? AND " + importedAlias + ".covered = ?)",
			Args: []any{choice.ImportID, int64(choice.Covered)},
		})
	}

	return or(parts)
}

// describeMisses turns the imports that could not answer into labelled gaps,
// naming the dimensions they do not carry. This sentence is what replaces the
// incumbent's silent zero.
func describeMisses(missed []importCandidate, required []string) []ImportGap {
	if len(missed) == 0 {
		return nil
	}

	byDimension := map[string]float64{}

	for _, candidate := range missed {
		for _, name := range required {
			if candidate.Covered&importedDimensions[name].Bit != 0 {
				continue
			}

			byDimension[name] += candidate.Pageviews
		}
	}

	gaps := make([]ImportGap, 0, len(byDimension))

	for name, pageviews := range byDimension {
		gaps = append(gaps, ImportGap{
			Dimension: name,
			Pageviews: pageviews,
			Reason: "the import that brought this history in does not break it down by " +
				strings.TrimPrefix(strings.TrimPrefix(name, "visit:"), "event:") +
				", so those pageviews are outside this answer rather than counted as zero",
		})
	}

	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Dimension < gaps[j].Dimension })

	return gaps
}

// importedRequirements lists the dimensions imported rows must carry to answer
// this query, and the gaps for anything they cannot express at all.
func (x *executor) importedRequirements() ([]string, []ImportGap) {
	seen := map[string]bool{}
	var required []string
	var gaps []ImportGap

	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		required = append(required, name)
	}

	for _, d := range x.plan.Dimensions {
		if d.Time {
			continue
		}

		if d.isProp() {
			gaps = append(gaps, ImportGap{
				Dimension: d.Name,
				Reason:    "imported history carries no custom properties, so this breakdown covers natively-collected traffic only",
			})
			continue
		}

		if _, ok := importedDimensions[d.Name]; !ok {
			gaps = append(gaps, ImportGap{
				Dimension: d.Name,
				Reason:    "imported history does not carry this dimension, so this breakdown covers natively-collected traffic only",
			})
			continue
		}

		add(d.Name)
	}

	for _, filter := range x.query.Filters {
		if filter.Operator == OpHasDone {
			gaps = append(gaps, ImportGap{
				Dimension: "has_done",
				Reason:    "imported history is stored as daily totals with no individual visits inside them, so a goal filter cannot select any of it",
			})
			continue
		}

		resolved, err := resolveDimension(filter.Dimension)
		if err != nil {
			continue
		}

		if resolved.isProp() {
			gaps = append(gaps, ImportGap{
				Dimension: filter.Dimension,
				Reason:    "imported history carries no custom properties, so this filter excludes all of it",
			})
			continue
		}

		if _, ok := importedDimensions[resolved.Name]; !ok {
			gaps = append(gaps, ImportGap{
				Dimension: filter.Dimension,
				Reason:    "imported history does not carry this dimension, so this filter excludes all of it",
			})
			continue
		}

		add(resolved.Name)
	}

	return required, gaps
}

// importedDimensionColumns compiles the query's group-by dimensions against the
// roll-up table.
func (x *executor) importedDimensionColumns() ([]compiledDim, error) {
	compiled := make([]compiledDim, 0, len(x.plan.Dimensions))

	for i, d := range x.plan.Dimensions {
		alias := fmt.Sprintf("d%d", i)

		if d.Time {
			compiled = append(compiled, compiledDim{
				dim: d, alias: alias,
				sql: bucketExpr(importedAlias+".timestamp", x.resolved.Interval, x.zoneSpans()),
			})
			continue
		}

		column, ok := ImportedColumn(d.Name)
		if !ok {
			return nil, invalid("%q cannot be grouped over imported data", d.Name)
		}

		compiled = append(compiled, compiledDim{
			dim: d, alias: alias,
			sql: expr{SQL: x.compile.pathColumn(importedAlias, column, d)},
		})
	}

	return compiled, nil
}

// importedFilters compiles the query's filters against the roll-up table. Every
// filter that reaches here is on a dimension the rows carry — anything else was
// turned into a gap before this ran — so the compilation is the ordinary
// interned-value membership test with no scope translation to do.
func (x *executor) importedFilters() ([]expr, error) {
	where := &whereBuilder{
		table:      tableEvents,
		alias:      importedAlias,
		ctx:        x.compile,
		sites:      x.query.SiteIDs,
		rangeStart: x.resolved.Start.Unix(),
		rangeEnd:   x.resolved.End.Unix(),
	}

	conditions := make([]expr, 0, len(x.query.Filters))

	for _, filter := range x.query.Filters {
		resolved, err := resolveDimension(filter.Dimension)
		if err != nil {
			return nil, err
		}

		column, ok := ImportedColumn(resolved.Name)
		if !ok {
			continue
		}

		predicate, err := where.values(expr{SQL: "value"}, filter)
		if err != nil {
			return nil, err
		}

		lookup := x.compile.pathColumn(importedAlias, column, resolved)

		inner := expr{
			SQL:  lookup + " IN (SELECT id FROM " + resolved.Interned.Table() + " WHERE " + predicate.SQL + ")",
			Args: predicate.Args,
		}

		conditions = append(conditions, negate(inner, filter.Negated()))
	}

	return conditions, nil
}

// importedColumnsFor builds the metric expressions the roll-up table can
// answer, and where each column belongs in the merged group.
func (x *executor) importedColumnsFor() ([]expr, []target) {
	var (
		columns []expr
		targets []target
	)

	for _, name := range x.query.Metrics {
		components, ok := importedComponents(name)
		if !ok {
			continue
		}

		for slot, component := range components {
			targets = append(targets, target{metric: name, slot: slot, column: len(columns)})
			columns = append(columns, component)
		}
	}

	return columns, targets
}

// addGap records a gap, keeping the first reason recorded for a dimension.
func (x *executor) addGap(gap ImportGap) {
	if x.gaps == nil {
		x.gaps = map[string]ImportGap{}
	}

	if _, exists := x.gaps[gap.Dimension]; exists {
		return
	}

	x.gaps[gap.Dimension] = gap
}

// setGapVolume attaches a pageview count to every gap that has none. It is used
// for the cases where nothing imported can answer at all, where the volume is a
// property of the range rather than of one dimension.
func (x *executor) setGapVolume(total float64) {
	for key, gap := range x.gaps {
		if gap.Pageviews == 0 {
			gap.Pageviews = total
			x.gaps[key] = gap
		}
	}
}

// importGaps renders the collected gaps in a stable order.
func (x *executor) importGaps() []ImportGap {
	if len(x.gaps) == 0 {
		return nil
	}

	out := make([]ImportGap, 0, len(x.gaps))
	for _, gap := range x.gaps {
		out = append(out, gap)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Dimension < out[j].Dimension })

	return out
}

// ImportedInterned returns the dimension table one dimension's values are
// interned in. The importer needs it to turn a CSV cell into the integer the
// roll-up column holds, and reading it out of the same registry the query
// compiler uses is what stops an import writing ids into the wrong table.
func ImportedInterned(name string) (intern.Dimension, bool) {
	found, ok := dimensions[name]
	if !ok || found.Interned == "" {
		return "", false
	}

	return found.Interned, true
}

// LocalDaySQL renders a UTC timestamp column as the site's local day label,
// with the bind arguments that go with it. It is exported for the exporter,
// which has to bucket by the same day boundary every report uses.
//
// Sharing the expression rather than writing a second one is the point:
// daylight saving means the offset is not constant across a range, and an
// export that used a single offset would put a day's traffic in the wrong row
// twice a year — in a file whose whole purpose is to be re-imported.
func LocalDaySQL(column string, loc *time.Location, from, to time.Time) (string, []any) {
	rendered := bucketExpr(column, IntervalDay, zoneOffsets(loc, from, to))

	return rendered.SQL, rendered.Args
}
