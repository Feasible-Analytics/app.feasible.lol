//
// pathclean.go
// Per-site path rewrite rules: ordered, first match wins.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package pathclean merges high-cardinality URLs into the routes a person
// actually thinks in — /users/3f2a…/settings becomes /users/:id/settings — and
// it does it in the two places that each fix a different problem.
//
// At ingest, so the dimension table stops growing. A site with an identifier in
// its URLs otherwise interns a new dim_pathname row per request, and that table
// is warmed into memory for every account on the box.
//
// At query time, so the fix is retroactive. This is the half the incumbent does
// not have at all: their answer to /about and /about/ being two permanent rows
// is that it is the customer's server's problem. Rewriting stored rows would be
// the easy implementation and the wrong one — it destroys the original path, so
// a rule written badly could never be taken back. Instead the rules are
// materialised into a map from one interned id to another, and the query layer
// groups through it. Change a rule and every historical report changes with it;
// delete every rule and the original paths are still there.
package pathclean

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// MaxRules bounds a site's rule list. Every rule is a regular expression run
// against every distinct path when the map is rebuilt, so the ceiling is a
// bound on that rebuild rather than an arbitrary product limit.
const MaxRules = 30

// MaxPatternLength bounds one pattern. A regular expression long enough to
// exceed this is not a route pattern, and refusing it here is cheaper than
// discovering it in a rebuild that takes a minute per rule.
const MaxPatternLength = 500

// TrailingSlashPattern and TrailingSlashReplacement merge /about/ into /about.
// They are named constants because the settings page offers this as a single
// switch: it is right for most sites and wrong for the few that serve different
// content at each spelling, so it ships disabled rather than decided for them.
const (
	TrailingSlashPattern     = `^(.+?)/+$`
	TrailingSlashReplacement = `$1`
	TrailingSlashLabel       = "Merge trailing slashes"
)

// Rule is one rewrite. Position is what makes the list ordered, and the order
// is load-bearing: first match wins, so a specific rule above a general one
// keeps its meaning instead of being swallowed by it.
type Rule struct {
	ID          int64
	SiteID      int64
	Position    int
	Pattern     string
	Replacement string
	Label       string
	Enabled     bool
}

// Ruleset is a site's rules compiled and ready to run. Compiling once and
// reusing it matters: the ingest path applies this per event.
type Ruleset struct {
	patterns     []*regexp.Regexp
	replacements []string
}

// Empty reports whether this ruleset would change anything at all. The ingest
// path and the query planner both check it so that a site with no rules pays
// nothing.
func (r *Ruleset) Empty() bool {
	return r == nil || len(r.patterns) == 0
}

// Clean applies the first rule that matches and returns the result. It stops at
// the first match rather than chaining every rule, because chained rewrites are
// impossible to predict from a rule list and the preview would stop meaning
// what it says.
func (r *Ruleset) Clean(path string) string {
	if r.Empty() || path == "" {
		return path
	}

	for i, pattern := range r.patterns {
		if location := pattern.FindStringIndex(path); location != nil {
			return pattern.ReplaceAllString(path, r.replacements[i])
		}
	}

	return path
}

// Compile turns a rule list into a ruleset, skipping the disabled ones. A bad
// pattern is an error naming the rule rather than a silently-skipped rewrite,
// because a rule that quietly does nothing looks exactly like a rule that is
// working on paths you have not seen yet.
func Compile(rules []Rule) (*Ruleset, error) {
	set := &Ruleset{}

	ordered := append([]Rule(nil), rules...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Position < ordered[j].Position })

	for _, rule := range ordered {
		if !rule.Enabled {
			continue
		}

		compiled, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return nil, fmt.Errorf("path cleaning rule %d (%q) is not a valid regular expression: %w", rule.Position+1, rule.Pattern, err)
		}

		set.patterns = append(set.patterns, compiled)
		set.replacements = append(set.replacements, rule.Replacement)
	}

	return set, nil
}

