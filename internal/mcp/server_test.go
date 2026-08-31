//
// server_test.go
// Dispatch, the tools, the resources and the prompts, end to end.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/access"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/publicapi"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// TestInitializeAnnouncesEverythingItHas checks the handshake. A capability that
// is absent means "not supported", so a client that reads an omitted tools
// capability will never call tools/list — and the server would look empty.
func TestInitializeAnnouncesEverythingItHas(t *testing.T) {
	f := newFixture(t)

	response := f.call(t, "initialize", `{"protocolVersion":"`+ProtocolVersion+`","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`)

	answer, ok := response.Result.(initializeResult)
	if !ok {
		t.Fatalf("initialize returned %T", response.Result)
	}

	if answer.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocolVersion = %q", answer.ProtocolVersion)
	}

	if answer.Capabilities.Tools == nil || answer.Capabilities.Resources == nil || answer.Capabilities.Prompts == nil {
		t.Fatalf("capabilities = %+v, want tools, resources and prompts all present", answer.Capabilities)
	}

	if answer.Instructions == "" {
		t.Error("a server with no instructions leaves a model guessing at dimension names")
	}
}

// TestUnknownMethodIsAProtocolError checks that a mistyped method is refused
// with the specification's own code rather than silently ignored.
func TestUnknownMethodIsAProtocolError(t *testing.T) {
	f := newFixture(t)

	response := f.Server.Handle(context.Background(), f.Key,
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/invoke"}`))

	if response == nil || response.Error == nil {
		t.Fatalf("response = %+v, want an error", response)
	}

	if response.Error.Code != codeMethodNotFound {
		t.Errorf("code = %d, want %d", response.Error.Code, codeMethodNotFound)
	}
}

// TestALockedAccountIsRefusedEveryMethodThatReadsData covers the second front
// end onto the same numbers. An assistant holding a key for a locked account
// could ask it every question the dashboard had just refused, which made the
// lock a property of one URL prefix rather than of the account.
func TestALockedAccountIsRefusedEveryMethodThatReadsData(t *testing.T) {
	f := newFixture(t)
	f.API.Access.Set(teamID, access.ReasonLifecycle)

	cases := []struct {
		name string
		body string
	}{
		{"a tool call", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query_stats","arguments":{"site_id":"example.com","metrics":["visitors"],"date_range":"7d"}}}`},
		{"listing resources", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`},
		{"reading a resource", `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"feasible://site/example.com/schema"}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := f.Server.Handle(context.Background(), f.Key, []byte(tc.body))

			if response == nil || response.Error == nil {
				t.Fatalf("a locked account got %+v, want a refusal", response)
			}

			// Its own code, not the unauthorized one: reconnecting fixes a bad
			// token and never fixes an unpaid invoice, and a client that cannot
			// tell them apart will loop through its authorisation flow forever.
			if response.Error.Code != codePaymentRequired {
				t.Fatalf("code = %d, want %d", response.Error.Code, codePaymentRequired)
			}

			if !strings.Contains(response.Error.Message, "not currently paying") {
				t.Errorf("the refusal does not say why: %q", response.Error.Message)
			}
			if !strings.Contains(response.Error.Message, "/billing") {
				t.Errorf("the refusal does not say where to go: %q", response.Error.Message)
			}
		})
	}
}

// TestALockedAccountStillCompletesTheHandshake is why the lock is per method
// rather than on the whole connection. A client that cannot finish initialize
// has nowhere to show the reason it failed, so it connects, sees what exists,
// and is told the moment it asks for something real.
func TestALockedAccountStillCompletesTheHandshake(t *testing.T) {
	f := newFixture(t)
	f.API.Access.Set(teamID, access.ReasonLifecycle)

	for _, method := range []string{"initialize", "ping", "tools/list", "resources/templates/list", "prompts/list"} {
		t.Run(method, func(t *testing.T) {
			response := f.Server.Handle(context.Background(), f.Key,
				[]byte(`{"jsonrpc":"2.0","id":1,"method":"`+method+`"}`))

			if response == nil {
				t.Fatalf("%s produced no response", method)
			}
			if response.Error != nil {
				t.Fatalf("%s was refused: %s (code %d)", method, response.Error.Message, response.Error.Code)
			}
		})
	}
}

