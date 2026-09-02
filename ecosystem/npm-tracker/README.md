<!--
  README.md
  The browser loader for feasible.lol: typed, server-side-rendering safe, and it does not bundle the script.

  Created: 2026-08-30
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# @feasible/tracker

The browser package for [feasible.lol](https://app.feasible.lol) — privacy-first web
analytics. It loads the tracker script from your analytics host and gives you a typed
wrapper and a queue stub around it. No dependencies, no build step.

## Install

```bash
npm install @feasible/tracker
```

```js
import { init, track } from "@feasible/tracker";

init({ domain: "example.com" });

await track("Signup", { props: { plan: "annual" } });
```

That is the whole installation. The script is fetched from the analytics host, so a fix to
the tracker reaches your site without you publishing anything.

## What this package is, and is not

**It does not bundle the tracker script.** The script is served from your analytics host —
`https://app.feasible.lol/js/script.js` by default, or your own host, or the per-site
randomised path — and it is licensed **AGPL-3.0-or-later**, the same as the analytics
server it comes with. This package is the loader: a script tag, a queue stub, and types.
That is why the loader is **MIT** and the script it loads is not. Vendoring the script into
an npm package would also mean every site running a stale copy that nobody can fix from
here.

## Two bugs this package exists not to repeat

Both are real failures shipped by an incumbent's npm package, and both are the reason for a
specific decision here.

### 1. `main`, `module` and `exports` that point at the wrong thing

Get these wrong and a bundler hands `require()` an ES module, or TypeScript never finds the
types, or the package simply fails to resolve. The manifest here is:

```json
"exports": {
  ".": {
    "types": "./dist/index.d.ts",
    "import": "./dist/index.mjs",
    "require": "./dist/index.cjs",
    "default": "./dist/index.mjs"
  },
  "./package.json": "./package.json"
},
"main": "./dist/index.cjs",
"module": "./dist/index.mjs",
"types": "./dist/index.d.ts",
"sideEffects": false,
"files": ["dist/index.mjs", "dist/index.cjs", "dist/index.d.ts", "README.md", "LICENSE"]
```

`types` comes **first** in the map, because conditions are matched in order and a types
condition placed after `import` is never reached. `main` is the CommonJS build and `module`
is the ESM one, never crossed. A test in this repository asserts every one of those paths
exists, that the conditions are in that order, and that both builds export the same names —
so the shape cannot rot quietly between releases.

### 2. Browser globals touched at import time, which broke server-side rendering

An analytics package that reads `window` or `localStorage` at module scope throws the
moment it is imported on a server — during a Next.js build, a Remix render, an Astro
prerender — and takes the whole page down with it.

**Nothing in this package reads `window`, `document`, `navigator` or `localStorage` at
module scope.** Every access is inside a function, behind a `typeof window === "undefined"`
guard. On a server:

- `init()` is a documented no-op and returns a stub with the same shape as the browser API.
- `track()` and `pageview()` resolve immediately with `{ sent: false, status: null, dropped: null }`.
- `enable()`, `disable()` and `isEnabled()` return `false` and write nothing.

So this is safe, on both sides of a render:

```js
init({ domain: "example.com" });
await track("Signup");
```

The regression guard is a test that imports this package in a Node process with **no DOM at
all** and calls every exported function. If a future change reaches for a browser global at
module scope, that test fails before the package can be published.

## The queue stub

`init()` installs the queue stub synchronously, before the script tag is added:

```js
window.feasible = window.feasible || function () {
	(window.feasible.q = window.feasible.q || []).push(arguments);
};
```

An event fired during hydration — before a deferred script has finished downloading — is
held in that queue and replayed when the script arrives, rather than being a
`ReferenceError` or an event that silently vanishes.

`track()` returns a promise that resolves with the server's answer, or with
`{ sent: true, status: null, dropped: null }` after three seconds if the callback never fires. It always
settles: the usual reason a callback never arrives is an ad blocker, and a form waiting
forever on a promise that cannot settle is a page this package made worse.

A completed request also includes `dropped`. It is `null` for an accepted event and carries an
inline reason such as `shield_ip` when the ingest response exposes one. Country, page and hostname
shields run later at the data shard, so they cannot appear in the browser result even though the
request returned `202`.

## API

### `init(options)`

| Option | Meaning |
|---|---|
| `domain` | The site as registered. Required — without it nothing is recorded and a warning is logged. |
| `host` | The analytics host the script is served from. Defaults to the hosted service. |
| `scriptPath` | The script path, for a site using its per-site randomised script. |
| `exclude` | Path globs not to record, as an array or a comma-separated string. `*` stops at a separator, `**` crosses them. Matched against path **and hash**. |
| `hashRouting` | Treat a hash change as a navigation. |
| `manual` | Do not send a pageview automatically; call `pageview()` yourself. |
| `trackLocalhost` | Count visits from localhost and `file:` URLs, which are excluded by default. |
| `alias` | A second global name for the tracker, so a site migrating keeps its existing snippet working. |

Safe to call more than once: the script tag is marked, so a re-render or a hot reload
cannot install it twice and double every pageview.

### `track(name, options)` and `pageview(options)`

```js
await track("Purchase", {
	props: { plan: "annual", seats: 4 },
	revenue: { amount: 99.5, currency: "USD" },
});
```

`options` also takes `callback`, `interactive: false` for something the visitor did not do,
and `u` to override the URL the event is recorded against.

`pageview(options)` takes a narrower set — `props`, `callback`, `u`, and `referrer` to
correct the referrer on an SPA route change. Revenue and `interactive` belong on a custom
event; a pageview has nowhere to carry them.

### `enable()` / `disable()` / `isEnabled()`

`disable()` writes the `feasible_ignore` opt-out that the script honours, so this browser
stops being counted; `enable()` clears it. It is per-browser and per-device — it is how
somebody keeps their own visits out of their own numbers, and it is the same switch as
setting the key by hand.

Both return whether the write actually happened. Reading `localStorage` **throws** in a
sandboxed frame, in private browsing inside an iframe and under a blocked-cookies setting,
so a caller that shows a confirmation should show it on `true`, not on being called.

## What the script measures on its own

Outbound link clicks, file downloads, form submissions, engagement time, scroll depth and
single-page-app navigations, with no configuration. You only need `track()` for events that
are specific to your product.

## Licence

MIT for this package. See `LICENSE`. The tracker script it loads is served by the analytics
host and is AGPL-3.0-or-later.