// Validate checks one rule before it is stored, so the error lands next to the
// field the customer typed it into rather than inside a background job an hour
// later.
func Validate(rule Rule) error {
	pattern := strings.TrimSpace(rule.Pattern)

	if pattern == "" {
		return fmt.Errorf("a path cleaning rule needs a pattern")
	}

	if len(pattern) > MaxPatternLength {
		return fmt.Errorf("a pattern may be at most %d characters, not %d", MaxPatternLength, len(pattern))
	}

	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Errorf("%q is not a valid regular expression: %w", pattern, err)
	}

	return nil
}

// List reads a site's rules in order.
func List(ctx context.Context, db *sql.DB, siteID int64) (rules []Rule, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, site_id, position, pattern, replacement, label, is_enabled
		FROM path_clean_rules WHERE site_id = ? ORDER BY position`, siteID)
	if err != nil {
		return nil, fmt.Errorf("pathclean: read rules: %w", err)
	}
	defer closePathRows(rows, &err, "read rules")

	for rows.Next() {
		var rule Rule
		var enabled int

		if err := rows.Scan(&rule.ID, &rule.SiteID, &rule.Position, &rule.Pattern,
			&rule.Replacement, &rule.Label, &enabled); err != nil {
			return nil, fmt.Errorf("pathclean: read rules: %w", err)
		}

		rule.Enabled = enabled == 1
		rules = append(rules, rule)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pathclean: read rules: %w", err)
	}

	return rules, nil
}

// closePathRows closes a read cursor and joins any driver cleanup failure to
// the path-cleaning operation that owns it.
func closePathRows(rows *sql.Rows, err *error, operation string) {
	if closeErr := rows.Close(); closeErr != nil {
		*err = errors.Join(*err, fmt.Errorf("pathclean: %s: close rows: %w", operation, closeErr))
	}
}

// Ruleset reads and compiles a site's rules in one call, which is what the
// ingest path and the preview both actually want.
func RulesetFor(ctx context.Context, db *sql.DB, siteID int64) (*Ruleset, error) {
	rules, err := List(ctx, db, siteID)
	if err != nil {
		return nil, err
	}

	return Compile(rules)
}

// Replace writes a site's whole rule list. The list is replaced rather than
// patched because position is the meaning of a rule and a partial update would
// need every reordering expressed as a diff — which is how two rules end up
// claiming position 3 and the UNIQUE constraint fails on a save that looked
// fine on screen.
func Replace(ctx context.Context, db *sql.DB, siteID int64, rules []Rule, now time.Time) error {
	if len(rules) > MaxRules {
		return fmt.Errorf("a site may hold at most %d path cleaning rules, not %d", MaxRules, len(rules))
	}

	for _, rule := range rules {
		if err := Validate(rule); err != nil {
			return err
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pathclean: save rules: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	if _, err := tx.ExecContext(ctx, "DELETE FROM path_clean_rules WHERE site_id = ?", siteID); err != nil {
		return fmt.Errorf("pathclean: save rules: %w", err)
	}

	for i, rule := range rules {
		enabled := 0
		if rule.Enabled {
			enabled = 1
		}

		_, err := tx.ExecContext(ctx, `
			INSERT INTO path_clean_rules (site_id, position, pattern, replacement, label, is_enabled, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			siteID, i, strings.TrimSpace(rule.Pattern), rule.Replacement, rule.Label, enabled, now.Unix())
		if err != nil {
			return fmt.Errorf("pathclean: save rule %d: %w", i+1, err)
		}
	}

	return tx.Commit()
}

// MaxMergeSources bounds how many original paths one merge lists. A rule that
// collapses four hundred URLs is understood from the first handful and the
// total; printing all four hundred buries the next merge under it.
const MaxMergeSources = 6

