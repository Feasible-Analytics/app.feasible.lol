___TERMS_OF_SERVICE___

By creating or modifying this file you agree to Google Tag Manager's Community
Template Gallery Developer Terms of Service available at
https://developers.google.com/tag-manager/gallery-tos (or such other URL as
Google may provide), as modified from time to time.


___INFO___

{
  "type": "TAG",
  "id": "cvt_temp_public_id",
  "version": 1,
  "securityGroups": [],
  "displayName": "Feasible Analytics",
  "categories": [
    "ANALYTICS"
  ],
  "brand": {
    "id": "brand_dummy",
    "displayName": "Feasible Analytics"
  },
  "description": "Loads the Feasible Analytics tracker and sends pageviews, custom events, custom properties and revenue. Cookieless, privacy-first web analytics.",
  "containerContexts": [
    "WEB"
  ]
}


___TEMPLATE_PARAMETERS___

[
  {
    "type": "RADIO",
    "name": "tagType",
    "displayName": "Tag type",
    "radioItems": [
      {
        "value": "script",
        "displayValue": "Load the script",
        "help": "Injects the tracker and sends the pageview. Use one of these, on an Initialization or All Pages trigger."
      },
      {
        "value": "event",
        "displayValue": "Send an event",
        "help": "Calls the tracker the script tag already loaded. Use one of these per event, on whatever trigger the event belongs to."
      }
    ],
    "simpleValueType": true,
    "defaultValue": "script"
  },
  {
    "type": "LABEL",
    "name": "scriptHeading",
    "displayName": "Where the tracker comes from",
    "enablingConditions": [
      {
        "paramName": "tagType",
        "paramValue": "script",
        "type": "EQUALS"
      }
    ]
  },
  {
    "type": "TEXT",
    "name": "domain",
    "displayName": "Site domain",
    "simpleValueType": true,
    "help": "The site exactly as it is registered with Feasible, for example <code>example.com</code>. No scheme, no www unless the site is registered with it, no trailing slash.",
    "valueValidators": [
      {
        "type": "NON_EMPTY"
      }
    ],
    "enablingConditions": [
      {
        "paramName": "tagType",
        "paramValue": "script",
        "type": "EQUALS"
      }
    ]
  },
  {
    "type": "TEXT",
    "name": "host",
    "displayName": "Analytics host",
    "simpleValueType": true,
    "defaultValue": "https://app.feasible.lol",
    "help": "Where the tracker is served from and where events are sent. Leave the default for the hosted service; change it for a self-hosted instance or your own proxy. A host other than the default also has to be added to this template's <strong>inject_script</strong> permission, which only the container owner can do.",
    "valueValidators": [
      {
        "type": "NON_EMPTY"
      }
    ],
    "enablingConditions": [
      {
        "paramName": "tagType",
        "paramValue": "script",
        "type": "EQUALS"
      }
    ]
  },
  {
    "type": "TEXT",
    "name": "scriptPath",
    "displayName": "Custom script path",
    "simpleValueType": true,
    "help": "Optional. Defaults to <code>/js/script.js</code>. Set it to the per-site path Feasible generated for you (<code>/js/fs-xxxxxxxxxxxxxxxx.js</code>), or to whatever path you serve the script from behind your own domain.",
    "enablingConditions": [
      {
        "paramName": "tagType",
        "paramValue": "script",
        "type": "EQUALS"
      }
    ]
  },
  {
    "type": "CHECKBOX",
    "name": "hashRouting",
    "checkboxText": "Count hash changes as pageviews",
    "simpleValueType": true,
    "help": "For single-page apps that route with <code>#</code> fragments rather than the History API.",
    "enablingConditions": [
      {
        "paramName": "tagType",
        "paramValue": "script",
        "type": "EQUALS"
      }
    ]
  },
  {
    "type": "CHECKBOX",
    "name": "manualPageviews",
    "checkboxText": "Send pageviews manually",
    "simpleValueType": true,
    "help": "Stops the tracker sending a pageview on load. Send them yourself with a <strong>Send an event</strong> tag whose event name is <code>pageview</code>.",
    "enablingConditions": [
      {
        "paramName": "tagType",
        "paramValue": "script",
        "type": "EQUALS"
      }
    ]
  },
  {
    "type": "CHECKBOX",
    "name": "captureOnLocalhost",
    "checkboxText": "Count traffic on localhost",
    "simpleValueType": true,
    "help": "Off by default so your own development traffic does not land in your reports. Turn it on while you are testing the container.",
    "enablingConditions": [
      {
        "paramName": "tagType",
        "paramValue": "script",
        "type": "EQUALS"
      }
    ]
  },
  {
    "type": "LABEL",
    "name": "eventHeading",
    "displayName": "What to send",
    "enablingConditions": [
      {
        "paramName": "tagType",
        "paramValue": "event",
        "type": "EQUALS"
      }
    ]
  },
  {
    "type": "TEXT",
    "name": "eventName",
    "displayName": "Event name",
    "simpleValueType": true,
    "help": "The name the event is reported under, for example <code>Signup</code>. The name <code>pageview</code> sends a pageview.",
    "valueValidators": [
      {
        "type": "NON_EMPTY"
      }
    ],
    "enablingConditions": [
      {
        "paramName": "tagType",
        "paramValue": "event",
        "type": "EQUALS"
      }
    ]
  },
  {
    "type": "SIMPLE_TABLE",
    "name": "props",
    "displayName": "Custom properties",
    "simpleTableColumns": [
      {
        "defaultValue": "",
        "displayName": "Name",
        "name": "name",
        "type": "TEXT",
        "isUnique": true
      },
      {
        "defaultValue": "",
        "displayName": "Value",
        "name": "value",
        "type": "TEXT"
      }
    ],
    "help": "Up to 30 properties. Names are capped at 300 characters and values at 2000. A row with an empty name is skipped.",
    "enablingConditions": [
      {
        "paramName": "tagType",
        "paramValue": "event",
        "type": "EQUALS"
      }
    ]
  },
  {
    "type": "TEXT",
    "name": "revenueAmount",
    "displayName": "Revenue amount",
    "simpleValueType": true,
    "help": "Optional. A number such as <code>19.99</code>, or a variable holding one. Leave it empty for an event that carries no money.",
    "enablingConditions": [
      {
        "paramName": "tagType",
        "paramValue": "event",
        "type": "EQUALS"
      }
    ]
  },
  {
    "type": "TEXT",
    "name": "revenueCurrency",
    "displayName": "Revenue currency",
    "simpleValueType": true,
    "defaultValue": "USD",
    "help": "An ISO 4217 code, for example <code>USD</code> or <code>GBP</code>. Required whenever an amount is set.",
    "enablingConditions": [
      {
        "paramName": "tagType",
        "paramValue": "event",
        "type": "EQUALS"
      }
    ]
  }
]


