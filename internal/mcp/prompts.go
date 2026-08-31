//
// prompts.go
// The three questions people actually ask, written out properly once.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"encoding/json"
	"strings"
)

// A prompt here is a saved piece of analyst judgement, not a convenience.
// "Why did traffic drop" asked cold gets a model listing every number it can
// reach; asked through this, it gets the order to look in, the traps to check
// first, and an instruction to say when the data does not support a conclusion.
// That last part is the one that matters: an assistant that always produces a
// confident cause is an assistant that is confidently wrong once a week.

// promptDefinition is one saved prompt.
type promptDefinition struct {
	Name        string
	Title       string
	Description string
	Arguments   []promptArgument

	// Build renders the message from the arguments it was given.
	Build func(args map[string]string) string
}

// promptArgument is one input a prompt takes.
type promptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// prompts is the registry.
var prompts = []promptDefinition{
	{
		Name:        "weekly_traffic_review",
		Title:       "Weekly traffic review",
		Description: "A written review of the last seven days against the seven before, of the kind somebody would send to a team on a Monday.",
		Arguments: []promptArgument{
			{Name: "site_id", Description: "The site's domain.", Required: true},
		},
		Build: func(args map[string]string) string {
			site := args["site_id"]

			return strings.Join([]string{
				"Write the weekly traffic review for " + site + ".",
				"",
				"Work in this order:",
				"1. Read " + schemaURI(site) + " so you use dimension and property names that exist on this site.",
				"2. compare_periods for visitors, visits, pageviews, bounce_rate and visit_duration over 7d against previous_period.",
				"3. explain_traffic_change on the same window, so the review says why rather than only what.",
				"4. query_stats over 7d grouped by visit:source and again by event:page, top ten each, to name the specific pages and sources worth mentioning.",
				"",
				"Then write it as prose for people who do not read dashboards:",
				"- Open with the one number that matters and whether it moved.",
				"- Say what caused the movement, naming the source, campaign or page.",
				"- Call out anything that appeared or stopped this week.",
				"- Finish with one thing worth doing next week, or say there is nothing.",
				"",
				"Do not report a cause the breakdowns do not support. If the change is spread thinly across everything, say that — it is a real and useful answer, and it usually means the cause is not in this data.",
			}, "\n")
		},
	},
	{
		Name:        "why_did_traffic_drop",
		Title:       "Why did traffic drop",
		Description: "Investigate a fall in traffic, in the order that rules out measurement problems before chasing marketing explanations.",
		Arguments: []promptArgument{
			{Name: "site_id", Description: "The site's domain.", Required: true},
			{Name: "date_range", Description: "The period that looks wrong — a preset such as 7d, or [\"2026-08-01\",\"2026-08-14\"]. Defaults to 7d.", Required: false},
		},
		Build: func(args map[string]string) string {
			site := args["site_id"]

			window := args["date_range"]
			if window == "" {
				window = "7d"
			}

			return strings.Join([]string{
				"Traffic to " + site + " appears to have fallen over " + window + ". Find out why.",
				"",
				"Start with explain_traffic_change for that window. Read its findings before running anything else — it has already pulled both periods and ranked what accounts for the change, including anything that stopped entirely, which a breakdown of the current period alone cannot show.",
				"",
				"Then rule out the boring causes before the interesting ones, in this order:",
				"1. Is the period still running? A part-finished day compared against a whole one always looks like a fall.",
				"2. Is the fall spread evenly across every page and source? That pattern is a tracking or deployment change, not an audience change. Check whether the drop starts on one exact day by running query_stats grouped by time:day.",
				"3. Did one source, campaign or referrer stop? A single referrer going to zero is usually a link that was removed or a campaign that ended.",
				"4. Did one country, device or browser drop? That is usually a blocker, a consent change or a regional outage.",
				"5. Only then consider seasonality — re-run the comparison with compare set to year_over_year and see whether the same dip happened last year.",
				"",
				"Report what the data supports and what it rules out. Say explicitly if the cause is not visible in this data; the honest answer is more useful than a plausible one.",
			}, "\n")
		},
	},
	{
		Name:        "campaign_performance",
		Title:       "Campaign performance",
		Description: "Compare campaigns on what they brought rather than on how much of it there was.",
		Arguments: []promptArgument{
			{Name: "site_id", Description: "The site's domain.", Required: true},
			{Name: "date_range", Description: "The period to review. Defaults to 28d.", Required: false},
		},
		Build: func(args map[string]string) string {
			site := args["site_id"]

			window := args["date_range"]
			if window == "" {
				window = "28d"
			}

			return strings.Join([]string{
				"Review campaign performance for " + site + " over " + window + ".",
				"",
				"1. Read " + schemaURI(site) + " to see which goals and custom properties this site has.",
				"2. query_stats grouped by visit:utm_campaign, with visitors, visits, bounce_rate and visit_duration, sorted by visitors. Also group by visit:utm_source and visit:utm_medium.",
				"3. If the site has goals, add conversion_rate with a has_done filter for the goal that matters, so the comparison is on outcomes rather than arrivals.",
				"4. compare_periods on the same grouping against previous_period, to separate a campaign that is growing from one that is merely large.",
				"",
				"Judge them on quality, not volume. A campaign sending twice the visitors at a 90% bounce rate and eight seconds on site is worth less than one sending half as many who stay — say so plainly, and name the campaigns on both sides of that line.",
				"",
				"Flag any campaign whose traffic bounces at over 90% with almost no time on site: that is usually click fraud, a bot, or a landing page that does not match the ad.",
			}, "\n")
		},
	},
}

// promptDefinitions renders the list for prompts/list.
func promptDefinitions() []map[string]any {
	described := make([]map[string]any, 0, len(prompts))

	for _, prompt := range prompts {
		described = append(described, map[string]any{
			"name":        prompt.Name,
			"title":       prompt.Title,
			"description": prompt.Description,
			"arguments":   prompt.Arguments,
		})
	}

	return described
}

// getPromptParams is the body of prompts/get.
type getPromptParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments"`
}

// getPrompt renders one prompt with its arguments filled in.
func (s *Server) getPrompt(request *rpcRequest) *rpcResponse {
	var params getPromptParams

	if err := json.Unmarshal(request.Params, &params); err != nil {
		return failure(request.ID, codeInvalidParams, "prompts/get needs a name and an arguments object")
	}

	for _, prompt := range prompts {
		if prompt.Name != params.Name {
			continue
		}

		for _, argument := range prompt.Arguments {
			if argument.Required && params.Arguments[argument.Name] == "" {
				return failure(request.ID, codeInvalidParams,
					"prompt %q needs the argument %q", prompt.Name, argument.Name)
			}
		}

		return result(request.ID, map[string]any{
			"description": prompt.Description,
			"messages": []map[string]any{{
				"role":    "user",
				"content": text(prompt.Build(params.Arguments)),
			}},
		})
	}

	return failure(request.ID, codeInvalidParams,
		"no prompt named %q — the prompts are %s", params.Name, strings.Join(promptNames(), ", "))
}

// promptNames lists what exists, for the error message a mistyped name gets.
func promptNames() []string {
	names := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		names = append(names, prompt.Name)
	}

	return names
}
