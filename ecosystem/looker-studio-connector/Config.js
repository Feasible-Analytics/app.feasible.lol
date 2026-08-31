//
// Config.js
// The configuration screen: which host, and which site.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// getConfig builds the configuration screen, in two steps.
//
// It is stepped because the second question cannot be asked until the first is
// answered: the list of sites comes from the API, and the API is wherever the
// person says it is. Asking for a site id as free text instead would mean every
// typo in a domain became an empty report with no explanation.
function getConfig(request) {
  var params = (request && request.configParams) || null;
  var config = {
    configParams: [
      {
        type: 'INFO',
        name: 'instructions',
        text: 'Choose the Feasible instance to read from, then the site. Leave the host empty for the hosted service at ' +
          DEFAULT_HOST + '.'
      },
      {
        type: 'TEXTINPUT',
        name: 'host',
        displayName: 'Analytics host',
        helpText: 'Where your Feasible instance lives. Leave it empty for the hosted service.',
        placeholder: storedHost()
      }
    ],
    dateRangeRequired: true
  };

  // The first call arrives with no configParams at all. Returning here with
  // isSteppedConfig set is what makes Looker Studio show a Next button rather
  // than a half-built form.
  if (params === null) {
    config.isSteppedConfig = true;

    return config;
  }

  var host = normaliseHost(params.host);
  var sites = [];
  var failure = '';

  try {
    sites = listSites(host, storedApiKey());
  } catch (error) {
    failure = error && error.message ? error.message : String(error);
  }

  if (sites.length > 0) {
    config.configParams.push({
      type: 'SELECT_SINGLE',
      name: 'siteId',
      displayName: 'Site',
      helpText: 'The site this data source reports on.',
      options: sites.map(function (site) {
        return {label: site.label, value: site.domain};
      })
    });

    config.isSteppedConfig = false;

    return config;
  }

  // The list could not be fetched, or the key can read no sites. A free-text
  // box is offered rather than a dead end, because somebody who knows their own
  // domain should not be blocked by our inability to enumerate it — a wrong
  // value here fails later with the API's own sentence naming the site.
  config.configParams.push({
    type: 'INFO',
    name: 'sitesUnavailable',
    text: failure === ''
      ? 'This key can read no sites yet. Type the domain of the site you want once it exists.'
      : 'The list of sites could not be loaded: ' + failure + ' Type the site\'s domain instead.'
  });

  config.configParams.push({
    type: 'TEXTINPUT',
    name: 'siteId',
    displayName: 'Site domain',
    helpText: 'The site exactly as it is registered with Feasible, for example example.com.',
    placeholder: 'example.com'
  });

  config.isSteppedConfig = false;

  return config;
}

// readConfig resolves the configuration one request runs against.
//
// The host falls back to the one the key was validated against, then to the
// hosted service, so a data source saved before the host field existed keeps
// working rather than pointing at nothing.
function readConfig(configParams) {
  var params = configParams || {};
  var host = params.host && String(params.host).trim() !== '' ? normaliseHost(params.host) : storedHost();
  var siteId = String(params.siteId || '').trim();

  if (siteId === '') {
    throwUserError('This data source has no site. Open it, choose Edit Connection and pick one.');
  }

  return {host: host, siteId: siteId};
}

// isAdminUser is false for everybody.
//
// It exists to decide who sees the debug text on an error, and this connector
// puts the same sentence in both — there is no operator standing behind a
// customer's own data source, so there is nobody for a privileged message to be
// for.
function isAdminUser() {
  return false;
}
