//
// schema.go
// The JSON Schema fragments every tool's arguments are described with.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// A tool's schema is the whole of its documentation as far as a model is
// concerned: it never sees a README. So every property here carries a
// description with an example in it, and the enumerations are real enumerations
// rather than free strings — a model given `"enum": [...]` picks from the list,
// where one given `"type": "string"` invents a plausible value and gets an
// error.

// object builds an object schema.
func object(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}

	if len(required) > 0 {
		schema["required"] = required
	}

	return schema
}

// str describes a string argument.
func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// enum describes a string argument with a fixed set of values.
func enum(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

// integer describes a bounded whole number.
func integer(description string, low, high int) map[string]any {
	return map[string]any{
		"type": "integer", "description": description, "minimum": low, "maximum": high,
	}
}

// flag describes a boolean argument.
func flag(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

// listOf describes an array argument.
func listOf(description string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": items}
}

// siteArg is the argument almost every tool takes. It is described once so that
// a model reads the same sentence about it on every tool rather than a slightly
// different one each time.
func siteArg() map[string]any {
	return str("The site's domain, exactly as list_sites returns it — for example example.com.")
}

// metricsArg describes the metric list, enumerated from the registry so the
// schema can never fall out of step with what the engine can actually count.
func metricsArg() map[string]any {
	return map[string]any{
		"type":        "array",
		"description": "What to count. The response returns one number per metric, in this order.",
		"items":       map[string]any{"type": "string", "enum": query.MetricNames()},
	}
}

// dimensionsArg describes the grouping list. The enumeration is left off on
// purpose: event:props:<key> is a family of names rather than a fixed one, and
// an enum that excluded it would tell a model custom properties do not exist.
func dimensionsArg() map[string]any {
	return listOf(
		"What to group by, at most five. Known names: "+strings.Join(query.DimensionNames(), ", ")+
			", plus event:props:<key> for a custom property. Read the site's schema resource for the "+
			"properties that site actually has.",
		map[string]any{"type": "string"})
}

// dateRangeArg describes the range, which is either a preset or a pair of dates.
func dateRangeArg() map[string]any {
	return map[string]any{
		"description": "Either a preset — day, 7d, 28d, 91d, month, last_month, year, 12mo, all, " +
			"24h or realtime — or a pair of dates as [\"2026-08-01\", \"2026-08-31\"], where the " +
			"second date is included in full.",
		"anyOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 2, "maxItems": 2},
		},
	}
}

// filtersArg describes the filter list in the array wire form the whole product
// uses, with an example — a model that has seen one example of a positional
// array gets the shape right, where a prose description of it rarely lands.
func filtersArg() map[string]any {
	return map[string]any{
		"type": "array",
		"description": "Predicates, ANDed together. Each is a positional array: " +
			"[\"is\", \"visit:country\", [\"US\", \"CA\"]] — the values inside one filter are ORed. " +
			"Operators: is, is_not, contains, contains_not, matches, matches_not (regular expression), " +
			"and has_done, which takes an inner filter and selects whole visits: " +
			"[\"has_done\", [\"is\", \"event:name\", [\"Signup\"]]].",
		"items": map[string]any{"type": "array"},
	}
}

// orderByArg describes the sort, in the same positional form.
func orderByArg() map[string]any {
	return map[string]any{
		"type": "array",
		"description": "Sort keys, as [\"visitors\", \"desc\"]. A key must be a metric or a dimension " +
			"the query already asked for.",
		"items": map[string]any{"type": "array"},
	}
}

// compareArg describes the comparison mode.
func compareArg() map[string]any {
	return enum(
		"Compare against an earlier period. previous_period is the same length immediately before; "+
			"year_over_year is the same dates a year earlier. A period still in progress is compared "+
			"against the same elapsed time, not against a whole earlier period.",
		"previous_period", "year_over_year")
}
