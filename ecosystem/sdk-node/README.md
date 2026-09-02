<!--
  README.md
  The Node SDK for feasible.lol: install it, send an event, and never forward the wrong IP.

  Created: 2026-08-30
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# @feasible/node

The official Node client for [feasible.lol](https://app.feasible.lol) — privacy-first web
analytics. Zero runtime dependencies, no build step, MIT licensed.

## Read this first: the two things you must forward

**A server-side call must carry the visitor's real IP and the visitor's real User-Agent.**

Your server is in a datacentre. The analytics endpoint sees the address the request came
from, and if that address is a datacentre with no forwarded visitor in it, the event is a
bot: it is classified, dropped, and your dashboard shows nothing. This is the single most
common way server-side analytics goes wrong, and the endpoint answers `400` with a
sentence naming what is missing rather than pretending it worked.

So `clientIp` and `userAgent` are **required properties on every event**, validated at call
time. A call missing either throws a `FeasibleValidationError` naming the property and
saying why it matters, before a request is built.

The easy way to get them right is to take them from the request you are already holding:

```js
import { visitorFromNodeRequest, visitorFromWebRequest } from "@feasible/node";

visitorFromNodeRequest(req);      // Express, Fastify, Koa, node:http
visitorFromWebRequest(request);   // Next.js route handlers, Remix, workers
```

Both return `{ clientIp, userAgent }` and take `CF-Connecting-IP`, then the **first** entry
of `X-Forwarded-For`, then the socket address. These helpers have no trusted-proxy
configuration. Use them only behind an application edge that strips client-supplied forwarding
headers and writes its own. On a directly exposed app, pass the socket address explicitly so a
client cannot choose its fingerprint or geolocation.

The client sends `clientIp` as `X-Forwarded-For`. **The ingest server honours that header only
from an address on its trusted-proxy list** (`FEASIBLE_INGEST_TRUSTED_PROXIES`); from any other
peer it uses the socket address, which is your server. On a self-hosted instance, add the
address your application calls from to that list. Check it with `debug()`: the derived event's
`client_ip_source` is `x-forwarded-for` when the header was used and `socket` when it was not.

## Install

```bash
npm install @feasible/node
```

Node 18 or newer. **No runtime dependencies** — the transport is the built-in `fetch`,
which also means connections are kept alive and reused between events for free.

Both module systems work:

```js
import { createClient } from "@feasible/node";        // ESM
const { createClient } = require("@feasible/node");   // CommonJS
```

There is no bundler and no TypeScript build in this package. The types in `index.d.ts` are
hand-written, and what is published is exactly what is in the repository.

## Use

```js
import express from "express";
import { createClient, visitorFromNodeRequest } from "@feasible/node";

const analytics = createClient({ domain: "example.com" });
const app = express();

app.get("/pricing", async (req, res) => {
	try {
		const result = await analytics.pageview({
			url: "https://example.com/pricing",
			...visitorFromNodeRequest(req),
		});

		if (result.dropReason) console.log(`analytics: classified as ${result.dropReason}`);
	} catch (error) {
		console.error(`analytics: ${error.message}`);
	}

	res.send("hello");
});
```

Self-hosting? Pass `host`. Nothing else changes.

### A custom event, with properties and money

```js
await analytics.track("Purchase", {
	url: "https://example.com/checkout",
	...visitorFromNodeRequest(req),
	props: { plan: "annual", seats: 4 },
	revenue: { amount: 99.5, currency: "USD" },
	title: "Checkout",
});
```

Thirty properties at most; names cap at 300 characters and values at 2000. Anything past
those limits is counted and reported by the server rather than quietly dropped.

### A conversion that happens later

A webhook, a queue worker, an offline sale — none of them have a referrer, so without help
every one of them is Direct forever and the campaign that earned it gets no credit:

```js
await analytics.track("Purchase", {
	url: "https://example.com/order/complete",
	clientIp: order.visitorIp,
	userAgent: order.visitorUserAgent,
	revenue: { amount: 240, currency: "USD" },
	attribution: { utmSource: "newsletter", utmMedium: "email", utmCampaign: "spring" },
});
```

Store the visitor's IP and User-Agent alongside whatever you are going to convert later.
They are the only two values that cannot be reconstructed after the fact.

## Testing without a network

Pass `disabled: true`, or set `FEASIBLE_DISABLED=1`, and the client sends nothing, resolves
successfully, and keeps every event in memory. That is how you assert analytics in a test
suite:

```js
const analytics = createClient({ domain: "example.com", disabled: true });

await checkout(analytics);

const events = analytics.recorded();
assert.equal(events.length, 1);
assert.equal(events[0].name, "Purchase");
```

Validation still runs in no-op mode. A test suite that never sends anything is exactly
where a missing IP would otherwise hide until production.

## What comes back

Every send resolves to a result:

| Field | Meaning |
|---|---|
| `statusCode` | The final HTTP status. `202` for anything the server understood. |
| `dropReason` | The `x-feasible-dropped` header, empty when the event counted. |
| `attempts` | How many requests it took. |
| `skipped` | The client is in no-op mode and nothing was sent. |

**A drop reason is not an error.** The server accepted the request and decided the event
was a bot, or an excluded visitor, or a site it does not know. It is a field on the result
so you can log it — never swallowed, and never retried.

## Errors

| Class | When |
|---|---|
| `FeasibleValidationError` | Something required was missing. Has `field` and a `code` — `missing_client_ip`, `missing_user_agent`, `missing_name`, `missing_url`, `missing_domain`. |
| `FeasibleApiError` | The server refused the request. Has `statusCode`, the server's own `body` verbatim, and `attempts`. |
| `FeasibleTransportError` | The request never reached a server. Has `attempt` and `cause`. |

## Retries

Three attempts by default, exponential backoff with jitter, capped at two seconds.

- **Retried:** transport errors, `429`, and any `5xx`. Nothing was counted, so nothing can
  be duplicated.
- **Not retried:** `400`. That is your bug — a malformed body or a missing header — and
  sending it again produces the same `400` while hiding the message that explains it.
- **Not retried:** a `202` carrying `x-feasible-dropped`. That is a classification the
  server already made, not a failure.

Pass `{ signal }` as the last argument to any call to cancel a send and its backoff along
with the request that started it.

## Debugging

`debug()` asks the server what it would derive from an event and returns that JSON instead
of writing anything. It is free of side effects and safe against production, which makes it
the first thing to reach for when somebody says their numbers look wrong:

```js
console.log(await analytics.debug({ name: "pageview", url, ...visitorFromNodeRequest(req) }));
```

## Package shape

```json
"exports": {
  ".": {
    "types": "./index.d.ts",
    "import": "./src/index.js",
    "require": "./src/core.cjs",
    "default": "./src/core.cjs"
  },
  "./package.json": "./package.json"
},
"main": "./src/core.cjs",
"module": "./src/index.js",
"types": "./index.d.ts",
"sideEffects": false
```

The implementation lives in a CommonJS core that the ESM entry re-exports by name. That is
deliberate: `require()` of an ES module only works from Node 20.19 onwards, and this package
supports Node 18, so a CJS core with a thin ESM wrapper is the only shape that gives both
`import` and `require` with no build step and no duplicated logic. A test asserts that every
path in the manifest exists and that both entries expose the same objects.

## Licence

MIT. See `LICENSE`.
