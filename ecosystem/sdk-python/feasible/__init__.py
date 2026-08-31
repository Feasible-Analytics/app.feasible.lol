#
# __init__.py
# The public surface of the feasible SDK.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

"""Server-side event tracking for feasible.lol.

The visitor's IP address and User-Agent are required arguments on every call.
A server-side request that forwards neither is classified as a datacentre bot
and dropped, so this SDK refuses to send rather than let the mistake through:

    from feasible import Client, Visitor

    client = Client(domain="example.com")
    visitor = Visitor.from_request(headers=request.headers, remote_addr=request.remote_addr)

    client.pageview(url="https://example.com/pricing", **visitor.as_kwargs())
"""

from .client import DEFAULT_HOST, HEADER_DROPPED, Client
from .errors import (
    APIError,
    BadRequestError,
    FeasibleError,
    InvalidEventError,
    MissingClientIPError,
    MissingUserAgentError,
    TransportError,
)
from .models import Attribution, RecordedEvent, Result, Revenue
from .request import Visitor
from .transport import Response, Transport, UrllibTransport

__version__ = "1.0.0"

# Everything a caller needs and nothing else. The module layout is an
# implementation detail, so the import path stays `from feasible import X` even
# if a file later moves.
__all__ = [
    "APIError",
    "Attribution",
    "BadRequestError",
    "Client",
    "DEFAULT_HOST",
    "FeasibleError",
    "HEADER_DROPPED",
    "InvalidEventError",
    "MissingClientIPError",
    "MissingUserAgentError",
    "RecordedEvent",
    "Response",
    "Result",
    "Revenue",
    "Transport",
    "TransportError",
    "UrllibTransport",
    "Visitor",
    "__version__",
]
