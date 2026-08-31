//
// params.test.js
// The connector's pure logic, exercised without a Google runtime.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Nothing here touches Apps Script. Params.js and Schema.js are written to run
// in both places for exactly this reason: the request building, the filter
// translation and the cache key are the parts that can silently be wrong, and
// clasp push followed by clicking through a chart is not a test.

const test = require("node:test");
const assert = require("node:assert");

const params = require("../Params.js");
const schema = require("../Schema.js");

const lookup = schema.apiDimensionFor;

test("planQuery picks the endpoint from the dimensions asked for", () => {
	assert.strictEqual(params.planQuery([]).endpoint, "aggregate");
	assert.strictEqual(params.planQuery(["date"]).endpoint, "timeseries");
	assert.strictEqual(params.planQuery(["source"]).endpoint, "breakdown");
	assert.strictEqual(params.planQuery(["source"]).dimensionField, "source");
});

test("planQuery refuses what the API cannot answer in one call, with a reason", () => {
	const both = params.planQuery(["date", "source"]);

	assert.strictEqual(both.endpoint, "");
	assert.match(both.error, /remove the date or remove source/);

	const two = params.planQuery(["source", "country"]);

	assert.strictEqual(two.endpoint, "");
	assert.match(two.error, /one dimension at a time/);
});

test("buildStatsParams turns a Looker date range into period=custom", () => {
	const built = params.buildStatsParams({
		siteId: "example.com",
		metrics: ["visitors", "pageviews"],
		endpoint: "timeseries",
		startDate: "2026-08-01",
		endDate: "2026-08-30",
	});

	assert.deepStrictEqual(built, {
		site_id: "example.com",
		metrics: "visitors,pageviews",
		period: "custom",
		date: "2026-08-01,2026-08-30",
		interval: "date",
	});
});

test("buildStatsParams asks a breakdown for the API's maximum page size", () => {
	const built = params.buildStatsParams({
		siteId: "example.com",
		metrics: [],
		endpoint: "breakdown",
		property: "visit:source",
		startDate: "2026-08-01",
		endDate: "2026-08-30",
		filters: "visit:country==US",
	});

	assert.strictEqual(built.property, "visit:source");
	assert.strictEqual(built.limit, params.BREAKDOWN_LIMIT);
	assert.strictEqual(built.filters, "visit:country==US");
	// A request with no metric still needs one, or the API answers with rows
	// that carry a dimension and nothing to plot.
	assert.strictEqual(built.metrics, "visitors");
});

test("buildStatsParams shrinks a schema-detection request", () => {
	const built = params.buildStatsParams({
		siteId: "example.com",
		metrics: ["visitors"],
		endpoint: "breakdown",
		property: "visit:source",
		startDate: "2026-08-01",
		endDate: "2026-08-30",
		sample: true,
	});

	assert.strictEqual(built.limit, params.SAMPLE_LIMIT);
});

test("lookerDateRange refuses a range that is not two ISO dates", () => {
	assert.strictEqual(params.lookerDateRange({ startDate: "2026-08-01", endDate: "2026-08-30" }).error, "");
	assert.match(params.lookerDateRange(null).error, /no date range/);
	assert.match(params.lookerDateRange({ startDate: "1 Aug", endDate: "2026-08-30" }).error, /no date range/);
});

test("translateFilters maps the four operators onto API predicates", () => {
	const cases = [
		[{ operator: "EQUALS", type: "INCLUDE" }, "visit:source==Google"],
		[{ operator: "EQUALS", type: "EXCLUDE" }, "visit:source!=Google"],
		[{ operator: "CONTAINS", type: "INCLUDE" }, "visit:source~Google"],
		[{ operator: "CONTAINS", type: "EXCLUDE" }, "visit:source!~Google"],
	];

	for (const [shape, expected] of cases) {
		const filter = Object.assign({ fieldName: "source", values: ["Google"] }, shape);
		const result = params.translateFilters([[filter]], lookup);

		assert.strictEqual(result.filters, expected);
		assert.strictEqual(result.applied, true);
	}
});

test("translateFilters joins an OR group with a pipe and AND groups with a semicolon", () => {
	const result = params.translateFilters(
		[
			[
				{ fieldName: "source", operator: "EQUALS", type: "INCLUDE", values: ["Google"] },
				{ fieldName: "source", operator: "EQUALS", type: "INCLUDE", values: ["Bing"] },
			],
			[{ fieldName: "country", operator: "EQUALS", type: "INCLUDE", values: ["US"] }],
		],
		lookup,
	);

	assert.strictEqual(result.filters, "visit:source==Google|Bing;visit:country==US");
	assert.strictEqual(result.applied, true);
});

