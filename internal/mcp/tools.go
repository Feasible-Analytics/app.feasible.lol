//
// tools.go
// Every tool this server offers, and what each one actually does.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/publicapi"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// toolset builds every tool. It is one function returning a slice rather than a
// package-level variable because each handler is a method on the server, and a
// tool table that could be built before the server exists would need a global.
func (s *Server) toolset() []*Tool {
	return []*Tool{
		s.listSitesTool(),
		s.queryStatsTool(),
		s.realtimeTool(),
		s.listGoalsTool(),
		s.createGoalTool(),
		s.listFunnelsTool(),
		s.getFunnelTool(),
		s.comparePeriodsTool(),
		s.explainTrafficChangeTool(),
		s.createSiteTool(),
		s.updateSiteTool(),
		s.listShieldsTool(),
		s.addShieldRuleTool(),
		s.createAnnotationTool(),
	}
}

// listSitesTool answers "what can I look at". It is the first call every
// session makes, which is why it also reports each site's timezone: every date
// in every later answer is bucketed in it, and a model that does not know a
// site runs on Tokyo time will misread "yesterday" by a day.
func (s *Server) listSitesTool() *Tool {
	return &Tool{
		Name:        "list_sites",
		Title:       "List sites",
		Description: "List the sites this credential can read, with the timezone each one's days are counted in.",
		ReadOnly:    true,
		InputSchema: object(map[string]any{}),
		Handler: func(ctx context.Context, key *apikeys.Key, _ json.RawMessage) (*toolResult, error) {
			list := s.API.SitesFor(key)

			rows := make([]map[string]any, 0, len(list))
			lines := make([]string, 0, len(list))

			for _, site := range list {
				rows = append(rows, map[string]any{
					"site_id":  site.Domain,
					"timezone": site.Timezone,
				})
				lines = append(lines, fmt.Sprintf("%s (timezone %s)", site.Domain, site.Timezone))
			}

			if len(rows) == 0 {
				return &toolResult{
					Content:           []content{text("This credential can see no sites yet. create_site adds one.")},
					StructuredContent: map[string]any{"sites": rows},
				}, nil
			}

			return &toolResult{
				Content:           []content{text(strings.Join(lines, "\n"))},
				StructuredContent: map[string]any{"sites": rows},
			}, nil
		},
	}
}

// statsArgs is the full v2 query surface as one tool's arguments.
//
// It is one tool rather than a tool per report on purpose. Every report in this
// product is the same request with different metrics and dimensions, and
// splitting it into a dozen narrow tools would mean a model has to guess which
// one can answer a question the query engine handles in one call — and would
// leave the combinations nobody wrote a tool for unreachable.
type statsArgs struct {
	SiteID         string          `json:"site_id"`
	Metrics        []string        `json:"metrics"`
	DateRange      query.DateRange `json:"date_range"`
	Dimensions     []string        `json:"dimensions"`
	Filters        []query.Filter  `json:"filters"`
	OrderBy        []query.Order   `json:"order_by"`
	Limit          int             `json:"limit"`
	Offset         int             `json:"offset"`
	Timezone       string          `json:"timezone"`
	Compare        string          `json:"compare"`
	IncludeBots    bool            `json:"include_bots"`
	IncludeImports bool            `json:"include_imported"`
	TotalRows      bool            `json:"include_total_rows"`
}

