<!--
  README.md
  The Python SDK for feasible.lol server-side event tracking.

  Created: 2026-08-30
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# feasible

Server-side event tracking for [feasible.lol](https://feasible.lol). Python 3.9 or newer, standard
library only — no `requests`, nothing to conflict with what your application already installs.

## Read this first: forward the visitor's IP and User-Agent

A browser sends its own address and its own `User-Agent`. Your server does not. When you call the
ingest endpoint from Python, **you** have to forward both, and if you do not, the request arrives from
a datacentre address with nothing to identify the visitor. The server classifies it as a bot and drops
it. You get a `202`, your code logs a success, and the numbers are quietly wrong for weeks.

So this SDK makes both **required arguments**:

```python
client.pageview(
    url="https://example.com/pricing",
    client_ip="203.0.113.9",       # the VISITOR's address, never your server's
    user_agent="Mozilla/5.0 ...",  # the VISITOR's agent, never your HTTP client's
)
```

Leave either one empty and you get a `MissingClientIPError` or a `MissingUserAgentError` naming the
parameter. Nothing is sent. That is deliberate — a loud failure in development is cheaper than silent
data loss in production.

The easy way to get both right is to read them off the request you are already handling:

```python
from feasible import Client, Visitor

client = Client(domain="example.com")

# Django, Flask, Starlette, FastAPI — anything with a headers mapping.
visitor = Visitor.from_request(headers=request.headers, remote_addr=request.remote_addr)

# Bare WSGI.
visitor = Visitor.from_wsgi(environ)

client.pageview(url="https://example.com/pricing", **visitor.as_kwargs())
```

Both helpers take `CF-Connecting-IP`, then the **first** entry of `X-Forwarded-For`, then the socket
address. They have no trusted-proxy configuration. Use them only behind an application edge that
strips client-supplied forwarding headers and writes its own. On a directly exposed app, pass the
socket address explicitly so a client cannot choose its fingerprint or geolocation.

The client sends the visitor's address as `X-Forwarded-For`. **The ingest server honours that header
only from an address on its trusted-proxy list** (`FEASIBLE_INGEST_TRUSTED_PROXIES`); from any other
peer it uses the socket address, which is your server. On a self-hosted instance, add the address your
application calls from to that list. Check it with `client.debug()`: the derived event's `client_ip_source` is
`x-forwarded-for` when the header was used and `socket` when it was not.

## Install

```bash
pip install feasible
```

## Sending events

```python
from feasible import Attribution, Client, Revenue

client = Client(
    domain="example.com",              # the site as registered, not the page URL
    host="https://app.feasible.lol",   # your own host if you self-host
    timeout=5.0,
    max_attempts=3,
)

client.pageview(
    url="https://example.com/pricing",
    client_ip=ip,
    user_agent=agent,
    title="Pricing",
    referrer="https://news.example/story",
)

result = client.event(
    name="Purchase",
    url="https://example.com/checkout/complete",
    client_ip=ip,
    user_agent=agent,
    props={"plan": "pro", "seats": 4},
    revenue=Revenue(amount=49.50, currency="USD"),
)
```

Every optional field of the wire contract is available: `title`, `referrer`, `props`, `revenue`,
`interactive`, `scroll_depth`, `engagement_time` and `viewport_width`.

### Offline and delayed conversions

An event that happens hours later — a webhook, a phone order, a refund — has no referrer of its own
and would be filed as Direct forever. Pass the attribution you already know:

```python
client.event(
    name="Purchase",
    url="https://example.com/orders/1024",
    client_ip=ip,
    user_agent=agent,
    revenue=Revenue(amount=250.00, currency="USD"),
    attribution=Attribution(
        referrer="https://news.example/story",
        utm_source="newsletter",
        utm_medium="email",
        utm_campaign="august",
    ),
)
```

The server applies these overrides to any event that carries them; nothing about them is specific to
a server-side caller.

## The result, and why an event might not be counted

The endpoint answers `202` for everything it understood — **including events it decided to drop**. The
reason comes back in a header, and this SDK surfaces it rather than swallowing it:

```python
result = client.pageview(url=url, client_ip=ip, user_agent=agent)

if result.was_dropped:
    # "datacenter_ip", "bot_user_agent", "spam_referrer", and so on.
    log.warning("feasible dropped the event: %s", result.dropped)
```

A dropped `202` is a classification, not a failure. It is never retried — the same event sent again
reaches the same classifier.

### Debugging one event

```python
derived = client.debug(name="pageview", url="https://example.com/", client_ip=ip, user_agent=agent)
```

`debug()` sends `X-Debug-Request: true` and returns the event the server *would* have written —
country, browser, source, channel and any drop reason — without writing anything. It is free of side
effects and safe to run against production.

## Testing your analytics

**This is the supported way to test analytics: turn the client into a no-op and assert on what it
recorded.** No network, no mock of this SDK, no test double that stops matching the payload the day
the wire contract gains a field.

```python
client = Client(domain="example.com", disabled=True)

checkout.complete(order)   # your code, which calls client.event(...)

events = client.recorded_events
assert len(events) == 1
assert events[0].name == "Purchase"
assert events[0].props["plan"] == "pro"
assert events[0].client_ip == "203.0.113.9"
```

Set `FEASIBLE_DISABLED=1` in the environment to do the same thing without touching application code —
what a CI container or a local development machine wants.

A disabled client still refuses a call with no address or no user agent. That is the point: the
mistake gets caught by your test suite instead of by a customer.

## Retries

Transport errors, `429` and `5xx` are retried with exponential backoff, capped, and jittered so a
fleet of servers that all failed at the same moment does not retry in lockstep. Three attempts by
default.

Nothing else is retried. A `400` raises `BadRequestError` immediately, carrying the server's own
explanation — it means the payload or the forwarded headers are wrong, and sending the same bytes
again gets the same answer.

## Errors

Everything inherits from `FeasibleError`, so one `except` contains the lot. Analytics should never be
the reason a checkout fails:

```python
from feasible import FeasibleError

try:
    client.event(name="Purchase", url=url, client_ip=ip, user_agent=agent)
except FeasibleError as error:
    log.warning("feasible: %s", error)
```

| Error | Meaning |
|---|---|
| `MissingClientIPError` | `client_ip` was empty. Nothing was sent. |
| `MissingUserAgentError` | `user_agent` was empty. Nothing was sent. |
| `InvalidEventError` | The event could not be built — a missing name or URL, a bad currency. |
| `BadRequestError` | The endpoint answered `400`. Never retried. |
| `APIError` | Any other unsuccessful status, including a `5xx` that outlived the retries. |
| `TransportError` | The request never arrived, on every attempt. |

## Custom transport

Pass anything with the `Transport.send` signature to route requests through your own HTTP client, a
proxy, or your instrumentation:

```python
client = Client(domain="example.com", transport=MyTransport())
```

## Tests

```bash
python3 -m unittest discover
```

The suite starts a real HTTP server on an ephemeral port inside the test process, asserts on the
bytes and headers that actually left, and shuts it down again.

## Licence

MIT. See `LICENSE`.