// TestAPayingAccountIsNotRefused is the case a bug in the check would break for
// everybody, so it is asserted rather than assumed.
func TestAPayingAccountIsNotRefused(t *testing.T) {
	f := newFixture(t)

	if answer := f.tool(t, "list_sites", ""); answer.IsError {
		t.Fatalf("a paying account's tool call failed: %+v", answer)
	}
}

// TestNotificationGetsNoAnswer checks the one rule a client's dispatcher cannot
// recover from: a response it cannot match to anything it sent.
func TestNotificationGetsNoAnswer(t *testing.T) {
	f := newFixture(t)

	if response := f.Server.Handle(context.Background(), f.Key,
		[]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); response != nil {
		t.Fatalf("a notification was answered with %+v", response)
	}
}

// TestToolsListIsCompleteAndDescribed checks that every tool the issue asks for
// exists and carries a schema a model can call it from.
func TestToolsListIsCompleteAndDescribed(t *testing.T) {
	f := newFixture(t)

	response := f.call(t, "tools/list", "")

	listing, ok := response.Result.(map[string]any)
	if !ok {
		t.Fatalf("tools/list returned %T", response.Result)
	}

	tools, ok := listing["tools"].([]map[string]any)
	if !ok {
		t.Fatalf("tools = %T", listing["tools"])
	}

	byName := map[string]map[string]any{}
	for _, tool := range tools {
		byName[tool["name"].(string)] = tool
	}

	// Every tool the milestone names. The incumbent's MCP routes are scaffolded
	// and answer 501; this list is the difference.
	expected := []string{
		"list_sites", "query_stats", "get_realtime_visitors",
		"list_goals", "create_goal",
		"list_funnels", "get_funnel",
		"compare_periods", "explain_traffic_change",
		"create_site", "update_site",
		"list_shields", "add_shield_rule",
		"create_annotation",
	}

	for _, name := range expected {
		tool, present := byName[name]
		if !present {
			t.Errorf("no tool named %s", name)
			continue
		}

		if description, _ := tool["description"].(string); len(description) < 30 {
			t.Errorf("%s has a description a model cannot choose from: %q", name, description)
		}

		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Errorf("%s has no object input schema", name)
		}
	}

	if len(tools) != len(expected) {
		t.Errorf("listed %d tools, expected %d", len(tools), len(expected))
	}
}

// TestQueryStatsRoundTrip is the test that matters most: a tool call reaching
// the real query engine over the real seeded database, and the numbers coming
// back being the ones somebody worked out from the fixture.
func TestQueryStatsRoundTrip(t *testing.T) {
	f := newFixture(t)

	answer := f.tool(t, "query_stats",
		`{"site_id":"example.com","metrics":["visitors","pageviews"],"date_range":"7d"}`)

	if answer.IsError {
		t.Fatalf("query_stats failed: %s", answer.Content[0].Text)
	}

	var result query.Result
	structured(t, answer, &result)

	if len(result.Results) != 1 {
		t.Fatalf("results = %+v, want one aggregate row", result.Results)
	}

	if result.Results[0].Metrics[0] != currentVisitors {
		t.Errorf("visitors = %v, want %d", result.Results[0].Metrics[0], currentVisitors)
	}

	if result.Results[0].Metrics[1] != currentPageviews {
		t.Errorf("pageviews = %v, want %d", result.Results[0].Metrics[1], currentPageviews)
	}

	// The readable rendering carries the same numbers, because a model with no
	// structured-output handling reads only that.
	if !strings.Contains(answer.Content[0].Text, "visitors") {
		t.Errorf("the text rendering does not name the metrics: %q", answer.Content[0].Text)
	}
}

