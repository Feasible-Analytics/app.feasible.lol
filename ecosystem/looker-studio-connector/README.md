<!--
  README.md
  The Looker Studio community connector for feasible.lol.

  Created: 2026-08-30
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Feasible Analytics — Looker Studio connector

A Looker Studio community connector, written in Google Apps Script, that reads
the feasible.lol Stats API. Twenty-three dimensions, eleven metrics, and a cache
that keeps a whole dashboard inside the API's rate limit.

## What is in here

| File | What it does |
|---|---|
| `appsscript.json` | The Apps Script manifest, including the `dataStudio` block the gallery reads. |
| `Params.js` | Request building, filter translation, cache keys. No Google runtime, so it is testable. |
| `Schema.js` | Every field, and the semantics Looker Studio needs to draw it. |
| `Auth.js` | `getAuthType`, `setCredentials`, `resetAuth`, `isAuthValid`. |
| `Config.js` | The stepped configuration screen, and `isAdminUser`. |
| `Data.js` | `getData` — one chart, one API call. |
| `Api.js` | The HTTP client, including the 429 path. |
| `Cache.js` | The per-user response cache, with chunking. |
| `Errors.js` | Turning an API failure into a sentence. |
| `test/params.test.js` | The pure-logic test suite. |

## Deploying it with clasp

You need Node and [clasp](https://github.com/google/clasp).

```bash
npm install -g @google/clasp
clasp login

# From this folder. The script has to be standalone, not bound to a document.
clasp create --type standalone --title "Feasible Analytics connector"
clasp push
```

`clasp create` writes a `.clasp.json` with the new script's id. Keep it out of
version control — it points at your copy, not anybody else's. `.claspignore`
already keeps `test/`, the licence and this page out of the push; a test file
pushed into Apps Script would be concatenated into the same global scope as the
connector and its `require` calls would break every entry point.

Then, to use it:

```bash
clasp deploy --description "v1"
clasp open        # Deploy → Test deployments → copy the Head Deployment id
```

Open `https://lookerstudio.google.com/datasources/create?connectorId=<deployment id>`.
For your own use the **head deployment** id is enough and picks up every
`clasp push` immediately. For anyone else, make a versioned deployment.

## Supplying an API key

Looker Studio asks for the key the first time somebody connects. Create one in
Feasible under **Settings → API keys**, with the **stats:read** and
**sites:read** scopes, and paste it in.

**Self-hosting?** The box takes your host, a space, then the key:

```
https://analytics.example.com fsk_live_abc123
```

Looker Studio's KEY authentication only gives a connector one text field, and
authentication happens before the configuration screen — so the host has to
travel with the key on the first round trip. After that it is remembered and the
configuration screen offers it as the default.

The key is stored in `PropertiesService.getUserProperties()`. **Never move it to
the script properties**: those are shared by everybody using a deployment, so a
key there is handed to the next person who connects. The connector validates the
key by calling `GET /api/v1/sites` and checking for a 200; the result of that
check is remembered for ten minutes so that a dashboard refresh does not spend a
rate-limit slot per chart proving the key still works.

Configuration is two steps. Step one is the host. Step two lists the sites the
key can actually read, so nobody has to type a domain exactly right — if the list
cannot be fetched, the connector falls back to a text box rather than a dead end.

## The cache

Looker Studio calls `getData` **once per chart**. A ten-chart dashboard is ten
requests for one refresh, and the Stats API counts every one against an hourly
limit. The cache is what makes a dashboard viable, not a nicety.

- `CacheService.getUserCache()` — per Google account, so nobody reads anybody
  else's numbers.
- The key is a 64-bit hash of the host, the endpoint and every query parameter,
  sorted, prefixed with a format version. The API key is never part of it: the
  cache is already scoped to one person, and a credential in a key is only a
  credential somewhere else.
- **300 seconds** by default.
- **30 seconds** for a `realtime` period, which describes the last half hour and
  moves continuously.
- Errors are never cached. A rate limit or a typo pinned in front of somebody for
  five minutes after they fixed it is worse than the original failure.

Apps Script caps one cache entry at 100 KB. A response longer than 25,000
characters is split across numbered keys with a small manifest entry naming the
count — 25,000 characters cannot exceed 75 KB whatever alphabet they are in.
Past eight chunks the response is not cached at all: reassembling a dozen entries
costs more than the request it saves, each one is separately evictable, and a
response that large is a chart pulling thousands of rows rather than something
being refreshed in a loop. A chunked entry with a missing part reads as a miss,
never as half a document.

## Rate limits

A 429 becomes a sentence saying when the limit resets, read from `Retry-After` or
`X-RateLimit-Reset`. The connector does not retry: Looker Studio calls it once
per chart, so a connector that sleeps and retries turns one rate limit into a
dashboard that hangs and then fails anyway.

## Filters

Looker Studio's filters are pushed down to the API where the operator maps:

| Looker Studio | API |
|---|---|
| `EQUALS` / `IN`, include | `==` |
| `EQUALS` / `IN`, exclude | `!=` |
| `CONTAINS`, include | `~` |
| `CONTAINS`, exclude | `!~` |

Everything else is left to Looker Studio: the regular-expression operators (the
API has *contains*, not regular expressions), the numeric comparisons (they apply
to metrics; the API filters dimensions), `IS_NULL`, `BETWEEN`, a filter on a
metric or on the date, and any OR group that mixes fields or operators — the API
expresses OR as several values on one predicate, so a mixed group has no single
form. The connector reports `filtersApplied` only when **every** group went down
the wire, so a skipped predicate costs rows over the wire and never correctness.

## What a chart can ask for

Each chart becomes one API call:

- No dimension → `/api/v1/stats/aggregate`
- Date only → `/api/v1/stats/timeseries` with `interval=date`
- One other dimension → `/api/v1/stats/breakdown` with that dimension as
  `property`

A chart that groups by the date **and** another dimension, or by two dimensions,
has no single call behind it. The connector says so in the chart rather than
silently dropping one of them.

## Running the tests

```bash
node --test test/params.test.js
```

No Google runtime, no network, no dependencies. `Params.js` and `Schema.js` end
with a `typeof module !== 'undefined'` guard so the same source runs in both
places — Apps Script has no module system and leaves `module` undefined, so the
guard is inert there.

## Before submitting it to the gallery

The `dataStudio` block in `appsscript.json` is what a reviewer and a user read.
Every one of these has to point somewhere real before submission:

| Field | Needs |
|---|---|
| `name` | The connector's name in the gallery. |
| `logoUrl` | A publicly reachable square image. |
| `company`, `companyUrl` | The publisher, and a page about them. |
| `addonUrl` | The connector's own page — what it does and how to set it up. |
| `supportUrl` | Where a person reports a problem. |
| `privacyPolicyUrl`, `termsOfServiceUrl` | Both are required for a listing. |
| `description`, `shortDescription` | The gallery listing text. |
| `authType`, `feeType` | `KEY` and `FREE` here. |

Then make a versioned deployment (`clasp deploy`), and submit the deployment id
through the [Looker Studio partner
form](https://developers.google.com/looker-studio/connector/submit). Google
reviews it by hand.

## Licence

MIT. See [LICENSE](LICENSE).
