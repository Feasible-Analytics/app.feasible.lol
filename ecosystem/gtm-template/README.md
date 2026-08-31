<!--
  README.md
  The Google Tag Manager custom template for feasible.lol.

  Created: 2026-08-30
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Feasible Analytics — Google Tag Manager template

A Google Tag Manager custom template that loads the feasible.lol tracker and sends
events to it. No cookies, no consent banner, and nothing in the tag that reads a
visitor's data layer.

The whole template is one file, `template.tpl`. Everything else here is licence
text, the gallery manifest, and this page.

## Installing it in a container

**From the Community Template Gallery** (once it is published):

1. In your container, open **Templates → Tag Templates → Search Gallery**.
2. Search for **Feasible Analytics** and add it.
3. Review the permissions it asks for. There are three, and they are listed
   under [Permissions](#permissions) below.

**From this repository**, before it is in the gallery, or if you have changed it:

1. Open **Templates → Tag Templates → New**.
2. In the template editor's overflow menu (⋮), choose **Import** and pick
   `template.tpl`.
3. Save it. It now appears under **Custom** when you create a tag.

## The two tag types

One radio button at the top of the tag decides which of the two things the tag
does. Every other field belongs to one of them and hides when the other is
selected.

### Load the script

Injects the tracker and sends the pageview. You need exactly one of these, on an
**Initialization** or **All Pages** trigger.

| Field | What it does |
|---|---|
| Site domain | The site as registered with Feasible, e.g. `example.com`. Required. |
| Analytics host | Where the script comes from and where events go. `https://app.feasible.lol` unless you self-host or proxy. |
| Custom script path | Optional. Defaults to `/js/script.js`. Use your per-site path (`/js/fs-xxxxxxxxxxxxxxxx.js`) or whatever path your proxy serves. |
| Count hash changes as pageviews | For single-page apps that route with `#` fragments. |
| Send pageviews manually | Stops the automatic pageview so you can send them yourself. |
| Count traffic on localhost | Off by default, so your own development traffic stays out of your reports. |

The tag installs the tracker's event queue **before** it injects the script. An
event tag that fires while the script is still downloading pushes onto that
queue, and the tracker replays it on arrival — so an event that beats the script
is delayed, not lost.

Outbound link clicks, file downloads, form submissions, engagement time, scroll
depth and single-page-app navigations are measured by the script itself. There is
no tag to add and no setting to turn on for any of them.

### Send an event

Calls the tracker the script tag already loaded. One tag per event, on whatever
trigger the event belongs to.

| Field | What it does |
|---|---|
| Event name | The name the event is reported under, e.g. `Signup`. The name `pageview` sends a pageview. |
| Custom properties | Key/value rows. Up to 30; names to 300 characters, values to 2000. A row with an empty name is skipped. |
| Revenue amount | Optional. A number, or a variable holding one. |
| Revenue currency | An ISO 4217 code. Required whenever an amount is set. |

Values can be GTM variables, which is the usual way a purchase tag gets its
amount out of the data layer.

Both tag types always end by calling `gtmOnSuccess` or `gtmOnFailure`. A tag that
calls neither shows as "still running" for the life of the page and blocks every
tag sequenced after it, so a misconfigured Feasible tag fails visibly in preview
rather than quietly disabling its neighbours.

## Permissions

The template declares three, and uses all three:

| Permission | Why |
|---|---|
| `inject_script` for `https://app.feasible.lol/*` | Loading the tracker. |
| `access_globals` for `feasible`, `feasible.q`, `__fsc` | Calling the tracker, queueing calls that arrive before it, and handing it the site's configuration. |
| `logging` in **debug** only | The sentence that names a configuration mistake in preview. It is off in production, so a live page's console stays clean. |

An over-broad permission is the commonest reason a gallery submission is
rejected — and it is the fastest thing for a reviewer to check. Nothing here is
wider than the call that needs it.

**If you change the analytics host**, the `inject_script` permission has to change
with it. GTM will not load a script from a URL the template did not declare, and
the tag says so by name in preview rather than failing silently. In the template
editor, open the **Permissions** tab, expand **Injects scripts**, and add your
host's URL pattern (for example `https://analytics.example.com/*`). A template
installed from the gallery cannot be edited this way — import `template.tpl`
directly and edit that copy instead.

## Tests

`template.tpl` ships its own `___TESTS___` block. Open the template in GTM's
template editor and use the **Tests** tab; there is no runner outside GTM. The
scenarios cover the script injecting from the configured host, a custom script
path, an event tag calling the tracker with its name, properties and revenue, an
event that arrives before the script and is queued, and a missing site domain
failing cleanly rather than hanging.

## Submitting it to the Community Template Gallery

The gallery reads a public GitHub repository. It has to look exactly like this:

1. **Put this folder in its own public repository** on GitHub, with
   `template.tpl`, `metadata.yaml` and `LICENSE` in the repository root. The
   gallery does not read subdirectories.
2. **Set the repository description** — it becomes the template's gallery
   description — and add the topic **`gtm-template`**, which is how the gallery
   finds the repository at all.
3. **Fill in `metadata.yaml`.** Set `homepage` to the repository URL, and replace
   the placeholder `sha` with the real commit SHA of the release. A sha that is
   not a commit on the default branch fails validation.
4. **Check `___INFO___` in `template.tpl`**: `displayName`, `description`,
   `categories` and `brand.displayName` are what a person reads in the gallery.
5. **Add the template's logo** as a square PNG at least 96×96 in the repository
   root; the gallery picks it up from there.
6. **Sign in** at
   [tagmanager.google.com/gallery](https://tagmanager.google.com/gallery/) with
   the GitHub account that owns the repository, and submit it.
7. Google reviews it by hand. The two things that come back most often are a
   permission wider than the code needs and a missing or unparseable
   `metadata.yaml`.

Each later release is a new commit plus a new entry at the **top** of `versions`
in `metadata.yaml`. The gallery treats the first entry as current.

## Licence

Apache Licence, Version 2.0. See [LICENSE](LICENSE).

The template is Apache-2.0 so that anyone can fork it, point it at their own
self-hosted instance and publish the result. The rest of feasible.lol is licensed
separately.