// TestQueryStatsGroupsAndFilters checks the parts of the v2 surface that make
// this one tool rather than a dozen narrow ones.
func TestQueryStatsGroupsAndFilters(t *testing.T) {
	f := newFixture(t)

	answer := f.tool(t, "query_stats", `{
		"site_id":"example.com","metrics":["visitors"],"date_range":"7d",
		"dimensions":["visit:source"],"order_by":[["visitors","desc"]]}`)

	var grouped query.Result
	structured(t, answer, &grouped)

	if len(grouped.Results) != 2 {
		t.Fatalf("got %d groups, want Google and Twitter", len(grouped.Results))
	}

	if grouped.Results[0].Dimensions[0] != "Google" || grouped.Results[0].Metrics[0] != 3 {
		t.Errorf("top group = %+v, want Google with 3", grouped.Results[0])
	}

	filtered := f.tool(t, "query_stats", `{
		"site_id":"example.com","metrics":["visitors"],"date_range":"7d",
		"filters":[["is","visit:source",["Twitter"]]]}`)

	var narrow query.Result
	structured(t, filtered, &narrow)

	if narrow.Results[0].Metrics[0] != 2 {
		t.Errorf("filtered visitors = %v, want 2 — the filter did not narrow anything", narrow.Results[0].Metrics[0])
	}
}

