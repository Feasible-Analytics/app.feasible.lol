//
// goals.go
// What a conversion is: the definitions, their limits, and the automatic ones.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package goals owns conversions: what counts as one, the funnels built out of
// them, the property allow-list a goal can constrain on, and the money a goal
// can carry. Every one of those is a paid tier at the incumbent and every one
// of them ships in this binary, including the self-hosted build.
//
// The definitions live in the account database beside the events they match,
// because every report that reads a goal immediately reads the events it
// selects, and a definition in another file would put a cross-database join on
// the one path that has to stay a single scan.
//
// Reports here go through the query compiler rather than hand-written
// aggregate SQL wherever the shape allows it. That is not laziness: the moment
// a goals report counts a visitor its own way, the goals page and the visitors
// graph disagree, and nobody can tell which of the two is lying. The two
// exceptions are funnels and journeys, which need per-visit ordering that no
// GROUP BY can express.
package goals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// Kind is how a goal is matched. There are exactly two and there will not be a
// third: a goal is either a page somebody reached or an event somebody fired,
// and anything else is a filter on one of those.
type Kind string

const (
	// KindPage matches pageviews whose path matches a pattern.
	KindPage Kind = "page"

	// KindEvent matches custom events by name.
	KindEvent Kind = "event"
)

// Limits on one goal. Both are product decisions rather than storage ones,
// which is why they are enforced here and not by a constraint.
const (
	// MaxProperties is how many property constraints one goal may carry. Three
	// is enough to say "Purchase, plan growth, annual, in the US" and few
	// enough that the resulting query is still one index scan.
	MaxProperties = 3

	// MaxDisplayName bounds the label. It is generous because it is a human
	// sentence, and bounded because it is rendered in a table cell.
	MaxDisplayName = 200

	// MaxPagePattern bounds a path pattern, matching the URL limit the ingest
	// path enforces: a pattern longer than any path it could match is not a
	// pattern anybody meant to type.
	MaxPagePattern = 2000

	// MaxEventName is the property-name limit, which is also what the tracker
	// accepts as an event name.
	MaxEventName = 300
)

// PropertyConstraint narrows a goal to events carrying a particular property
// value. It is a plain equality rather than an operator set because a goal is
// a definition people share and compare — "Purchase where plan is growth" is
// unambiguous, where "Purchase where plan contains gro" is a conversation.
type PropertyConstraint struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Goal is one definition. It carries its own creation time because conversions
// are counted from it forward and never before it: a goal created today cannot
// claim last month's traffic, and every report clamps its window here.
type Goal struct {
	ID     int64 `json:"id"`
	SiteID int64 `json:"site_id"`

	Kind Kind `json:"kind"`

	// DisplayName is what the customer called it. Empty is normal and means
	// "describe yourself from the pattern".
	DisplayName string `json:"display_name"`

	// PagePattern is the path pattern for a page goal, already trimmed.
	PagePattern string `json:"page_pattern,omitempty"`

	// EventName is the custom event name for an event goal, already trimmed.
	EventName string `json:"event_name,omitempty"`

	// IsRevenue marks a goal that carries money, and Currency is the currency
	// it is set up in.
	IsRevenue bool   `json:"is_revenue"`
	Currency  string `json:"currency,omitempty"`

	// IsAutomatic marks a goal we created rather than a person. It is stored
	// rather than guessed from the name so renaming one does not turn it into
	// somebody else's goal.
	IsAutomatic bool `json:"is_automatic"`

	// CreatedAt is unix seconds, and it is the earliest instant this goal can
	// have a conversion.
	CreatedAt int64 `json:"created_at"`

	Properties []PropertyConstraint `json:"properties,omitempty"`
}

// Label is what a report shows for this goal. A goal with no name describes
// itself from what it matches, so an automatic goal and a goal somebody forgot
// to name both read as something rather than as an empty cell.
func (g Goal) Label() string {
	if g.DisplayName != "" {
		return g.DisplayName
	}

	if g.Kind == KindPage {
		return "Visit " + g.PagePattern
	}

	return g.EventName
}