// queryStatsTool is the whole read surface.
func (s *Server) queryStatsTool() *Tool {
	return &Tool{
		Name:  "query_stats",
		Title: "Query stats",
		Description: "Run any analytics query: pick metrics, group by dimensions, filter, sort and " +
			"paginate. This is the same engine the dashboard runs on, so a number here is the number " +
			"the customer sees.",
		ReadOnly: true,
		InputSchema: object(map[string]any{
			"site_id":            siteArg(),
			"metrics":            metricsArg(),
			"date_range":         dateRangeArg(),
			"dimensions":         dimensionsArg(),
			"filters":            filtersArg(),
			"order_by":           orderByArg(),
			"limit":              integer("Rows to return. Defaults to 100.", 1, query.MaxLimit),
			"offset":             integer("Rows to skip, for paging.", 0, 1000000),
			"timezone":           str("IANA timezone to bucket days in. Defaults to the site's own, which is almost always what you want."),
			"compare":            compareArg(),
			"include_bots":       flag("Include traffic classified as automated. Off by default, because bot traffic in a report is simply a wrong number."),
			"include_imported":   flag("Include data imported from another analytics product."),
			"include_total_rows": flag("Also report how many groups exist before pagination."),
		}, "site_id", "metrics"),
		Handler: s.runStats,
	}
}

// runStats answers query_stats.
func (s *Server) runStats(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
	args := &statsArgs{}
	if err := decodeArgs(raw, args); err != nil {
		return toolFailure("%s", err.Error()), nil
	}

	site, err := s.API.SiteFor(key, args.SiteID)
	if err != nil {
		return toolFailure("%s", err.Error()), nil
	}

	if len(args.Metrics) == 0 {
		return toolFailure("metrics is required — name at least one of %s", strings.Join(query.MetricNames(), ", ")), nil
	}

	comparison, err := comparisonFor(args.Compare)
	if err != nil {
		return toolFailure("%s", err.Error()), nil
	}

	result, err := s.API.Query(ctx, site, query.Query{
		SiteIDs:    []int64{site.ID},
		Metrics:    args.Metrics,
		Dimensions: args.Dimensions,
		Filters:    args.Filters,
		DateRange:  args.DateRange,
		Timezone:   firstNonEmpty(args.Timezone, site.Timezone),
		OrderBy:    args.OrderBy,
		Pagination: query.Pagination{Limit: args.Limit, Offset: args.Offset},
		Include: query.Include{
			Bots:        args.IncludeBots,
			Imports:     args.IncludeImports,
			TotalRows:   args.TotalRows,
			Comparisons: comparison,
		},
	})
	if err != nil {
		return queryFailure(err)
	}

	return &toolResult{
		Content:           []content{text(renderTable(args.Metrics, args.Dimensions, result))},
		StructuredContent: result,
	}, nil
}

// comparisonFor turns the tool's compare argument into the engine's request.
func comparisonFor(mode string) (*query.Comparison, error) {
	switch mode {
	case "":
		return nil, nil
	case query.ComparePreviousPeriod, query.CompareYearOverYear:
		return &query.Comparison{Mode: mode}, nil
	}

	return nil, fmt.Errorf("compare must be previous_period or year_over_year, not %q", mode)
}

// queryFailure turns an engine error into a tool result. A caller's mistake
// carries the engine's own message, which names the field and lists the valid
// values — exactly what a model needs to correct itself on the next call.
func queryFailure(err error) (*toolResult, error) {
	var callerError *query.Error
	if ok := asQueryError(err, &callerError); ok {
		return toolFailure("%s", callerError.Message), nil
	}

	return nil, err
}

// asQueryError unwraps a caller-facing engine error. It is a named function
// rather than errors.As at the call sites so the two places that need it cannot
// end up unwrapping different types.
func asQueryError(err error, target **query.Error) bool {
	return errors.As(err, target)
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}