// TestToolArgumentMistakesComeBackAsReadableFailures checks the contract that
// keeps a model from getting stuck.
//
// A tool that fails because of what it was asked comes back as a *successful*
// response carrying isError, so the model reads the reason and corrects itself.
// A protocol error would be invisible to it and the conversation would stall.
func TestToolArgumentMistakesComeBackAsReadableFailures(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name string
		args string
	}{
		{"a site this key cannot see", `{"site_id":"notyours.com","metrics":["visitors"]}`},
		{"a site that does not exist", `{"site_id":"nowhere.example","metrics":["visitors"]}`},
		{"no site at all", `{"metrics":["visitors"]}`},
		{"no metrics", `{"site_id":"example.com","metrics":[]}`},
		{"a metric that does not exist", `{"site_id":"example.com","metrics":["vistors"]}`},
		{"a dimension that does not exist", `{"site_id":"example.com","metrics":["visitors"],"dimensions":["visit:planet"]}`},
		{"an argument name a model invented", `{"site_id":"example.com","metrics":["visitors"],"group_by":["visit:source"]}`},
		{"a metric where a list belongs", `{"site_id":"example.com","metrics":"visitors"}`},
		{"a comparison mode nobody has", `{"site_id":"example.com","metrics":["visitors"],"compare":"last_tuesday"}`},
		{"a date range nobody has", `{"site_id":"example.com","metrics":["visitors"],"date_range":"fortnight"}`},
		{"a timezone that is not a place", `{"site_id":"example.com","metrics":["visitors"],"timezone":"Mars/Olympus"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answer := f.tool(t, "query_stats", tc.args)

			if !answer.IsError {
				t.Fatalf("the call succeeded: %+v", answer.StructuredContent)
			}

			if len(answer.Content) == 0 || answer.Content[0].Text == "" {
				t.Fatal("a failed tool call must carry a reason the model can read")
			}
		})
	}
}

// TestListSitesShowsOnlyTheKeysOwn checks tenancy on the tool a session starts
// with.
func TestListSitesShowsOnlyTheKeysOwn(t *testing.T) {
	f := newFixture(t)

	answer := f.tool(t, "list_sites", "")

	var listing struct {
		Sites []struct {
			SiteID   string `json:"site_id"`
			Timezone string `json:"timezone"`
		} `json:"sites"`
	}

	structured(t, answer, &listing)

	if len(listing.Sites) != 1 || listing.Sites[0].SiteID != "example.com" {
		t.Fatalf("sites = %+v, want only this key's own", listing.Sites)
	}

	// The timezone is reported because every date in every later answer is
	// bucketed in it, and a model that does not know a site runs on Tokyo time
	// will misread "yesterday" by a day.
	if listing.Sites[0].Timezone == "" {
		t.Error("list_sites must report each site's timezone")
	}
}

// TestRealtimeVisitorsAnswersANumber checks the tool a status page is built on.
func TestRealtimeVisitorsAnswersANumber(t *testing.T) {
	f := newFixture(t)

	answer := f.tool(t, "get_realtime_visitors", `{"site_id":"example.com"}`)
	if answer.IsError {
		t.Fatalf("failed: %s", answer.Content[0].Text)
	}

	var payload struct {
		SiteID   string `json:"site_id"`
		Visitors int64  `json:"visitors"`
	}

	structured(t, answer, &payload)

	if payload.SiteID != "example.com" {
		t.Errorf("site_id = %q", payload.SiteID)
	}
}

// TestComparePeriodsCarriesBothNumbers checks that the comparison reaches the
// engine and comes back with the earlier period attached.
func TestComparePeriodsCarriesBothNumbers(t *testing.T) {
	f := newFixture(t)

	answer := f.tool(t, "compare_periods",
		`{"site_id":"example.com","metrics":["visitors"],"date_range":"7d","compare":"previous_period"}`)
	if answer.IsError {
		t.Fatalf("failed: %s", answer.Content[0].Text)
	}

	var result query.Result
	structured(t, answer, &result)

	row := result.Results[0]

	if row.Comparison == nil {
		t.Fatal("no comparison came back")
	}

	if row.Metrics[0] != currentVisitors || row.Comparison.Metrics[0] != previousVisitors {
		t.Errorf("now %v against before %v, want %d against %d",
			row.Metrics[0], row.Comparison.Metrics[0], currentVisitors, previousVisitors)
	}

	if len(result.Meta.ComparisonDateRange) != 2 {
		t.Error("the comparison window must be echoed back")
	}
}

// TestCreateAndUpdateSiteThroughTools checks the two write tools, including that
// a site created by an assistant is immediately queryable — the same guarantee
// the HTTP endpoint gives, because they go through the same function.
func TestCreateAndUpdateSiteThroughTools(t *testing.T) {
	f := newFixture(t)

	answer := f.tool(t, "create_site", `{"domain":"New.Example.ORG","display_name":"New","timezone":"Europe/Berlin"}`)
	if answer.IsError {
		t.Fatalf("create_site failed: %s", answer.Content[0].Text)
	}

	var created struct {
		Domain   string `json:"domain"`
		Timezone string `json:"timezone"`
	}

	structured(t, answer, &created)

	if created.Domain != "new.example.org" {
		t.Errorf("domain = %q, want it normalised", created.Domain)
	}

	if queried := f.tool(t, "query_stats",
		`{"site_id":"new.example.org","metrics":["visitors"],"date_range":"7d"}`); queried.IsError {
		t.Fatalf("the new site was not queryable straight away: %s", queried.Content[0].Text)
	}

	updated := f.tool(t, "update_site", `{"site_id":"new.example.org","display_name":"Renamed"}`)
	if updated.IsError {
		t.Fatalf("update_site failed: %s", updated.Content[0].Text)
	}

	var after struct {
		DisplayName string `json:"display_name"`
		Timezone    string `json:"timezone"`
	}

	structured(t, updated, &after)

	if after.DisplayName != "Renamed" {
		t.Errorf("display_name = %q", after.DisplayName)
	}

	if after.Timezone != "Europe/Berlin" {
		t.Errorf("an unmentioned field was overwritten: timezone = %q", after.Timezone)
	}
}

// TestMissingFeaturesSayWhatTheyAreRatherThanVanishing checks the tools whose
// dependency this build does not carry.
//
// Every one of them exists, validates its arguments and answers with a sentence
// naming the feature. Omitting them would leave a model unable to tell "this
// product cannot do that" from "that tool is not connected yet", and the first
// is a much worse thing to tell somebody.
func TestMissingFeaturesSayWhatTheyAreRatherThanVanishing(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		tool    string
		args    string
		feature string
	}{
		{"list_goals", `{"site_id":"example.com"}`, "goals"},
		{"create_goal", `{"site_id":"example.com","event_name":"Signup"}`, "goals"},
		{"list_funnels", `{"site_id":"example.com"}`, "funnels"},
		{"get_funnel", `{"site_id":"example.com","funnel_id":1}`, "funnels"},
		{"list_shields", `{"site_id":"example.com"}`, "shield"},
		{"add_shield_rule", `{"site_id":"example.com","type":"ip","value":"203.0.113.4"}`, "shield"},
		{"create_annotation", `{"site_id":"example.com","date":"2026-08-30","note":"Launched"}`, "annotations"},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			answer := f.tool(t, tc.tool, tc.args)

			if !answer.IsError {
				t.Fatalf("%s succeeded against a build with no %s", tc.tool, tc.feature)
			}

			message := answer.Content[0].Text

			if !strings.Contains(message, tc.feature) {
				t.Errorf("the refusal does not name the feature: %q", message)
			}

			if !strings.Contains(message, "not available") {
				t.Errorf("the refusal does not say it is a missing feature: %q", message)
			}
		})
	}
}

// TestMissingFeaturesStillCheckTheirArguments checks that a call with bad
// arguments is told so even when the feature behind it is absent — otherwise
// that error would be waiting for the integrator on the day it lands.
func TestMissingFeaturesStillCheckTheirArguments(t *testing.T) {
	f := newFixture(t)

	answer := f.tool(t, "create_goal", `{"site_id":"example.com","event_name":"Signup","page_path":"/thanks"}`)

	if !answer.IsError {
		t.Fatal("a goal naming both an event and a page was accepted")
	}

	if strings.Contains(answer.Content[0].Text, "not available") {
		t.Errorf("the argument mistake was hidden behind the missing feature: %q", answer.Content[0].Text)
	}
}

// TestSiteSchemaResourceTellsAModelWhatExists checks the resource that stops a
// model guessing dimension names.
func TestSiteSchemaResourceTellsAModelWhatExists(t *testing.T) {
	f := newFixture(t)

	listing := f.call(t, "resources/list", "{}")

	resources := listing.Result.(map[string]any)["resources"].([]map[string]any)
	if len(resources) != 1 {
		t.Fatalf("listed %d resources, want one per visible site", len(resources))
	}

	uri := resources[0]["uri"].(string)
	if uri != "feasible://site/example.com/schema" {
		t.Fatalf("uri = %q", uri)
	}

	read := f.call(t, "resources/read", `{"uri":"`+uri+`"}`)

	contents := read.Result.(map[string]any)["contents"].([]map[string]any)

	var schema siteSchema
	if err := json.Unmarshal([]byte(contents[0]["text"].(string)), &schema); err != nil {
		t.Fatal(err)
	}

	if schema.SiteID != "example.com" || schema.Timezone != "UTC" {
		t.Errorf("schema = %+v", schema)
	}

	if len(schema.Metrics) == 0 || len(schema.Dimensions) == 0 {
		t.Fatal("the schema lists no metrics or dimensions")
	}

	// The custom property is the part nothing else can tell a model: no generic
	// documentation knows that this particular site reports `plan`.
	if len(schema.PropertyDimensions) != 1 || schema.PropertyDimensions[0] != "event:props:plan" {
		t.Errorf("property dimensions = %v, want the site's own", schema.PropertyDimensions)
	}

	// An empty goals list has to say which kind of empty it is, or a model will
	// confidently report that this site tracks no conversions.
	if schema.GoalsAvailable {
		t.Error("goals are not wired into this build but the schema claims they are")
	}
}

// TestSchemaResourceIsScopedToTheKey checks tenancy on the resource surface.
func TestSchemaResourceIsScopedToTheKey(t *testing.T) {
	f := newFixture(t)

	response := f.Server.Handle(context.Background(), f.Key,
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"feasible://site/notyours.com/schema"}}`))

	if response.Error == nil {
		t.Fatal("another team's schema was readable")
	}
}