test("translateFilters escapes the characters the filter grammar reads as syntax", () => {
	const result = params.translateFilters(
		[[{ fieldName: "page", operator: "CONTAINS", type: "INCLUDE", values: ["/a;b|c=d"] }]],
		lookup,
	);

	assert.strictEqual(result.filters, "event:page~/a\\;b\\|c\\=d");
});

test("translateFilters leaves a predicate it cannot express to Looker Studio", () => {
	const regexp = params.translateFilters(
		[[{ fieldName: "page", operator: "REGEXP_PARTIAL_MATCH", type: "INCLUDE", values: ["^/blog"] }]],
		lookup,
	);

	assert.strictEqual(regexp.filters, "");
	assert.strictEqual(regexp.applied, false);
	assert.deepStrictEqual(regexp.skipped, ["REGEXP_PARTIAL_MATCH on page"]);

	// A metric filter has no dimension to name, and the date is the range
	// rather than a filterable dimension.
	assert.strictEqual(lookup("visitors"), "");
	assert.strictEqual(lookup("date"), "");

	const mixed = params.translateFilters(
		[
			[
				{ fieldName: "source", operator: "EQUALS", type: "INCLUDE", values: ["Google"] },
				{ fieldName: "country", operator: "EQUALS", type: "INCLUDE", values: ["US"] },
			],
		],
		lookup,
	);

	assert.strictEqual(mixed.applied, false);
	assert.deepStrictEqual(mixed.skipped, ["a mixed OR group on source"]);
});

test("translateFilters reports one pushed-down group and one skipped as not applied", () => {
	const result = params.translateFilters(
		[
			[{ fieldName: "source", operator: "EQUALS", type: "INCLUDE", values: ["Google"] }],
			[{ fieldName: "page", operator: "IS_NULL", type: "INCLUDE", values: [] }],
		],
		lookup,
	);

	assert.strictEqual(result.filters, "visit:source==Google");
	assert.strictEqual(result.applied, false);
});

test("cacheKeyInput identifies a call regardless of parameter order", () => {
	const one = params.cacheKeyInput("https://app.feasible.lol", "/api/v1/stats/breakdown", {
		site_id: "example.com",
		metrics: "visitors",
		property: "visit:source",
	});

	const two = params.cacheKeyInput("https://app.feasible.lol", "/api/v1/stats/breakdown", {
		property: "visit:source",
		metrics: "visitors",
		site_id: "example.com",
	});

	assert.strictEqual(one, two);
});

test("cacheKey separates every input that changes the answer", () => {
	const base = { site_id: "example.com", metrics: "visitors", period: "custom", date: "2026-08-01,2026-08-30" };
	const key = params.cacheKey("https://app.feasible.lol", "/api/v1/stats/timeseries", base);
	const seen = {};

	const variants = [
		["https://analytics.example.com", "/api/v1/stats/timeseries", base],
		["https://app.feasible.lol", "/api/v1/stats/aggregate", base],
		["https://app.feasible.lol", "/api/v1/stats/timeseries", Object.assign({}, base, { site_id: "other.com" })],
		["https://app.feasible.lol", "/api/v1/stats/timeseries", Object.assign({}, base, { metrics: "pageviews" })],
		["https://app.feasible.lol", "/api/v1/stats/timeseries", Object.assign({}, base, { date: "2026-07-01,2026-07-31" })],
		["https://app.feasible.lol", "/api/v1/stats/timeseries", Object.assign({}, base, { filters: "visit:country==US" })],
	];

	for (const [host, path, shape] of variants) {
		const other = params.cacheKey(host, path, shape);

		assert.notStrictEqual(other, key);
		assert.strictEqual(seen[other], undefined);

		seen[other] = true;
	}

	// Stable across calls, prefixed with the format version, and short enough
	// for Apps Script's 250-character key limit.
	assert.strictEqual(key, params.cacheKey("https://app.feasible.lol", "/api/v1/stats/timeseries", base));
	assert.strictEqual(key.indexOf(params.CACHE_PREFIX), 0);
	assert.strictEqual(key.length, params.CACHE_PREFIX.length + 16);
});

test("cacheKey never carries the API key", () => {
	const input = params.cacheKeyInput("https://app.feasible.lol", "/api/v1/stats/aggregate", {
		site_id: "example.com",
		metrics: "visitors",
	});

	assert.strictEqual(input.indexOf("Bearer"), -1);
	assert.strictEqual(input.indexOf("apiKey"), -1);
});

