<!--
load-balancer.md
Health checks on both tiers, and the difference between downgraded and dead.

Created: 2026-08-31
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# The load balancer

A managed load balancer sits in front of both public process groups. Not a
reverse proxy on one box: that box would become the single point of failure.

## Routing

The same three rules the local `Caddyfile` uses, so a routing bug found on a
laptop is the routing bug production would have had.

| Path | Target group | Why |
|---|---|---|
| `/internal/*` | **none — return 404** | Health and metrics belong on the loopback listener |
| `/api/event*` | ingest tier | The front door scales without dragging the dashboard along |
| everything else | app tier | Dashboard, API, tracker script, static assets |

The edge denies `/internal/*`, and operational routes live on a second listener
bound to `127.0.0.1`. Current main exposes health and metrics there; it has no
internal delivery, routing, or salt-distribution endpoint.

This holds in Tailscale mode too. `FEASIBLE_APP_INTERNAL_LISTEN` and
`FEASIBLE_INGEST_INTERNAL_LISTEN` stay on loopback there, so the internal
listener is not reachable from any device on the tailnet either.

## Liveness and readiness are different questions

Both listeners answer both paths. They are not interchangeable.

| | `/health/live` | `/health/ready` |
|---|---|---|
| Question | Is this process running | May it take traffic right now |
| Checks | **nothing** | every registered dependency |
| Goes false | never, until the process exits | during shutdown, and while a required dependency is down |
| Read by | the supervisor, for the restart decision | the load balancer, for the traffic decision |
| Body | `ok` | JSON, one line per component |

**Liveness deliberately checks nothing.** A liveness probe that failed on a slow
database would turn one slow database into a restart loop across every replica at
once — every instance fails the probe simultaneously, every instance is killed,
and the thing that was merely slow is now down.

**Never point the load balancer's health check at `/health/live`.** It is always
200 from the moment the listener binds, so it would keep sending traffic to a
process that cannot serve it, including one that is shutting down.

## What each process considers required

Registered in code, not configurable, and different per process on purpose.

| Component | App (`serve`) | Ingest (`ingest`) | If it fails |
|---|---|---|---|
| `control_db` | required | required | 503 |
| `account_directory` | required | required | 503 |
| `salts` | required | required | 503 |
| `geolocation` | **optional** | **optional** | still 200 |
| `routing_map` | required — **built** | required — **not empty** | 503 |

`routing_map` is the one place the two shapes genuinely differ. **An ingestor
with an empty map answers 202 to everything and drops it all**, so an empty map
is unready. **An app with no sites is a fresh install**, and refusing it traffic
would mean nobody could reach the page where they add the first site — so the app
only asks whether the map has been built.

## Degraded is not dead

A degraded component is reported and **never changes the top-level status**. The
process answers 200 and keeps taking traffic.

```json
{
  "status": "ready",
  "components": [
    {"name": "control_db", "status": "ok"},
    {"name": "account_directory", "status": "ok"},
    {"name": "salts", "status": "ok"},
    {"name": "geolocation", "status": "degraded",
     "detail": "no geolocation database is loaded — countries will be unknown"},
    {"name": "routing_map", "status": "ok"}
  ]
}
```

That is a **200**. A missing geolocation database means countries are unknown,
which is a worse dashboard and not a broken one; a process that refused traffic
over an optional data file would turn a downgraded report into an outage, and it
would do it on every instance at once because they all share the same missing
file.

**Configure the load balancer on the status code alone.** Do not write a health
check that parses the body and treats `"degraded"` anywhere in it as unhealthy —
that is exactly the mistake this design is shaped to prevent. The body is for the
person the page wakes up, not for the load balancer.

A failed **required** component is a different thing entirely: status
`not_ready`, HTTP **503**, and the component that failed is named with its error.

```json
{
  "status": "not_ready",
  "components": [
    {"name": "control_db", "status": "ok"},
    {"name": "account_directory", "status": "ok"},
    {"name": "salts", "status": "ok"},
    {"name": "geolocation", "status": "ok"},
    {"name": "routing_map", "status": "failed",
     "detail": "the routing map is empty — every event would be dropped as an unknown site"}
  ]
}
```

