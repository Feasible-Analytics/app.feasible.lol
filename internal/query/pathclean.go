//
// pathclean.go
// Grouping and filtering through the path cleaning map, so the rules are retroactive.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"context"
	"fmt"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// pathCleanTable is the materialised id-to-id map the pathclean package
// maintains. It is named here rather than imported so that this package keeps
// its rule of building every statement from its own constants.
const pathCleanTable = "path_clean_map"

// cleanedPathID renders the expression that turns a stored pathname id into the
// id a report should group it under.
//
// A scalar subquery rather than a join, for two reasons. It composes: the same
// string can be dropped into a GROUP BY, into a filter's membership test and
// into an ORDER BY without any of them having to agree about which tables are
// in the FROM clause. And it carries no bind parameters, because the site is
// correlated from the row rather than passed in — so a filter compiler that
// deals only in column names does not have to grow an argument list.
//
// The map holds no identity mappings, so a site with no rules has no rows here
// and the planner never emits this at all.
func cleanedPathID(alias, column string) string {
	qualified := alias + "." + column

	return "COALESCE((SELECT pcm.target_id FROM " + pathCleanTable + " pcm" +
		" WHERE pcm.site_id = " + alias + ".site_id AND pcm.source_id = " + qualified + "), " + qualified + ")"
}

// pathColumn returns the expression a dimension should be read through on one
// table. It is the single decision point for path cleaning inside the compiler:
// every grouper and every filter goes through here, so a report can never group
// by the cleaned path and filter on the raw one.
func (c compileContext) pathColumn(alias, column string, d dimension) string {
	if c.pathClean && d.Interned == intern.Pathname {
		return cleanedPathID(alias, column)
	}

	return alias + "." + column
}

// hasPathCleaning reports whether any of a query's sites has cleaning rules
// materialised. It is one indexed read per query rather than per row, and it is
// what keeps the whole mechanism free for the sites that do not use it.
func hasPathCleaning(ctx context.Context, engine *Engine, sites []int64) (bool, error) {
	if len(sites) == 0 {
		return false, nil
	}

	condition := inInt64("site_id", sites)

	var exists int
	err := engine.db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM "+pathCleanTable+" WHERE "+condition.SQL+")", condition.Args...).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("query: read path cleaning map: %w", err)
	}

	return exists == 1, nil
}