// renderTable turns a result into something readable.
//
// It is a fixed-width table rather than JSON because a model reading twenty
// rows of numbers gets the comparison right far more often from a table than
// from a nested object, and the structured payload is still attached for
// anything that would rather compute than read.
func renderTable(metrics, dimensions []string, result *query.Result) string {
	var out strings.Builder

	fmt.Fprintf(&out, "%s to %s (%s, %s buckets)\n",
		result.Query.DateRange[0], result.Query.DateRange[1], result.Query.Timezone, result.Meta.Interval)

	header := append(append([]string{}, dimensions...), metrics...)
	out.WriteString(strings.Join(header, " | ") + "\n")

	for _, row := range result.Results {
		cells := append([]string{}, row.Dimensions...)

		for i := range metrics {
			value := 0.0
			if i < len(row.Metrics) {
				value = row.Metrics[i]
			}

			cell := strconv.FormatFloat(value, 'f', -1, 64)

			if row.Comparison != nil && i < len(row.Comparison.Change) && row.Comparison.Change[i] != nil {
				cell += fmt.Sprintf(" (%+.1f%%)", *row.Comparison.Change[i])
			}

			cells = append(cells, cell)
		}

		out.WriteString(strings.Join(cells, " | ") + "\n")
	}

	if len(result.Results) == 0 {
		out.WriteString("(no rows — the range has no matching traffic)\n")
	}

	// Warnings are printed rather than left in the structured payload, because
	// a re-scoped or partially-covered metric that announces itself is the
	// difference between a number a model can trust and one it repeats without
	// the caveat that made it meaningful.
	for _, name := range sortedKeys(result.Meta.MetricWarnings) {
		out.WriteString("note: " + name + " — " + result.Meta.MetricWarnings[name].Warning + "\n")
	}

	return out.String()
}

// realtimeArgs is the realtime tool's argument.
type realtimeArgs struct {
	SiteID string `json:"site_id"`
}

// realtimeTool answers how many people are on the site now.
func (s *Server) realtimeTool() *Tool {
	return &Tool{
		Name:  "get_realtime_visitors",
		Title: "Realtime visitors",
		Description: "How many visitors are on the site right now — meaning in the last thirty minutes, " +
			"which is how long a visit stays open.",
		ReadOnly:    true,
		InputSchema: object(map[string]any{"site_id": siteArg()}, "site_id"),
		Handler: func(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
			args := &realtimeArgs{}
			if err := decodeArgs(raw, args); err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			site, err := s.API.SiteFor(key, args.SiteID)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			result, err := s.API.Query(ctx, site, query.Query{
				SiteIDs:   []int64{site.ID},
				Metrics:   []string{"visitors"},
				DateRange: query.DateRange{Preset: query.RangeRealtime},
				Timezone:  site.Timezone,
			})
			if err != nil {
				return queryFailure(err)
			}

			visitors := 0.0
			if len(result.Results) > 0 && len(result.Results[0].Metrics) > 0 {
				visitors = result.Results[0].Metrics[0]
			}

			return &toolResult{
				Content: []content{text(fmt.Sprintf("%d visitors on %s in the last 30 minutes.",
					int64(visitors), site.Domain))},
				StructuredContent: map[string]any{"site_id": site.Domain, "visitors": int64(visitors)},
			}, nil
		},
	}
}

// compareArgs is the compare_periods tool's arguments.
type compareArgs struct {
	SiteID     string          `json:"site_id"`
	Metrics    []string        `json:"metrics"`
	DateRange  query.DateRange `json:"date_range"`
	Dimensions []string        `json:"dimensions"`
	Filters    []query.Filter  `json:"filters"`
	Compare    string          `json:"compare"`
	Limit      int             `json:"limit"`
}

