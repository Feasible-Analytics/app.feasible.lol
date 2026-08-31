#
# client.py
# The server-side ingest client: one event, the visitor's address, and the visitor's user agent.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

"""The Client: everything an application needs to report an event from a server."""

import json
import os
import random
import time
from typing import Any, Dict, List, Mapping, Optional

from .errors import (
    APIError,
    BadRequestError,
    InvalidEventError,
    MissingClientIPError,
    MissingUserAgentError,
    TransportError,
)
from .models import Attribution, RecordedEvent, Result, Revenue
from .transport import Transport, UrllibTransport

__all__ = ["Client", "DEFAULT_HOST", "HEADER_DROPPED"]

# The hosted endpoint. Self-hosted installs pass their own host.
DEFAULT_HOST = "https://app.feasible.lol"

# The response header carrying the reason an accepted event was not counted.
HEADER_DROPPED = "x-feasible-dropped"

# The path is part of the wire contract and is not configurable.
_EVENT_PATH = "/api/event"

# Values of FEASIBLE_DISABLED that mean "do not send anything".
_TRUTHY = ("1", "true", "yes", "on")


class Client:
    """Sends events to ``POST /api/event`` from your server.

    Read this before your first call. A server-side event carries two things the
    browser would have carried by itself, and both are required arguments here:

    * ``client_ip`` — the visitor's real IP, forwarded as ``X-Forwarded-For``.
    * ``user_agent`` — the visitor's real ``User-Agent``, forwarded verbatim.

    A request that arrives from a datacentre address with neither is classified
    as a bot and dropped, and nothing in the response says so loudly enough to
    notice. Passing your own server's address is worse than passing nothing: it
    looks like data. :meth:`feasible.Visitor.from_request` reads both off the
    incoming request, resolving the address the way the ingest server does.

    In a test environment pass ``disabled=True``, or set ``FEASIBLE_DISABLED=1``:
    nothing is sent, calls succeed, and every event is kept in memory for
    :attr:`recorded_events` to assert on.
    """

    def __init__(
        self,
        domain: str,
        host: str = DEFAULT_HOST,
        timeout: float = 5.0,
        max_attempts: int = 3,
        backoff_base: float = 0.25,
        backoff_cap: float = 5.0,
        disabled: Optional[bool] = None,
        transport: Optional[Transport] = None,
    ) -> None:
        """Build a client for one site.

        ``domain`` is the site as registered — what the tracking script would put
        in ``data-domain`` — and not the URL of a page.

        ``disabled`` is tri-state rather than a plain false so that an explicit
        ``disabled=False`` in application code still wins over
        ``FEASIBLE_DISABLED=1`` in the environment, while leaving it unset defers
        to the environment, which is what a shared test container wants.
        """
        if not (domain or "").strip():
            raise InvalidEventError('domain is required: it is the site as registered, such as "example.com"')

        if max_attempts < 1:
            raise InvalidEventError("max_attempts must be at least 1")

        self.domain = domain.strip()
        self.endpoint = host.strip().rstrip("/") + _EVENT_PATH
        self.timeout = timeout
        self.max_attempts = max_attempts
        self.backoff_base = backoff_base
        self.backoff_cap = backoff_cap
        self.disabled = _disabled_by_environment() if disabled is None else disabled
        self.transport = transport or UrllibTransport()
        self._recorded: List[RecordedEvent] = []

    def pageview(
        self,
        url: str,
        client_ip: str,
        user_agent: str,
        title: Optional[str] = None,
        referrer: Optional[str] = None,
        props: Optional[Mapping[str, Any]] = None,
        revenue: Optional[Revenue] = None,
        attribution: Optional[Attribution] = None,
        interactive: Optional[bool] = None,
        scroll_depth: Optional[int] = None,
        engagement_time: Optional[int] = None,
        viewport_width: Optional[int] = None,
    ) -> Result:
        """Record a pageview.

        Every report is built from pageviews, so this gets its own method rather
        than leaving callers to remember that the name is the literal string
        ``"pageview"``.
        """
        return self.event(
            "pageview",
            url,
            client_ip,
            user_agent,
            title=title,
            referrer=referrer,
            props=props,
            revenue=revenue,
            attribution=attribution,
            interactive=interactive,
            scroll_depth=scroll_depth,
            engagement_time=engagement_time,
            viewport_width=viewport_width,
        )

    def event(
        self,
        name: str,
        url: str,
        client_ip: str,
        user_agent: str,
        title: Optional[str] = None,
        referrer: Optional[str] = None,
        props: Optional[Mapping[str, Any]] = None,
        revenue: Optional[Revenue] = None,
        attribution: Optional[Attribution] = None,
        interactive: Optional[bool] = None,
        scroll_depth: Optional[int] = None,
        engagement_time: Optional[int] = None,
        viewport_width: Optional[int] = None,
    ) -> Result:
        """Record a custom event — a signup, a purchase, a plan change.

        ``url`` is required even for an event with no page of its own, because
        every report groups by page and an event without one cannot be found
        again. For an offline conversion, pass the URL it belongs to and set
        ``attribution`` so it is not filed as Direct forever.
        """
        payload = self._build_payload(
            name,
            url,
            title,
            referrer,
            props,
            revenue,
            attribution,
            interactive,
            scroll_depth,
            engagement_time,
            viewport_width,
        )

        return self._dispatch(payload, client_ip, user_agent, debug=False)

    def debug(
        self,
        name: str,
        url: str,
        client_ip: str,
        user_agent: str,
        title: Optional[str] = None,
        referrer: Optional[str] = None,
        props: Optional[Mapping[str, Any]] = None,
        revenue: Optional[Revenue] = None,
        attribution: Optional[Attribution] = None,
        interactive: Optional[bool] = None,
        scroll_depth: Optional[int] = None,
        engagement_time: Optional[int] = None,
        viewport_width: Optional[int] = None,
    ) -> Dict[str, Any]:
        """Ask the server what it would derive from this event, and write nothing.

        It answers "why is this visit attributed to the wrong country" in one
        call, against production, with no side effects.
        """
        payload = self._build_payload(
            name,
            url,
            title,
            referrer,
            props,
            revenue,
            attribution,
            interactive,
            scroll_depth,
            engagement_time,
            viewport_width,
        )

        result = self._dispatch(payload, client_ip, user_agent, debug=True)

        if not result.sent:
            return {}

        try:
            derived = json.loads(result.body)
        except ValueError as error:
            raise APIError(result.status, result.body, result.attempts) from error

        if not isinstance(derived, dict):
            raise APIError(result.status, result.body, result.attempts)

        return derived

    @property
    def recorded_events(self) -> List[RecordedEvent]:
        """The events a disabled client kept.

        This is the supported way to test analytics: assert on what your code
        reported without a network, a mock of this SDK, or a test double that
        stops matching the payload the day the wire contract gains a field.
        """
        return list(self._recorded)

    def clear_recorded_events(self) -> None:
        """Empty the recording, so one test case cannot see another's events."""
        self._recorded = []

    def _build_payload(
        self,
        name: str,
        url: str,
        title: Optional[str],
        referrer: Optional[str],
        props: Optional[Mapping[str, Any]],
        revenue: Optional[Revenue],
        attribution: Optional[Attribution],
        interactive: Optional[bool],
        scroll_depth: Optional[int],
        engagement_time: Optional[int],
        viewport_width: Optional[int],
    ) -> Dict[str, Any]:
        """Assemble the wire payload.

        An absent value is omitted rather than sent as null: the endpoint reads
        a null as a value and would overwrite what it derived with nothing.
        """
        name = (name or "").strip()
        url = (url or "").strip()

        if not name:
            raise InvalidEventError('name is required: "pageview" for a pageview, or your own event name')

        if not url:
            raise InvalidEventError("url is required: the full URL of the page the event happened on")

        payload: Dict[str, Any] = {"n": name, "u": url, "d": self.domain}

        if referrer and referrer.strip():
            payload["r"] = referrer

        if title and title.strip():
            payload["t"] = title

        if props:
            payload["p"] = dict(props)

        if revenue is not None:
            payload["$"] = revenue.to_wire()

        # Interactive is only sent when the caller said so, because the server
        # defaults an absent flag to true and that is what an ordinary event is.
        if interactive is not None:
            payload["i"] = interactive

        if scroll_depth is not None:
            payload["sd"] = scroll_depth

        if engagement_time is not None:
            payload["e"] = engagement_time

        if viewport_width is not None:
            payload["w"] = viewport_width

        if attribution is not None:
            payload.update(attribution.to_wire())

        return payload

    def _dispatch(self, payload: Dict[str, Any], client_ip: str, user_agent: str, debug: bool) -> Result:
        """Validate the visitor, then either record or send.

        The validation runs in no-op mode too, on purpose: a test suite that
        never exercised the check would let a call with no address ship to
        production unnoticed, which is the exact failure this package prevents.
        """
        ip = (client_ip or "").strip()
        agent = (user_agent or "").strip()

        if not ip:
            raise MissingClientIPError("client_ip")

        if not agent:
            raise MissingUserAgentError("user_agent")

        if self.disabled:
            self._recorded.append(RecordedEvent(payload=payload, client_ip=ip, user_agent=agent, debug=debug))

            return Result(status=0, dropped=None, attempts=0, sent=False, body="")

        headers = {
            # text/plain is deliberate. It is what the browser tracker sends, it
            # avoids a CORS preflight, and the endpoint reads the body as JSON
            # regardless of the declared type.
            "Content-Type": "text/plain",
            "X-Forwarded-For": ip,
            "User-Agent": agent,
        }

        if debug:
            headers["X-Debug-Request"] = "true"

        body = json.dumps(payload, separators=(",", ":"), ensure_ascii=False)

        return self._post(headers, body)

    def _post(self, headers: Mapping[str, str], body: str) -> Result:
        """Send with retries.

        What is retried is deliberately narrow: a transport failure, a 429 and a
        5xx are conditions a second attempt can genuinely fix, and nothing else
        is. A 400 is the caller's bug and would fail identically; a 202 carrying
        a drop reason is a classification, not a failure, and retrying reaches
        the same classifier.
        """
        attempt = 0

        while True:
            attempt += 1

            try:
                response = self.transport.send(self.endpoint, headers, body, self.timeout)
            except TransportError as error:
                if attempt >= self.max_attempts:
                    raise TransportError(str(error), attempts=attempt) from error

                self._pause(attempt)
                continue

            status = response.status

            if status == 400:
                raise BadRequestError(status, response.body, attempt)

            if status == 429 or status >= 500:
                if attempt >= self.max_attempts:
                    raise APIError(status, response.body, attempt)

                self._pause(attempt)
                continue

            if status < 200 or status >= 300:
                raise APIError(status, response.body, attempt)

            return Result(
                status=status,
                dropped=response.header(HEADER_DROPPED),
                attempts=attempt,
                sent=True,
                body=response.body,
            )

    def _pause(self, attempt: int) -> None:
        """Wait before the next attempt.

        The delay is exponential so a struggling endpoint is not hammered,
        capped so a background job does not sleep for minutes, and jittered so a
        fleet of servers that all failed at the same moment does not retry in
        lockstep and repeat the outage.
        """
        delay = min(self.backoff_cap, self.backoff_base * (2 ** (attempt - 1)))

        if delay <= 0:
            return

        time.sleep(delay / 2 + random.random() * (delay / 2))


def _disabled_by_environment() -> bool:
    """Read the environment switch.

    A test container, a CI job or a local development machine sets one variable
    and the whole application stops writing to the customer's real numbers.
    """
    return os.environ.get("FEASIBLE_DISABLED", "").strip().lower() in _TRUTHY
