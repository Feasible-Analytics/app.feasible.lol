//
// Schema.js
// The fields the connector offers, and what Looker Studio is told about each one.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// The semantics on these fields are the whole reason a geo map, a time series
// and a percentage render correctly the moment somebody drops a field on a
// chart. Getting one wrong does not raise an error — it produces a chart that
// draws, and is wrong. Each one is set from what the API actually returns
// rather than from what the field is called.

// DIMENSION_FIELDS is every dimension, in the order they appear in the field
// picker.
//
// `dimension` is the API's fully-qualified name and `key` is what comes back in
// a breakdown row: the API drops the scope prefix on the way out, so
// `visit:source` is returned as `source` and `event:name` as `name`. Keeping
// both here is what stops the mapping being guessed at the point of use.
var DIMENSION_FIELDS = [
  {
    name: 'date',
    label: 'Date',
    description: 'The day the visit happened, in the site\'s timezone.',
    dimension: 'time:day',
    key: 'date',
    dataType: 'STRING',
    // Looker Studio only treats a dimension as a date — sorting it
    // chronologically, filling gaps, offering date comparisons — when it is
    // declared YEAR_MONTH_DAY and given YYYYMMDD.
    semanticType: 'YEAR_MONTH_DAY',
    group: 'Time'
  },
  {
    name: 'page',
    label: 'Page',
    description: 'The path of the page the event happened on.',
    dimension: 'event:page',
    key: 'page',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Page'
  },
  {
    name: 'hostname',
    label: 'Hostname',
    description: 'The host the page was served from.',
    dimension: 'event:hostname',
    key: 'hostname',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Page'
  },
  {
    name: 'page_title',
    label: 'Page title',
    description: 'The title of the page as the browser reported it.',
    dimension: 'event:page_title',
    key: 'page_title',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Page'
  },
  {
    name: 'event_name',
    label: 'Event name',
    description: 'The event\'s name. Pageviews are reported as "pageview".',
    dimension: 'event:name',
    key: 'name',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Page'
  },
  {
    name: 'entry_page',
    label: 'Entry page',
    description: 'The first page of the visit.',
    dimension: 'visit:entry_page',
    key: 'entry_page',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Page'
  },
  {
    name: 'exit_page',
    label: 'Exit page',
    description: 'The last page of the visit.',
    dimension: 'visit:exit_page',
    key: 'exit_page',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Page'
  },
  {
    name: 'referrer',
    label: 'Referrer',
    description: 'The full referrer the visit arrived from.',
    dimension: 'visit:referrer',
    key: 'referrer',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Acquisition'
  },
  {
    name: 'source',
    label: 'Source',
    description: 'The referrer reduced to a source name, with any utm_source taking precedence.',
    dimension: 'visit:source',
    key: 'source',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Acquisition'
  },
  {
    name: 'channel',
    label: 'Channel',
    description: 'The acquisition channel the visit is grouped into.',
    dimension: 'visit:channel',
    key: 'channel',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Acquisition'
  },
  {
    name: 'utm_source',
    label: 'UTM source',
    description: 'The utm_source on the landing URL.',
    dimension: 'visit:utm_source',
    key: 'utm_source',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Acquisition'
  },
  {
    name: 'utm_medium',
    label: 'UTM medium',
    description: 'The utm_medium on the landing URL.',
    dimension: 'visit:utm_medium',
    key: 'utm_medium',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Acquisition'
  },
  {
    name: 'utm_campaign',
    label: 'UTM campaign',
    description: 'The utm_campaign on the landing URL.',
    dimension: 'visit:utm_campaign',
    key: 'utm_campaign',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Acquisition'
  },
  {
    name: 'country',
    label: 'Country',
    description: 'The visitor\'s country, as an ISO 3166-1 alpha-2 code.',
    dimension: 'visit:country',
    key: 'country',
    dataType: 'STRING',
    // COUNTRY_CODE rather than COUNTRY because the API returns "US", not
    // "United States". Declaring COUNTRY would leave a geo map blank, since
    // Looker Studio would be matching a code against a list of names.
    semanticType: 'COUNTRY_CODE',
    group: 'Location'
  },
  {
    name: 'region',
    label: 'Region',
    description: 'The visitor\'s first-level region.',
    dimension: 'visit:region',
    key: 'region',
    dataType: 'STRING',
    // The geo database supplies an ISO 3166-2 code where it has one and an
    // English name where it does not, so neither REGION nor REGION_CODE fits
    // every row. REGION is the tolerant one, and the rows it cannot place are
    // still readable in a table.
    semanticType: 'REGION',
    group: 'Location'
  },
  {
    name: 'city',
    label: 'City',
    description: 'The visitor\'s city, by its English name.',
    dimension: 'visit:city',
    key: 'city',
    dataType: 'STRING',
    semanticType: 'CITY',
    group: 'Location'
  },
  {
    name: 'device',
    label: 'Device',
    description: 'Desktop, tablet or mobile, inferred from the viewport.',
    dimension: 'visit:device',
    key: 'device',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Technology'
  },
  {
    name: 'screen_size',
    label: 'Screen size',
    description: 'The viewport size bucket the visit fell into.',
    dimension: 'visit:screen',
    key: 'screen',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Technology'
  },
  {
    name: 'browser',
    label: 'Browser',
    description: 'The visitor\'s browser.',
    dimension: 'visit:browser',
    key: 'browser',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Technology'
  },
  {
    name: 'browser_version',
    label: 'Browser version',
    description: 'The visitor\'s browser version.',
    dimension: 'visit:browser_version',
    key: 'browser_version',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Technology'
  },
  {
    name: 'os',
    label: 'Operating system',
    description: 'The visitor\'s operating system.',
    dimension: 'visit:os',
    key: 'os',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Technology'
  },
  {
    name: 'os_version',
    label: 'OS version',
    description: 'The visitor\'s operating system version.',
    dimension: 'visit:os_version',
    key: 'os_version',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Technology'
  },
  {
    name: 'language',
    label: 'Language',
    description: 'The browser\'s preferred language.',
    dimension: 'visit:language',
    key: 'language',
    dataType: 'STRING',
    semanticType: 'TEXT',
    group: 'Technology'
  }
];

