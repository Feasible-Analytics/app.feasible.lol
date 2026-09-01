#
# request.py
# Reading the visitor's address and user agent off an incoming request.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

"""The Visitor helper: one place that knows which header wins, and which entry of it."""

from dataclasses import dataclass
from typing import Any, Dict, Mapping, Optional

from .errors import MissingClientIPError, MissingUserAgentError

__all__ = ["Visitor"]


@dataclass(frozen=True)
class Visitor:
    """The two things a server-side call must forward.

    They travel together because they are always needed together, and because
    the resolution rules are worth writing down once rather than at every call
    site in an application.
    """

    client_ip: str
    user_agent: str

    def __post_init__(self) -> None:
        """Refuse to hold a half-filled visitor. An empty address or user agent
        is exactly the mistake this package exists to prevent, so it is rejected
        at construction, where the traceback points at the code that lost the
        value rather than at the SDK."""
        ip = (self.client_ip or "").strip()
        agent = (self.user_agent or "").strip()

        if not ip:
            raise MissingClientIPError("client_ip")

        if not agent:
            raise MissingUserAgentError("user_agent")

        object.__setattr__(self, "client_ip", ip)
        object.__setattr__(self, "user_agent", agent)

    @classmethod
    def from_wsgi(cls, environ: Mapping[str, Any]) -> "Visitor":
        """Read the visitor from a WSGI ``environ``.

        This is the entry point for anything speaking WSGI directly — a bare
        application, or a framework that hands you the raw environment.
        """
        headers = {
            "cf-connecting-ip": _text(environ.get("HTTP_CF_CONNECTING_IP")),
            "x-forwarded-for": _text(environ.get("HTTP_X_FORWARDED_FOR")),
            "user-agent": _text(environ.get("HTTP_USER_AGENT")),
        }

        return cls.from_headers(headers, remote_addr=_text(environ.get("REMOTE_ADDR")))

    @classmethod
    def from_headers(cls, headers: Mapping[str, Any], remote_addr: Optional[str] = None) -> "Visitor":
        """Read the visitor from a plain headers mapping plus the socket address.

        Django's ``request.headers``, Flask's ``request.headers``, Starlette and
        FastAPI's ``request.headers`` and a bare dict all work here, which is why
        this rather than the WSGI environ is the general case: ASGI has no
        environ at all.
        """
        lookup = {str(name).lower(): _text(value) for name, value in headers.items()}

        return cls(
            client_ip=resolve_client_ip(lookup, remote_addr),
            user_agent=lookup.get("user-agent", ""),
        )

    @classmethod
    def from_request(
        cls,
        headers: Optional[Mapping[str, Any]] = None,
        environ: Optional[Mapping[str, Any]] = None,
        remote_addr: Optional[str] = None,
    ) -> "Visitor":
        """Read the visitor from whichever of the two shapes you have.

        One name to remember across every framework, so the documentation does
        not have to fork per web stack.
        """
        if environ is not None:
            return cls.from_wsgi(environ)

        if headers is None:
            raise TypeError("from_request needs either headers= or environ=")

        return cls.from_headers(headers, remote_addr=remote_addr)

    def as_kwargs(self) -> Dict[str, str]:
        """Return the pair as keyword arguments, so a call reads
        ``client.pageview(url=url, **visitor.as_kwargs())`` and neither value
        can be forgotten or transposed."""
        return {"client_ip": self.client_ip, "user_agent": self.user_agent}


def resolve_client_ip(headers: Mapping[str, str], remote_addr: Optional[str] = None) -> str:
    """Resolve the visitor's address from forwarding headers and the socket.

    This assumes the application edge stripped client-supplied forwarding
    headers; unlike the ingest service, this helper has no proxy allow-list.

    The first entry is the one that matters. Every proxy appends itself to
    ``X-Forwarded-For``, so the last entry is the nearest proxy — taking it, as
    several frameworks do, reports your own load balancer as the visitor and
    collapses every visit in the world into one.

    The mapping's keys must already be lower-cased; ``Visitor.from_headers``
    does that.
    """
    cloudflare = headers.get("cf-connecting-ip", "").strip()
    if cloudflare:
        return cloudflare

    forwarded = headers.get("x-forwarded-for", "").strip()
    if forwarded:
        first = forwarded.split(",")[0].strip()
        if first:
            return first

    return (remote_addr or "").strip()


def _text(value: Any) -> str:
    """Coerce one header value to a trimmed string.

    Anything that is not a string — a framework storing a list of values, say —
    is treated as absent, because guessing which entry was meant is how the
    wrong address ends up on every event.
    """
    return value.strip() if isinstance(value, str) else ""