test("cacheTtlSeconds is short for a realtime window and long for a closed one", () => {
	assert.strictEqual(params.cacheTtlSeconds({ period: "custom" }), params.CACHE_TTL_SECONDS);
	assert.strictEqual(params.cacheTtlSeconds({ period: "realtime" }), params.REALTIME_TTL_SECONDS);
	assert.strictEqual(params.cacheTtlSeconds(null), params.CACHE_TTL_SECONDS);
});

test("normaliseHost falls back to the hosted service and drops trailing slashes", () => {
	assert.strictEqual(params.normaliseHost(""), params.DEFAULT_HOST);
	assert.strictEqual(params.normaliseHost(null), params.DEFAULT_HOST);
	assert.strictEqual(params.normaliseHost("https://analytics.example.com//"), "https://analytics.example.com");
});

test("parseCredential accepts a key alone or a host and a key", () => {
	assert.deepStrictEqual(params.parseCredential("  fsk_abc123  "), { host: "", key: "fsk_abc123", error: "" });

	assert.deepStrictEqual(params.parseCredential("https://analytics.example.com/ fsk_abc123"), {
		host: "https://analytics.example.com",
		key: "fsk_abc123",
		error: "",
	});

	assert.match(params.parseCredential("").error, /Paste your Feasible API key/);
	assert.match(params.parseCredential("one two three").error, /does not look like an API key/);
});

test("lookerDate strips the dashes a YEAR_MONTH_DAY field cannot have", () => {
	assert.strictEqual(params.lookerDate("2026-08-30"), "20260830");
	assert.strictEqual(params.lookerDate("2026-08-30 14:00"), "20260830");
	assert.strictEqual(params.lookerDate(undefined), "");
});

test("every schema field carries the semantics a chart needs", () => {
	const fields = schema.DIMENSION_FIELDS.concat(schema.METRIC_FIELDS);
	const names = {};

	for (const field of fields) {
		assert.strictEqual(names[field.name], undefined, field.name + " is defined twice");
		names[field.name] = true;

		const rendered = schema.lookerField(field);

		assert.match(rendered.name, /^[a-z][a-z0-9_]*$/);
		assert.ok(rendered.label.length > 0);
		assert.ok(rendered.description.length > 0);
		assert.ok(rendered.semantics.semanticType.length > 0);
		assert.strictEqual(typeof rendered.semantics.isReaggregatable, "boolean");
	}

	assert.strictEqual(schema.DIMENSION_FIELDS.length, 23);
	assert.strictEqual(schema.METRIC_FIELDS.length, 11);
});

test("the schema declares the semantics Looker Studio needs to draw the awkward fields", () => {
	const byName = {};

	for (const field of schema.DIMENSION_FIELDS.concat(schema.METRIC_FIELDS)) {
		byName[field.name] = field;
	}

	// The API returns "US", not "United States", so a COUNTRY field would leave
	// a geo map blank.
	assert.strictEqual(byName.country.semanticType, "COUNTRY_CODE");
	assert.strictEqual(byName.city.semanticType, "CITY");
	assert.strictEqual(byName.date.semanticType, "YEAR_MONTH_DAY");

	// Rates arrive as 0-100 and PERCENT renders a fraction.
	for (const name of ["bounce_rate", "exit_rate", "conversion_rate", "scroll_depth"]) {
		assert.strictEqual(byName[name].semanticType, "PERCENT");
		assert.strictEqual(byName[name].scale, 0.01);
	}

	for (const name of ["visit_duration", "time_on_page"]) {
		assert.strictEqual(byName[name].semanticType, "DURATION");
		assert.strictEqual(byName[name].scale, 1);
	}

	// A count may be added up across rows; a unique count, an average and a
	// rate may not.
	for (const name of ["visits", "pageviews", "events"]) {
		assert.strictEqual(byName[name].isReaggregatable, true);
	}

	for (const name of ["visitors", "bounce_rate", "visit_duration", "views_per_visit"]) {
		assert.strictEqual(byName[name].isReaggregatable, false);
	}
});

test("every dimension names the API dimension and the key it is returned under", () => {
	for (const field of schema.DIMENSION_FIELDS) {
		assert.ok(field.dimension.indexOf(":") > 0, field.name + " has no scoped API dimension");

		const short = field.dimension.substring(field.dimension.lastIndexOf(":") + 1);

		// The API drops the scope prefix on the way out, so the response key is
		// always the last segment of the dimension it came from.
		assert.strictEqual(field.key, field.name === "date" ? "date" : short);
	}
});
