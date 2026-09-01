//
// funnel.go
// Funnels: the steps, and how far each visit got down them.
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
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// How many steps a funnel may have. One step is a goal report with extra
// chrome; nine is a chart nobody can read, and the bit mask the walk uses fits
// comfortably inside the limit.
const (
	MinFunnelSteps = 2
	MaxFunnelSteps = 8
)

// MaxFunnelName bounds a funnel's name for the same reason a goal's is
// bounded: it is a human sentence rendered in a heading.
const MaxFunnelName = 200

// Step is one position in a funnel.
type Step struct {
	Position int   `json:"position"`
	GoalID   int64 `json:"goal_id"`

	// Goal is filled in when a funnel is read back, so a caller rendering a
	// chart does not have to look every step up itself.
	Goal Goal `json:"goal"`
}

// Funnel is an ordered list of goals.
type Funnel struct {
	ID     int64  `json:"id"`
	SiteID int64  `json:"site_id"`
	Name   string `json:"name"`

	// StrictOrder requires the configured steps to be consecutive events.
	// With it off they must still happen in sequence, but unrelated activity is
	// allowed between them.
	StrictOrder bool `json:"strict_order"`

	CreatedAt int64  `json:"created_at"`
	Steps     []Step `json:"steps"`
}

// CreateFunnel stores a funnel and its steps. The steps are written in one
// transaction with the funnel, because a funnel with half its steps is a chart
// that would render a drop-off nobody's visitors actually had.
func CreateFunnel(ctx context.Context, db *sql.DB, funnel Funnel, now time.Time) (Funnel, error) {
	funnel.Name = strings.TrimSpace(funnel.Name)

	if funnel.SiteID == 0 {
		return Funnel{}, invalid("a funnel needs a site")
	}

	if funnel.Name == "" {
		return Funnel{}, invalid("a funnel needs a name")
	}

	if len(funnel.Name) > MaxFunnelName {
		return Funnel{}, invalid("a funnel name may be at most %d characters", MaxFunnelName)
	}

	if len(funnel.Steps) < MinFunnelSteps || len(funnel.Steps) > MaxFunnelSteps {
		return Funnel{}, invalid("a funnel has between %d and %d steps, not %d",
			MinFunnelSteps, MaxFunnelSteps, len(funnel.Steps))
	}
	if err := validateFunnelSteps(ctx, db, funnel.SiteID, funnel.Steps); err != nil {
		return Funnel{}, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Funnel{}, fmt.Errorf("goals: create funnel: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO funnels (site_id, name, strict_order, created_at)
		VALUES (?,?,?,?)
		ON CONFLICT(site_id, name) DO UPDATE SET strict_order = excluded.strict_order`,
		funnel.SiteID, funnel.Name, boolToInt(funnel.StrictOrder), now.Unix(),
	); err != nil {
		return Funnel{}, fmt.Errorf("goals: create funnel: %w", err)
	}

	if err := tx.QueryRowContext(ctx,
		"SELECT id, created_at FROM funnels WHERE site_id = ? AND name = ?", funnel.SiteID, funnel.Name,
	).Scan(&funnel.ID, &funnel.CreatedAt); err != nil {
		return Funnel{}, fmt.Errorf("goals: create funnel: %w", err)
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM funnel_steps WHERE funnel_id = ?", funnel.ID); err != nil {
		return Funnel{}, fmt.Errorf("goals: create funnel: %w", err)
	}

	for i := range funnel.Steps {
		// Positions are assigned from the slice order rather than trusted from
		// the caller: two steps claiming position three is a chart with a hole
		// in it, and the order the caller wrote them in is what they meant.
		funnel.Steps[i].Position = i + 1

		if _, err := tx.ExecContext(ctx,
			"INSERT INTO funnel_steps (funnel_id, position, goal_id) VALUES (?,?,?)",
			funnel.ID, funnel.Steps[i].Position, funnel.Steps[i].GoalID,
		); err != nil {
			return Funnel{}, fmt.Errorf("goals: create funnel step %d: %w", i+1, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return Funnel{}, fmt.Errorf("goals: create funnel: %w", err)
	}

	return GetFunnel(ctx, db, funnel.ID)
}

// UpdateFunnel atomically replaces a funnel's name, mode, and ordered steps
// while preserving its identity and creation time.
func UpdateFunnel(ctx context.Context, db *sql.DB, funnel Funnel) (Funnel, error) {
	if funnel.ID < 1 {
		return Funnel{}, invalid("a funnel update needs an id")
	}
	existing, err := GetFunnel(ctx, db, funnel.ID)
	if err != nil {
		return Funnel{}, err
	}
	if funnel.SiteID != 0 && funnel.SiteID != existing.SiteID {
		return Funnel{}, ErrNotFound
	}
	funnel.SiteID = existing.SiteID
	funnel.CreatedAt = existing.CreatedAt
	funnel.Name = strings.TrimSpace(funnel.Name)
	if funnel.Name == "" || len(funnel.Name) > MaxFunnelName {
		return Funnel{}, invalid("a funnel needs a name of at most %d characters", MaxFunnelName)
	}
	if len(funnel.Steps) < MinFunnelSteps || len(funnel.Steps) > MaxFunnelSteps {
		return Funnel{}, invalid("a funnel has between %d and %d steps, not %d", MinFunnelSteps, MaxFunnelSteps, len(funnel.Steps))
	}
	if err := validateFunnelSteps(ctx, db, funnel.SiteID, funnel.Steps); err != nil {
		return Funnel{}, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Funnel{}, fmt.Errorf("goals: update funnel: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op
	if _, err := tx.ExecContext(ctx, "UPDATE funnels SET name = ?, strict_order = ? WHERE id = ? AND site_id = ?",
		funnel.Name, boolToInt(funnel.StrictOrder), funnel.ID, funnel.SiteID); err != nil {
		return Funnel{}, fmt.Errorf("goals: update funnel: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM funnel_steps WHERE funnel_id = ?", funnel.ID); err != nil {
		return Funnel{}, fmt.Errorf("goals: update funnel: %w", err)
	}
	for i := range funnel.Steps {
		funnel.Steps[i].Position = i + 1
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO funnel_steps (funnel_id, position, goal_id) VALUES (?,?,?)",
			funnel.ID, i+1, funnel.Steps[i].GoalID); err != nil {
			return Funnel{}, fmt.Errorf("goals: update funnel step %d: %w", i+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Funnel{}, fmt.Errorf("goals: update funnel: %w", err)
	}
	return GetFunnel(ctx, db, funnel.ID)
}

// validateFunnelSteps ensures every step is a distinct goal belonging to the
// funnel's site; a repeated goal would make one event satisfy two conceptual
// stages and a cross-site goal would mix unrelated audiences.
func validateFunnelSteps(ctx context.Context, db *sql.DB, siteID int64, steps []Step) error {
	seen := map[int64]bool{}
	for _, step := range steps {
		if step.GoalID < 1 {
			return invalid("every funnel step needs a goal")
		}
		if seen[step.GoalID] {
			return invalid("goal %d appears more than once in this funnel", step.GoalID)
		}
		seen[step.GoalID] = true
		var owner int64
		if err := db.QueryRowContext(ctx, "SELECT site_id FROM goals WHERE id = ?", step.GoalID).Scan(&owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return invalid("goal %d does not exist", step.GoalID)
			}
			return fmt.Errorf("goals: validate funnel step: %w", err)
		}
		if owner != siteID {
			return invalid("goal %d belongs to another site", step.GoalID)
		}
	}
	return nil
}

// DeleteFunnel removes a funnel and its steps. The goals it pointed at stay:
// they are definitions in their own right and are almost always on the goals
// report as well.
func DeleteFunnel(ctx context.Context, db *sql.DB, id int64) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM funnel_steps WHERE funnel_id = ?", id); err != nil {
		return fmt.Errorf("goals: delete funnel: %w", err)
	}

	result, err := db.ExecContext(ctx, "DELETE FROM funnels WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("goals: delete funnel: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("goals: delete funnel: %w", err)
	}

	if affected == 0 {
		return ErrNotFound
	}

	return nil
}

// ListFunnels returns a site's funnels with their steps and the goals behind
// them.
func ListFunnels(ctx context.Context, db *sql.DB, siteID int64) ([]Funnel, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT id FROM funnels WHERE site_id = ? ORDER BY created_at, id", siteID)
	if err != nil {
		return nil, fmt.Errorf("goals: list funnels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []int64

	for rows.Next() {
		var id int64

		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("goals: list funnels: %w", err)
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("goals: list funnels: %w", err)
	}

	list := make([]Funnel, 0, len(ids))

	for _, id := range ids {
		funnel, err := GetFunnel(ctx, db, id)
		if err != nil {
			return nil, err
		}

		list = append(list, funnel)
	}

	return list, nil
}

// GetFunnel reads one funnel, its steps in position order, and each step's
// goal.
func GetFunnel(ctx context.Context, db *sql.DB, id int64) (Funnel, error) {
	var (
		funnel Funnel
		strict int
	)

	if err := db.QueryRowContext(ctx,
		"SELECT id, site_id, name, strict_order, created_at FROM funnels WHERE id = ?", id,
	).Scan(&funnel.ID, &funnel.SiteID, &funnel.Name, &strict, &funnel.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Funnel{}, ErrNotFound
		}

		return Funnel{}, fmt.Errorf("goals: get funnel: %w", err)
	}

	funnel.StrictOrder = strict != 0

	rows, err := db.QueryContext(ctx,
		"SELECT position, goal_id FROM funnel_steps WHERE funnel_id = ? ORDER BY position", id)
	if err != nil {
		return Funnel{}, fmt.Errorf("goals: get funnel: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var step Step

		if err := rows.Scan(&step.Position, &step.GoalID); err != nil {
			return Funnel{}, fmt.Errorf("goals: get funnel: %w", err)
		}

		funnel.Steps = append(funnel.Steps, step)
	}

	if err := rows.Err(); err != nil {
		return Funnel{}, fmt.Errorf("goals: get funnel: %w", err)
	}

	for i, step := range funnel.Steps {
		goal, err := Get(ctx, db, step.GoalID)
		if err != nil {
			return Funnel{}, err
		}

		funnel.Steps[i].Goal = goal
	}

	return funnel, nil
}

// FunnelRequest asks for one funnel over a period.
type FunnelRequest struct {
	FunnelID  int64
	DateRange query.DateRange
	Timezone  string
	Filters   []query.Filter
	Exact     bool
}

// FunnelStep is one step's numbers.
type FunnelStep struct {
	Position int    `json:"position"`
	Label    string `json:"label"`
	Goal     Goal   `json:"goal"`

	// Visitors and Visits are how many got this far. A visit counts when it
	// reached this step; a visitor counts when any single visit of theirs did.
	Visitors int64 `json:"visitors"`
	Visits   int64 `json:"visits"`

	// DropOff is how many visitors reached the step before this one and not
	// this one, and DropOffRate is that as a share of the previous step. Both
	// are zero on the first step, which nobody can drop off before.
	DropOff     int64   `json:"drop_off"`
	DropOffRate float64 `json:"drop_off_rate"`

	// ConversionRate is this step against the first one, which is the number
	// people mean by "our funnel converts at 4%".
	ConversionRate float64 `json:"conversion_rate"`
}

// FunnelResult is the whole chart.
type FunnelResult struct {
	Funnel Funnel       `json:"funnel"`
	Steps  []FunnelStep `json:"steps"`

	// From and To are the window actually measured, which starts at the latest
	// of the report range, funnel creation, and every step goal's creation.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	// Partial marks a funnel whose window was cut short because one of its
	// goals is newer than the report range. Without it a step added last week
	// looks like a catastrophic drop-off rather than a goal that did not exist
	// yet.
	Partial bool `json:"partial"`
}

// RunFunnel measures a funnel.
//
// This is one of the two reports in the product that does not go through the
// query compiler, and the reason is ordering: "did this visit do A and then B"
// is a question about the sequence of a visit's events, and no GROUP BY
// expresses it. So the matching events are read in visit order — there are
// only ever a few of them per visit, because they are goal events rather than
// pageviews — and the walk happens here.
//
// The window starts at the latest creation time of any step's goal. A step
// added last week would otherwise show every visit before it as a drop-off,
// which is a cliff on the chart that nothing in the customer's product caused.
func RunFunnel(ctx context.Context, db *sql.DB, engine *query.Engine, req FunnelRequest) (*FunnelResult, error) {
	funnel, err := GetFunnel(ctx, db, req.FunnelID)
	if err != nil {
		return nil, err
	}

	if len(funnel.Steps) < MinFunnelSteps {
		return nil, invalid("funnel %q has %d steps and needs at least %d", funnel.Name, len(funnel.Steps), MinFunnelSteps)
	}

	resolved, err := resolveRange(ctx, db, engine, funnel.SiteID, req.DateRange, req.Timezone)
	if err != nil {
		return nil, err
	}

	full := NewWindow(resolved.Start, resolved.End)
	window := full.clampTo(funnel.CreatedAt)

	for _, step := range funnel.Steps {
		window = window.clampTo(step.Goal.CreatedAt)
	}

	result := &FunnelResult{
		Funnel:  funnel,
		From:    time.Unix(window.Start, 0).In(resolved.Location),
		To:      resolved.End,
		Partial: window.Start > full.Start,
	}

	result.Steps = make([]FunnelStep, len(funnel.Steps))
	for i, step := range funnel.Steps {
		result.Steps[i] = FunnelStep{Position: step.Position, Label: step.Goal.Label(), Goal: step.Goal}
	}

	if window.Empty() {
		return result, nil
	}

	filterSQL, filterArgs, err := engine.EventFilterSQL(ctx, []int64{funnel.SiteID}, resolved, req.Filters, "e")
	if err != nil {
		return nil, err
	}
	visits, visitors, err := walkFunnel(ctx, db, funnel, window, filterSQL, filterArgs)
	if err != nil {
		return nil, err
	}

	for i := range result.Steps {
		result.Steps[i].Visits = visits[i]
		result.Steps[i].Visitors = visitors[i]

		if i > 0 {
			result.Steps[i].DropOff = visitors[i-1] - visitors[i]
			result.Steps[i].DropOffRate = percentage(result.Steps[i].DropOff, visitors[i-1])
		}

		result.Steps[i].ConversionRate = percentage(visitors[i], visitors[0])
	}

	return result, nil
}

// walkFunnel reads every event that matches any step, in visit order, and
// works out how far each visit got.
//
// The walk advances at most one step per event. Two steps that both match the
// same event — a wildcard goal and the exact page beneath it — would otherwise
// be satisfied by a single pageview, and a funnel that can be completed by one
// event is not measuring a flow.
func walkFunnel(ctx context.Context, db *sql.DB, funnel Funnel, window Window, filterSQL string, filterArgs []any) (visits, visitors []int64, err error) {
	predicates, err := stepPredicates(ctx, db, funnel)
	if err != nil {
		return nil, nil, err
	}

	steps := len(funnel.Steps)

	visits = make([]int64, steps)
	visitors = make([]int64, steps)

	where, args := baseConditions("e", funnel.SiteID, window)

	// The mask is built in the select list and the same predicates are
	// repeated in the where clause, so their arguments are bound twice — once
	// in each position, in statement order.
	var (
		mask     []string
		matchAny []string
		binds    []any
	)

	for i, predicate := range predicates {
		mask = append(mask, fmt.Sprintf("(CASE WHEN %s THEN %d ELSE 0 END)", predicate.SQL, 1<<uint(i)))
		matchAny = append(matchAny, "("+predicate.SQL+")")
		binds = append(binds, predicate.Args...)
	}

	params := append([]any{}, binds...)
	params = append(params, args...)

	statement := "SELECT e.session_id, e.user_id, " + strings.Join(mask, " + ") + " AS reached" +
		" FROM events e WHERE " + where
	if filterSQL != "" {
		statement += " AND (" + filterSQL + ")"
		params = append(params, filterArgs...)
	}
	if funnel.StrictOrder {
		// Strict order must see unrelated visitor actions because any one of
		// them interrupts an exact-consecutive sequence. Internal engagement
		// heartbeats are measurements rather than actions and do not interrupt.
		engagementID, err := eventNameID(ctx, db, ingest.EventEngagement)
		if err != nil {
			return nil, nil, err
		}
		statement += " AND e.name_id <> ?"
		params = append(params, engagementID)
	} else {
		// Sequential mode can skip unrelated events, so selecting only events
		// that match at least one step keeps the ordered walk small.
		statement += " AND (" + strings.Join(matchAny, " OR ") + ")"
		params = append(params, binds...)
	}
	statement += " ORDER BY e.session_id, e.timestamp, e.id"

	rows, err := db.QueryContext(ctx, statement, params...)
	if err != nil {
		return nil, nil, fmt.Errorf("goals: funnel: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// A visitor's score is the furthest any single visit of theirs got. A
	// funnel is a thing that happens inside a visit, so steps done in two
	// different visits do not add up to a completed funnel.
	best := map[int64]int{}

	var (
		current int64
		haveRow bool
		user    int64
		reached int
	)

	// finishVisit is the end of one visit: it turns the visit's accumulated
	// state into the step it reached and folds that into the totals.
	finishVisit := func() {
		if !haveRow {
			return
		}

		got := reached

		for i := 0; i < got; i++ {
			visits[i]++
		}

		if got > best[user] {
			best[user] = got
		}
	}

	for rows.Next() {
		var (
			session int64
			owner   int64
			matched int
		)

		if err := rows.Scan(&session, &owner, &matched); err != nil {
			return nil, nil, fmt.Errorf("goals: funnel: %w", err)
		}

		if !haveRow || session != current {
			finishVisit()

			current, user, reached, haveRow = session, owner, 0, true
		}

		if !funnel.StrictOrder {
			if reached < steps && matched&(1<<uint(reached)) != 0 {
				reached++
			}
			continue
		}

		if reached < steps && matched&(1<<uint(reached)) != 0 {
			reached++
			continue
		}

		// A mismatch breaks a strict sequence. The same event may immediately
		// begin a new attempt when it matches the first step.
		reached = 0
		if matched&1 != 0 {
			reached = 1
		}
	}

	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("goals: funnel: %w", err)
	}

	finishVisit()

	for _, got := range best {
		for i := 0; i < got; i++ {
			visitors[i]++
		}
	}

	return visits, visitors, nil
}

// predicate is one step's matching condition and its bind parameters.
type predicate struct {
	SQL  string
	Args []any
}

// stepPredicates compiles every step of a funnel into a condition on the
// events table. Ids are resolved through the dimension tables here rather than
// joined in the statement: the dimension tables are small and hot, and joining
// one before aggregating would drag a text column through the whole scan.
func stepPredicates(ctx context.Context, db *sql.DB, funnel Funnel) ([]predicate, error) {
	pageviewID, err := eventNameID(ctx, db, ingest.EventPageview)
	if err != nil {
		return nil, err
	}

	predicates := make([]predicate, 0, len(funnel.Steps))

	for _, step := range funnel.Steps {
		compiled, err := goalPredicate(ctx, db, step.Goal, pageviewID)
		if err != nil {
			return nil, err
		}

		predicates = append(predicates, compiled)
	}

	return predicates, nil
}

// goalPredicate compiles one goal into a condition on the events table, with
// its property constraints attached.
func goalPredicate(ctx context.Context, db *sql.DB, goal Goal, pageviewID int64) (predicate, error) {
	var compiled predicate

	switch goal.Kind {
	case KindPage:
		if hasWildcard(goal.PagePattern) {
			source, err := compilePattern(goal.PagePattern)
			if err != nil {
				return predicate{}, err
			}

			compiled = predicate{
				SQL: "e.name_id = ? AND e.pathname_id IN (SELECT id FROM dim_pathname WHERE " +
					query.MatchFunction + "(?, value, 1))",
				Args: []any{pageviewID, source},
			}

			break
		}

		// An exact path is an equality against one interned id, which is a
		// single index probe rather than a scan of every distinct path the
		// site has ever served.
		var pathID int64

		err := db.QueryRowContext(ctx, "SELECT id FROM dim_pathname WHERE value = ?", goal.PagePattern).Scan(&pathID)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			// A path this site has never served matches nothing. Minus one
			// rather than zero: id 0 is the empty string, and matching that
			// would count every event with no path at all.
			pathID = -1
		case err != nil:
			return predicate{}, fmt.Errorf("goals: read path: %w", err)
		}

		compiled = predicate{SQL: "e.name_id = ? AND e.pathname_id = ?", Args: []any{pageviewID, pathID}}

	case KindEvent:
		id, err := eventNameID(ctx, db, goal.EventName)
		if err != nil {
			return predicate{}, err
		}

		ids := []any{id}
		if goal.EventName == EventFormSubmission {
			legacy, err := eventNameID(ctx, db, EventFormSubmitLegacy)
			if err != nil {
				return predicate{}, err
			}
			ids = append(ids, legacy)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		compiled = predicate{SQL: "e.name_id IN (" + placeholders + ")", Args: ids}

	case KindScroll:
		compiled = predicate{SQL: "e.scroll_depth >= ? AND e.scroll_depth <= 100", Args: []any{goal.ScrollDepth}}
		if goal.PagePattern != "" {
			source, err := compilePattern(goal.PagePattern)
			if err != nil {
				return predicate{}, err
			}
			compiled.SQL += " AND e.pathname_id IN (SELECT id FROM dim_pathname WHERE " + query.MatchFunction + "(?, value, 1))"
			compiled.Args = append(compiled.Args, source)
		}

	default:
		return predicate{}, invalid("a goal is %q, %q, or %q, not %q", KindPage, KindEvent, KindScroll, goal.Kind)
	}

	if len(goal.Properties) == 0 {
		return compiled, nil
	}

	// Property constraints live in the cold table, so they are an existence
	// check against it rather than a column on the row being scanned.
	var (
		clauses []string
		args    []any
	)

	for _, property := range goal.Properties {
		clauses = append(clauses, "json_extract(ed.props, ?) = ?")
		args = append(args, jsonPath(property.Name), property.Value)
	}

	compiled.SQL += " AND e.id IN (SELECT ed.event_id FROM event_details ed WHERE " + strings.Join(clauses, " AND ") + ")"
	compiled.Args = append(compiled.Args, args...)

	return compiled, nil
}
