//
// journey.go
// Where visitors went before and after a page.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// How many steps a journey shows. Twenty is the ceiling because past that the
// tail is single visits and the list stops being a list of anything; ten is
// what the card shows without being asked.
const (
	MaxJourneySteps     = 20
	DefaultJourneySteps = 10
)

// The two ends of a visit. A step with nothing after it is where the visit
// ended, and one with nothing before it is where the visit started; both are
// rows on the chart rather than missing rows, because "most people leave from
// here" is the single most useful thing this report says.
const (
	ExitStep  = "(exit)"
	EntryStep = "(entry)"
)

// JourneyRequest asks where visitors go around one page.
type JourneyRequest struct {
	SiteID int64

	DateRange query.DateRange
	Timezone  string

	// Page is the exact path, matched exactly.
	//
	// Trailing slashes are deliberately not normalised here. /about/ and
	// /about are two rows in Top Pages, and a journeys report that quietly
	// merged them would not line up with the report beside it — which is
	// precisely the bug the incumbent shipped and then had to remove. If a
	// site wants those merged, that belongs in path-cleaning rules, applied
	// once and applied everywhere.
	Page string

	// Limit is how many steps each list returns.
	Limit int

	AnchorType string
	Anchor     string
	Direction  string
	Trail      []JourneyAnchor
	Grouping   string
	Filters    []query.Filter
	Exact      bool
}

// JourneyAnchor identifies a page, custom event, or configured goal in the
// interactive Explore trail.
type JourneyAnchor struct {
	Type   string `json:"type"`
	Value  string `json:"value"`
	Label  string `json:"label,omitempty"`
	GoalID int64  `json:"goal_id,omitempty"`
}

// ExploreStep is one selectable continuation from the current anchor.
type ExploreStep struct {
	Anchor   JourneyAnchor `json:"anchor"`
	Terminal bool          `json:"terminal,omitempty"`
	Visitors int64         `json:"visitors"`
	Visits   int64         `json:"visits"`
	Events   int64         `json:"events"`
}

// JourneyStep is one destination or origin.
type JourneyStep struct {
	// Value is a path, an event name, or one of the two end markers.
	Value string `json:"value"`

	// Terminal marks the entry or exit bucket rather than a real step.
	Terminal bool `json:"terminal,omitempty"`

	Visitors int64 `json:"visitors"`
	Visits   int64 `json:"visits"`
	Events   int64 `json:"events"`
}

// JourneyResult is the whole Explore card for one page.
type JourneyResult struct {
	Page string `json:"page"`

	// NextPages and PreviousPages are the pageviews either side of this one
	// inside the same visit.
	NextPages     []JourneyStep `json:"next_pages"`
	PreviousPages []JourneyStep `json:"previous_pages"`

	// NextEvents and PreviousEvents are the custom events either side of it.
	// They are computed over a different ordering — pageviews and events
	// together — because "what did they click here" and "which page did they
	// come from" are two different questions about the same visit.
	NextEvents     []JourneyStep `json:"next_events"`
	PreviousEvents []JourneyStep `json:"previous_events"`

	// Views and Visitors are the page itself, so every percentage the card
	// draws has its denominator in the same response.
	Views    int64 `json:"views"`
	Visitors int64 `json:"visitors"`

	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	Anchor    JourneyAnchor   `json:"anchor"`
	Direction string          `json:"direction"`
	Trail     []JourneyAnchor `json:"trail"`
	Steps     []ExploreStep   `json:"steps"`
}

