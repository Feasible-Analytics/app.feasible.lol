//
// properties.go
// The property allow-list, its declared scopes, and the report card built on it.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// Scope is what a property describes. It is the one decision the incumbent
// never made, and the reason a family of numbers over there is quietly wrong.
//
// An event-scoped property describes one hit: the product in an "Add to Cart",
// the file in a download. A session-scoped one describes the whole visit: the
// A/B variant, the browser language, the referrer the visit started with.
//
// The difference decides a denominator. A conversion rate filtered by an
// event-scoped property divides by everybody, because the property only exists
// on the conversion itself. Filtered by a session-scoped property it must
// divide by the visitors who *had* that value — the visitors in that A/B
// variant — or the variant with fewer visitors always looks worse than it is.
type Scope string

const (
	// ScopeEvent is a property of one hit.
	ScopeEvent Scope = "event"

	// ScopeSession is a property of a whole visit.
	ScopeSession Scope = "session"
)

// NoneBucket is the label for events that did not carry the property at all.
// It is a bucket rather than an omission because "half your Add to Cart events
// have no product" is the single most useful thing a property report can tell
// somebody, and dropping those rows hides it.
const NoneBucket = "(none)"

// DefaultPropertyRows is how many values a property report returns when the
// caller does not say. It is the size of the report card, not a limit on the
// data.
const DefaultPropertyRows = 100

// Property is one entry in a site's allow-list.
type Property struct {
	ID        int64  `json:"id"`
	SiteID    int64  `json:"site_id"`
	Name      string `json:"name"`
	Scope     Scope  `json:"scope"`
	CreatedAt int64  `json:"created_at"`
}

// validatePropertyName refuses a name that could not be stored, reported on,
// or written into a JSON path. The length limit is the ingest limit rather
// than a second opinion: a name the tracker will truncate is a name no report
// would ever match.
func validatePropertyName(name string) error {
	if name == "" {
		return invalid("a property needs a name")
	}

	if len(name) > ingest.MaxPropNameLength {
		return invalid("a property name may be at most %d characters, not %d", ingest.MaxPropNameLength, len(name))
	}

	// A quote or a backslash would have to be escaped into the JSON path the
	// value is read by. A property nobody can name is not worth the escaping
	// code that would then have to be exactly right.
	if strings.ContainsAny(name, "\"\\") {
		return invalid("a property name cannot contain a quote or a backslash")
	}

	return nil
}

// validateScope refuses anything but the two scopes. There is no default and
// no third option on purpose: an unscoped property is precisely the thing this
// table exists to make impossible.
func validateScope(scope Scope) error {
	switch scope {
	case ScopeEvent, ScopeSession:
		return nil
	}

	return invalid("a property is scoped %q or %q, not %q — event means it describes one hit, "+
		"session means it describes the whole visit", ScopeEvent, ScopeSession, scope)
}

