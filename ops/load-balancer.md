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
| `/internal/*` | **none — return 404** | Private protocol and operations endpoints never reach the public edge |
| `/api/event*` | ingest tier | The front door scales without dragging the dashboard along |
| everything else | app tier | Dashboard, API, tracker script, static assets |

The edge denies `/internal/*`. In local and single-host deployments the second
listener stays on loopback. Hosted app shards bind it to a private interface
with TLS and HMAC authentication because ingesters poll `/internal/domains` and
`/internal/salts` and deliver batches to `/internal/ingest`. Each configured
shard URL addresses one owning shard; it is not a round-robin app URL.

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
| `system_db` | required | **not opened** | app 503 |
| `account_directory` | required | **not opened** | app 503 |
| `outbox` | not used in direct mode | required | ingest 503 |
| `salts` | required | required from memory; refreshed privately | 503 when no current salt remains |
| `geolocation` | **optional** | **optional** | still 200 |
| `routing_map` | required — **built** | required — live or disk-cached snapshot built | 503 |

`routing_map` is the one place the two shapes genuinely differ. An app with no
sites is a fresh install. An ingester with an incomplete map retains known
routes and holds unknown claims in `buffer.db`; it drops an unknown domain only
after every configured shard has checked in within 60 seconds. App unavailability
therefore does not remove a healthy ingester from the event load balancer.

## Degraded is not dead

A degraded component is reported and **never changes the top-level status**. The
process answers 200 and keeps taking traffic.

```json
{
  "status": "ready",
  "components": [
    {"name": "system_db", "status": "ok"},
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
    {"name": "system_db", "status": "ok"},
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

**Fail open matters because readiness failures can be correlated.** App shards
have separate account storage, while each ingester has a separate durable
outbox volume. A load balancer that removes every event target on a shared
configuration or salt failure has converted retryable backpressure into a total
edge outage. Where the platform offers it, prefer sending traffic to unhealthy
targets over sending it nowhere.

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

## Event-serving processes own separate durable storage

Every ingester has its own persistent `buffer.db` volume and never opens an app
shard's `system.db` or account databases. A public `202` follows the local
outbox commit. The ingester removes that row only after the owning app responds
with the exact UUID it committed. An ingester may be replaced freely only after
its queue drains or its volume moves with it.

## Two things to verify after any load-balancer change

**The visitor's address is still the visitor's address.** The ingest tier reads
`X-Feasible-IP`, then `CF-Connecting-IP`, then `X-Forwarded-For` from right to
left until the first untrusted hop, and finally the socket peer. All forwarded
headers are ignored unless the socket peer is on
`FEASIBLE_INGEST_TRUSTED_PROXIES`. A load balancer omitted from that list, or
one that does not set a client-address header, collapses every visitor into its
own address: one country, one fingerprint, no error anywhere.
Every trusted edge must strip or overwrite client-supplied `X-Feasible-IP` and
`CF-Connecting-IP`; those headers take precedence and an allow-list cannot tell
whether the proxy created them or merely passed them through. An edge may append
`X-Forwarded-For`, because the right-to-left walk ignores spoofed entries before
the address that edge observed.

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
