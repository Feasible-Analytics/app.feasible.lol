//
// Errors.js
// Turning an API failure into a sentence a person reading a chart can act on.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Looker Studio shows an unhandled exception as "Data Set Configuration Error"
// with a stack trace behind a "show details" link. Nobody looking at a broken
// chart reads that, and it names our file and line numbers rather than what they
// got wrong. A user error is a sentence in the chart itself, which is the only
// place the message will actually be seen.

// throwUserError ends the request with a message the report's reader sees.
//
// The debug text is the same sentence rather than a stack trace: this connector
// has no separate operator, so a message that only an "admin" could see — and
// isAdminUser is always false here — would be a message nobody ever reads.
function throwUserError(message) {
  DataStudioApp.createCommunityConnector()
    .newUserError()
    .setText(message)
    .setDebugText(message)
    .throwException();
}

// getConnectorError reads the sentence out of a failed API response.
//
// Every refusal from the Stats API is `{"error": "a sentence"}`, written for the
// caller and naming the parameter that was wrong. Surfacing that verbatim is
// worth far more than any message we could invent from a status code, so the
// generic fallbacks below only run when the body is not what it should be.
function getConnectorError(response) {
  var status = response.getResponseCode();
  var body = '';

  try {
    body = response.getContentText();
  } catch (error) {
    body = '';
  }

  var parsed = null;

  try {
    parsed = JSON.parse(body);
  } catch (error) {
    parsed = null;
  }

  if (parsed && typeof parsed.error === 'string' && parsed.error !== '') {
    return parsed.error;
  }

  if (status === 401 || status === 403) {
    return 'Feasible rejected the API key. Open the data source, choose Edit Connection and paste a current key.';
  }

  if (status === 404) {
    return 'Feasible answered 404. Check the analytics host in this data source\'s configuration.';
  }

  if (status >= 500) {
    return 'Feasible answered ' + status + '. That is a fault on their side; try refreshing the report in a few minutes.';
  }

  return 'Feasible answered ' + status + ' with no explanation.';
}

// rateLimitMessage says when the limit resets rather than telling somebody to
// try again later.
//
// Retrying inside a Looker Studio request is the wrong shape: a report calls
// getData once per chart, so a connector that sleeps and retries turns one rate
// limit into a dashboard that hangs for a minute and then fails anyway. Naming
// the reset time lets the reader decide.
function rateLimitMessage(response) {
  var headers = normaliseHeaders(response);
  var retryAfter = parseInt(headers['retry-after'], 10);
  var reset = parseInt(headers['x-ratelimit-reset'], 10);
  var seconds = 0;

  if (retryAfter > 0) {
    seconds = retryAfter;
  } else if (reset > 0) {
    seconds = Math.max(0, reset - Math.floor(Date.now() / 1000));
  }

  if (seconds > 0) {
    return 'Feasible\'s API rate limit has been reached. It resets in ' + describeSeconds(seconds) +
      '. A Looker Studio report calls this connector once per chart, so refreshing a large dashboard' +
      ' repeatedly is the usual cause.';
  }

  return 'Feasible\'s API rate limit has been reached. A Looker Studio report calls this connector' +
    ' once per chart, so refreshing a large dashboard repeatedly is the usual cause.';
}

// describeSeconds renders a wait in the units a person would use for it.
function describeSeconds(seconds) {
  if (seconds < 90) {
    return seconds + ' seconds';
  }

  return Math.ceil(seconds / 60) + ' minutes';
}

// normaliseHeaders lowercases the response's header names.
//
// UrlFetchApp returns them with whatever capitalisation the server sent, and a
// repeated header comes back as an array rather than a string — so reading
// headers['Retry-After'] directly works until the day it silently does not.
function normaliseHeaders(response) {
  var raw = response.getAllHeaders();
  var out = {};
  var name;

  for (name in raw) {
    if (Object.prototype.hasOwnProperty.call(raw, name)) {
      var value = raw[name];

      out[name.toLowerCase()] = Array.isArray(value) ? value[0] : value;
    }
  }

  return out;
}