// comparePeriodsTool puts two periods side by side.
//
// It is a tool of its own rather than a note in query_stats's description
// because the comparison window is resolved server-side against one clock. Two
// separate calls either side of midnight compare the wrong days, and a model has
// no way of noticing that it did.
func (s *Server) comparePeriodsTool() *Tool {
	return &Tool{
		Name:  "compare_periods",
		Title: "Compare periods",
		Description: "Run a query over one period and the period before it, and report both numbers and " +
			"the percentage change. Both windows are resolved against the same clock and the same " +
			"timezone, so a comparison taken near midnight still lines up.",
		ReadOnly: true,
		InputSchema: object(map[string]any{
			"site_id":    siteArg(),
			"metrics":    metricsArg(),
			"date_range": dateRangeArg(),
			"dimensions": dimensionsArg(),
			"filters":    filtersArg(),
			"compare":    compareArg(),
			"limit":      integer("Rows to return. Defaults to 100.", 1, query.MaxLimit),
		}, "site_id", "metrics"),
		Handler: func(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
			args := &compareArgs{}
			if err := decodeArgs(raw, args); err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			site, err := s.API.SiteFor(key, args.SiteID)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			if len(args.Metrics) == 0 {
				return toolFailure("metrics is required"), nil
			}

			mode := args.Compare
			if mode == "" {
				mode = query.ComparePreviousPeriod
			}

			comparison, err := comparisonFor(mode)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			result, err := s.API.Query(ctx, site, query.Query{
				SiteIDs:    []int64{site.ID},
				Metrics:    args.Metrics,
				Dimensions: args.Dimensions,
				Filters:    args.Filters,
				DateRange:  args.DateRange,
				Timezone:   site.Timezone,
				Pagination: query.Pagination{Limit: args.Limit},
				Include:    query.Include{Comparisons: comparison},
			})
			if err != nil {
				return queryFailure(err)
			}

			return &toolResult{
				Content:           []content{text(renderComparison(args.Metrics, args.Dimensions, result))},
				StructuredContent: result,
			}, nil
		},
	}
}

// renderComparison prints both periods next to each other.
func renderComparison(metrics, dimensions []string, result *query.Result) string {
	var out strings.Builder

	fmt.Fprintf(&out, "%s to %s, against %s\n",
		result.Query.DateRange[0], result.Query.DateRange[1],
		strings.Join(result.Meta.ComparisonDateRange, " to "))

	header := append(append([]string{}, dimensions...), []string{"metric", "now", "before", "change"}...)
	out.WriteString(strings.Join(header, " | ") + "\n")

	for _, row := range result.Results {
		for i, name := range metrics {
			cells := append([]string{}, row.Dimensions...)

			current := 0.0
			if i < len(row.Metrics) {
				current = row.Metrics[i]
			}

			previous := 0.0
			change := "n/a"

			if row.Comparison != nil {
				if i < len(row.Comparison.Metrics) {
					previous = row.Comparison.Metrics[i]
				}
				if i < len(row.Comparison.Change) && row.Comparison.Change[i] != nil {
					change = fmt.Sprintf("%+.1f%%", *row.Comparison.Change[i])
				}
			}

			cells = append(cells, name,
				strconv.FormatFloat(current, 'f', -1, 64),
				strconv.FormatFloat(previous, 'f', -1, 64),
				change)

			out.WriteString(strings.Join(cells, " | ") + "\n")
		}
	}

	return out.String()
}

// createSiteArgs is the create_site tool's arguments.
type createSiteArgs struct {
	Domain      string `json:"domain"`
	DisplayName string `json:"display_name"`
	Timezone    string `json:"timezone"`
}

// createSiteTool registers a domain.
func (s *Server) createSiteTool() *Tool {
	return &Tool{
		Name:        "create_site",
		Title:       "Create site",
		Description: "Register a new site on this team and return the tracking snippet to install.",
		Permission:  teams.PermManageSites,
		InputSchema: object(map[string]any{
			"domain":       str("The bare hostname, such as example.com — not a URL."),
			"display_name": str("What to call it in the dashboard. Defaults to the domain."),
			"timezone":     str("IANA timezone the site's days are counted in, such as America/Los_Angeles. Defaults to Etc/UTC."),
		}, "domain"),
		Handler: func(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
			args := &createSiteArgs{}
			if err := decodeArgs(raw, args); err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			site, err := s.API.NewSite(ctx, key, args.Domain, args.DisplayName, args.Timezone)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			return &toolResult{
				Content: []content{text(fmt.Sprintf(
					"Created %s in timezone %s. It will start counting as soon as the tracking script is installed.",
					site.Domain, site.Timezone))},
				StructuredContent: site,
			}, nil
		},
	}
}