___SANDBOXED_JS_FOR_WEB_TEMPLATE___

//
// template.tpl
// The Google Tag Manager custom template for the feasible.lol tracker.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//
// Licensed under the Apache Licence, Version 2.0. See LICENSE in this folder.
//

const callInWindow = require('callInWindow');
const copyFromWindow = require('copyFromWindow');
const createQueue = require('createQueue');
const injectScript = require('injectScript');
const logToConsole = require('logToConsole');
const makeNumber = require('makeNumber');
const makeString = require('makeString');
const queryPermission = require('queryPermission');
const setInWindow = require('setInWindow');

// Every require above maps to one entry in ___WEB_PERMISSIONS___ and to
// nothing wider. An over-broad permission — a wildcard script host, a global
// key the template never touches, logging left on in production — is the
// commonest reason a Community Template Gallery submission is rejected, and it
// is also the thing a reviewer can check fastest. If a require is deleted here,
// delete its permission too.

// The global the tracker installs, and the queue it drains when it loads. Both
// names are part of the tracker's published contract rather than settings: a
// container that could rename them would produce a tag that sends nothing and
// reports success.
const GLOBAL = 'feasible';
const QUEUE = 'feasible.q';

// The global the tracker reads its configuration out of on its first line.
// GTM builds the script element itself and gives a template no way to put
// data-* attributes on it, so this is the only channel a tag manager has for
// telling the tracker which site it is reporting for.
const CONFIG = '__fsc';

// The path the shared tracker is served from when no custom one is configured.
const DEFAULT_PATH = '/js/script.js';

