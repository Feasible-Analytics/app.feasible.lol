<!--
  README.md
  feasible.lol — open-source, privacy-first web analytics.

  Created: 2026-08-30
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# feasible.lol

Privacy-first web analytics. One Go binary. One SQLite database per account.

No cookies. No personal data. No consent banner. GDPR, CCPA and PECR compliant by
construction, and a tracking script under 3 KB.

## Status

Early development. Nothing here works yet.

## Why

Good privacy-friendly analytics exists, but it is priced per pageview and its best
features sit behind the top tier. Self-hosting it means Docker, PostgreSQL,
ClickHouse and at least 2 GB of RAM.

feasible.lol takes a different position:

- **One binary.** No Docker, no external database, no reverse proxy required. It
  runs on a $5 VPS or a Raspberry Pi, on any CPU architecture.
- **One price.** $9.99/month or $100/year, up to a million pageviews a month.
  Every feature included — funnels, custom properties, revenue tracking, the API.
  No tiers.
- **Self-hosting is complete and free.** Not a reduced edition. The same build,
  every feature, every release.

## Running it

You need Go 1.23 or newer. Caddy is only needed for the three-process mode, and
Node only once there is a dashboard to compile.

```bash
cp .env.sample .env
make build
make dev-solo          # one process, everything in it — the self-hoster path
```

For the production shape, run each process in its own terminal so its log stream
stays readable:

```bash
make caddy             # :19300 — the only port you open in a browser
make app               # :19301, plus :19401 internal, loopback only
make ingest            # :19302
make testsite          # :19303 — a real page with the snippet installed
```

`make dev` runs all three at once. Every runnable target has a `-ts` twin
(`make app-ts`, `make dev-ts`) that binds to the Tailscale address and moves
`FEASIBLE_APP_BASE_URL` with it, so the app is reachable from another machine.
The internal listener stays on `127.0.0.1` in every mode.

`make` on its own lists everything.

### Local services you do not need

- **Email.** `FEASIBLE_APP_MAIL_TRANSPORT=log` prints the message to stdout and
  writes the rendered HTML to `tmp/mail/*.html`. No SMTP server, no mail catcher.
- **Geolocation.** A missing GeoIP database degrades to "unknown" rather than
  failing. An optional data file must never stop you running the app.
- **Stripe.** Webhooks come through the CLI:

  ```bash
  stripe listen --forward-to localhost:19301/webhooks/stripe
  ```

## What it needs to run

One binary, one directory, no Docker and no database server. These are measured
numbers rather than guesses — see `internal/bench/RESULTS.md` for how, and for
the machine they were taken on.

| | Minimum | Comfortable |
|---|---|---|
| CPU | 1 core | 2 cores |
| Memory | 512 MB | 2 GB |
| Disk | 1 GB plus your data | SSD, and see below |
| OS | Linux, macOS or BSD, x86-64 or arm64 | |
| Anything else | nothing | |

**Disk is the number that grows.** An event is roughly 250 bytes with its
indexes, so a million pageviews a month is about a gigabyte a year before
roll-ups and rather less after old raw rows age out. Give the write-ahead logs
room: they are checkpointed automatically, but a busy install wants headroom
rather than a full disk.

**Throughput.** One process sustains a few thousand events a second on a modern
laptop core, which is far more than a site sending a million pageviews a month
generates — that is under half an event a second on average. Reports read from
summary tables in the low hundreds of milliseconds over a year of data; the same
report from raw rows takes seconds, which is why the roll-up worker exists.

**Building** needs Node as well, because the dashboard and the stylesheet are
compiled before Go embeds them. Running never does.

## Watching it run

Every process serves two probes and, on its loopback listener, a metrics
endpoint:

```bash
curl localhost:19301/health/live     # is the process up
curl localhost:19301/health/ready    # can it serve, component by component
curl localhost:19401/metrics         # Prometheus text format (app)
curl localhost:19402/metrics         # the ingest tier's own
```

`/health/live` checks nothing on purpose: a liveness probe that failed on a slow
database would turn one slow database into a restart loop everywhere at once.
`/health/ready` returns 503 with a JSON body naming every dependency and what
was wrong with it, so a failure is a diagnosis rather than a word.

The metrics endpoint is loopback-only and carries no customer data — no site,
domain, path or country appears as a label, and no IP address exists anywhere in
this system to leak. Drop counts by reason, write-buffer depth, roll-up freshness
and report latency are all there; per-site numbers belong to the customer and
live on their own ingestion-health panel.

### Before you push

```bash
make test
make lint
make check-env         # every environment variable is documented in .env.sample
```

## License

[GNU AGPL-3.0-or-later](LICENSE).

You may run, modify and self-host this freely. If you offer it to others as a
network service, you must make your source available to those users.
