<!--
  README.md
  The WordPress plugin: what it does, how the proxy works, and how to develop on it.

  Created: 2026-08-30
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Feasible Analytics for WordPress

Privacy-first analytics for WordPress, served from **your own domain on randomised paths**.

GPL-2.0-or-later. Requires WordPress 5.9 and PHP 7.4.

## What it does

Nothing, until an administrator confirms the site domain and ticks **Turn on tracking** on the
settings screen. Activation stores the random path segments and nothing else: no domain, no
tracking, no request to any server. Until tracking is on, no script reaches a page and both proxy
routes answer `503` with `x-feasible-error: tracking-off`.

Once it is on, it:

- **Serves the bundled tracking script and the event endpoint from your domain**, on paths generated when
  the plugin is activated. This is the single most effective thing a self-hosted analytics tool can
  do about ad blockers, and it is the reason this plugin exists.
- **Injects the snippet** into `wp_head` or `wp_footer`, with no theme editing.
- **Tracks site searches** — the search term, how many results came back, and whether it found
  anything at all. WordPress knows this and the browser script cannot.
- **Tracks 404s**, with the path that was asked for and where the visitor came from.
- **Switches off outbound-link, download and form-submission events** at the proxy, per site.
- **Embeds your dashboard in wp-admin** through a shared link, so you never leave WordPress.

## The proxy

A blocker's filter list names files. The hosted script is on a list; a file called
`analytics.js` on your own domain goes on a list the week somebody notices it. A path nobody can
predict is not a path anybody can list, and the remedy when one is finally listed is a button.

On activation the plugin generates three random twelve-character segments and stores them: a
namespace, a script name and an event name. It then answers two routes:

```
https://your-site.example/<namespace>/<script>.js     the tracker bundle
https://your-site.example/<namespace>/<event>         the event endpoint
```

**The script route** serves `assets/js/feasible.js`, the tracker bundled inside the plugin, with an
`ETag` so the browser's hourly revalidation is a `304` rather than a download. The plugin never
fetches code from anywhere: the plugin directory does not allow it, and a bundled file cannot fail
upstream. The file is a build output of `tracker/` (see Development), byte for byte the same script
the analytics host serves, and a Go test fails if the two copies differ.

**The event route** never forwards the body it received. `Feasible_Payload` decodes the JSON, keeps
only the keys the event endpoint reads, sanitises and size-limits each one for its type, sets the
domain to the one configured here, and encodes the result again. A body that is not a JSON object
with an event name is answered `400` with `x-feasible-error: invalid-event`. Each cap sits above the
endpoint's own limit, so anything the endpoint would cut still reaches it and is counted on the
health panel. The rebuilt event goes upstream with the visitor's real
`X-Forwarded-For` and the visitor's real `User-Agent`. Without those two the analytics server sees
your web host, and every visitor to your site becomes one person sitting in a datacentre — a failure
that produces no error anywhere and is usually noticed weeks later, if at all. The address is
resolved as `CF-Connecting-IP`, then the **first** entry of `X-Forwarded-For`, then `REMOTE_ADDR`.
This helper has no trusted-proxy configuration, so the WordPress edge must strip client-supplied
forwarding headers and write its own. A directly exposed installation should use `REMOTE_ADDR`
instead so a client cannot choose its fingerprint or geolocation.

The hosted service counts the event at the nearest public address in `X-Forwarded-For`, whoever the
sender is, so there is nothing to set up. A self-hosted instance honours the header only from an
address on its trusted-proxy list (`FEASIBLE_INGEST_TRUSTED_PROXIES`); from any other peer it uses
the socket address, which is your web host. There, add your web host's outbound address to that
list. Confirm either one with `X-Debug-Request: true` — the derived event's `client_ip_source`
should read `x-forwarded-for`.

The upstream status code and the `x-feasible-dropped` header come back to the browser unchanged, so
a classified event is still visible to whoever is debugging it. `X-Debug-Request: true` passes
through as well, which means you can answer "why was that event not counted" with one `curl` against
your own domain.

### Two routing modes, and why

With **pretty permalinks** on, the two routes are real paths, registered as rewrite rules.

With **plain permalinks**, a bare path never reaches WordPress at all — there are no rewrite rules in
front of it, so the web server answers the 404 itself and PHP never runs. In that case the plugin
falls back to a query-string route on the site index. It works, it is still unpredictable, and it is
slightly less tidy. The settings screen says which mode you are in rather than leaving you to guess.