// updateSiteArgs is the update_site tool's arguments. The optional fields are
// pointers so that "not mentioned" and "set to empty" are different things —
// otherwise renaming a site would also switch its public dashboard off.
type updateSiteArgs struct {
	SiteID      string  `json:"site_id"`
	Domain      *string `json:"domain"`
	DisplayName *string `json:"display_name"`
	Timezone    *string `json:"timezone"`
	IsPublic    *bool   `json:"is_public"`
}

// updateSiteTool changes a site's settings.
func (s *Server) updateSiteTool() *Tool {
	return &Tool{
		Name:        "update_site",
		Title:       "Update site",
		Description: "Change a site's domain, display name, timezone or public-dashboard setting. Anything left out is untouched.",
		InputSchema: object(map[string]any{
			"site_id":      siteArg(),
			"domain":       str("A new hostname. Renaming keeps the site's history."),
			"display_name": str("A new dashboard name."),
			"timezone":     str("A new IANA timezone. This changes what every past day means, so it is not a small change."),
			"is_public":    flag("Whether the dashboard is readable without signing in."),
		}, "site_id"),
		Handler: func(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
			args := &updateSiteArgs{}
			if err := decodeArgs(raw, args); err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			site, err := s.API.SiteFor(key, args.SiteID)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			record, err := s.API.EditSite(ctx, key, site, args.Domain, args.DisplayName, args.Timezone, args.IsPublic)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			return &toolResult{
				Content:           []content{text("Updated " + record.Domain + ".")},
				StructuredContent: record,
			}, nil
		},
	}
}

// siteOnlyArgs is the argument shape of every tool that only names a site.
type siteOnlyArgs struct {
	SiteID string `json:"site_id"`
}

// listGoalsTool lists a site's conversions.
func (s *Server) listGoalsTool() *Tool {
	return &Tool{
		Name:        "list_goals",
		Title:       "List goals",
		Description: "List the conversions this site counts.",
		ReadOnly:    true,
		Permission:  teams.PermManageSiteSettings,
		InputSchema: object(map[string]any{"site_id": siteArg()}, "site_id"),
		Handler: func(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
			args := &siteOnlyArgs{}
			if err := decodeArgs(raw, args); err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			site, err := s.API.SiteFor(key, args.SiteID)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			if s.API.Goals == nil {
				return toolFailure("%s", publicapi.Unavailable("goals")), nil
			}

			goals, err := s.API.Goals.ListGoals(ctx, site.ID)
			if err != nil {
				return nil, err
			}

			lines := make([]string, 0, len(goals))
			for _, goal := range goals {
				lines = append(lines, fmt.Sprintf("%d: %s (%s%s)", goal.ID, goal.DisplayName, goal.EventName, goal.PagePath))
			}

			if len(lines) == 0 {
				lines = append(lines, "This site counts no goals yet.")
			}

			return &toolResult{
				Content:           []content{text(strings.Join(lines, "\n"))},
				StructuredContent: map[string]any{"goals": goals},
			}, nil
		},
	}
}

// createGoalArgs is the create_goal tool's arguments.
type createGoalArgs struct {
	SiteID      string `json:"site_id"`
	DisplayName string `json:"display_name"`
	EventName   string `json:"event_name"`
	PagePath    string `json:"page_path"`
	Currency    string `json:"currency"`
}

// createGoalTool registers a conversion.
func (s *Server) createGoalTool() *Tool {
	return &Tool{
		Name:        "create_goal",
		Title:       "Create goal",
		Description: "Register a conversion, counted either when a named custom event fires or when a page is viewed.",
		InputSchema: object(map[string]any{
			"site_id":      siteArg(),
			"display_name": str("What to call it in reports."),
			"event_name":   str("The custom event name to count, such as Signup. Give this or page_path, not both."),
			"page_path":    str("The path to count a view of, such as /thanks. Give this or event_name, not both."),
			"currency":     str("Three-letter ISO code, if the goal carries revenue."),
		}, "site_id"),
		Handler: func(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
			args := &createGoalArgs{}
			if err := decodeArgs(raw, args); err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			site, err := s.API.SiteFor(key, args.SiteID)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			// The arguments are checked before the feature is, so a model
			// building a call against a build with no goals still learns that
			// its arguments were wrong rather than only that goals are missing.
			goal, err := publicapi.ValidateGoalRequest(args.DisplayName, args.EventName, args.PagePath, args.Currency)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			if s.API.Goals == nil {
				return toolFailure("%s", publicapi.Unavailable("goals")), nil
			}

			created, err := s.API.Goals.CreateGoal(ctx, site.ID, *goal)
			if err != nil {
				return nil, err
			}

			return &toolResult{
				Content:           []content{text("Created goal " + created.DisplayName + ".")},
				StructuredContent: created,
			}, nil
		},
	}
}

