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
make app               # :19301 — application and signed internal routes
make ingest            # :19302 — events, health and metrics
make testsite          # :19303 — a real page with the snippet installed
```

`make dev` runs all three at once. Every runnable target has a `-ts` twin
(`make app-ts`, `make dev-ts`) that binds to the Tailscale address and moves
`FEASIBLE_APP_BASE_URL` with it, so the app is reachable from another machine.
Each process has one listener. Caddy exposes customer paths and denies
`/internal/*` and `/metrics`; trusted services and monitoring connect directly
over the protected network.

`make` on its own lists everything.

### Hosted topology

The single-process self-hosted mode writes directly to account SQLite. Hosted
production separates the failure domains:

```text
public load balancer -> any ingester -> owning app shard -> account SQLite
                              |
                              +-> local persistent buffer.db
```

Each ingester derives the privacy-safe event, discards the raw IP address,
commits the event to its own `FEASIBLE_INGEST_BUFFER_PATH`, and only then
returns `202`. It polls the complete ordered `FEASIBLE_INGEST_SHARDS` list for
domain ownership and removes an outbox row only when the owning app names that
UUID after commit. An app outage delays dashboards while ingesters keep pageviews.
The shard list is a JSON array because its order defines stable shard identity.

App listeners publish authenticated domain and salt snapshots and accept
durable batches alongside dashboard traffic. `FEASIBLE_APP_SHARD_ID` is the
app's one-based stable position in the ingester list. In hosted production these
listeners are reachable only over protected networking and internal requests
use `FEASIBLE_INTERNAL_KEYS`; `/internal/*` is never exposed by the public load
balancer. See [.env.sample](.env.sample) for the complete app
and ingester configuration and [ops/load-balancer.md](ops/load-balancer.md) for
failure and drain behavior.

The shared metadata file on each app shard is `system.db`. An installation from
before that rename must stop all Feasible processes and run `feasible db migrate`
once; the command moves the former filename and SQLite sidecars before applying
migrations.

### Local services you do not need

- **Email.** `FEASIBLE_APP_MAIL_TRANSPORT=log` prints the message to stdout and
  writes the rendered HTML to `tmp/mail/*.html`. No SMTP server, no mail catcher.
- **Geolocation.** A missing GeoIP database degrades to "unknown" rather than
  failing. An optional data file must never stop you running the app.
- **Stripe.** Webhooks come through the CLI:

  ```bash
  stripe listen --forward-to localhost:19301/webhooks/stripe
  ```

  Before a hosted deployment takes traffic, verify the configured product,
  prices, webhook event subscriptions, Managed Payments activation, accepted
  terms and tax-code eligibility with:

  ```bash
  feasible billing preflight --checkout-smoke
  ```

  The smoke creates no customer or charge and immediately expires monthly and
  yearly Checkout Sessions. The read-only form, without `--checkout-smoke`,
  reports the Stripe Dashboard-only checks as required and exits non-zero rather
  than claiming the deployment is ready.

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

**Disk is the number that grows.** A million pageviews is about 300 MB once it
is indexed and summarised — roughly 210 bytes an event, measured rather than
guessed. Raw rows age out and roll-ups do not, so a site sending a million a
month settles well below twelve times that a year. Leave the write-ahead logs
headroom: they are checkpointed automatically, but a full disk is not a state
SQLite can write its way out of.

**Throughput.** One process sustains around six thousand events a second through
the whole accept path, which is far more than a site sending a million pageviews
a month generates — that is under half an event a second on average. Accepting
and deriving an event costs about thirteen microseconds; a success response
still waits for a durable transaction, either the direct account commit or the
hosted ingester outbox commit. Reports
read from summary tables in under a tenth of a second over a year of data; the
same report from raw rows takes seconds, which is why the roll-up worker exists.

**Building** needs Node as well, because the dashboard and the stylesheet are
compiled before Go embeds them. Running never does.

## Watching it run

Every process serves two probes and a metrics endpoint on its single listener:

```bash
curl localhost:19301/health/live     # is the process up
curl localhost:19301/health/ready    # can it serve, component by component
curl localhost:19301/metrics         # Prometheus text format (app)
curl localhost:19302/metrics         # the ingest tier's own
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

### Team membership API

`PUT /api/v1/teams/memberships` creates a revocable invitation that expires in
48 hours; it never inserts a membership directly. The response includes the
invitation id, normalized email, role, expiry, and one-time invitation token,
with the same status and shape whether or not the address already has an
account. `owner` is not an invitational role: ownership changes only through the
ownership-transfer workflow. The verified recipient must accept the invitation
before gaining access.

`PUT /api/v1/sites/guests` follows the same rule for site-only access. It
creates a revocable 48-hour `guest_viewer` or `guest_editor` invitation for any
email address and returns the same response shape for existing and unknown
accounts. No `guest_memberships` row exists until the verified recipient
accepts. `DELETE /api/v1/sites/guest-invitations/{invitation_id}?site_id=...`
revokes an outstanding site invitation.

### When it goes wrong

[`ops/`](ops/) holds the operational half: continuous replication with
Litestream, the load balancer's health-check settings, a runbook per failure this
system actually has, and the game day that breaks things on purpose to check the
runbooks are true. [`ops/README.md`](ops/README.md) lists every metric the binary
emits, which is also the list a runbook is allowed to name.

## Cutting a release

Releases are deliberate stability checkpoints, not an automatic result of every
merge. When `main` is in a state we are confident shipping, open the repository's
**Actions** tab, select **Release**, choose **Run workflow**, and run it from
`main`. This is always a manual decision.

The optional version field accepts either `1.2.3` or `v1.2.3`. Leave it blank
for the normal release: the workflow increments the patch component of the most
recent semantic release tag (`x.y.z`). A repository with no previous release
starts at `v0.0.1`.

The workflow snapshots current `main`, creates the version tag and GitHub
Release, and attaches Linux, Windows and macOS builds for amd64 and arm64 plus a
SHA-256 checksum manifest. Do not run it until the pull requests making up the
release are merged and `main` is stable.

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