### With the proxy off

The snippet loads the script from the analytics host directly (the per-site token path when one is
set) and the browser sends events there. The two routes on this site answer `503`.

### Rotating the paths

If a path is ever listed, press **Rotate paths** on the settings screen. Three new segments are
generated and the rewrite rules are flushed. The old paths stop answering immediately, and any
browser holding a cached copy of the script keeps working until its cache expires.

## Optional measurements

| Measurement | Where it runs | Notes |
|---|---|---|
| Site search | WordPress | Sends `Search` with the normalised term, the result count, and `none`/`some`. |
| 404 errors | WordPress | Sends `404` with the path and the referrer. |
| Outbound links | Browser script | Switch it off and the proxy stops forwarding `Outbound Link: Click`. |
| File downloads | Browser script | Same, for `File Download`. You can also override which extensions count. |
| Form submissions | Browser script | Same, for `Form: Submission`. |

The last three are measured by the script itself and cannot be switched off inside it. The proxy is
the only place the control can honestly live, which is why those three switches need the proxy on —
and why the settings screen says so instead of pretending otherwise. A suppressed event is answered
`202` with `x-feasible-dropped: disabled-in-wordpress`, so it is your choice said out loud rather
than an event that silently vanished.

**Search terms are normalised** — trimmed, lowercased, whitespace collapsed, control characters
removed, cut to a hundred characters. Without that, "Blue Widget", "blue  widget" and "blue widget "
are three rows for one question, and the most common search on the site hides in the tail.

## The dashboard inside wp-admin

Paste a shared-link URL from your analytics account into the settings screen, and the plugin adds a
**Dashboard** page that frames it. The URL is checked to be a `/share/` path on your configured host
before anything is framed — framing an arbitrary URL inside an authenticated admin session is not
something a plugin should offer. The frame loads the link with `embed=true` added, because the
service refuses to be framed without it.

## Installation

1. Install the zip `bin/package.sh` builds (see below), or copy this folder into
   `wp-content/plugins/feasible-analytics`, and activate it.
2. Go to **Feasible → Settings**. Check the site domain matches the site as it is registered, tick
   **Turn on tracking**, and save.
3. Leave the proxy on. The screen shows the two URLs it is serving.
4. Turn on the measurements you want.
5. Optional: paste a shared dashboard link to get the dashboard inside wp-admin.

## Development

The PHP needs no build step and no dependency. The one build output is `assets/js/feasible.js`,
which `tracker/build.js` writes alongside the copy the Go binary embeds. It is committed, so a
checkout of this folder runs as it is.

```bash
# Syntax-check every file. A parse error in a file the tests never load is a
# plugin that white-screens a site on activation.
find . -name '*.php' -print0 | xargs -0 -n1 php -l

# Run the tests. They stub the handful of WordPress functions they need, so they
# run under plain php with no WordPress and no PHPUnit.
php tests/run.php
```

The tests cover the parts worth testing without a WordPress runtime: the event-body sanitiser, the
search-term normaliser, the visitor-IP precedence, the event-suppression list, the shape of the
generated paths, and which share links the dashboard page frames.

### Packaging for the plugin directory

```bash
bin/package.sh
# -> build/feasible-analytics-<version>.zip
```

It rebuilds the tracker from `../../tracker` (running `npm ci` there first if needed), checks that
the version in the plugin header, the `FEASIBLE_VERSION` constant and the readme's stable tag agree,
runs the syntax check and the tests, and zips only what a site needs: the main file,
`uninstall.php`, `readme.txt`, `LICENSE`, `includes/`, `assets/` and `languages/`. The tests, `bin/`
and this README stay out.

Before uploading, run Plugin Check and activate the zip on a clean WordPress with `WP_DEBUG` and
`WP_DEBUG_LOG` on, on the oldest PHP the readme claims and on a current one.

## Translations

Every user-facing string goes through `__()` with the `feasible-analytics` text domain, and
`languages/feasible-analytics.pot` is the catalogue. Drop a `.po`/`.mo` pair beside it to translate.

## Licence

GPL-2.0-or-later, which is the WordPress plugin convention and what the plugin directory requires.
See `LICENSE`.