// Journey answers the next-page and previous-page breakdowns for one page.
//
// Like funnels, this cannot go through the query compiler: "the page after
// this one" is a question about the order of a visit's events, and the answer
// comes from a window function rather than from a GROUP BY. Everything else
// about the window — which sites, which range, and the two exclusions — is the
// same clause the compiler builds, so the totals here reconcile with Top Pages.
func Journey(ctx context.Context, db *sql.DB, engine *query.Engine, req JourneyRequest) (*JourneyResult, error) {
	if req.AnchorType == "" {
		req.AnchorType = "page"
	}
	if req.Anchor == "" {
		req.Anchor = req.Page
	}
	if req.Page == "" && req.AnchorType == "page" {
		req.Page = req.Anchor
	}
	if req.Anchor == "" {
		return nil, invalid("a journey needs an anchor")
	}
	if req.Direction == "" {
		req.Direction = "forward"
	}
	if req.Direction != "forward" && req.Direction != "backward" {
		return nil, invalid("a journey direction is forward or backward, not %q", req.Direction)
	}
	if req.AnchorType != "page" && req.AnchorType != "event" && req.AnchorType != "goal" {
		return nil, invalid("a journey anchor is page, event, or goal, not %q", req.AnchorType)
	}

	limit := req.Limit
	if limit <= 0 {
		limit = DefaultJourneySteps
	}
	if limit > MaxJourneySteps {
		limit = MaxJourneySteps
	}

	resolved, err := resolveRange(ctx, db, engine, req.SiteID, req.DateRange, req.Timezone)
	if err != nil {
		return nil, err
	}

	window := NewWindow(resolved.Start, resolved.End)

	result := &JourneyResult{
		Page: req.Page, From: resolved.Start, To: resolved.End,
		Anchor:    JourneyAnchor{Type: req.AnchorType, Value: req.Anchor},
		Direction: req.Direction, Trail: req.Trail, Steps: []ExploreStep{},
	}

	if window.Empty() {
		return result, nil
	}

	pageviewID, err := eventNameID(ctx, db, ingest.EventPageview)
	if err != nil {
		return nil, err
	}

	engagementID, err := eventNameID(ctx, db, ingest.EventEngagement)
	if err != nil {
		return nil, err
	}
	steps, anchor, err := exploreJourney(ctx, db, engine, req, resolved, window, pageviewID, engagementID, limit)
	if err != nil {
		return nil, err
	}
	result.Anchor = anchor
	result.Steps = steps
	if req.AnchorType != "page" {
		return result, nil
	}

	var pathID int64

	err = db.QueryRowContext(ctx, "SELECT id FROM dim_pathname WHERE value = ?", req.Page).Scan(&pathID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A path this site has never served has no journey, and answering an
		// empty report is more honest than matching id 0, which is the empty
		// string every event with no path carries.
		return result, nil
	case err != nil:
		return nil, fmt.Errorf("goals: journey: %w", err)
	}

	next, previous, err := pageJourney(ctx, db, req.SiteID, window, pageviewID, pathID)
	if err != nil {
		return nil, err
	}

	result.NextPages = trim(next, limit)
	result.PreviousPages = trim(previous, limit)

	nextEvents, previousEvents, err := eventJourney(ctx, db, req.SiteID, window, pageviewID, engagementID, pathID)
	if err != nil {
		return nil, err
	}

	result.NextEvents = trim(nextEvents, limit)
	result.PreviousEvents = trim(previousEvents, limit)

	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT e.user_id)
		FROM events e
		WHERE `+conditionsFor(req.SiteID, window)+` AND e.name_id = ? AND e.pathname_id = ?`,
		bindsFor(req.SiteID, window, pageviewID, pathID)...,
	).Scan(&result.Views, &result.Visitors); err != nil {
		return nil, fmt.Errorf("goals: journey: %w", err)
	}

	return result, nil
}

// exploreJourney returns one direction of a typed journey. Calling the same
// endpoint with a selected step as the new anchor supports arbitrary depth
// while the trail preserves the path the user took.
func exploreJourney(ctx context.Context, db *sql.DB, engine *query.Engine, req JourneyRequest, resolved query.Resolved, window Window, pageviewID, engagementID int64, limit int) ([]ExploreStep, JourneyAnchor, error) {
	anchor := JourneyAnchor{Type: req.AnchorType, Value: req.Anchor, Label: req.Anchor}
	var anchorPredicate predicate
	switch req.AnchorType {
	case "page":
		var pathID int64
		err := db.QueryRowContext(ctx, "SELECT id FROM dim_pathname WHERE value = ?", req.Anchor).Scan(&pathID)
		if errors.Is(err, sql.ErrNoRows) {
			return []ExploreStep{}, anchor, nil
		}
		if err != nil {
			return nil, anchor, fmt.Errorf("goals: explore page anchor: %w", err)
		}
		anchorPredicate = predicate{SQL: "e.name_id = ? AND e.pathname_id = ?", Args: []any{pageviewID, pathID}}
	case "event":
		nameID, err := eventNameID(ctx, db, req.Anchor)
		if err != nil {
			return nil, anchor, err
		}
		anchorPredicate = predicate{SQL: "e.name_id = ?", Args: []any{nameID}}
	case "goal":
		goalID, err := strconv.ParseInt(req.Anchor, 10, 64)
		if err != nil || goalID < 1 {
			return nil, anchor, invalid("a goal journey anchor needs a positive goal id")
		}
		goal, err := Get(ctx, db, goalID)
		if err != nil || goal.SiteID != req.SiteID {
			return nil, anchor, ErrNotFound
		}
		anchorPredicate, err = goalPredicate(ctx, db, goal, pageviewID)
		if err != nil {
			return nil, anchor, err
		}
		anchor.GoalID, anchor.Label = goal.ID, goal.Label()
	}

	filterSQL, filterArgs, err := engine.EventFilterSQL(ctx, []int64{req.SiteID}, resolved, req.Filters, "e")
	if err != nil {
		return nil, anchor, err
	}
	where, args := baseConditions("e", req.SiteID, window)
	if filterSQL != "" {
		where += " AND (" + filterSQL + ")"
		args = append(args, filterArgs...)
	}
	args = append(args, engagementID)
	adjacent := "LEAD"
	known := "LAG"
	terminal := ExitStep
	if req.Direction == "backward" {
		adjacent, known, terminal = "LAG", "LEAD", EntryStep
	}
	var trailColumns []string
	var trailPredicates []predicate
	for index := range req.Trail {
		offset := index + 1
		prefix := fmt.Sprintf("trail_%d_", offset)
		trailColumns = append(trailColumns,
			fmt.Sprintf("%s(e.id, %d) OVER (PARTITION BY e.session_id ORDER BY e.timestamp, e.id) AS %sid", known, offset, prefix),
			fmt.Sprintf("%s(e.name_id, %d) OVER (PARTITION BY e.session_id ORDER BY e.timestamp, e.id) AS %sname_id", known, offset, prefix),
			fmt.Sprintf("%s(e.pathname_id, %d) OVER (PARTITION BY e.session_id ORDER BY e.timestamp, e.id) AS %spathname_id", known, offset, prefix),
			fmt.Sprintf("%s(e.scroll_depth, %d) OVER (PARTITION BY e.session_id ORDER BY e.timestamp, e.id) AS %sscroll_depth", known, offset, prefix))
		trailAnchor := req.Trail[len(req.Trail)-1-index]
		compiled, err := journeyAnchorPredicate(ctx, db, req.SiteID, trailAnchor, pageviewID, prefix)
		if err != nil {
			return nil, anchor, err
		}
		trailPredicates = append(trailPredicates, compiled)
	}
	extraColumns := ""
	if len(trailColumns) > 0 {
		extraColumns = ", " + strings.Join(trailColumns, ", ")
	}
	valueExpression := `(CASE WHEN adjacent_name IS NULL THEN NULL
		WHEN adjacent_name = ` + strconv.FormatInt(pageviewID, 10) + ` THEN (SELECT value FROM dim_pathname WHERE id = adjacent_path)
		ELSE (SELECT value FROM dim_event_name WHERE id = adjacent_name) END)`
	if req.Grouping == "prefix" {
		valueExpression = `(CASE WHEN adjacent_name IS NULL THEN NULL
		WHEN adjacent_name = ` + strconv.FormatInt(pageviewID, 10) + ` THEN (
			SELECT CASE WHEN instr(substr(value, 2), '/') > 0
				THEN '/' || substr(substr(value, 2), 1, instr(substr(value, 2), '/') - 1) || '/**'
				ELSE value END FROM dim_pathname WHERE id = adjacent_path)
		ELSE (SELECT value FROM dim_event_name WHERE id = adjacent_name) END)`
	}
	outerConditions := []string{"(" + anchorPredicate.SQL + ")"}
	args = append(args, anchorPredicate.Args...)
	for _, compiled := range trailPredicates {
		outerConditions = append(outerConditions, "("+compiled.SQL+")")
		args = append(args, compiled.Args...)
	}
	statement := `WITH ordered AS (
		SELECT e.id, e.site_id, e.session_id, e.user_id, e.name_id, e.pathname_id, e.scroll_depth,
		       ` + adjacent + `(e.name_id) OVER (PARTITION BY e.session_id ORDER BY e.timestamp, e.id) AS adjacent_name,
		       ` + adjacent + `(e.pathname_id) OVER (PARTITION BY e.session_id ORDER BY e.timestamp, e.id) AS adjacent_path` + extraColumns + `
		FROM events e WHERE ` + where + ` AND e.name_id <> ?
	)
	SELECT adjacent_name, ` + valueExpression + ` AS adjacent_value,
	       COUNT(DISTINCT user_id), COUNT(DISTINCT session_id), COUNT(*)
	FROM ordered e WHERE ` + strings.Join(outerConditions, " AND ") + `
	GROUP BY adjacent_name, adjacent_value ORDER BY 5 DESC LIMIT ?`
	args = append(args, int64(limit))
	rows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, anchor, fmt.Errorf("goals: explore journey: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type rawStep struct {
		name  sql.NullInt64
		value sql.NullString
		step  ExploreStep
	}
	var raw []rawStep
	for rows.Next() {
		var item rawStep
		if err := rows.Scan(&item.name, &item.value, &item.step.Visitors, &item.step.Visits, &item.step.Events); err != nil {
			return nil, anchor, fmt.Errorf("goals: explore journey: %w", err)
		}
		raw = append(raw, item)
	}
	if err := rows.Err(); err != nil {
		return nil, anchor, fmt.Errorf("goals: explore journey: %w", err)
	}
	steps := make([]ExploreStep, 0, len(raw))
	for _, item := range raw {
		if !item.name.Valid {
			item.step.Anchor = JourneyAnchor{Type: "terminal", Value: terminal, Label: terminal}
			item.step.Terminal = true
		} else if item.name.Int64 == pageviewID {
			value := item.value.String
			item.step.Anchor = JourneyAnchor{Type: "page", Value: value, Label: value}
		} else {
			value := item.value.String
			item.step.Anchor = JourneyAnchor{Type: "event", Value: value, Label: value}
		}
		steps = append(steps, item.step)
	}
	return steps, anchor, nil
}

// journeyAnchorPredicate compiles a typed trail anchor against one set of
// ordered-event columns, including goal property constraints through event id.
func journeyAnchorPredicate(ctx context.Context, db *sql.DB, siteID int64, anchor JourneyAnchor, pageviewID int64, prefix string) (predicate, error) {
	alias := prefix
	switch anchor.Type {
	case "page":
		var pathID int64
		if err := db.QueryRowContext(ctx, "SELECT id FROM dim_pathname WHERE value = ?", anchor.Value).Scan(&pathID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return predicate{SQL: "0 = 1"}, nil
			}
			return predicate{}, err
		}
		return predicate{SQL: alias + "name_id = ? AND " + alias + "pathname_id = ?", Args: []any{pageviewID, pathID}}, nil
	case "event":
		nameID, err := eventNameID(ctx, db, anchor.Value)
		if err != nil {
			return predicate{}, err
		}
		return predicate{SQL: alias + "name_id = ?", Args: []any{nameID}}, nil
	case "goal":
		goalID := anchor.GoalID
		if goalID == 0 {
			goalID, _ = strconv.ParseInt(anchor.Value, 10, 64)
		}
		goal, err := Get(ctx, db, goalID)
		if err != nil || goal.SiteID != siteID {
			return predicate{}, ErrNotFound
		}
		compiled, err := goalPredicate(ctx, db, goal, pageviewID)
		if err != nil {
			return predicate{}, err
		}
		compiled.SQL = strings.ReplaceAll(compiled.SQL, "e.name_id", alias+"name_id")
		compiled.SQL = strings.ReplaceAll(compiled.SQL, "e.pathname_id", alias+"pathname_id")
		compiled.SQL = strings.ReplaceAll(compiled.SQL, "e.scroll_depth", alias+"scroll_depth")
		compiled.SQL = strings.ReplaceAll(compiled.SQL, "e.id", alias+"id")
		return compiled, nil
	default:
		return predicate{}, invalid("a journey trail anchor is page, event, or goal, not %q", anchor.Type)
	}
}

// firstPathSegment groups a path by its first non-empty segment.
func firstPathSegment(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "/"
	}
	return "/" + parts[0] + "/**"
}

// conditionsFor and bindsFor are the base clause split into its two halves, so
// a statement that needs to append its own conditions can interleave the
// arguments in the right order.
func conditionsFor(siteID int64, window Window) string {
	sqlText, _ := baseConditions("e", siteID, window)

	return sqlText
}

// bindsFor returns the base clause's arguments followed by any extras, which
// is the order every statement in this file binds them in.
func bindsFor(siteID int64, window Window, extra ...any) []any {
	_, args := baseConditions("e", siteID, window)

	return append(args, extra...)
}

// pageJourney reads the pageview before and after each view of the page.
//
// The ordering is pageviews only. A custom event fired between two pageviews
// is not a page, and letting one interrupt the sequence would report "the page
// after /pricing" as a signup button.
func pageJourney(ctx context.Context, db *sql.DB, siteID int64, window Window, pageviewID, pathID int64) ([]JourneyStep, []JourneyStep, error) {
	statement := `
		WITH steps AS (
			SELECT e.session_id AS session_id, e.user_id AS user_id, e.pathname_id AS page,
			       LAG(e.pathname_id) OVER (PARTITION BY e.session_id ORDER BY e.timestamp, e.id) AS before,
			       LEAD(e.pathname_id) OVER (PARTITION BY e.session_id ORDER BY e.timestamp, e.id) AS after
			FROM events e
			WHERE ` + conditionsFor(siteID, window) + ` AND e.name_id = ?
		)
		SELECT 'after' AS direction, after AS step,
		       COUNT(DISTINCT user_id), COUNT(DISTINCT session_id), COUNT(*)
		FROM steps WHERE page = ? GROUP BY step
		UNION ALL
		SELECT 'before', before,
		       COUNT(DISTINCT user_id), COUNT(DISTINCT session_id), COUNT(*)
		FROM steps WHERE page = ? GROUP BY before`

	rows, err := db.QueryContext(ctx, statement, bindsFor(siteID, window, pageviewID, pathID, pathID)...)
	if err != nil {
		return nil, nil, fmt.Errorf("goals: journey pages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		ids    = map[int64]bool{}
		after  []JourneyStep
		before []JourneyStep
		items  []journeyRow
	)

	for rows.Next() {
		var row journeyRow

		if err := rows.Scan(&row.Direction, &row.Step, &row.Visitors, &row.Visits, &row.Events); err != nil {
			return nil, nil, fmt.Errorf("goals: journey pages: %w", err)
		}

		if row.Step.Valid {
			ids[row.Step.Int64] = true
		}

		items = append(items, row)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("goals: journey pages: %w", err)
	}

	labels, err := labelsFor(ctx, db, "dim_pathname", ids)
	if err != nil {
		return nil, nil, err
	}

	for _, row := range items {
		step := JourneyStep{Visitors: row.Visitors, Visits: row.Visits, Events: row.Events}

		switch {
		case row.Step.Valid:
			step.Value = labels[row.Step.Int64]
		case row.Direction == "after":
			step.Value, step.Terminal = ExitStep, true
		default:
			step.Value, step.Terminal = EntryStep, true
		}

		if row.Direction == "after" {
			after = append(after, step)
			continue
		}

		before = append(before, step)
	}

	return after, before, nil
}

// eventJourney reads the custom event immediately before and after a view of
// the page. Its ordering includes both pageviews and custom events, because
// "what did they do on this page" is only answerable if a pageview and the
// click that followed it are adjacent in the same sequence.
//
// Engagement pings are excluded. They are not something a visitor did — they
// exist to carry time on page and scroll depth — and every one of them would
// otherwise be the step after every page.
func eventJourney(ctx context.Context, db *sql.DB, siteID int64, window Window, pageviewID, engagementID, pathID int64) ([]JourneyStep, []JourneyStep, error) {
	statement := `
		WITH steps AS (
			SELECT e.session_id AS session_id, e.user_id AS user_id,
			       e.name_id AS name, e.pathname_id AS page,
			       LAG(e.name_id) OVER (PARTITION BY e.session_id ORDER BY e.timestamp, e.id) AS before,
			       LEAD(e.name_id) OVER (PARTITION BY e.session_id ORDER BY e.timestamp, e.id) AS after
			FROM events e
			WHERE ` + conditionsFor(siteID, window) + ` AND e.name_id <> ?
		)
		SELECT 'after' AS direction, after AS step,
		       COUNT(DISTINCT user_id), COUNT(DISTINCT session_id), COUNT(*)
		FROM steps WHERE name = ? AND page = ? AND after IS NOT NULL AND after <> ? GROUP BY step
		UNION ALL
		SELECT 'before', before,
		       COUNT(DISTINCT user_id), COUNT(DISTINCT session_id), COUNT(*)
		FROM steps WHERE name = ? AND page = ? AND before IS NOT NULL AND before <> ? GROUP BY before`

	args := bindsFor(siteID, window, engagementID,
		pageviewID, pathID, pageviewID,
		pageviewID, pathID, pageviewID)

	rows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("goals: journey events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		ids    = map[int64]bool{}
		items  []journeyRow
		after  []JourneyStep
		before []JourneyStep
	)

	for rows.Next() {
		var row journeyRow

		if err := rows.Scan(&row.Direction, &row.Step, &row.Visitors, &row.Visits, &row.Events); err != nil {
			return nil, nil, fmt.Errorf("goals: journey events: %w", err)
		}

		if row.Step.Valid {
			ids[row.Step.Int64] = true
		}

		items = append(items, row)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("goals: journey events: %w", err)
	}

	labels, err := labelsFor(ctx, db, "dim_event_name", ids)
	if err != nil {
		return nil, nil, err
	}

	for _, row := range items {
		if !row.Step.Valid {
			continue
		}

		step := JourneyStep{
			Value:    labels[row.Step.Int64],
			Visitors: row.Visitors,
			Visits:   row.Visits,
			Events:   row.Events,
		}

		if row.Direction == "after" {
			after = append(after, step)
			continue
		}

		before = append(before, step)
	}

	return after, before, nil
}

// journeyRow is one raw row of a journey statement, before its interned id has
// been turned back into a string.
type journeyRow struct {
	Direction string
	Step      sql.NullInt64
	Visitors  int64
	Visits    int64
	Events    int64
}

// labelsFor turns a set of interned ids back into strings. It runs after the
// aggregate rather than as a join inside it, because joining a dimension table
// before grouping drags a text column through the whole scan where doing it
// afterwards touches only the rows being returned.
func labelsFor(ctx context.Context, db *sql.DB, table string, ids map[int64]bool) (map[int64]string, error) {
	labels := map[int64]string{}

	if len(ids) == 0 {
		return labels, nil
	}

	args := make([]any, 0, len(ids))
	marks := make([]byte, 0, len(ids)*2)

	for id := range ids {
		args = append(args, id)
		marks = append(marks, '?', ',')
	}

	// The table name is a constant from this package rather than anything a
	// caller supplied, which is why it can be concatenated where every value
	// still travels as a bind parameter.
	rows, err := db.QueryContext(ctx,
		"SELECT id, value FROM "+table+" WHERE id IN ("+string(marks[:len(marks)-1])+")", args...)
	if err != nil {
		return nil, fmt.Errorf("goals: read %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id    int64
			value string
		)

		if err := rows.Scan(&id, &value); err != nil {
			return nil, fmt.Errorf("goals: read %s: %w", table, err)
		}

		labels[id] = value
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("goals: read %s: %w", table, err)
	}

	return labels, nil
}

// trim sorts a journey list biggest first and cuts it to the requested length.
// The sort is here rather than in SQL because both directions come back from
// one statement, and ordering inside a UNION ALL would order the union rather
// than each half of it.
func trim(steps []JourneyStep, limit int) []JourneyStep {
	sort.SliceStable(steps, func(i, j int) bool {
		if steps[i].Events != steps[j].Events {
			return steps[i].Events > steps[j].Events
		}

		return steps[i].Value < steps[j].Value
	})

	if len(steps) > limit {
		steps = steps[:limit]
	}

	return steps
}
