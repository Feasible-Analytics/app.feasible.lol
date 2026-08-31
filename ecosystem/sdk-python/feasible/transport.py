#
# transport.py
# The seam between the SDK and the network, over the standard library only.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

"""HTTP transport built on urllib, plus the interface a caller can replace."""

import urllib.error
import urllib.request
from dataclasses import dataclass, field
from typing import Dict, Mapping, Optional

from .errors import TransportError

__all__ = ["Response", "Transport", "UrllibTransport"]


@dataclass(frozen=True)
class Response:
    """One HTTP answer, reduced to the three things the SDK reads.

    Header names arrive lower-cased so that no part of the SDK has to guess the
    capitalisation the endpoint or a proxy in front of it chose.
    """

    status: int
    headers: Mapping[str, str] = field(default_factory=dict)
    body: str = ""

    def header(self, name: str) -> Optional[str]:
        """Read one header without the caller knowing its capitalisation."""
        return self.headers.get(name.lower())


class Transport:
    """How a request leaves the process.

    It is a replaceable object so an application with its own HTTP client,
    proxy or instrumentation can hand one in rather than have this package open
    its own connections, and so a test can assert on the exact bytes with no
    socket at all.
    """

    def send(self, url: str, headers: Mapping[str, str], body: str, timeout: float) -> Response:
        """Perform one request and return what came back.

        Implementations raise :class:`feasible.errors.TransportError` when
        nothing came back at all, which is the signal the retry loop reads.
        """
        raise NotImplementedError


class UrllibTransport(Transport):
    """The default transport, on ``urllib.request``.

    The standard library is the whole dependency list on purpose: an analytics
    SDK that drags in an HTTP stack is an SDK that causes version conflicts in
    applications whose actual work has nothing to do with analytics.
    """

    def send(self, url: str, headers: Mapping[str, str], body: str, timeout: float) -> Response:
        """POST the body and normalise whatever comes back.

        An HTTP error status is returned as a response rather than raised: a 400
        carries the endpoint's own explanation in its body, and letting urllib's
        exception hide that sentence is what turns a two-minute fix into a
        support ticket.
        """
        request = urllib.request.Request(
            url,
            data=body.encode("utf-8"),
            headers=dict(headers),
            method="POST",
        )

        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                return Response(
                    status=int(response.status),
                    headers=_lower(response.headers.items()),
                    body=response.read().decode("utf-8", "replace"),
                )
        except urllib.error.HTTPError as error:
            try:
                payload = error.read()
            except Exception:  # pragma: no cover - a body that cannot be read is still a status
                payload = b""
            finally:
                # An HTTPError is a live response object. Leaving it open holds a
                # socket until the garbage collector notices, which under load is
                # a file-descriptor leak in the caller's process rather than ours.
                error.close()

            return Response(
                status=int(error.code),
                headers=_lower(error.headers.items() if error.headers else []),
                body=payload.decode("utf-8", "replace"),
            )
        except Exception as error:
            # A timeout, a refused connection, a DNS failure: nothing came back,
            # which is the one case worth trying again.
            raise TransportError("the request to {0} failed: {1}".format(url, error)) from error


def _lower(items) -> Dict[str, str]:
    """Lower-case the header names of one response.

    Doing it in one place keeps a proxy's choice of capitalisation from becoming
    a conditional anywhere else in the SDK.
    """
    return {str(name).lower(): str(value) for name, value in items}