// NoBackfillNotice is the sentence the creation form has to show. It is a
// constant here rather than a string in a template because the behaviour is
// decided by this package, and a warning that lives away from the behaviour is
// a warning that stops being true.
const NoBackfillNotice = "Conversions are counted from the moment this goal is created. " +
	"Traffic that already happened is not counted towards it, even though it is still in your data."

// Error is a caller's mistake, carrying the message the caller reads. It is a
// distinct type so the HTTP layer can answer 400 with something useful rather
// than turning a mistyped pattern into a 500.
type Error struct {
	Message string
}

// Error renders the message, which is written for the person holding the
// failing request rather than for a log.
func (e *Error) Error() string {
	return e.Message
}

// invalid builds a caller-facing validation error.
func invalid(format string, args ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, args...)}
}

// ErrNotFound is returned when a goal or funnel id does not exist. It is a
// sentinel so a caller can turn it into a 404 without string matching.
var ErrNotFound = errors.New("goals: not found")

// Normalise trims every field a person types and fills in what the kind
// implies. Trimming is the whole reason this exists: a leading or trailing
// space on a path is invisible in a text box and silently prevents every
// match, and it is the actual cause behind reports that wildcards "interfere
// with each other".
func (g *Goal) Normalise() {
	g.DisplayName = strings.TrimSpace(g.DisplayName)
	g.PagePattern = strings.TrimSpace(g.PagePattern)
	g.EventName = strings.TrimSpace(g.EventName)
	g.Currency = strings.ToUpper(strings.TrimSpace(g.Currency))

	// A goal matches one way. Keeping the other field would let a later edit
	// change the kind and silently start matching a pattern nobody remembers
	// typing.
	switch g.Kind {
	case KindPage:
		g.EventName = ""
	case KindEvent:
		g.PagePattern = ""
	}

	for i := range g.Properties {
		g.Properties[i].Name = strings.TrimSpace(g.Properties[i].Name)
		g.Properties[i].Value = strings.TrimSpace(g.Properties[i].Value)
	}

	if !g.IsRevenue {
		g.Currency = ""
	}
}

// Validate refuses a definition that could never match or could never be
// reported. It runs after Normalise so that the checks see the trimmed values
// the database will actually hold.
func (g *Goal) Validate() error {
	if g.SiteID == 0 {
		return invalid("a goal needs a site")
	}

	switch g.Kind {
	case KindPage:
		if g.PagePattern == "" {
			return invalid("a page goal needs a path, for example /thank-you or /blog/**")
		}
		if !strings.HasPrefix(g.PagePattern, "/") {
			return invalid("a page goal's path must start with /, not %q", g.PagePattern)
		}
		if len(g.PagePattern) > MaxPagePattern {
			return invalid("a page goal's path may be at most %d characters", MaxPagePattern)
		}
		if _, err := compilePattern(g.PagePattern); err != nil {
			return err
		}

	case KindEvent:
		if g.EventName == "" {
			return invalid("an event goal needs an event name, for example Signup")
		}
		if len(g.EventName) > MaxEventName {
			return invalid("an event name may be at most %d characters", MaxEventName)
		}

	default:
		return invalid("a goal is either %q or %q, not %q", KindPage, KindEvent, g.Kind)
	}

	if len(g.DisplayName) > MaxDisplayName {
		return invalid("a goal name may be at most %d characters", MaxDisplayName)
	}

	if len(g.Properties) > MaxProperties {
		return invalid("a goal may carry at most %d property constraints, not %d", MaxProperties, len(g.Properties))
	}

	for _, property := range g.Properties {
		if property.Name == "" {
			return invalid("a property constraint needs a property name")
		}

		if err := validatePropertyName(property.Name); err != nil {
			return err
		}

		// The value is bounded by what the tracker will store. A constraint on
		// a longer value could never match an event, because the event's own
		// value was cut to this length on the way in.
		if len(property.Value) > ingest.MaxPropValueLength {
			return invalid("a property value may be at most %d characters", ingest.MaxPropValueLength)
		}
	}

	if g.IsRevenue {
		if err := validateCurrency(g.Currency); err != nil {
			return err
		}
	}

	return nil
}

