//
// Api.js
// The HTTP client for the Feasible Stats API.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// encodeQuery builds a query string. Every value is encoded, including the
// filter string — its `;`, `|` and `~` are grammar to the API and would
// otherwise be mangled or truncated by a proxy on the way there.
function encodeQuery(params) {
  var parts = [];
  var name;

  for (name in params) {
    if (Object.prototype.hasOwnProperty.call(params, name)) {
      parts.push(encodeURIComponent(name) + '=' + encodeURIComponent(params[name]));
    }
  }

  return parts.join('&');
}

// statsRequest performs one call and returns the response body as text.
//
// The body comes back as a string rather than as parsed JSON because that is
// what the cache stores: parsing on the way in and re-serialising on the way to
// the cache would cost a round trip through JSON for no gain, and would quietly
// change number formatting.
//
// muteHttpExceptions is on so that a 4xx reaches this code as a response we can
// read the API's sentence out of, rather than as an Apps Script exception whose
// message is the raw HTML of an error page.
function statsRequest(host, path, params, apiKey) {
  var url = host + path + '?' + encodeQuery(params);
  var response;

  try {
    response = UrlFetchApp.fetch(url, {
      method: 'get',
      muteHttpExceptions: true,
      headers: {
        'Authorization': 'Bearer ' + apiKey,
        'Accept': 'application/json'
      }
    });
  } catch (error) {
    // A throw here is DNS, TLS or a timeout — the host is wrong or unreachable,
    // and naming it is far more useful than the exception's own text.
    throwUserError('Could not reach ' + host + '. Check the analytics host in this data source\'s configuration.');
    return '';
  }

  var status = response.getResponseCode();

  if (status === 429) {
    throwUserError(rateLimitMessage(response));
    return '';
  }

  if (status !== 200) {
    throwUserError(getConnectorError(response));
    return '';
  }

  return response.getContentText();
}

// listSites returns the sites an API key may read, as {domain, label} pairs.
//
// It is the cheapest authenticated call there is, which is why it doubles as the
// key check in isAuthValid: a key that can list sites can read stats, and a key
// that cannot fails here with the API's own sentence rather than at the first
// chart somebody builds.
function listSites(host, apiKey) {
  var body = statsRequest(host, '/api/v1/sites', {limit: 1000}, apiKey);
  var payload = JSON.parse(body);
  var sites = payload.sites || [];
  var out = [];
  var i;

  for (i = 0; i < sites.length; i++) {
    var site = sites[i];
    var label = site.display_name && site.display_name !== site.domain
      ? site.display_name + ' (' + site.domain + ')'
      : site.domain;

    out.push({domain: site.domain, label: label});
  }

  return out;
}

// keyWorks reports whether a key authenticates, without turning a temporary
// outage into a sign-out.
//
// Looker Studio calls isAuthValid on every request and drops the stored
// credential when it answers false. So only an outright rejection counts: if the
// host is down, or slow, the key is still the right key, and forcing somebody to
// paste it again every time their instance restarts would be worse than a chart
// that fails with a readable message.
function keyWorks(host, apiKey) {
  var response;

  try {
    response = UrlFetchApp.fetch(host + '/api/v1/sites?limit=1', {
      method: 'get',
      muteHttpExceptions: true,
      headers: {'Authorization': 'Bearer ' + apiKey}
    });
  } catch (error) {
    return {valid: true, checked: false, message: ''};
  }

  var status = response.getResponseCode();

  if (status === 200) {
    return {valid: true, checked: true, message: ''};
  }

  if (status === 401 || status === 403) {
    return {valid: false, checked: true, message: getConnectorError(response)};
  }

  return {valid: true, checked: false, message: getConnectorError(response)};
}