// refuse ends the tag on a configuration mistake, with a sentence in the
// preview console naming what is wrong.
//
// Calling gtmOnFailure matters as much as the message. A tag that returns
// without calling either callback stays "still running" for the life of the
// page, and every tag sequenced after it never fires at all — so a typo in one
// field silently disables a chain of unrelated tags.
const refuse = (message) => {
  logToConsole('Feasible Analytics: ' + message);
  data.gtmOnFailure();
};

// text reads a field as a trimmed string. Fields can carry a GTM variable, so a
// number or an undefined arrives here as often as a string does.
const text = (value) => {
  if (value === undefined || value === null) {
    return '';
  }

  return makeString(value).trim();
};

// normaliseHost drops trailing slashes so that a host typed with one and a path
// that starts with one cannot produce a double slash. The browser would treat
// that as a different URL, which costs a second download and a second entry in
// every cache between here and the origin.
const normaliseHost = (value) => {
  let host = text(value);

  while (host.length > 0 && host.charAt(host.length - 1) === '/') {
    host = host.substring(0, host.length - 1);
  }

  return host;
};

// scriptUrl builds the URL the tracker is loaded from. The custom path exists
// for the two real deployments that are not the default: a per-site randomised
// script, and a customer proxying the script under their own domain to keep it
// first-party.
const scriptUrl = (host, path) => {
  let suffix = text(path);

  if (suffix === '') {
    suffix = DEFAULT_PATH;
  }

  if (suffix.charAt(0) !== '/') {
    suffix = '/' + suffix;
  }

  return host + suffix;
};

// buildProps turns the key/value table into the object the tracker sends.
//
// A row with no name is skipped rather than sent as an empty key, because a GTM
// variable that resolves to nothing leaves exactly that shape behind and an
// empty property name is refused at ingest — dropping the whole event with it.
const buildProps = (rows) => {
  if (!rows) {
    return undefined;
  }

  const props = {};
  let count = 0;

  for (let i = 0; i < rows.length; i++) {
    const name = text(rows[i].name);

    if (name === '') {
      continue;
    }

    props[name] = rows[i].value;
    count++;
  }

  if (count === 0) {
    return undefined;
  }

  return props;
};

// buildRevenue reads the revenue pair, or nothing when no amount was given.
//
// An unparseable amount is refused rather than sent as NaN. NaN survives the
// wire as null and lands as a sale worth nothing, which is far harder to notice
// than a tag that goes red in preview.
const buildRevenue = (rawAmount, rawCurrency) => {
  const amount = makeNumber(rawAmount);

  // The sandbox has no isNaN, and NaN is the one value that is not equal to
  // itself.
  if (amount !== amount) {
    refuse('the revenue amount "' + rawAmount + '" is not a number');
    return undefined;
  }

  const currency = text(rawCurrency);

  if (currency === '') {
    refuse('a revenue amount needs a currency, for example USD');
    return undefined;
  }

  return {amount: amount, currency: currency};
};

// loadScript is the "Load the script" branch: configure, install the queue,
// inject.
const loadScript = () => {
  const domain = text(data.domain);

  if (domain === '') {
    refuse('this tag has no site domain, so nothing was loaded');
    return;
  }

  const host = normaliseHost(data.host);

  if (host === '') {
    refuse('this tag has no analytics host, so nothing was loaded');
    return;
  }

  const url = scriptUrl(host, data.scriptPath);

  // Asking first turns "the container owner pointed this at their own proxy"
  // from a silent no-op into a sentence naming the URL that has to be added to
  // the template's permission.
  if (!queryPermission('inject_script', url)) {
    refuse('this template is not permitted to load a script from ' + url +
      ' — add that URL to the template\'s inject_script permission');
    return;
  }

  // The tracker layers this over anything it reads from its own script tag and
  // then clears it, so a second copy of the script cannot pick up the first
  // site's configuration.
  setInWindow(CONFIG, {
    d: domain,
    h: !!data.hashRouting,
    m: !!data.manualPageviews,
    l: !!data.captureOnLocalhost
  }, true);

  // The queue goes in before the injection, not after it. An event tag that
  // fires while the script is still in flight — a click on a page that was slow
  // to load, or any tag GTM happens to run first — has somewhere to land, and
  // the tracker replays the queue the moment it arrives. Without this the event
  // is simply lost, which is the failure nobody reports because nothing
  // visibly breaks.
  createQueue(QUEUE);

  // The URL doubles as the cache token, so two containers, or a script tag
  // that fires twice, inject one script and both report success.
  injectScript(url, data.gtmOnSuccess, data.gtmOnFailure, url);
};

