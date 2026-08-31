//
// Data.js
// One Looker Studio request, one Stats API call, one table of rows back.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// getData answers one chart.
//
// The shape of the whole function follows from one constraint: a chart must cost
// one API call. Looker Studio calls this once per chart, the API counts every
// call against an hourly limit, and a connector that made two calls per chart
// would halve the size of dashboard somebody could build.
function getData(request) {
  var config = readConfig(request.configParams);
  var apiKey = storedApiKey();
  var fields = resolveFields(request.fields);
  var dimensionNames = [];
  var metricNames = [];
  var i;

  for (i = 0; i < fields.length; i++) {
    if (isDimension(fields[i])) {
      dimensionNames.push(fields[i].name);
    } else {
      metricNames.push(fields[i].name);
    }
  }

  var plan = planQuery(dimensionNames);

  if (plan.error) {
    throwUserError(plan.error);
  }

  var range = lookerDateRange(request.dateRange);

  if (range.error) {
    throwUserError(range.error);
  }

  var filters = translateFilters(request.dimensionsFilters, apiDimensionFor);

  var params = buildStatsParams({
    siteId: config.siteId,
    metrics: metricNames,
    endpoint: plan.endpoint,
    property: plan.endpoint === 'breakdown' ? fieldByName(plan.dimensionField).dimension : '',
    startDate: range.startDate,
    endDate: range.endDate,
    filters: filters.filters,
    sample: !!(request.scriptParams && request.scriptParams.sampleExtraction)
  });

  var body = cachedStatsRequest(config.host, endpointPath(plan.endpoint), params, apiKey);
  var payload = parsePayload(body);

  return {
    schema: schemaFor(fields),
    rows: buildRows(plan, fields, payload),
    // Only claimed when every predicate went down the wire. Claiming it after
    // skipping one would leave Looker Studio trusting rows that were never
    // filtered, and the chart would be wrong in a way nothing on the page shows.
    filtersApplied: filters.applied
  };
}

// resolveFields turns the names Looker Studio asked for into our field
// definitions, naming any it does not recognise.
//
// A saved report can outlive a field — somebody duplicates a data source, or a
// release drops a dimension — and "unknown field foo" is a sentence they can act
// on where a crash inside the row builder is not.
function resolveFields(requested) {
  var list = requested || [];
  var fields = [];
  var i;

  for (i = 0; i < list.length; i++) {
    var field = fieldByName(list[i].name);

    if (!field) {
      throwUserError('This chart asks for a field called "' + list[i].name +
        '", which Feasible does not report. Remove it from the chart.');
    }

    fields.push(field);
  }

  if (fields.length === 0) {
    throwUserError('This chart asks for no fields at all.');
  }

  return fields;
}

// schemaFor is the schema of the answer: the requested fields, in the order they
// were requested, because that is the order the row values are in.
function schemaFor(fields) {
  var schema = [];
  var i;

  for (i = 0; i < fields.length; i++) {
    schema.push(lookerField(fields[i]));
  }

  return schema;
}

// parsePayload reads the response body.
//
// A body that is not JSON means something between here and the API answered
// instead of it — a proxy's error page, a captive portal, a login redirect — and
// saying so points at the right place, where a JSON parse error names a column
// number in a document nobody will look at.
function parsePayload(body) {
  try {
    return JSON.parse(body);
  } catch (error) {
    throwUserError('Feasible answered with something that is not JSON. Check that the analytics host points at your' +
      ' Feasible instance and not at a proxy or login page.');
  }

  return {};
}

// buildRows turns the API's answer into Looker Studio's rows.
//
// The three endpoints return three shapes — an object of totals, a row per day,
// a row per dimension value — and each row's values must line up with the
// requested field order, not with the order the API happened to use.
function buildRows(plan, fields, payload) {
  var results = payload.results;
  var rows = [];
  var i;

  if (plan.endpoint === 'aggregate') {
    rows.push({values: aggregateValues(fields, results || {})});

    return rows;
  }

  var list = results || [];

  for (i = 0; i < list.length; i++) {
    rows.push({values: rowValues(fields, list[i])});
  }

  return rows;
}

// aggregateValues reads the totals response, which is keyed by metric name and
// wraps each number in an object carrying its change against a comparison
// period we never ask for.
function aggregateValues(fields, results) {
  var values = [];
  var i;

  for (i = 0; i < fields.length; i++) {
    var entry = results[fields[i].name];

    values.push(scaleMetric(fields[i], entry ? entry.value : 0));
  }

  return values;
}

// rowValues reads one timeseries or breakdown row.
function rowValues(fields, row) {
  var values = [];
  var i;

  for (i = 0; i < fields.length; i++) {
    var field = fields[i];

    if (isDimension(field)) {
      values.push(dimensionValue(field, row));
    } else {
      values.push(scaleMetric(field, row[field.name]));
    }
  }

  return values;
}

// dimensionValue reads a dimension out of a row.
//
// The API returns a breakdown's dimension under its short name — `visit:source`
// comes back as `source` — which is why every dimension in the schema carries the
// key it is returned under rather than deriving it here.
function dimensionValue(field, row) {
  if (field.name === 'date') {
    return lookerDate(row.date);
  }

  var value = row[field.key];

  if (value === undefined || value === null) {
    return '';
  }

  return String(value);
}

// scaleMetric converts an API number into the units its semantic type means.
//
// The API reports a rate as 0–100 and Looker Studio's PERCENT type renders a
// fraction, so the scale on the field definition is the one place that
// difference is applied. A missing metric reads as zero rather than as null: a
// period with no traffic should flatten a chart, not put a gap in it.
function scaleMetric(field, value) {
  var number = Number(value);

  if (!isFinite(number)) {
    return 0;
  }

  return number * field.scale;
}