// listFunnelsTool lists a site's funnels.
func (s *Server) listFunnelsTool() *Tool {
	return &Tool{
		Name:        "list_funnels",
		Title:       "List funnels",
		Description: "List the funnels defined on this site, with their steps.",
		ReadOnly:    true,
		InputSchema: object(map[string]any{"site_id": siteArg()}, "site_id"),
		Handler: func(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
			args := &siteOnlyArgs{}
			if err := decodeArgs(raw, args); err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			site, err := s.API.SiteFor(key, args.SiteID)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			if s.API.Funnels == nil {
				return toolFailure("%s", publicapi.Unavailable("funnels")), nil
			}

			funnels, err := s.API.Funnels.ListFunnels(ctx, site.ID)
			if err != nil {
				return nil, err
			}

			return &toolResult{
				Content:           []content{text(fmt.Sprintf("%d funnels on %s.", len(funnels), site.Domain))},
				StructuredContent: map[string]any{"funnels": funnels},
			}, nil
		},
	}
}

// getFunnelArgs is the get_funnel tool's arguments.
type getFunnelArgs struct {
	SiteID   string `json:"site_id"`
	FunnelID int64  `json:"funnel_id"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// getFunnelTool reports a funnel's conversion at each step.
func (s *Server) getFunnelTool() *Tool {
	return &Tool{
		Name:        "get_funnel",
		Title:       "Get funnel",
		Description: "Report how many visitors reached each step of a funnel over a date range.",
		ReadOnly:    true,
		InputSchema: object(map[string]any{
			"site_id":   siteArg(),
			"funnel_id": integer("The funnel's id, from list_funnels.", 1, 1000000000),
			"from":      str("First day, as YYYY-MM-DD."),
			"to":        str("Last day, as YYYY-MM-DD, included in full."),
		}, "site_id", "funnel_id"),
		Handler: func(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
			args := &getFunnelArgs{}
			if err := decodeArgs(raw, args); err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			site, err := s.API.SiteFor(key, args.SiteID)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			if s.API.Funnels == nil {
				return toolFailure("%s", publicapi.Unavailable("funnels")), nil
			}

			report, err := s.API.Funnels.GetFunnel(ctx, site.ID, args.FunnelID, args.From, args.To)
			if err != nil {
				return nil, err
			}

			return &toolResult{
				Content:           []content{text(fmt.Sprintf("Funnel %s: %d entered.", report.Funnel.Name, report.EntryVisitors))},
				StructuredContent: report,
			}, nil
		},
	}
}

// listShieldsTool lists what is being kept out of the numbers.
func (s *Server) listShieldsTool() *Tool {
	return &Tool{
		Name:        "list_shields",
		Title:       "List shield rules",
		Description: "List the rules that keep traffic out of this site's numbers — blocked IP addresses, countries, pages and hostnames.",
		ReadOnly:    true,
		Permission:  teams.PermManageSiteSettings,
		InputSchema: object(map[string]any{"site_id": siteArg()}, "site_id"),
		Handler: func(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
			args := &siteOnlyArgs{}
			if err := decodeArgs(raw, args); err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			site, err := s.API.SiteFor(key, args.SiteID)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			if s.API.Shields == nil {
				return toolFailure("%s", publicapi.Unavailable("shield rules")), nil
			}

			rules, err := s.API.Shields.ListShields(ctx, site.ID)
			if err != nil {
				return nil, err
			}

			return &toolResult{
				Content:           []content{text(fmt.Sprintf("%d shield rules on %s.", len(rules), site.Domain))},
				StructuredContent: map[string]any{"shields": rules},
			}, nil
		},
	}
}

// shieldArgs is the add_shield_rule tool's arguments.
type shieldArgs struct {
	SiteID string `json:"site_id"`
	Type   string `json:"type"`
	Value  string `json:"value"`
}

// addShieldRuleTool blocks something from being counted.
func (s *Server) addShieldRuleTool() *Tool {
	return &Tool{
		Name:        "add_shield_rule",
		Title:       "Add shield rule",
		Description: "Stop counting traffic that matches a rule. This changes future numbers only; it does not remove traffic already counted.",
		InputSchema: object(map[string]any{
			"site_id": siteArg(),
			"type":    enum("What kind of thing to block.", "ip", "country", "page", "hostname"),
			"value":   str("The address, two-letter country code, path or hostname to block."),
		}, "site_id", "type", "value"),
		Handler: func(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
			args := &shieldArgs{}
			if err := decodeArgs(raw, args); err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			site, err := s.API.SiteFor(key, args.SiteID)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			switch args.Type {
			case "ip", "country", "page", "hostname":
			default:
				return toolFailure("type must be ip, country, page or hostname, not %q", args.Type), nil
			}

			if strings.TrimSpace(args.Value) == "" {
				return toolFailure("value is required"), nil
			}

			if s.API.Shields == nil {
				return toolFailure("%s", publicapi.Unavailable("shield rules")), nil
			}

			rule, err := s.API.Shields.AddShieldRule(ctx, site.ID, publicapi.ShieldRule{Type: args.Type, Value: args.Value})
			if err != nil {
				return nil, err
			}

			return &toolResult{
				Content:           []content{text("Blocked " + rule.Type + " " + rule.Value + ".")},
				StructuredContent: rule,
			}, nil
		},
	}
}

// annotationArgs is the create_annotation tool's arguments.
type annotationArgs struct {
	SiteID string `json:"site_id"`
	Date   string `json:"date"`
	Note   string `json:"note"`
}

// createAnnotationTool pins a note to a date.
//
// This is the tool that closes the loop on the others: an assistant that has
// just worked out why traffic moved should be able to write the answer onto the
// chart, so the next person to look does not have to work it out again.
func (s *Server) createAnnotationTool() *Tool {
	return &Tool{
		Name:        "create_annotation",
		Title:       "Create annotation",
		Description: "Pin a note to a date on this site's charts — a release, a campaign launch, an outage.",
		InputSchema: object(map[string]any{
			"site_id": siteArg(),
			"date":    str("The day the note belongs to, as YYYY-MM-DD."),
			"note":    str("What happened."),
		}, "site_id", "date", "note"),
		Handler: func(ctx context.Context, key *apikeys.Key, raw json.RawMessage) (*toolResult, error) {
			args := &annotationArgs{}
			if err := decodeArgs(raw, args); err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			site, err := s.API.SiteFor(key, args.SiteID)
			if err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			if err := validateDate(args.Date); err != nil {
				return toolFailure("%s", err.Error()), nil
			}

			if strings.TrimSpace(args.Note) == "" {
				return toolFailure("note is required"), nil
			}

			if s.API.Annotations == nil {
				return toolFailure("%s", publicapi.Unavailable("annotations")), nil
			}

			created, err := s.API.Annotations.CreateAnnotation(ctx, site.ID,
				publicapi.Annotation{Date: args.Date, Note: args.Note})
			if err != nil {
				return nil, err
			}

			return &toolResult{
				Content:           []content{text("Noted " + created.Date + ": " + created.Note)},
				StructuredContent: created,
			}, nil
		},
	}
}