// Allow registers a property, or re-scopes one that is already registered.
// Re-scoping is allowed because the first guess is often wrong and the fix has
// to be one dropdown rather than a support ticket; it changes what future
// reports divide by, which is the point.
func Allow(ctx context.Context, db *sql.DB, siteID int64, name string, scope Scope, now time.Time) (Property, error) {
	name = strings.TrimSpace(name)

	if err := validatePropertyName(name); err != nil {
		return Property{}, err
	}

	if err := validateScope(scope); err != nil {
		return Property{}, err
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO allowed_properties (site_id, name, scope, created_at)
		VALUES (?,?,?,?)
		ON CONFLICT(site_id, name) DO UPDATE SET scope = excluded.scope`,
		siteID, name, string(scope), now.Unix(),
	); err != nil {
		return Property{}, fmt.Errorf("goals: allow property: %w", err)
	}

	property := Property{SiteID: siteID, Name: name, Scope: scope}

	if err := db.QueryRowContext(ctx,
		"SELECT id, created_at FROM allowed_properties WHERE site_id = ? AND name = ?", siteID, name,
	).Scan(&property.ID, &property.CreatedAt); err != nil {
		return Property{}, fmt.Errorf("goals: allow property: %w", err)
	}

	return property, nil
}

// Disallow removes a property from the allow-list. The data stays: the events
// already carry the property and deleting the registration must not delete
// anybody's history, which is why this is one row in a small table rather than
// anything that touches the fact tables.
func Disallow(ctx context.Context, db *sql.DB, siteID int64, name string) error {
	if _, err := db.ExecContext(ctx,
		"DELETE FROM allowed_properties WHERE site_id = ? AND name = ?", siteID, strings.TrimSpace(name),
	); err != nil {
		return fmt.Errorf("goals: disallow property: %w", err)
	}

	return nil
}

// Allowed lists a site's registered properties in name order, which is how the
// settings screen and the filter dropdown both want them.
func Allowed(ctx context.Context, db *sql.DB, siteID int64) ([]Property, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT id, site_id, name, scope, created_at FROM allowed_properties WHERE site_id = ? ORDER BY name", siteID)
	if err != nil {
		return nil, fmt.Errorf("goals: allowed properties: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var list []Property

	for rows.Next() {
		var (
			property Property
			scope    string
		)

		if err := rows.Scan(&property.ID, &property.SiteID, &property.Name, &scope, &property.CreatedAt); err != nil {
			return nil, fmt.Errorf("goals: allowed properties: %w", err)
		}

		property.Scope = Scope(scope)
		list = append(list, property)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("goals: allowed properties: %w", err)
	}

	return list, nil
}

// Scopes returns the declared scope of every registered property across a set
// of sites, in the shape the query compiler wants. It is what turns a filter
// on `event:props:ab_test_group` from a guess into a decision somebody made.
//
// Two sites of one account disagreeing about a name resolves to event scope,
// which is the conservative answer: an event-scoped denominator counts
// everybody, where a session-scoped one silently narrows the set a rate is
// measured over.
func Scopes(ctx context.Context, db *sql.DB, siteIDs []int64) (map[string]string, error) {
	if len(siteIDs) == 0 {
		return nil, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(siteIDs)), ",")

	args := make([]any, 0, len(siteIDs))
	for _, id := range siteIDs {
		args = append(args, id)
	}

	rows, err := db.QueryContext(ctx,
		"SELECT name, scope FROM allowed_properties WHERE site_id IN ("+placeholders+")", args...)
	if err != nil {
		return nil, fmt.Errorf("goals: property scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	scopes := map[string]string{}

	for rows.Next() {
		var name, scope string

		if err := rows.Scan(&name, &scope); err != nil {
			return nil, fmt.Errorf("goals: property scopes: %w", err)
		}

		if existing, ok := scopes[name]; ok && existing != scope {
			scopes[name] = string(ScopeEvent)
			continue
		}

		scopes[name] = scope
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("goals: property scopes: %w", err)
	}

	return scopes, nil
}

// PropertyRequest asks for one property's values over a window.
type PropertyRequest struct {
	SiteID int64
	Window Window

	// Name is the property to break down.
	Name string

	// EventName narrows the report to one custom event, which is how the card
	// is actually read: "the plans people signed up on", not "every plan value
	// anywhere on the site". Empty means every event that carries the
	// property.
	EventName string

	Limit int
}

// PropertyRow is one value of a property.
type PropertyRow struct {
	Value string `json:"value"`

	// Missing marks the bucket of events that did not carry the property. It
	// is a flag rather than only a label so that a real value of "(none)" and
	// an absent property stay distinguishable in the data even though they
	// read the same.
	Missing bool `json:"missing,omitempty"`

	Visitors int64 `json:"visitors"`
	Visits   int64 `json:"visits"`
	Events   int64 `json:"events"`
}

// Values answers the property report card.
//
// It is hand-written SQL rather than a compiler query for one reason: the
// compiler drops events that do not carry the property, because a breakdown of
// "plan" is a list of plans. A report card has to show the opposite — how many
// events were missing it — and that bucket is the whole diagnostic value of
// the card.
//
// Properties live in the cold event_details table, so this report pays a join
// the common query path does not. That is the trade the hot/cold split makes:
// every scan that never looks at props does not drag a JSON blob off disk.
func Values(ctx context.Context, db *sql.DB, req PropertyRequest) ([]PropertyRow, error) {
	name := strings.TrimSpace(req.Name)

	if err := validatePropertyName(name); err != nil {
		return nil, err
	}

	limit := req.Limit
	if limit <= 0 {
		limit = DefaultPropertyRows
	}

	if req.Window.Empty() {
		return nil, nil
	}

	where, args := baseConditions("e", req.SiteID, req.Window)

	// The path is the first bind parameter of the statement, so it is
	// prepended rather than appended.
	params := append([]any{jsonPath(name)}, args...)

	if req.EventName != "" {
		id, err := eventNameID(ctx, db, req.EventName)
		if err != nil {
			return nil, err
		}

		where += " AND e.name_id = ?"
		params = append(params, id)
	}

	params = append(params, int64(limit))

	rows, err := db.QueryContext(ctx, `
		SELECT json_extract(ed.props, ?) AS value,
		       COUNT(DISTINCT e.user_id),
		       COUNT(DISTINCT e.session_id),
		       COUNT(*)
		FROM events e
		LEFT JOIN event_details ed ON ed.event_id = e.id
		WHERE `+where+`
		GROUP BY value
		ORDER BY 4 DESC, value
		LIMIT ?`, params...)
	if err != nil {
		return nil, fmt.Errorf("goals: property values: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var list []PropertyRow

	for rows.Next() {
		var (
			value sql.NullString
			row   PropertyRow
		)

		if err := rows.Scan(&value, &row.Visitors, &row.Visits, &row.Events); err != nil {
			return nil, fmt.Errorf("goals: property values: %w", err)
		}

		row.Value = value.String
		if !value.Valid {
			row.Missing = true
			row.Value = NoneBucket
		}

		list = append(list, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("goals: property values: %w", err)
	}

	return list, nil
}

// Seen lists the property names that actually arrived in a window, busiest
// first. It is what populates the "add a property" screen: a customer should
// be choosing from the names their own tracker is sending, not typing one from
// memory and wondering why the report is empty.
func Seen(ctx context.Context, db *sql.DB, siteID int64, window Window, limit int) ([]string, error) {
	if limit <= 0 {
		limit = DefaultPropertyRows
	}

	if window.Empty() {
		return nil, nil
	}

	where, args := baseConditions("e", siteID, window)
	args = append(args, int64(limit))

	rows, err := db.QueryContext(ctx, `
		SELECT each.key, COUNT(*) AS hits
		FROM events e
		JOIN event_details ed ON ed.event_id = e.id
		JOIN json_each(ed.props) each
		WHERE `+where+`
		GROUP BY each.key
		ORDER BY hits DESC, each.key
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("goals: seen properties: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var names []string

	for rows.Next() {
		var (
			name string
			hits int64
		)

		if err := rows.Scan(&name, &hits); err != nil {
			return nil, fmt.Errorf("goals: seen properties: %w", err)
		}

		names = append(names, name)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("goals: seen properties: %w", err)
	}

	return names, nil
}

// Health is what the ingestion health panel shows about properties. The counts
// come from the ingest tier, which already refuses to drop anything silently;
// this turns them into the sentence a customer can act on.
type Health struct {
	// OverLimit is how many properties were sent past the cap on one event and
	// therefore not stored.
	OverLimit int64 `json:"over_limit"`

	// NamesTruncated and ValuesTruncated count the fields that were cut to
	// their limit rather than dropped.
	NamesTruncated  int64 `json:"names_truncated"`
	ValuesTruncated int64 `json:"values_truncated"`

	// Unsupported counts properties whose value was an object, an array or
	// null. Nothing is stored for them, and a value that vanishes with no
	// number beside it is a support ticket nobody can answer.
	Unsupported int64 `json:"unsupported"`

	// Message is the sentence to show, empty when there is nothing to say.
	Message string `json:"message,omitempty"`
}

// PropertyHealth pulls one site's property counters out of an ingest snapshot.
//
// The cap is thirty properties an event and it is not configurable, which is
// defensible only if the customer can see when they hit it: the incumbent
// drops the thirty-first with no error, no warning and no rejection, and a
// self-hoster sending fifty spent a long time working out why some of them
// vanished.
func PropertyHealth(snapshot ingest.Snapshot, siteID int64) Health {
	var health Health

	for _, count := range snapshot.Truncations {
		if count.SiteID != siteID {
			continue
		}

		switch count.Reason {
		case ingest.TruncationProps:
			health.OverLimit += count.Count
		case ingest.TruncationPropName:
			health.NamesTruncated += count.Count
		case ingest.TruncationPropValue:
			health.ValuesTruncated += count.Count
		case ingest.TruncationPropUnsupported:
			health.Unsupported += count.Count
		}
	}

	var parts []string

	if health.OverLimit > 0 {
		parts = append(parts, fmt.Sprintf("%d properties past the %d-per-event limit were not stored",
			health.OverLimit, ingest.MaxProps))
	}
	if health.NamesTruncated > 0 {
		parts = append(parts, fmt.Sprintf("%d property names were cut to %d characters",
			health.NamesTruncated, ingest.MaxPropNameLength))
	}
	if health.ValuesTruncated > 0 {
		parts = append(parts, fmt.Sprintf("%d property values were cut to %d characters",
			health.ValuesTruncated, ingest.MaxPropValueLength))
	}
	if health.Unsupported > 0 {
		parts = append(parts, fmt.Sprintf("%d properties held an object, an array or null and could not be stored",
			health.Unsupported))
	}

	health.Message = strings.Join(parts, "; ")

	return health
}

// PIINotice is what the properties settings screen has to say out loud.
// Properties are customer-controlled free text that lands verbatim in API
// responses and in exports, and the only defence that works is telling people
// plainly not to put personal data in them.
const PIINotice = "Custom properties are stored and returned exactly as your site sends them. " +
	"Do not put names, email addresses, or anything else personal in a property value."