// METRIC_FIELDS is every metric.
//
// `scale` converts what the API returns into what the semantic type means. The
// API reports a rate as 0–100 and Looker Studio's PERCENT renders a fraction as
// a percentage, so a bounce rate sent through unscaled would read 6,520%.
//
// `isReaggregatable` says whether Looker Studio may add a metric up across rows
// it did not ask the API for. Counts may; unique visitors, averages and rates
// may not — summing a bounce rate over twelve months produces a number with no
// meaning, and Looker will happily draw it.
var METRIC_FIELDS = [
  {
    name: 'visitors',
    label: 'Visitors',
    description: 'Unique visitors.',
    dataType: 'NUMBER',
    semanticType: 'NUMBER',
    isReaggregatable: false,
    scale: 1
  },
  {
    name: 'visits',
    label: 'Visits',
    description: 'Sessions, ended after 30 minutes of inactivity.',
    dataType: 'NUMBER',
    semanticType: 'NUMBER',
    isReaggregatable: true,
    scale: 1
  },
  {
    name: 'pageviews',
    label: 'Pageviews',
    description: 'Pageview events.',
    dataType: 'NUMBER',
    semanticType: 'NUMBER',
    isReaggregatable: true,
    scale: 1
  },
  {
    name: 'events',
    label: 'Events',
    description: 'All events, pageviews and custom events alike.',
    dataType: 'NUMBER',
    semanticType: 'NUMBER',
    isReaggregatable: true,
    scale: 1
  },
  {
    name: 'bounce_rate',
    label: 'Bounce rate',
    description: 'Visits that left without a second interaction.',
    dataType: 'NUMBER',
    semanticType: 'PERCENT',
    isReaggregatable: false,
    scale: 0.01
  },
  {
    name: 'visit_duration',
    label: 'Visit duration',
    description: 'Average length of a visit, in seconds. Bounces count as zero.',
    dataType: 'NUMBER',
    semanticType: 'DURATION',
    isReaggregatable: false,
    scale: 1
  },
  {
    name: 'views_per_visit',
    label: 'Views per visit',
    description: 'Average pageviews per visit.',
    dataType: 'NUMBER',
    semanticType: 'NUMBER',
    isReaggregatable: false,
    scale: 1
  },
  {
    name: 'time_on_page',
    label: 'Time on page',
    description: 'Average engaged time on a page, in seconds.',
    dataType: 'NUMBER',
    semanticType: 'DURATION',
    isReaggregatable: false,
    scale: 1
  },
  {
    name: 'scroll_depth',
    label: 'Scroll depth',
    description: 'Average furthest point reached down the page.',
    dataType: 'NUMBER',
    semanticType: 'PERCENT',
    isReaggregatable: false,
    scale: 0.01
  },
  {
    name: 'exit_rate',
    label: 'Exit rate',
    description: 'Pageviews on which the visit ended.',
    dataType: 'NUMBER',
    semanticType: 'PERCENT',
    isReaggregatable: false,
    scale: 0.01
  },
  {
    name: 'conversion_rate',
    label: 'Conversion rate',
    description: 'Visits that converted.',
    dataType: 'NUMBER',
    semanticType: 'PERCENT',
    isReaggregatable: false,
    scale: 0.01
  }
];

