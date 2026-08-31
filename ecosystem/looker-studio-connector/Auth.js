//
// Auth.js
// Storing and checking the API key.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// The key and the host live in the *user* properties.
//
// PropertiesService.getScriptProperties() is shared by every person who uses a
// deployment of this script. Putting a credential there would hand one
// customer's API key — and therefore every site it can read — to the next person
// who connected to the same deployment. The user store is per Google account and
// is the only correct place for this. Nothing in this connector may write a key
// anywhere else.
var API_KEY_PROPERTY = 'feasible.apiKey';
var HOST_PROPERTY = 'feasible.host';

// How long a successful key check is trusted for.
//
// Looker Studio calls isAuthValid on every request, and an API call there would
// double this connector's rate-limit cost for no new information. Ten minutes is
// short enough that a revoked key stops working while somebody is still looking
// at the report, and long enough that a dashboard refresh checks once rather
// than once per chart.
var AUTH_CACHE_SECONDS = 600;
var AUTH_CACHE_KEY = 'fs1:auth-ok';

// getAuthType tells Looker Studio to collect a single secret.
//
// KEY rather than OAuth because the Stats API authenticates a bearer token
// belonging to a team, not a Google identity — there is no consent screen a
// Google account could satisfy, and pretending otherwise would put an OAuth
// flow in front of a value the person already has in their clipboard.
function getAuthType() {
  return {
    type: 'KEY',
    helpUrl: 'https://feasible.lol/docs/api#keys'
  };
}

// setCredentials stores what the person pasted, after proving it works.
//
// Validating here rather than at the first chart is the difference between "that
// key is not valid" next to the box they typed it in, and a report full of
// broken charts an hour later.
function setCredentials(request) {
  var credential = parseCredential(request && request.key);

  if (credential.error) {
    return {errorCode: 'INVALID_CREDENTIALS'};
  }

  var host = credential.host === '' ? DEFAULT_HOST : credential.host;
  var check = keyWorks(host, credential.key);

  if (!check.valid) {
    return {errorCode: 'INVALID_CREDENTIALS'};
  }

  var properties = PropertiesService.getUserProperties();

  properties.setProperty(API_KEY_PROPERTY, credential.key);
  properties.setProperty(HOST_PROPERTY, host);

  // A key that has just been proved good does not need proving again on the
  // next request.
  if (check.checked) {
    CacheService.getUserCache().put(AUTH_CACHE_KEY, '1', AUTH_CACHE_SECONDS);
  }

  return {errorCode: 'NONE'};
}

// resetAuth forgets the key.
//
// The cached "this key works" flag goes with it. Leaving it behind would let a
// person who has just disconnected keep working for ten minutes, which reads as
// the disconnect not having happened.
function resetAuth() {
  var properties = PropertiesService.getUserProperties();

  properties.deleteProperty(API_KEY_PROPERTY);
  properties.deleteProperty(HOST_PROPERTY);

  CacheService.getUserCache().remove(AUTH_CACHE_KEY);
}

// isAuthValid reports whether the stored key still authenticates.
//
// Answering false makes Looker Studio drop the credential and ask for it again,
// so it is reserved for an outright rejection. An unreachable host answers true:
// the key is still the right key, and the failure gets a readable message from
// getData instead of silently signing somebody out because their instance was
// restarting.
function isAuthValid() {
  var properties = PropertiesService.getUserProperties();
  var apiKey = properties.getProperty(API_KEY_PROPERTY);

  if (!apiKey) {
    return false;
  }

  var cache = CacheService.getUserCache();

  if (cache.get(AUTH_CACHE_KEY) !== null) {
    return true;
  }

  var host = normaliseHost(properties.getProperty(HOST_PROPERTY));
  var check = keyWorks(host, apiKey);

  if (check.valid && check.checked) {
    cache.put(AUTH_CACHE_KEY, '1', AUTH_CACHE_SECONDS);
  }

  return check.valid;
}

// storedApiKey reads the key, refusing the request rather than sending an empty
// Authorization header that the API would answer 401 to for no obvious reason.
function storedApiKey() {
  var apiKey = PropertiesService.getUserProperties().getProperty(API_KEY_PROPERTY);

  if (!apiKey) {
    throwUserError('This data source has no Feasible API key. Open it, choose Edit Connection and paste one.');
  }

  return apiKey;
}

// storedHost is the host the key was validated against, and the default the
// configuration screen offers.
function storedHost() {
  return normaliseHost(PropertiesService.getUserProperties().getProperty(HOST_PROPERTY));
}