// sendEvent is the "Send an event" branch: call the tracker if it is here, and
// queue the call if it is not.
const sendEvent = () => {
  const name = text(data.eventName);

  if (name === '') {
    refuse('this tag has no event name, so nothing was sent');
    return;
  }

  const options = {};
  const props = buildProps(data.props);

  if (props) {
    options.props = props;
  }

  const rawAmount = text(data.revenueAmount);

  if (rawAmount !== '') {
    const revenue = buildRevenue(rawAmount, data.revenueCurrency);

    // buildRevenue has already called gtmOnFailure with the reason.
    if (!revenue) {
      return;
    }

    options.revenue = revenue;
  }

  const existing = copyFromWindow(GLOBAL);

  if (typeof existing === 'function') {
    callInWindow(GLOBAL, name, options);
  } else {
    // The script has not arrived yet. Pushing the call's arguments onto the
    // queue is what the tracker replays, so the event survives the race rather
    // than throwing on a global that is not a function yet.
    const queue = createQueue(QUEUE);
    queue([name, options]);
  }

  data.gtmOnSuccess();
};

if (data.tagType === 'event') {
  sendEvent();
} else {
  loadScript();
}


___WEB_PERMISSIONS___

[
  {
    "instance": {
      "key": {
        "publicId": "inject_script",
        "versionId": "1"
      },
      "param": [
        {
          "key": "urls",
          "value": {
            "type": 2,
            "listItem": [
              {
                "type": 1,
                "string": "https://app.feasible.lol/*"
              }
            ]
          }
        }
      ]
    },
    "clientAnnotations": {
      "isEditedByUser": true
    },
    "isRequired": true
  },
  {
    "instance": {
      "key": {
        "publicId": "access_globals",
        "versionId": "1"
      },
      "param": [
        {
          "key": "keys",
          "value": {
            "type": 2,
            "listItem": [
              {
                "type": 3,
                "mapKey": [
                  {
                    "type": 1,
                    "string": "key"
                  },
                  {
                    "type": 1,
                    "string": "read"
                  },
                  {
                    "type": 1,
                    "string": "write"
                  },
                  {
                    "type": 1,
                    "string": "execute"
                  }
                ],
                "mapValue": [
                  {
                    "type": 1,
                    "string": "feasible"
                  },
                  {
                    "type": 8,
                    "boolean": true
                  },
                  {
                    "type": 8,
                    "boolean": true
                  },
                  {
                    "type": 8,
                    "boolean": true
                  }
                ]
              },
              {
                "type": 3,
                "mapKey": [
                  {
                    "type": 1,
                    "string": "key"
                  },
                  {
                    "type": 1,
                    "string": "read"
                  },
                  {
                    "type": 1,
                    "string": "write"
                  },
                  {
                    "type": 1,
                    "string": "execute"
                  }
                ],
                "mapValue": [
                  {
                    "type": 1,
                    "string": "feasible.q"
                  },
                  {
                    "type": 8,
                    "boolean": true
                  },
                  {
                    "type": 8,
                    "boolean": true
                  },
                  {
                    "type": 8,
                    "boolean": false
                  }
                ]
              },
              {
                "type": 3,
                "mapKey": [
                  {
                    "type": 1,
                    "string": "key"
                  },
                  {
                    "type": 1,
                    "string": "read"
                  },
                  {
                    "type": 1,
                    "string": "write"
                  },
                  {
                    "type": 1,
                    "string": "execute"
                  }
                ],
                "mapValue": [
                  {
                    "type": 1,
                    "string": "__fsc"
                  },
                  {
                    "type": 8,
                    "boolean": false
                  },
                  {
                    "type": 8,
                    "boolean": true
                  },
                  {
                    "type": 8,
                    "boolean": false
                  }
                ]
              }
            ]
          }
        }
      ]
    },
    "clientAnnotations": {
      "isEditedByUser": true
    },
    "isRequired": true
  },
  {
    "instance": {
      "key": {
        "publicId": "logging",
        "versionId": "1"
      },
      "param": [
        {
          "key": "environments",
          "value": {
            "type": 1,
            "string": "debug"
          }
        }
      ]
    },
    "clientAnnotations": {
      "isEditedByUser": true
    },
    "isRequired": true
  }
]