// TestPromptsCarryTheOrderToLookIn checks the saved prompts.
//
// A prompt here is analyst judgement rather than a convenience: it says what to
// rule out first and instructs the model to admit when the data does not support
// a conclusion, which is the part that stops it being confidently wrong.
func TestPromptsCarryTheOrderToLookIn(t *testing.T) {
	f := newFixture(t)

	listing := f.call(t, "prompts/list", "")

	described := listing.Result.(map[string]any)["prompts"].([]map[string]any)

	names := map[string]bool{}
	for _, prompt := range described {
		names[prompt["name"].(string)] = true
	}

	for _, expected := range []string{"weekly_traffic_review", "why_did_traffic_drop", "campaign_performance"} {
		if !names[expected] {
			t.Errorf("no prompt named %s", expected)
		}
	}

	got := f.call(t, "prompts/get", `{"name":"why_did_traffic_drop","arguments":{"site_id":"example.com"}}`)

	messages := got.Result.(map[string]any)["messages"].([]map[string]any)
	body := messages[0]["content"].(content).Text

	if !strings.Contains(body, "example.com") {
		t.Error("the prompt did not carry its argument through")
	}

	if !strings.Contains(body, "explain_traffic_change") {
		t.Error("the drop prompt should send the model to the tool built for the question")
	}

	if !strings.Contains(strings.ToLower(body), "still running") {
		t.Error("the drop prompt should rule out a part-finished period before anything else")
	}
}

