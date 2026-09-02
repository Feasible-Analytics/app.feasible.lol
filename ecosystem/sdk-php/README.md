<!--
  README.md
  The PHP SDK for feasible.lol server-side event tracking.

  Created: 2026-08-30
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# feasible-php

Server-side event tracking for [feasible.lol](https://feasible.lol). PHP 8.1 or newer, no runtime
dependencies, cURL when you have it and a stream context when you do not.

## Read this first: forward the visitor's IP and User-Agent

A browser sends its own address and its own `User-Agent`. Your server does not. When you call the
ingest endpoint from PHP, **you** have to forward both, and if you do not, the request arrives from a
datacentre address with nothing to identify the visitor. The server classifies it as a bot and drops
it. You get a `202`, your code logs a success, and the numbers are quietly wrong for weeks.

So this SDK makes both **required arguments**:

```php
$client->pageview(
    url: 'https://example.com/pricing',
    clientIp: '203.0.113.9',        // the VISITOR's address, never your server's
    userAgent: 'Mozilla/5.0 ...',   // the VISITOR's agent, never your HTTP client's
);
```

Leave either one empty and you get a `MissingClientIpException` or a `MissingUserAgentException`
naming the parameter. Nothing is sent. That is deliberate — a loud failure in development is cheaper
than silent data loss in production.

The easy way to get both right is to read them off the request you are already handling:

```php
use Feasible\Client;
use Feasible\Visitor;

$client  = new Client(domain: 'example.com');
$visitor = Visitor::fromRequest();   // reads $_SERVER; pass your own array to test it

$client->pageview(...$visitor->args(), url: 'https://example.com/pricing');
```

`Visitor::fromRequest()` takes `CF-Connecting-IP`, then the **first** entry of
`X-Forwarded-For`, then `REMOTE_ADDR`. It has no trusted-proxy configuration. Use it only behind an
application edge that strips client-supplied forwarding headers and writes its own. On a directly
exposed app, pass `REMOTE_ADDR` explicitly so a client cannot choose its fingerprint or
geolocation.

The client sends the visitor's address as `X-Forwarded-For`. **The ingest server honours that header
only from an address on its trusted-proxy list** (`FEASIBLE_INGEST_TRUSTED_PROXIES`); from any other
peer it uses the socket address, which is your server. On a self-hosted instance, add the address your
application calls from to that list. Check it with `$client->debug()`: the derived event's `client_ip_source` is
`x-forwarded-for` when the header was used and `socket` when it was not.

## Install

```bash
composer require feasible/feasible-php
```

Or drop `src/` in and point a PSR-4 autoloader at `Feasible\`. There are no dependencies to install.

## Sending events

```php
use Feasible\Attribution;
use Feasible\Client;
use Feasible\Revenue;

$client = new Client(
    domain: 'example.com',                 // the site as registered, not the page URL
    host: 'https://app.feasible.lol',      // your own host if you self-host
    timeout: 5.0,
    maxAttempts: 3,
);

// A pageview.
$client->pageview(
    url: 'https://example.com/pricing',
    clientIp: $ip,
    userAgent: $agent,
    title: 'Pricing',
    referrer: 'https://news.example/story',
);

// A custom event with properties and revenue.
$result = $client->event(
    name: 'Purchase',
    url: 'https://example.com/checkout/complete',
    clientIp: $ip,
    userAgent: $agent,
    props: ['plan' => 'pro', 'seats' => 4],
    revenue: new Revenue(amount: 49.50, currency: 'USD'),
);
```

Every optional field of the wire contract is available: `title`, `referrer`, `props`, `revenue`,
`interactive`, `scrollDepth`, `engagementTime` and `viewportWidth`.

### Offline and delayed conversions

An event that happens hours later — a webhook, a phone order, a refund — has no referrer of its own
and would be filed as Direct forever. Pass the attribution you already know:

```php
$client->event(
    name: 'Purchase',
    url: 'https://example.com/orders/1024',
    clientIp: $ip,
    userAgent: $agent,
    revenue: new Revenue(amount: 250.00, currency: 'USD'),
    attribution: new Attribution(
        referrer: 'https://news.example/story',
        utmSource: 'newsletter',
        utmMedium: 'email',
        utmCampaign: 'august',
    ),
);
```

The server applies these overrides to any event that carries them; nothing about them is specific to
a server-side caller.

## The result, and why an event might not be counted

The endpoint answers `202` for everything it understood — **including events it decided to drop**. The
reason comes back in a header, and this SDK surfaces it rather than swallowing it:

```php
$result = $client->pageview(url: $url, clientIp: $ip, userAgent: $agent);

if ($result->wasDropped()) {
    // "datacenter_ip", "bot_user_agent", "spam_referrer", and so on.
    error_log('feasible dropped the event: ' . $result->dropped);
}
```

A dropped `202` is a classification, not a failure. It is never retried — the same event sent again
reaches the same classifier.

### Debugging one event

```php
$derived = $client->debug(
    name: 'pageview',
    url: 'https://example.com/',
    clientIp: $ip,
    userAgent: $agent,
);
```

`debug()` sends `X-Debug-Request: true` and returns the event the server *would* have written —
country, browser, source, channel and any drop reason — without writing anything. It is free of side
effects and safe to run against production.

## Testing your analytics

**This is the supported way to test analytics: turn the client into a no-op and assert on what it
recorded.** No network, no mock of this SDK, no test double that stops matching the payload the day
the wire contract gains a field.

```php
$client = new Client(domain: 'example.com', disabled: true);

$checkout->complete($order);   // your code, which calls $client->event(...)

$events = $client->recordedEvents();
assert(count($events) === 1);
assert($events[0]->name() === 'Purchase');
assert($events[0]->props()['plan'] === 'pro');
assert($events[0]->clientIp === '203.0.113.9');
```

Set `FEASIBLE_DISABLED=1` in the environment to do the same thing without touching application code —
what a CI container or a local development machine wants.

A disabled client still refuses a call with no address or no user agent. That is the point: the
mistake gets caught by your test suite instead of by a customer.

## Retries

Transport errors, `429` and `5xx` are retried with exponential backoff, capped, and jittered so a
fleet of servers that all failed at the same moment does not retry in lockstep. Three attempts by
default.

Nothing else is retried. A `400` throws `BadRequestException` immediately, carrying the server's own
explanation — it means the payload or the forwarded headers are wrong, and sending the same bytes
again gets the same answer.

## Exceptions

Everything extends `Feasible\Exception\FeasibleException`, so one catch block contains the lot.
Analytics should never be the reason a checkout fails:

```php
try {
    $client->event(name: 'Purchase', url: $url, clientIp: $ip, userAgent: $agent);
} catch (\Feasible\Exception\FeasibleException $e) {
    error_log('feasible: ' . $e->getMessage());
}
```

| Exception | Meaning |
|---|---|
| `MissingClientIpException` | `clientIp` was empty. Nothing was sent. |
| `MissingUserAgentException` | `userAgent` was empty. Nothing was sent. |
| `InvalidEventException` | The event could not be built — a missing name or URL, a bad currency. |
| `BadRequestException` | The endpoint answered `400`. Never retried. |
| `ApiException` | Any other unsuccessful status, including a `5xx` that outlived the retries. |
| `TransportException` | The request never arrived, on every attempt. |

## Custom transport

Pass anything implementing `Feasible\Transport\Transport` to route requests through your own HTTP
client, a proxy, or your instrumentation:

```php
$client = new Client(domain: 'example.com', transport: new MyTransport());
```

## Tests

```bash
php tests/run.php
```

No PHPUnit, no install step, no network. The suite injects a recording transport and asserts on the
exact bytes and headers the SDK would have sent.

## Licence

MIT. See `LICENSE`.
