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