// TestPromptRequiresItsArguments checks that a prompt with a missing required
// argument is refused rather than rendered with a hole in it.
func TestPromptRequiresItsArguments(t *testing.T) {
	f := newFixture(t)

	response := f.Server.Handle(context.Background(), f.Key,
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":{"name":"weekly_traffic_review","arguments":{}}}`))

	if response.Error == nil {
		t.Fatal("a prompt rendered without its required argument")
	}
}

// ── The fixture every test in this package runs against ─────────────────────

// testNow is the clock every test resolves its dates against.
var testNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

// The tenant these tests run as.
const (
	teamID = 7
	siteID = 3
)

// fixture builds the server, the credential and the traffic behind them.
type fixture struct {
	Server  *Server
	API     *publicapi.API
	Key     *apikeys.Key
	Raw     string
	Control *sql.DB
	BaseURL string
}

// newFixture assembles everything. It is the real stack rather than a set of
// fakes, because the thing worth proving about this package is that a tool call
// reaches the query engine and comes back with the same number the HTTP API
// would give — and two fakes always agree with each other.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	ctx := context.Background()
	dir := t.TempDir()

	control, err := store.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(ctx, control, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	stamp := testNow.Unix()

	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO teams (id, name, created_at, updated_at) VALUES (?, 'Test', ?, ?)`, []any{teamID, stamp, stamp}},
		{`INSERT INTO teams (id, name, created_at, updated_at) VALUES (8, 'Other', ?, ?)`, []any{stamp, stamp}},
		{`INSERT INTO users (id, email, created_at, updated_at) VALUES (1, 'a@example.test', ?, ?)`, []any{stamp, stamp}},
		{`INSERT INTO sites (id, account_id, domain, display_name, timezone, created_at, updated_at)
		  VALUES (?, ?, 'example.com', 'Example', 'UTC', ?, ?)`, []any{siteID, teamID, stamp, stamp}},
		{`INSERT INTO sites (id, account_id, domain, timezone, created_at, updated_at)
		  VALUES (4, 8, 'notyours.com', 'UTC', ?, ?)`, []any{stamp, stamp}},
		{`INSERT INTO site_custom_properties (site_id, key, created_at) VALUES (?, 'plan', ?)`, []any{siteID, stamp}},
	} {
		if _, err := control.Exec(statement.sql, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.sql, err)
		}
	}

	manager := accounts.NewManager(dir)
	t.Cleanup(func() { manager.CloseAll() })

	account, err := manager.Open(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}

	seed(t, account)

	cache := sites.New(control)
	if err := cache.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	keys := apikeys.NewStore(control)

	key, raw, err := keys.Create(ctx, teamID, 1, "mcp", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// A real gate with nothing in it. Every test starts against an account that
	// is paying; the ones that care lock it with Set.
	api := &publicapi.API{
		Keys:     keys,
		Limiter:  apikeys.NewLimiter(0),
		Access:   access.New(nil, nil, nil, nil),
		Sites:    cache,
		Control:  publicapi.NewControlStore(control),
		Accounts: manager,
		BaseURL:  "https://example.test",
		Now:      func() time.Time { return testNow },
	}

	return &fixture{
		Server:  New(api, nil),
		API:     api,
		Key:     key,
		Raw:     raw,
		Control: control,
		BaseURL: "https://example.test",
	}
}

// visit is one seeded session.
type visit struct {
	day       int
	source    string
	page      string
	pageviews int
}

// traffic is deliberately two whole weeks with one source that stops between
// them.
//
// The source that stops is what makes explain_traffic_change testable: a source
// gone to zero has no rows in the later period at all, so a fixture where every
// source appears in both weeks would pass against an implementation that never
// looks at the earlier one — which is the whole mistake this tool exists not to
// make.
var traffic = []visit{
	{day: 2, source: "Google", page: "/home", pageviews: 2},
	{day: 2, source: "Google", page: "/home", pageviews: 2},
	{day: 2, source: "Google", page: "/home", pageviews: 2},
	{day: 1, source: "Twitter", page: "/pricing", pageviews: 2},
	{day: 1, source: "Twitter", page: "/pricing", pageviews: 2},

	{day: 10, source: "Google", page: "/home", pageviews: 2},
	{day: 10, source: "Google", page: "/home", pageviews: 2},
	{day: 10, source: "Google", page: "/home", pageviews: 2},
	{day: 10, source: "Google", page: "/home", pageviews: 2},
	{day: 10, source: "Google", page: "/home", pageviews: 2},
	{day: 9, source: "Newsletter", page: "/blog", pageviews: 2},
	{day: 9, source: "Newsletter", page: "/blog", pageviews: 2},
	{day: 9, source: "Newsletter", page: "/blog", pageviews: 2},
	{day: 9, source: "Newsletter", page: "/blog", pageviews: 2},
}

// The totals the fixture implies, worked out by hand so a test asserts against
// a number somebody derived rather than whatever the code returned.
const (
	currentVisitors  = 5
	currentPageviews = 10
	previousVisitors = 9
)

// seed writes the traffic into an account database.
func seed(t *testing.T, account *accounts.Account) {
	t.Helper()

	ctx := context.Background()

	pageview, err := account.Intern.ID(ctx, intern.EventName, ingest.EventPageview)
	if err != nil {
		t.Fatal(err)
	}

	for index, entry := range traffic {
		user := int64(1000 + index)
		session := int64(index + 1)
		at := testNow.AddDate(0, 0, -entry.day).Truncate(time.Hour).Unix()

		page, err := account.Intern.ID(ctx, intern.Pathname, entry.page)
		if err != nil {
			t.Fatal(err)
		}

		source, err := account.Intern.ID(ctx, intern.Source, entry.source)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration, is_bounce,
			                      pageviews, events, entry_page_id, exit_page_id, source_id)
			VALUES (?, ?, ?, ?, ?, 60, 0, ?, ?, ?, ?, ?)`,
			session, siteID, user, at, at+60, entry.pageviews, entry.pageviews, page, page, source); err != nil {
			t.Fatal(err)
		}

		for i := 0; i < entry.pageviews; i++ {
			if _, err := account.Writer().ExecContext(ctx, `
				INSERT INTO events (site_id, timestamp, name_id, user_id, session_id, pathname_id, source_id, scroll_depth)
				VALUES (?, ?, ?, ?, ?, ?, ?, 255)`,
				siteID, at+int64(i), pageview, user, session, page, source); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// call sends one JSON-RPC request through the server and returns the response.
func (f *fixture) call(t *testing.T, method, params string) *rpcResponse {
	t.Helper()

	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `"`
	if params != "" {
		body += `,"params":` + params
	}
	body += `}`

	response := f.Server.Handle(context.Background(), f.Key, []byte(body))
	if response == nil {
		t.Fatalf("%s produced no response", method)
	}

	if response.Error != nil {
		t.Fatalf("%s failed: %s (code %d)", method, response.Error.Message, response.Error.Code)
	}

	return response
}

// tool calls one tool and returns its result.
func (f *fixture) tool(t *testing.T, name, args string) *toolResult {
	t.Helper()

	if args == "" {
		args = "{}"
	}

	response := f.call(t, "tools/call", `{"name":"`+name+`","arguments":`+args+`}`)

	answer, ok := response.Result.(*toolResult)
	if !ok {
		t.Fatalf("%s returned %T, not a tool result", name, response.Result)
	}

	return answer
}

// structured re-encodes a tool's structured payload into a typed value, which
// is how a test asserts on numbers rather than on the rendered text.
func structured(t *testing.T, answer *toolResult, target any) {
	t.Helper()

	raw, err := json.Marshal(answer.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("structured content did not decode: %v (%s)", err, string(raw))
	}
}
