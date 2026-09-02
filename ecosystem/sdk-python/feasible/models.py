#
# models.py
# The value objects an event is built from and the result it comes back as.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

"""Dataclasses for revenue, attribution overrides, results and recorded events."""

import re
from dataclasses import dataclass, field
from typing import Any, Dict, Optional, Union

from .errors import InvalidEventError

__all__ = ["Revenue", "Attribution", "Result", "RecordedEvent"]

# Three letters, the shape of every ISO 4217 code. Checked here so a typo fails
# at the call site rather than being quietly ignored by the server.
_CURRENCY = re.compile(r"^[A-Z]{3}$")


@dataclass(frozen=True)
class Revenue:
    """The money one event reports, sent as the ``$`` field.

    It is a type rather than a loose dict so a currency typo fails immediately:
    the server ignores a revenue object with no currency, and revenue that is
    silently zero is the hardest kind of missing data to notice.
    """

    amount: Union[int, float]
    currency: str

    def __post_init__(self) -> None:
        """Normalise the currency to the upper-case form the server stores, so
        that "usd" and "USD" do not become two rows on the same report."""
        code = (self.currency or "").strip().upper()

        if not _CURRENCY.match(code):
            raise InvalidEventError(
                "revenue currency {0!r} is not a three-letter ISO 4217 code, such as USD or GBP".format(self.currency)
            )

        object.__setattr__(self, "currency", code)

    def to_wire(self) -> Dict[str, Any]:
        """Render the wire shape. The key names are the server's and are not
        configurable, so they live in exactly one place."""
        return {"amount": self.amount, "currency": self.currency}


@dataclass(frozen=True)
class Attribution:
    """The server-side attribution overrides for an event with no referrer.

    A delayed or offline conversion — a webhook hours later, a phone order, a
    refund — has no referrer of its own and would be filed as Direct forever, so
    the campaign that earned it is passed explicitly. The server applies them
    to any event that carries them.
    """

    referrer: Optional[str] = None
    utm_source: Optional[str] = None
    utm_medium: Optional[str] = None
    utm_campaign: Optional[str] = None
    utm_content: Optional[str] = None
    utm_term: Optional[str] = None

    def to_wire(self) -> Dict[str, str]:
        """Render only the fields that were set. An absent key is omitted rather
        than sent as null, because the endpoint reads a null as a value and
        would overwrite what it derived with nothing."""
        pairs = {
            "referrer": self.referrer,
            "utm_source": self.utm_source,
            "utm_medium": self.utm_medium,
            "utm_campaign": self.utm_campaign,
            "utm_content": self.utm_content,
            "utm_term": self.utm_term,
        }

        return {key: value for key, value in pairs.items() if value is not None and value.strip() != ""}


@dataclass(frozen=True)
class Result:
    """The answer to a send.

    ``dropped`` is a field rather than something the SDK swallows, because the
    endpoint answers 202 even for events it decided not to count: without it, a
    filter silently discarding half a customer's traffic looks like success.
    """

    status: int
    dropped: Optional[str] = None
    attempts: int = 1
    sent: bool = True
    body: str = ""

    @property
    def was_dropped(self) -> bool:
        """Report whether the event was accepted but classified. It is not a
        failure and must not be retried — the retry reaches the same
        classifier and gets the same answer."""
        return bool(self.dropped)


@dataclass(frozen=True)
class RecordedEvent:
    """One event a disabled client kept in memory instead of sending.

    A test suite needs to assert that the checkout reported the revenue it
    charged. The alternatives are a network call from the test or a hand-written
    mock of this SDK, and both stop catching mistakes the moment the payload
    changes.
    """

    payload: Dict[str, Any] = field(default_factory=dict)
    client_ip: str = ""
    user_agent: str = ""
    debug: bool = False

    @property
    def name(self) -> str:
        """The event name, which is what a test asserts on first and is
        otherwise a single-letter key lookup at every call site."""
        return str(self.payload.get("n", ""))

    @property
    def url(self) -> str:
        """The page the event happened on, for the same reason as ``name``."""
        return str(self.payload.get("u", ""))

    @property
    def props(self) -> Dict[str, Any]:
        """The custom properties, defaulting to an empty dict so a test can
        index into them without first checking whether any were sent."""
        props = self.payload.get("p")

        return props if isinstance(props, dict) else {}