// Create stores a goal and returns it with its id. Creating the same
// definition twice is not an error and does not create a second row: two
// identical goals would count every conversion twice on a report where nobody
// could see why.
func Create(ctx context.Context, db *sql.DB, goal Goal, now time.Time) (Goal, error) {
	goal.Normalise()

	if err := goal.Validate(); err != nil {
		return Goal{}, err
	}

	goal.CreatedAt = now.Unix()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Goal{}, fmt.Errorf("goals: create: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	signature := goal.signature()

	// The insert is a no-op when the definition already exists, and the id is
	// then read back, so a caller that creates the automatic goals on every
	// site refresh does not have to know which ones it made last time.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO goals (site_id, kind, display_name, page_pattern, event_name,
			is_revenue, currency, is_automatic, created_at, signature)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(site_id, signature) DO NOTHING`,
		goal.SiteID, string(goal.Kind), goal.DisplayName, goal.PagePattern, goal.EventName,
		boolToInt(goal.IsRevenue), goal.Currency, boolToInt(goal.IsAutomatic), goal.CreatedAt, signature,
	); err != nil {
		return Goal{}, fmt.Errorf("goals: create: %w", err)
	}

	var existing Goal
	if err := tx.QueryRowContext(ctx, `
		SELECT id, created_at, display_name, is_revenue, currency, is_automatic
		FROM goals
		WHERE site_id = ? AND signature = ?`,
		goal.SiteID, signature,
	).Scan(&existing.ID, &existing.CreatedAt, &existing.DisplayName,
		&existing.IsRevenue, &existing.Currency, &existing.IsAutomatic); err != nil {
		return Goal{}, fmt.Errorf("goals: create: %w", err)
	}

	goal.ID = existing.ID

	// An existing goal keeps its own creation time and its own name. Re-running
	// creation must never move the instant conversions start counting from, or
	// a report would change under a customer who did nothing.
	goal.CreatedAt = existing.CreatedAt
	goal.DisplayName = existing.DisplayName
	goal.IsRevenue = existing.IsRevenue
	goal.Currency = existing.Currency
	goal.IsAutomatic = existing.IsAutomatic

	if _, err := tx.ExecContext(ctx, "DELETE FROM goal_properties WHERE goal_id = ?", goal.ID); err != nil {
		return Goal{}, fmt.Errorf("goals: create: %w", err)
	}

	for _, property := range goal.Properties {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO goal_properties (goal_id, name, value) VALUES (?,?,?)",
			goal.ID, property.Name, property.Value,
		); err != nil {
			return Goal{}, fmt.Errorf("goals: create: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return Goal{}, fmt.Errorf("goals: create: %w", err)
	}

	return goal, nil
}

// signature renders everything that decides which events a goal matches.
//
// It exists because two goals on the same event with different property
// constraints are different goals — "Purchase" and "Purchase where plan is
// growth" are two rows on a report — and the constraints live in a second
// table that a unique index cannot reach. Rendering them into one string moves
// the uniqueness rule into a place the database can enforce.
//
// The constraints are sorted so that the same definition written in a
// different order is the same goal, rather than a duplicate that quietly
// doubles a number.
func (g Goal) signature() string {
	parts := make([]string, 0, len(g.Properties)+3)
	parts = append(parts, string(g.Kind), g.PagePattern, g.EventName)

	constraints := make([]string, 0, len(g.Properties))
	for _, property := range g.Properties {
		constraints = append(constraints, property.Name+"="+property.Value)
	}

	sort.Strings(constraints)

	parts = append(parts, constraints...)

	// The unit separator cannot appear in a path, an event name or a property
	// value that survived validation, so two different definitions can never
	// render to one signature.
	return strings.Join(parts, "\x1f")
}

// Rename changes a goal's label without touching what it matches or when it
// started counting. It is separate from Create because renaming must not be
// able to move the creation time, which is the one field a customer would
// never expect an edit to change.
func Rename(ctx context.Context, db *sql.DB, id int64, name string) error {
	name = strings.TrimSpace(name)

	if len(name) > MaxDisplayName {
		return invalid("a goal name may be at most %d characters", MaxDisplayName)
	}

	result, err := db.ExecContext(ctx, "UPDATE goals SET display_name = ? WHERE id = ?", name, id)
	if err != nil {
		return fmt.Errorf("goals: rename: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("goals: rename: %w", err)
	}

	if affected == 0 {
		return ErrNotFound
	}

	return nil
}

// Delete removes a goal and its property constraints. A funnel step still
// pointing at it keeps the row alive through the foreign key, which is
// deliberate: deleting a goal out from under a funnel would leave a chart with
// a step nobody could explain.
func Delete(ctx context.Context, db *sql.DB, id int64) error {
	var steps int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM funnel_steps WHERE goal_id = ?", id).Scan(&steps); err != nil {
		return fmt.Errorf("goals: delete: %w", err)
	}

	if steps > 0 {
		return invalid("this goal is a step in %d funnel step(s) — remove it from the funnel first", steps)
	}

	if _, err := db.ExecContext(ctx, "DELETE FROM goal_properties WHERE goal_id = ?", id); err != nil {
		return fmt.Errorf("goals: delete: %w", err)
	}

	result, err := db.ExecContext(ctx, "DELETE FROM goals WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("goals: delete: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("goals: delete: %w", err)
	}

	if affected == 0 {
		return ErrNotFound
	}

	return nil
}

// List returns a site's goals with their property constraints, oldest first.
// Oldest first is the order they were created in, which is the order the
// customer already has in their head.
func List(ctx context.Context, db *sql.DB, siteID int64) ([]Goal, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, site_id, kind, display_name, page_pattern, event_name,
		       is_revenue, currency, is_automatic, created_at
		FROM goals
		WHERE site_id = ?
		ORDER BY created_at, id`, siteID)
	if err != nil {
		return nil, fmt.Errorf("goals: list: %w", err)
	}
	defer rows.Close()

	var list []Goal

	for rows.Next() {
		var (
			goal Goal
			kind string
		)

		if err := rows.Scan(&goal.ID, &goal.SiteID, &kind, &goal.DisplayName,
			&goal.PagePattern, &goal.EventName, &goal.IsRevenue, &goal.Currency,
			&goal.IsAutomatic, &goal.CreatedAt); err != nil {
			return nil, fmt.Errorf("goals: list: %w", err)
		}

		goal.Kind = Kind(kind)
		list = append(list, goal)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("goals: list: %w", err)
	}

	if len(list) == 0 {
		return nil, nil
	}

	byID := make(map[int64]int, len(list))
	for i, goal := range list {
		byID[goal.ID] = i
	}

	// The constraints are read in one pass rather than a query per goal: a
	// site with forty goals would otherwise pay forty round trips to render
	// one page.
	constraints, err := db.QueryContext(ctx, `
		SELECT goal_id, name, value
		FROM goal_properties
		WHERE goal_id IN (SELECT id FROM goals WHERE site_id = ?)
		ORDER BY id`, siteID)
	if err != nil {
		return nil, fmt.Errorf("goals: list: %w", err)
	}
	defer constraints.Close()

	for constraints.Next() {
		var (
			goalID     int64
			constraint PropertyConstraint
		)

		if err := constraints.Scan(&goalID, &constraint.Name, &constraint.Value); err != nil {
			return nil, fmt.Errorf("goals: list: %w", err)
		}

		if index, ok := byID[goalID]; ok {
			list[index].Properties = append(list[index].Properties, constraint)
		}
	}

	if err := constraints.Err(); err != nil {
		return nil, fmt.Errorf("goals: list: %w", err)
	}

	return list, nil
}

