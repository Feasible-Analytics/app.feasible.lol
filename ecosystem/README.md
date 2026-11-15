<!--
  README.md
  What lives under ecosystem/, and why it is here rather than in its own repository.

  Created: 2026-08-30
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# ecosystem

The integrations that live outside the binary: the WordPress plugin, the server-side SDKs, the tag
manager template, the browser package and the Looker Studio connector.

**Every folder here is destined to become its own repository.** They are staged together only so the
first version can be written and reviewed in one place against the wire contract they all have to
match. Each one is already self-contained — its own README, its own licence, its own manifest, its
own tests, no path reaching outside its own directory — so splitting one out is `git subtree split`
and a `gh repo create`, with no rework.

| Folder | Becomes | Licence | Why that licence |
|---|---|---|---|
| `wordpress-plugin/` | `feasible-wordpress` | GPL-2.0-or-later | WordPress plugin convention, and the directory requires it. |
| `sdk-go/` | `feasible-go` | MIT | An SDK that a customer must relicense to use is an SDK nobody uses. |
| `sdk-php/` | `feasible-php` | MIT | Same. |
| `sdk-python/` | `feasible-python` | MIT | Same. |
| `sdk-ruby/` | `feasible-ruby` | MIT | Same. |
| `sdk-node/` | `feasible-node` | MIT | Same. |
| `npm-tracker/` | `feasible-tracker-npm` | MIT | A loader, not the script. The script itself is served from the analytics host and stays AGPL-3.0-or-later. |
| `gtm-template/` | `feasible-gtm-template` | Apache-2.0 | What the tag manager community gallery expects. |
| `looker-studio-connector/` | `feasible-looker-studio` | MIT | Apps Script, deployed by the customer into their own Google account. |

## What has been split out

Two folders have their public repository. The copy here is where a change is written and reviewed;
the public repository is what people install from, so a change lands in both.

| Folder | Public repository | How it is released |
|---|---|---|
| `sdk-go/` | [Feasible-Analytics/feasible-go](https://github.com/Feasible-Analytics/feasible-go) | A `v*` tag on `main`. `go get` reads it straight from GitHub. |
| `gtm-template/` | [Feasible-Analytics/feasible-gtm-template](https://github.com/Feasible-Analytics/feasible-gtm-template) | A new entry at the top of `versions` in `metadata.yaml`, naming the commit in that repository. |

The main product stays AGPL-3.0-or-later. Nothing in this directory is linked into the binary, and
nothing in the binary depends on anything here.

## The one thing every server-side SDK exists to prevent

A server-side call that does not forward the visitor's real IP and the visitor's real User-Agent is
counted at the caller's own server: no location, every visitor merged into one, and usually filed as
`datacenter_ip` and left out of the reports. That single mistake is the largest support burden in this
product category, and it is invisible: the endpoint answers `202` either way.

So in every SDK here, both are **required arguments**, and every one of them ships a helper that
takes them off the incoming request rather than making the caller remember. A self-hosted install helps too —
a request arriving from a datacentre address with neither is answered with a `400` and a sentence
naming what is missing. The hosted service answers it `202` and files it as `datacenter_ip`.

## The wire contract they all match

`POST <host>/api/event`, `Content-Type: text/plain`, a JSON body of short keys — `n` name, `u` url,
`d` domain, `r` referrer, `p` props, `t` title, `$` revenue — and the two headers above. The endpoint
answers `202` for everything it understood, including events it decided to drop, with the reason in
the `x-feasible-dropped` response header. `X-Debug-Request: true` returns the derived event instead
of writing it, which is how a customer debugs their own integration with one `curl`.

The full contract is in `internal/ingest/payload.go` and `internal/ingest/handler.go`. If those and
one of these disagree, those are right.

## What is deliberately not here

**Mobile SDKs.** iOS, Android and React Native are scoped but not built. They are not a thin wrapper
around the browser script — a hybrid app serves from `localhost` or `file://`, which the script
ignores by default — so they need their own session handling, their own offline queue and their own
lifecycle model, and doing that badly is worse than not doing it.
