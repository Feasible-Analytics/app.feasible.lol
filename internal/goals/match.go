//
// match.go
// Turning a goal into the filters the query compiler already knows how to run.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package goals

import (
	"regexp"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// The two wildcards a page goal may use, and the only two. One star stays
// inside a path segment and two cross them, so /blog/* is the posts directly
// under /blog and /blog/** is everything beneath it however deep. Without the
// distinction there is no way to write "one level down", which is most of what
// anybody wants a wildcard for.
const (
	wildcardSegment = "*"
	wildcardDeep    = "**"
)

// compilePattern turns a path pattern into an anchored regular expression.
//
// It is anchored on both ends because a goal is a statement about the whole
// path: an unanchored /pricing would also match /enterprise-pricing-guide, and
// a customer counting sales would have no way to tell from the number that it
// had happened.
func compilePattern(pattern string) (string, error) {
	var out strings.Builder

	out.WriteString("^")

	for i := 0; i < len(pattern); {
		if pattern[i] != '*' {
			// The literal run up to the next star is quoted in one piece, so a
			// dot or a question mark in a real path cannot become a wildcard.
			next := strings.IndexByte(pattern[i:], '*')
			if next < 0 {
				out.WriteString(regexp.QuoteMeta(pattern[i:]))
				break
			}

			out.WriteString(regexp.QuoteMeta(pattern[i : i+next]))
			i += next

			continue
		}

		if strings.HasPrefix(pattern[i:], wildcardDeep) {
			out.WriteString(".*")
			i += len(wildcardDeep)

			continue
		}

		out.WriteString("[^/]*")
		i += len(wildcardSegment)
	}

	out.WriteString("$")

	source := out.String()

	// The compile is thrown away and only its error is kept. It runs at
	// definition time so a pattern that cannot compile is refused while
	// somebody is looking at the form, rather than at three in the morning
	// inside a report.
	if _, err := regexp.Compile(source); err != nil {
		return "", invalid("%q is not a usable path pattern: %v", pattern, err)
	}

	return source, nil
}

// Matches reports whether a path satisfies a page goal's pattern. The query
// layer matches inside SQL for speed; this is the same rule in Go, and it is
// what a test asserts the wildcard semantics against without a database.
func Matches(pattern, path string) bool {
	source, err := compilePattern(strings.TrimSpace(pattern))
	if err != nil {
		return false
	}

	compiled, err := regexp.Compile(source)
	if err != nil {
		return false
	}

	return compiled.MatchString(path)
}

// hasWildcard reports whether a pattern needs regular-expression matching at
// all. Most goals are an exact path, and an exact path is an equality against
// an interned id rather than a scan of every distinct path the site has.
func hasWildcard(pattern string) bool {
	return strings.Contains(pattern, wildcardSegment)
}

// Filters compiles a goal into the query filters that select its conversions.
//
// Going through the query compiler rather than writing the SQL here is the
// whole design of this package: a goals report that counted visitors its own
// way would disagree with the visitors graph on the same screen, and there
// would be no way to tell which of the two was wrong.
func (g Goal) Filters() ([]query.Filter, error) {
	var filters []query.Filter

	switch g.Kind {
	case KindPage:
		// A page goal is a pageview, not any event that happened to be on that
		// path. Without this the goal would also count the engagement pings
		// and custom events fired from the page, which is two to three times
		// the real number.
		filters = append(filters, query.Filter{
			Operator:  query.OpIs,
			Dimension: "event:name",
			Values:    []string{ingest.EventPageview},
		})

		if !hasWildcard(g.PagePattern) {
			filters = append(filters, query.Filter{
				Operator:  query.OpIs,
				Dimension: "event:page",
				Values:    []string{g.PagePattern},
			})

			break
		}

		source, err := compilePattern(g.PagePattern)
		if err != nil {
			return nil, err
		}

		filters = append(filters, query.Filter{
			Operator:  query.OpMatches,
			Dimension: "event:page",
			Values:    []string{source},
		})

	case KindEvent:
		filters = append(filters, query.Filter{
			Operator:  query.OpIs,
			Dimension: "event:name",
			Values:    []string{g.EventName},
		})

	default:
		return nil, invalid("a goal is either %q or %q, not %q", KindPage, KindEvent, g.Kind)
	}

	for _, property := range g.Properties {
		filters = append(filters, query.Filter{
			Operator:  query.OpIs,
			Dimension: "event:props:" + property.Name,
			Values:    []string{property.Value},
		})
	}

	return filters, nil
}