Every check still runs after one has failed. The first failure is rarely the
whole story, and a report that stopped at it would send somebody to fix the
wrong thing.

## Settings

| Setting | Value | Why this number |
|---|---|---|
| Health check path | `/health/ready` | Not `/health/live` — see above |
| Protocol | HTTP | TLS terminates at the load balancer |
| Healthy status codes | `200` only | `503` is the process asking to be left alone |
| Interval | 5 s | |
| Timeout | 2 s | The probe pings the database rather than querying it, so it is fast or it is broken |
| Unhealthy threshold | 2 | Ten seconds to remove a failing instance |
| Healthy threshold | 2 | Ten seconds to bring one back, so a flapping process does not oscillate |
| Idle timeout | 60 s | **Must stay below the app's 90 s** |
| Fail open when no target is healthy | on, if available | |

**The idle timeout is not a preference.** The app closes an idle keep-alive
connection after 90 seconds. A load balancer with a *longer* idle timeout keeps
reusing a connection the app has already closed, which surfaces as sporadic 502s
under low traffic and nothing at all under load — one of the hardest failures to
reproduce on purpose.

**Fail open matters because readiness failures here are correlated.** Every
instance in a tier reads the same `control.db` and the same data directory, so a
required component that fails tends to fail everywhere at once. A load balancer
that removes every target and then serves nothing has converted a degraded tier
into a total outage. Where the platform offers it, prefer sending traffic to
unhealthy targets over sending it nowhere.

## Draining, and why the deploy script deregisters explicitly

On SIGTERM the process flips readiness to false, **waits two seconds**, then
stops accepting and gives in-flight requests up to fifteen seconds to finish.

Two seconds is not enough for a load balancer checking every five seconds with a
threshold of two to notice. That is deliberate: **readiness draining is the
safety net for unplanned restarts, not the mechanism for planned ones.** For a
deploy, deregister first.

```bash
# 1. Deregister this instance from the load balancer (platform-specific).
# 2. Wait until it owes nothing.
scripts/drain.sh http://127.0.0.1:19402/metrics
# 3. Now send SIGTERM.
```

`scripts/drain.sh` polls `feasible_ingest_buffer_events` until it reaches zero
and **exits non-zero if it does not**. A buffer that will not drain means direct
account storage is slow or unavailable; see
[runbooks/write-buffer-growing.md](runbooks/write-buffer-growing.md). A metrics
endpoint it cannot read is also a failure, not an empty buffer.

Only unplanned hardware failure should ever be exposed to this. Everything else
is a deregistration, a drain, and then a termination.

## Event-serving processes share authoritative storage

Every process that can receive `/api/event` must see the same `control.db`,
account directories, and salt key. A 202 follows the account commit, so there is
no per-instance outbox volume to recover. Pointing replicas at separate local
directories creates divergent routing, receipts, sessions, and salts and is not
a supported high-availability shape.

## Two things to verify after any load-balancer change

**The visitor's address is still the visitor's address.** The ingest tier reads
`X-Feasible-IP` (only from an address on `FEASIBLE_INGEST_TRUSTED_PROXIES`), then
`CF-Connecting-IP`, then the first entry of `X-Forwarded-For`, then the socket
peer. A load balancer that does not set `X-Forwarded-For` collapses every visitor
in the world into one address: one country, one fingerprint, no error anywhere.

```bash
curl -s -X POST https://<public-host>/api/event \
  -H 'content-type: application/json' \
  -H 'X-Debug-Request: true' \
  -d '{"n":"pageview","d":"example.com","u":"https://example.com/"}'
```

The reply names `client_ip` and `client_ip_source`. `client_ip_source` of
`socket` behind a load balancer is the bug; `x-forwarded-for` is correct.

**`FEASIBLE_APP_BASE_URL` is the public hostname.** Cookies do not set, redirects
bounce and OAuth rejects the redirect URI when it is wrong, all with no useful
error message. `feasible serve -check` prints the resolved value and exits
without listening.
