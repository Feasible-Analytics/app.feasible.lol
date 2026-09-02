<!--
  README.md
  The Go SDK for feasible.lol: install it, send an event, and never forward the wrong IP.

  Created: 2026-08-30
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# feasible-go

The official Go client for [feasible.lol](https://app.feasible.lol) — privacy-first web
analytics. Standard library only, no dependencies, MIT licensed.

## Read this first: the two things you must forward

**A server-side call must carry the visitor's real IP and the visitor's real User-Agent.**

Your server is in a datacentre. The analytics endpoint sees the address the request came
from, and if that address is a datacentre with no forwarded visitor in it, the event is a
bot: it is classified, dropped, and your dashboard shows nothing. This is the single most
common way server-side analytics goes wrong, and the endpoint answers `400` with a
sentence naming what is missing rather than pretending it worked.

So in this SDK they are not optional fields on a struct. They are a `Visitor`, and a
`Visitor` is a **required argument** to every constructor:

```go
event := feasible.NewEvent("Signup", "https://example.com/join", visitor)
```

There is no way to build an event without one, and a `Visitor` missing either half is
refused before a request is built, with a typed error naming the field.

The easy way to get one right is to take it from the request you are already holding:

```go
visitor := feasible.FromRequest(r)
```

`FromRequest` takes `CF-Connecting-IP`, then the **first** entry of
`X-Forwarded-For`, then the socket address. Unlike the ingest service, this helper has no
trusted-proxy configuration. Use it only when your application edge strips client-supplied
forwarding headers and writes its own. On a directly exposed app, construct `Visitor` from
the socket address so a client cannot choose its fingerprint or geolocation.

The client sends the visitor's address as `X-Forwarded-For`. **The ingest server honours that
header only from an address on its trusted-proxy list** (`FEASIBLE_INGEST_TRUSTED_PROXIES`);
from any other peer it uses the socket address, which is your server. On a self-hosted
instance, add the address your application calls from to that list. Check it with
`client.Debug`: the derived event's `client_ip_source` is `x-forwarded-for` when the header
was used and `socket` when it was not.

## Install

```bash
go get github.com/Feasible-Analytics/feasible-go
```

Go 1.23 or newer. **No dependencies.** That is deliberate: an analytics client is a thing
you add to a service that already has an opinion about its dependency tree, and it should
not bring a transitive graph with it. Everything here is `net/http` and `encoding/json`.

## Use

```go
package main

import (
	"log"
	"net/http"

	feasible "github.com/Feasible-Analytics/feasible-go"
)

func main() {
	client, err := feasible.New(feasible.Options{Domain: "example.com"})
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/pricing", func(w http.ResponseWriter, r *http.Request) {
		result, err := client.Pageview(r.Context(), feasible.FromRequest(r), "https://example.com/pricing")
		if err != nil {
			log.Printf("analytics: %v", err)
		} else if result.Dropped() {
			log.Printf("analytics: event classified as %s", result.DropReason)
		}

		w.Write([]byte("hello"))
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

Self-hosting? Set `Options.Host` to your own host. Nothing else changes.

### A custom event, with properties and money

```go
event := feasible.NewEvent("Purchase", "https://example.com/checkout", feasible.FromRequest(r))
event.Props = map[string]any{"plan": "annual", "seats": 4}
event.Revenue = &feasible.Revenue{Amount: 99.50, Currency: "USD"}
event.Title = "Checkout"

result, err := client.Send(r.Context(), event)
```

`WithProp` adds one property at a time and allocates the map for you:

```go
event := feasible.NewEvent("Signup", "https://example.com/join", feasible.FromRequest(r)).
	WithProp("plan", "annual").
	WithProp("trial", true)
```

Thirty properties at most; names cap at 300 characters and values at 2000. Anything past
those limits is counted and reported by the server rather than quietly dropped.

### A conversion that happens later

A webhook, a queue worker, an offline sale — none of them have a referrer, so without help
every one of them is Direct forever and the campaign that earned it gets no credit. Set the
attribution explicitly:

```go
event := feasible.NewEvent("Purchase", "https://example.com/order/complete",
	feasible.NewVisitor(order.VisitorIP, order.VisitorUserAgent))
event.Revenue = &feasible.Revenue{Amount: 240, Currency: "USD"}
event.Attribution = feasible.Attribution{
	UTMSource:   "newsletter",
	UTMMedium:   "email",
	UTMCampaign: "spring",
}
```

The server applies these overrides to any event that carries them; nothing about them is
specific to a server-side caller.

Store the visitor's IP and User-Agent alongside whatever you are going to convert later.
They are the only two values that cannot be reconstructed after the fact.

## Testing without a network

Set `Options.Disabled` or `FEASIBLE_DISABLED=1` and the client sends nothing, succeeds, and
keeps every event in memory. That is how you assert analytics in a test suite:

```go
client, _ := feasible.New(feasible.Options{Domain: "example.com", Disabled: true})

checkout(client)

events := client.Recorded()
if len(events) != 1 || events[0].Name != "Purchase" {
	t.Fatalf("checkout did not report a purchase: %+v", events)
}
```

Validation still runs in no-op mode. A test suite that never sends anything is exactly
where a missing IP would otherwise hide until production.

## What comes back

`Send` returns a `*Result`:

| Field | Meaning |
|---|---|
| `StatusCode` | The final HTTP status. `202` for anything the server understood. |
| `DropReason` | The `x-feasible-dropped` header, empty when the event counted. |
| `Attempts` | How many requests it took. |
| `Skipped` | The client is in no-op mode and nothing was sent. |

**A drop reason is not an error.** The server accepted the request and decided the event
was a bot, or an excluded visitor, or a site it does not know. It is surfaced as a field so
you can log it — never swallowed, and never retried.

## Errors

| Type | When |
|---|---|
| `*ValidationError` | Something required was missing. Wraps `ErrMissingClientIP`, `ErrMissingUserAgent`, `ErrMissingName`, `ErrMissingURL` or `ErrMissingDomain`, so `errors.Is` works. |
| `*APIError` | The server refused the request. Carries the status and the server's own sentence verbatim. |

```go
if errors.Is(err, feasible.ErrMissingClientIP) {
	// you forgot to forward the visitor
}
```

## Retries

Three attempts by default, exponential backoff with jitter, capped at two seconds.

- **Retried:** transport errors, `429`, and any `5xx`. Nothing was counted, so nothing can
  be duplicated.
- **Not retried:** `400`. That is your bug — a malformed body or a missing header — and
  sending it again produces the same `400` while hiding the message that explains it.
- **Not retried:** a `202` carrying `x-feasible-dropped`. That is a classification the
  server already made, not a failure.

Backoff waits are cancelled by your `context`, so a handler that has given up is not held
open by a retry nobody is waiting for.

## Debugging

`Debug` asks the server what it would derive from an event and returns that JSON instead of
writing anything. It is free of side effects and safe against production, which makes it
the first thing to reach for when somebody says their numbers look wrong:

```go
raw, err := client.Debug(ctx, event)
fmt.Println(string(raw))
```

## Licence

MIT. See `LICENSE`.