// Merge is one preview entry: the paths that would collapse into a single row
// and how many event rows each of them accounts for. It is what the settings
// page shows before anything is saved, because a regular expression that eats
// half a site's URLs looks identical to a correct one until you see the list.
type Merge struct {
	Target string

	// Sources is the busiest few of the paths that would merge, and Hidden is
	// how many more there are. Rows always counts every one of them.
	Sources []MergeSource
	Hidden  int

	// Paths is how many original paths merge in total, and Rows how many stored
	// rows they account for between them.
	Paths int
	Rows  int64
}

// MergeSource is one original path inside a merge.
type MergeSource struct {
	Path string
	Rows int64
}

// Preview works out what a rule list would do without saving it. Counting rows
// per path uses the site/pathname index, so the answer comes from an index scan
// rather than from reading the events themselves.
func Preview(ctx context.Context, db *sql.DB, siteID int64, rules []Rule, limit int) ([]Merge, error) {
	set, err := Compile(rules)
	if err != nil {
		return nil, err
	}

	paths, err := pathRows(ctx, db, siteID)
	if err != nil {
		return nil, err
	}

	byTarget := map[string]*Merge{}

	for _, path := range paths {
		cleaned := set.Clean(path.value)
		if cleaned == path.value {
			continue
		}

		merge, ok := byTarget[cleaned]
		if !ok {
			merge = &Merge{Target: cleaned}
			byTarget[cleaned] = merge
		}

		merge.Sources = append(merge.Sources, MergeSource{Path: path.value, Rows: path.rows})
		merge.Paths++
		merge.Rows += path.rows
	}

	merges := make([]Merge, 0, len(byTarget))
	for _, merge := range byTarget {
		sort.Slice(merge.Sources, func(i, j int) bool {
			if merge.Sources[i].Rows != merge.Sources[j].Rows {
				return merge.Sources[i].Rows > merge.Sources[j].Rows
			}
			return merge.Sources[i].Path < merge.Sources[j].Path
		})

		if len(merge.Sources) > MaxMergeSources {
			merge.Hidden = len(merge.Sources) - MaxMergeSources
			merge.Sources = merge.Sources[:MaxMergeSources]
		}

		merges = append(merges, *merge)
	}

	// Biggest first: the merges worth checking before saving are the ones that
	// move the most rows, and a preview that led with a path seen twice would
	// bury the rule that swallowed a third of the site.
	sort.Slice(merges, func(i, j int) bool {
		if merges[i].Rows != merges[j].Rows {
			return merges[i].Rows > merges[j].Rows
		}
		return merges[i].Target < merges[j].Target
	})

	if limit > 0 && len(merges) > limit {
		merges = merges[:limit]
	}

	return merges, nil
}

// pathEntry is one interned path and how many event rows carry it.
type pathEntry struct {
	id    int64
	value string
	rows  int64
}