___TESTS___

scenarios:
- name: The script tag injects the tracker from the configured host
  code: |-
    const mockData = {
      tagType: 'script',
      domain: 'example.com',
      host: 'https://app.feasible.lol/',
      scriptPath: '',
      hashRouting: false,
      manualPageviews: false,
      captureOnLocalhost: false
    };

    let injected = '';
    let baked;

    mock('queryPermission', () => true);
    mock('setInWindow', (key, value) => {
      baked = value;
      return true;
    });
    mock('createQueue', () => {
      return () => {};
    });
    mock('injectScript', (url, onSuccess) => {
      injected = url;
      onSuccess();
    });

    runCode(mockData);

    assertThat(injected).isEqualTo('https://app.feasible.lol/js/script.js');
    assertThat(baked.d).isEqualTo('example.com');
    assertApi('gtmOnSuccess').wasCalled();
    assertApi('gtmOnFailure').wasNotCalled();
- name: A custom script path is used instead of the default
  code: |-
    const mockData = {
      tagType: 'script',
      domain: 'example.com',
      host: 'https://analytics.example.com',
      scriptPath: 'js/fs-abcdefghijklmnop.js'
    };

    let injected = '';

    mock('queryPermission', () => true);
    mock('setInWindow', () => true);
    mock('createQueue', () => {
      return () => {};
    });
    mock('injectScript', (url, onSuccess) => {
      injected = url;
      onSuccess();
    });

    runCode(mockData);

    assertThat(injected).isEqualTo('https://analytics.example.com/js/fs-abcdefghijklmnop.js');
    assertApi('gtmOnSuccess').wasCalled();
- name: An event tag calls the tracker with its name, properties and revenue
  code: |-
    const mockData = {
      tagType: 'event',
      eventName: 'Signup',
      props: [
        {name: 'plan', value: 'pro'},
        {name: '', value: 'skipped'}
      ],
      revenueAmount: '19.99',
      revenueCurrency: 'USD'
    };

    let sentName;
    let sentOptions;

    mock('copyFromWindow', () => {
      return () => {};
    });
    mock('callInWindow', (path, name, options) => {
      sentName = name;
      sentOptions = options;
    });

    runCode(mockData);

    assertThat(sentName).isEqualTo('Signup');
    assertThat(sentOptions.props.plan).isEqualTo('pro');
    assertThat(sentOptions.props['']).isEqualTo(undefined);
    assertThat(sentOptions.revenue.amount).isEqualTo(19.99);
    assertThat(sentOptions.revenue.currency).isEqualTo('USD');
    assertApi('gtmOnSuccess').wasCalled();
- name: An event fired before the script arrives is queued, not lost
  code: |-
    const mockData = {
      tagType: 'event',
      eventName: 'Signup'
    };

    let queued;

    mock('copyFromWindow', () => undefined);
    mock('createQueue', (path) => {
      assertThat(path).isEqualTo('feasible.q');
      return (value) => {
        queued = value;
      };
    });
    mock('callInWindow', () => {
      fail('the tag called a global that is not a function yet');
    });

    runCode(mockData);

    assertThat(queued[0]).isEqualTo('Signup');
    assertApi('gtmOnSuccess').wasCalled();
- name: A script tag with no site domain fails instead of hanging
  code: |-
    const mockData = {
      tagType: 'script',
      domain: '',
      host: 'https://app.feasible.lol'
    };

    mock('injectScript', () => {
      fail('the tag injected a script it had no domain for');
    });

    runCode(mockData);

    assertApi('gtmOnFailure').wasCalled();
    assertApi('gtmOnSuccess').wasNotCalled();
    assertApi('injectScript').wasNotCalled();


___NOTES___

Created on 2026-08-30. Copyright (c) 2026 Cloudmanic Labs, LLC.
Licensed under the Apache Licence, Version 2.0.