// fieldByName resolves a field Looker Studio asked for. It returns null rather
// than throwing so the caller can name the field in a sentence a person can act
// on — a field can survive in a saved report after the connector stopped
// offering it.
function fieldByName(name) {
  var i;

  for (i = 0; i < DIMENSION_FIELDS.length; i++) {
    if (DIMENSION_FIELDS[i].name === name) {
      return DIMENSION_FIELDS[i];
    }
  }

  for (i = 0; i < METRIC_FIELDS.length; i++) {
    if (METRIC_FIELDS[i].name === name) {
      return METRIC_FIELDS[i];
    }
  }

  return null;
}

// isDimension reports whether a resolved field groups rows rather than counting
// them. Dimensions carry an API dimension name and metrics do not, so the
// distinction is a property of the table rather than a second list to keep in
// step with it.
function isDimension(field) {
  return !!field && !!field.dimension;
}

// apiDimensionFor maps a Looker Studio field name onto the API dimension a
// filter has to name. It answers the empty string for a metric or an unknown
// field, which is how the filter translation knows a predicate cannot be pushed
// down.
function apiDimensionFor(name) {
  var field = fieldByName(name);

  if (!isDimension(field) || field.name === 'date') {
    return '';
  }

  return field.dimension;
}

// lookerField renders one of our field definitions as the object Looker Studio
// expects in a schema.
function lookerField(field) {
  return {
    name: field.name,
    label: field.label,
    description: field.description,
    dataType: field.dataType,
    group: field.group || 'Metrics',
    semantics: {
      conceptType: isDimension(field) ? 'DIMENSION' : 'METRIC',
      semanticType: field.semanticType,
      isReaggregatable: isDimension(field) ? false : field.isReaggregatable
    }
  };
}

// getSchema is the Looker Studio entry point. It returns every field regardless
// of configuration: the field set does not depend on the site, and a schema that
// changed with the configuration would invalidate saved reports whenever
// somebody edited the data source.
function getSchema(request) {
  var schema = [];
  var i;

  for (i = 0; i < DIMENSION_FIELDS.length; i++) {
    schema.push(lookerField(DIMENSION_FIELDS[i]));
  }

  for (i = 0; i < METRIC_FIELDS.length; i++) {
    schema.push(lookerField(METRIC_FIELDS[i]));
  }

  return {schema: schema};
}

// See the note at the foot of Params.js: the guard is what lets the Node test
// suite read this table without an Apps Script runtime.
if (typeof module !== 'undefined') {
  module.exports = {
    DIMENSION_FIELDS: DIMENSION_FIELDS,
    METRIC_FIELDS: METRIC_FIELDS,
    fieldByName: fieldByName,
    isDimension: isDimension,
    apiDimensionFor: apiDimensionFor,
    lookerField: lookerField,
    getSchema: getSchema
  };
}
