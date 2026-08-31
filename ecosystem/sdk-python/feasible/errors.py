#
# errors.py
# Every exception this package raises, and why each one is its own type.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

"""Exceptions raised by the feasible SDK."""

from typing import Optional

__all__ = [
    "FeasibleError",
    "MissingClientIPError",
    "MissingUserAgentError",
    "InvalidEventError",
    "TransportError",
    "APIError",
    "BadRequestError",
]


class FeasibleError(Exception):
    """The one type that contains everything this SDK raises.

    Analytics is never the reason a checkout should fail, so an application that
    wants to swallow tracking problems needs a single ``except`` rather than a
    list that goes stale the moment a new error is added.
    """


class MissingClientIPError(FeasibleError):
    """The visitor's address was not supplied.

    Sending anyway is worse than not sending: the request leaves from a
    datacentre address, the server classifies it as a bot or answers 400, and
    the numbers look believable enough that nobody notices for weeks.
    """

    def __init__(self, parameter: str = "client_ip") -> None:
        """Name the parameter, not the concept, so the fix is the next thing the
        reader types rather than something they have to go and look up."""
        self.parameter = parameter
        super().__init__(
            "{0} is required and was empty. Pass the visitor's real IP address, not your "
            "server's: it becomes the X-Forwarded-For header, and without it every event is "
            "attributed to your server rather than to the visitor. Visitor.from_request() "
            "reads it off the incoming request for you.".format(parameter)
        )


class MissingUserAgentError(FeasibleError):
    """The visitor's User-Agent was not supplied.

    An event with no user agent has no browser, no operating system and no
    device, and a request carrying neither an address nor a user agent is
    treated as a datacentre bot and dropped.
    """

    def __init__(self, parameter: str = "user_agent") -> None:
        """Name the parameter so the call site can be fixed without reading the
        ingest contract."""
        self.parameter = parameter
        super().__init__(
            "{0} is required and was empty. Pass the visitor's real User-Agent, not your HTTP "
            "client's: it is what browser, OS and device are derived from, and a request with "
            "neither an address nor a user agent is treated as a datacentre bot. "
            "Visitor.from_request() reads it off the incoming request for you.".format(parameter)
        )


class InvalidEventError(FeasibleError):
    """The event could not be built.

    Catching a missing name or URL locally saves a round trip to learn the same
    thing from a 400, and it raises at the call site that is actually wrong.
    """


class TransportError(FeasibleError):
    """The request never reached the endpoint, on every attempt.

    Kept apart from an HTTP error because the two need different responses:
    this one is usually egress, DNS or a firewall, not the payload.
    """

    def __init__(self, message: str, attempts: int = 1) -> None:
        """Carry the attempt count so a log line can say whether the endpoint was
        tried once or three times before the caller gave up."""
        self.attempts = attempts
        super().__init__(message)


class APIError(FeasibleError):
    """The endpoint answered with a status this SDK will not treat as accepted.

    The status and the body both travel with the error because the endpoint
    explains itself in the body, and hiding that sentence is what turns a
    two-minute fix into a support ticket.
    """

    def __init__(self, status: int, body: str, attempts: int = 1, message: Optional[str] = None) -> None:
        """Build the message from the server's own words, falling back to the
        status when the body is empty."""
        self.status = status
        self.body = body
        self.attempts = attempts

        detail = (body or "").strip() or "the response body was empty"
        super().__init__(message or "the ingest endpoint answered {0}: {1}".format(status, detail))


class BadRequestError(APIError):
    """The endpoint refused the request with a 400.

    A 400 describes something wrong with what was sent — a missing key, or the
    visitor's address and user agent not being forwarded — so it has its own
    type and is never retried: the same bytes produce the same answer.
    """