// pathRows reads every interned path with the number of this site's events on
// it. The counts come from a grouped read of the site/pathname index rather
// than from the events themselves, which is what keeps a preview affordable on
// a site with millions of rows.
func pathRows(ctx context.Context, db *sql.DB, siteID int64) (entries []pathEntry, err error) {
	counts := map[int64]int64{}

	rows, err := db.QueryContext(ctx,
		"SELECT pathname_id, COUNT(*) FROM events WHERE site_id = ? GROUP BY pathname_id", siteID)
	if err != nil {
		return nil, fmt.Errorf("pathclean: count paths: %w", err)
	}

	for rows.Next() {
		var id, count int64
		if scanErr := rows.Scan(&id, &count); scanErr != nil {
			err = fmt.Errorf("pathclean: count paths: %w", scanErr)
			closePathRows(rows, &err, "count paths")
			return nil, err
		}
		counts[id] = count
	}

	err = rows.Err()
	if err != nil {
		err = fmt.Errorf("pathclean: count paths: %w", err)
	}
	closePathRows(rows, &err, "count paths")
	if err != nil {
		return nil, err
	}

	// Imported history is counted too. A path that only exists in an import is
	// exactly the kind of thing a cleaning rule is written for, and leaving it
	// out of the preview would show the customer a smaller merge than the one
	// they are about to make.
	imported, err := db.QueryContext(ctx,
		"SELECT pathname_id, SUM(pageviews) FROM imported_rollups WHERE site_id = ? GROUP BY pathname_id", siteID)
	if err != nil {
		return nil, fmt.Errorf("pathclean: count imported paths: %w", err)
	}

	for imported.Next() {
		var id int64
		var count sql.NullInt64

		if scanErr := imported.Scan(&id, &count); scanErr != nil {
			err = fmt.Errorf("pathclean: count imported paths: %w", scanErr)
			closePathRows(imported, &err, "count imported paths")
			return nil, err
		}

		counts[id] += count.Int64
	}

	err = imported.Err()
	if err != nil {
		err = fmt.Errorf("pathclean: count imported paths: %w", err)
	}
	closePathRows(imported, &err, "count imported paths")
	if err != nil {
		return nil, err
	}

	values, err := db.QueryContext(ctx, "SELECT id, value FROM "+intern.Pathname.Table()+" WHERE id <> 0")
	if err != nil {
		return nil, fmt.Errorf("pathclean: read paths: %w", err)
	}
	defer closePathRows(values, &err, "read paths")

	for values.Next() {
		var entry pathEntry
		if err := values.Scan(&entry.id, &entry.value); err != nil {
			return nil, fmt.Errorf("pathclean: read paths: %w", err)
		}

		entry.rows = counts[entry.id]
		entries = append(entries, entry)
	}

	if err := values.Err(); err != nil {
		return nil, fmt.Errorf("pathclean: read paths: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })

	return entries, nil
}

// Materialise rebuilds the id-to-id map a query groups through. It runs after
// the rules change and after an import brings in paths nobody has seen, and it
// is the only thing that makes the rules retroactive.
//
// Identity mappings are never stored. That is what lets the query planner ask
// one cheap question — does this site have any rows here at all — and skip the
// whole mechanism for the sites that have no rules.
func Materialise(ctx context.Context, db *sql.DB, cache *intern.Cache, siteID int64) (int, error) {
	rules, err := List(ctx, db, siteID)
	if err != nil {
		return 0, err
	}

	set, err := Compile(rules)
	if err != nil {
		return 0, err
	}

	paths, err := pathRows(ctx, db, siteID)
	if err != nil {
		return 0, err
	}

	// The target ids are resolved before the transaction opens. Interning can
	// insert a row, and an account's writer is a pool of exactly one connection
	// — a query issued while a transaction holds it waits for a connection only
	// that transaction can release.
	type mapping struct{ source, target int64 }

	var mappings []mapping

	for _, path := range paths {
		cleaned := set.Clean(path.value)
		if cleaned == path.value {
			continue
		}

		target, err := cache.ID(ctx, intern.Pathname, cleaned)
		if err != nil {
			return 0, fmt.Errorf("pathclean: intern %q: %w", cleaned, err)
		}

		if target == path.id {
			continue
		}

		mappings = append(mappings, mapping{source: path.id, target: target})
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("pathclean: rebuild map: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	if _, err := tx.ExecContext(ctx, "DELETE FROM path_clean_map WHERE site_id = ?", siteID); err != nil {
		return 0, fmt.Errorf("pathclean: rebuild map: %w", err)
	}

	for _, entry := range mappings {
		_, err := tx.ExecContext(ctx,
			"INSERT INTO path_clean_map (site_id, source_id, target_id) VALUES (?, ?, ?)",
			siteID, entry.source, entry.target)
		if err != nil {
			return 0, fmt.Errorf("pathclean: rebuild map: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("pathclean: rebuild map: %w", err)
	}

	return len(mappings), nil
}