// Get returns one goal by id, with its constraints.
func Get(ctx context.Context, db *sql.DB, id int64) (Goal, error) {
	var (
		goal Goal
		kind string
	)

	if err := db.QueryRowContext(ctx, `
		SELECT id, site_id, kind, display_name, page_pattern, event_name,
		       is_revenue, currency, is_automatic, created_at
		FROM goals WHERE id = ?`, id,
	).Scan(&goal.ID, &goal.SiteID, &kind, &goal.DisplayName, &goal.PagePattern,
		&goal.EventName, &goal.IsRevenue, &goal.Currency, &goal.IsAutomatic, &goal.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Goal{}, ErrNotFound
		}

		return Goal{}, fmt.Errorf("goals: get: %w", err)
	}

	goal.Kind = Kind(kind)

	rows, err := db.QueryContext(ctx, "SELECT name, value FROM goal_properties WHERE goal_id = ? ORDER BY id", id)
	if err != nil {
		return Goal{}, fmt.Errorf("goals: get: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var constraint PropertyConstraint

		if err := rows.Scan(&constraint.Name, &constraint.Value); err != nil {
			return Goal{}, fmt.Errorf("goals: get: %w", err)
		}

		goal.Properties = append(goal.Properties, constraint)
	}

	if err := rows.Err(); err != nil {
		return Goal{}, fmt.Errorf("goals: get: %w", err)
	}

	return goal, nil
}

