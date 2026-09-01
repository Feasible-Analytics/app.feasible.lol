//
// resources.go
// Each site's schema, so a model reads what exists instead of guessing.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/publicapi"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// A model that has to guess a dimension name will guess "utm_campaign" or
// "referrer_source" or "country_code" and get an error, then guess again. Every
// one of those round trips costs the person waiting, and some of them end with
// the model deciding the data does not exist.
//
// So each site publishes its own schema: the metrics it can count, the
// dimensions it can group by, the goals it has and — the part nothing else can
// tell you — the custom properties this particular site actually reports.

// schemaURIPrefix and schemaURISuffix bracket a site's schema URI.
const (
	schemaURIPrefix = "feasible://site/"
	schemaURISuffix = "/schema"
)

// schemaURI is where one site's schema lives.
func schemaURI(domain string) string {
	return schemaURIPrefix + domain + schemaURISuffix
}

// siteFromURI reads the domain back out of a schema URI.
func siteFromURI(uri string) (string, bool) {
	rest, ok := strings.CutPrefix(uri, schemaURIPrefix)
	if !ok {
		return "", false
	}

	domain, ok := strings.CutSuffix(rest, schemaURISuffix)
	if !ok || domain == "" {
		return "", false
	}

	return domain, true
}

// resourceTemplates describes the URI shape, so a client that wants to build one
// itself can, without having to list every site first.
func resourceTemplates() []map[string]any {
	return []map[string]any{{
		"uriTemplate": schemaURIPrefix + "{domain}" + schemaURISuffix,
		"name":        "site-schema",
		"title":       "Site schema",
		"description": "What one site can be asked for: its metrics, dimensions, goals and custom properties.",
		"mimeType":    "application/json",
	}}
}

// listResources enumerates one schema per site the credential can see.
func (s *Server) listResources(_ context.Context, key *apikeys.Key, request *rpcRequest) *rpcResponse {
	if refusal := authorizeScope(key, resourceScopes["resources/list"], teams.PermViewDashboard); refusal != "" {
		return failure(request.ID, codeUnauthorized, "%s", refusal)
	}

	list := s.API.SitesFor(key)
	resources := make([]map[string]any, 0, len(list))

	for _, site := range list {
		resources = append(resources, map[string]any{
			"uri":         schemaURI(site.Domain),
			"name":        site.Domain + " schema",
			"title":       site.Domain,
			"description": "Metrics, dimensions, goals and custom properties available for " + site.Domain + ".",
			"mimeType":    "application/json",
		})
	}

	return result(request.ID, map[string]any{"resources": resources})
}

// readResourceParams is the body of resources/read.
type readResourceParams struct {
	URI string `json:"uri"`
}

// siteSchema is what a schema resource contains.
type siteSchema struct {
	SiteID   string `json:"site_id"`
	Timezone string `json:"timezone"`

	// Metrics and Dimensions come from the engine's own registries rather than
	// from a list written here, so a metric added to the product appears in the
	// schema the same day rather than the day somebody remembers this file.
	Metrics    []string `json:"metrics"`
	Dimensions []string `json:"dimensions"`

	// PropertyDimensions are the event:props:<key> names this site can actually
	// be grouped by. They are the reason this resource exists: nothing in the
	// generic documentation can tell a model that this site reports `plan` and
	// `signup_source` and nothing else.
	PropertyDimensions []string `json:"property_dimensions"`

	// PropertyAggregates are the aggregates a property above can be wrapped in
	// to make a metric — sum(event:props:seats) and the rest. They belong
	// beside the property names rather than in the metric list, because the
	// two only mean anything together.
	PropertyAggregates []string `json:"property_aggregates"`

	Goals []publicapi.Goal `json:"goals"`

	// GoalsAvailable says whether the goals list is empty because there are
	// none or because the feature is not in this build. An empty list with no
	// explanation would have a model confidently reporting that a site tracks
	// no conversions.
	GoalsAvailable bool `json:"goals_available"`

	DateRangePresets []string `json:"date_range_presets"`
	FilterOperators  []string `json:"filter_operators"`

	Notes []string `json:"notes"`
}

// readResource answers one schema.
func (s *Server) readResource(ctx context.Context, key *apikeys.Key, request *rpcRequest) *rpcResponse {
	if refusal := authorizeScope(key, resourceScopes["resources/read"], teams.PermViewDashboard); refusal != "" {
		return failure(request.ID, codeUnauthorized, "%s", refusal)
	}

	var params readResourceParams
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return failure(request.ID, codeInvalidParams, "resources/read needs a uri")
	}

	domain, ok := siteFromURI(params.URI)
	if !ok {
		return failure(request.ID, codeInvalidParams,
			"no resource at %q — schemas live at %s<domain>%s", params.URI, schemaURIPrefix, schemaURISuffix)
	}

	site, err := s.API.SiteFor(key, domain)
	if err != nil {
		return failure(request.ID, codeUnauthorized, "%s is not available to this credential", domain)
	}

	schema := siteSchema{
		SiteID:             site.Domain,
		Timezone:           site.Timezone,
		Metrics:            query.MetricNames(),
		Dimensions:         query.DimensionNames(),
		PropertyAggregates: query.AggregateNames(),
		Goals:              []publicapi.Goal{},
		DateRangePresets:   presets(),
		FilterOperators: []string{
			query.OpIs, query.OpIsNot, query.OpContains, query.OpContainsNot,
			query.OpMatches, query.OpMatchesNot, query.OpHasDone,
		},
		Notes: []string{
			"Every date is bucketed in the site's own timezone, " + site.Timezone + ", not the caller's.",
			"Bot traffic and imported data are excluded unless the query asks for them.",
			"A session-scoped metric such as bounce_rate grouped by an event-scoped dimension such as event:page is answered at the entry page, and the response says so in meta.metric_warnings.",
		},
	}

	properties, err := s.API.AllowedProperties(ctx, site.ID)
	if err != nil {
		return failure(request.ID, codeInternalError, "the site's properties could not be read")
	}

	for _, property := range properties {
		schema.PropertyDimensions = append(schema.PropertyDimensions, "event:props:"+property.Key)
	}

	if s.API.Goals != nil {
		schema.GoalsAvailable = true

		goals, err := s.API.Goals.ListGoals(ctx, site.ID)
		if err == nil {
			schema.Goals = goals
		}
	} else {
		schema.Notes = append(schema.Notes,
			"Goals are not available on this build, so the goals list is empty for that reason rather than because the site counts none.")
	}

	encoded, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return failure(request.ID, codeInternalError, "the schema could not be encoded")
	}

	return result(request.ID, map[string]any{
		"contents": []map[string]any{{
			"uri":      params.URI,
			"name":     site.Domain + " schema",
			"mimeType": "application/json",
			"text":     string(encoded),
		}},
	})
}

// presets lists the date-range names a query accepts. It is written out rather
// than derived because the engine keeps them as separate constants and a
// registry for ten strings would be more machinery than the strings.
func presets() []string {
	return []string{
		query.RangeDay, query.RangeLast7Days, query.RangeLast28Days, query.RangeLast91Days,
		query.RangeMonth, query.RangeLastMonth, query.RangeYear, query.RangeLast12Months,
		query.RangeAll, query.RangeLast24Hours, query.RangeRealtime, query.RangeLast5Minutes,
	}
}