// The event names the tracker sends for the behaviours it can see on its own.
// They are the established names rather than ours, because matching the wire
// format is what lets somebody migrate by changing one hostname — and because
// a customer's existing snippet is already sending exactly these.
const (
	// EventNotFound is fired by the tracker's 404 extension.
	EventNotFound = "404"

	// EventOutboundClick, EventFileDownload and EventFormSubmission are the
	// other three the tracker can detect without the customer writing code.
	EventOutboundClick  = "Outbound Link: Click"
	EventFileDownload   = "File Download"
	EventFormSubmission = "Form: Submission"
)

// automaticGoals is what every new site gets. The 404 goal is the important
// one: the single commonest reason 404 tracking silently fails is pasting the
// snippet and never creating the goal, and the goal costs nothing because it
// does not appear on the dashboard unless 404 events actually arrive.
var automaticGoals = []struct {
	Name  string
	Label string
}{
	{Name: EventNotFound, Label: "404 pages"},
	{Name: EventOutboundClick, Label: "Outbound link clicks"},
	{Name: EventFileDownload, Label: "File downloads"},
	{Name: EventFormSubmission, Label: "Form submissions"},
}

// EnsureAutomatic creates the goals every site should have, and is safe to call
// again on a site that already has them: creation is keyed on the definition,
// so a second call finds the existing rows and leaves their creation times
// alone.
//
// It is called when a site is created rather than when the first matching event
// arrives, because a goal that does not exist until the traffic does would
// start counting after the thing the customer was trying to measure.
func EnsureAutomatic(ctx context.Context, db *sql.DB, siteID int64, now time.Time) ([]Goal, error) {
	created := make([]Goal, 0, len(automaticGoals))

	for _, automatic := range automaticGoals {
		goal, err := Create(ctx, db, Goal{
			SiteID:      siteID,
			Kind:        KindEvent,
			EventName:   automatic.Name,
			DisplayName: automatic.Label,
			IsAutomatic: true,
		}, now)
		if err != nil {
			return nil, err
		}

		created = append(created, goal)
	}

	return created, nil
}

// boolToInt renders a Go bool the way SQLite stores one.
func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}
