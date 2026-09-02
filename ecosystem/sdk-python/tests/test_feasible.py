#
# test_feasible.py
# The whole suite: a real HTTP server on an ephemeral port, started and stopped in-process.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

"""Tests for the feasible SDK. Run with: python3 -m unittest discover"""

import json
import os
import re
import socket
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# The package sits beside the tests rather than in an installed environment, so
# the suite runs on a clean checkout with no install step and no network.
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

# The only shape the server accepts in the idempotency field.
UUID_V4 = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")

from feasible import (  # noqa: E402
    APIError,
    Attribution,
    BadRequestError,
    Client,
    MissingClientIPError,
    MissingUserAgentError,
    Revenue,
    TransportError,
    Visitor,
)


class _Handler(BaseHTTPRequestHandler):
    """Records every request and answers from a script.

    A real socket is used rather than a stub because the point of the assertions
    is the bytes and headers that actually leave the process: a stub can only
    prove the SDK called itself the way the test expected.
    """

    # HTTP/1.1 would keep the connection alive and make the test wait on a read
    # that never returns; every attempt getting its own connection is also what
    # a retry does in production.
    protocol_version = "HTTP/1.0"

    def do_POST(self) -> None:
        """Read the whole body, record it, and answer with the next scripted
        response so a test reads as a story: these are the things the server
        says, and this is what the SDK does about them."""
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length).decode("utf-8")

        self.server.requests.append(
            {
                "path": self.path,
                "headers": {name.lower(): value for name, value in self.headers.items()},
                "body": body,
            }
        )

        status, headers, payload = self.server.next_response()
        encoded = payload.encode("utf-8")

        self.send_response(status)
        for name, value in headers.items():
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def log_message(self, *args) -> None:
        """Silence the default access log, which would otherwise interleave with
        the test output and hide a real failure."""
        return


class _Server(ThreadingHTTPServer):
    """The test endpoint. Binds port 0 so a suite can never collide with a
    running service or with another copy of itself in CI."""

    daemon_threads = True

    def __init__(self):
        """Start on an ephemeral port with an empty script, meaning every request
        gets a bare 202 until a test says otherwise."""
        super().__init__(("127.0.0.1", 0), _Handler)
        self.requests = []
        self.script = []

    def next_response(self):
        """Hand out the next scripted answer, falling back to a plain 202 once
        the script runs out."""
        if self.script:
            return self.script.pop(0)

        return (202, {}, "")

    @property
    def host(self) -> str:
        """The base URL the SDK should be pointed at."""
        return "http://127.0.0.1:{0}".format(self.server_address[1])


class FeasibleTestCase(unittest.TestCase):
    """Shared fixture: one server per test, torn down whatever happens."""

    def setUp(self) -> None:
        """Start the endpoint in a background thread. It lives for exactly one
        test, so no test can see another's requests."""
        self.server = _Server()

        # A short poll interval keeps tearDown from waiting half a second per
        # test for serve_forever to notice the shutdown flag.
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
        self.thread.start()

        # backoff_base is zero so the retry tests assert on the rule rather than
        # sleeping through a real exponential delay.
        self.client = Client(domain="example.com", host=self.server.host, backoff_base=0.0)

    def tearDown(self) -> None:
        """Shut the endpoint down and close the socket, so a failing test cannot
        leave a listener behind."""
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def payload(self, index: int = 0):
        """The decoded body of one recorded request, so assertions are about JSON
        keys rather than about a string whitespace could break."""
        return json.loads(self.server.requests[index]["body"])


class PayloadTest(FeasibleTestCase):
    """What actually goes on the wire."""

    def test_pageview_sends_only_the_required_keys(self):
        """An absent value must be omitted, never sent as null: the endpoint
        reads a null as a value and overwrites what it derived."""
        self.client.pageview(
            url="https://example.com/pricing",
            client_ip="203.0.113.9",
            user_agent="Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)",
        )

        self.assertEqual(["k", "n", "u", "d"], list(self.payload().keys()))
        self.assertRegex(self.payload()["k"], UUID_V4)
        self.assertEqual("pageview", self.payload()["n"])
        self.assertEqual("https://example.com/pricing", self.payload()["u"])
        self.assertEqual("example.com", self.payload()["d"])
        self.assertEqual("/api/event", self.server.requests[0]["path"])

    def test_custom_event_sends_props_revenue_and_overrides(self):
        """Every optional field travels under the single-letter key the ingest
        contract fixes, and the attribution overrides keep their long names."""
        self.client.event(
            name="Purchase",
            url="https://example.com/checkout/complete",
            client_ip="203.0.113.9",
            user_agent="curl/8.4.0",
            title="Order complete",
            referrer="https://news.example/story",
            props={"plan": "pro", "seats": 4, "trial": False},
            revenue=Revenue(amount=49.5, currency="usd"),
            attribution=Attribution(utm_source="newsletter", utm_campaign="august"),
            interactive=False,
            scroll_depth=80,
            engagement_time=12000,
            viewport_width=1440,
        )

        payload = self.payload()
        self.assertEqual(
            ["k", "n", "u", "d", "r", "t", "p", "$", "i", "sd", "e", "w", "utm_source", "utm_campaign"],
            list(payload.keys()),
        )
        self.assertEqual({"plan": "pro", "seats": 4, "trial": False}, payload["p"])
        self.assertEqual({"amount": 49.5, "currency": "USD"}, payload["$"])
        self.assertFalse(payload["i"])
        self.assertEqual("https://news.example/story", payload["r"])
        self.assertEqual("newsletter", payload["utm_source"])

    def test_content_type_is_text_plain(self):
        """text/plain is what the browser tracker sends; it avoids a CORS
        preflight and the endpoint reads the body as JSON regardless."""
        self.client.pageview(url="https://example.com/", client_ip="203.0.113.9", user_agent="curl/8.4.0")

        self.assertEqual("text/plain", self.server.requests[0]["headers"]["content-type"])

    def test_visitor_headers_are_forwarded_verbatim(self):
        """Anything other than the visitor's own values attributes the event to
        the server rather than to the visitor."""
        self.client.pageview(
            url="https://example.com/",
            client_ip="198.51.100.23",
            user_agent="Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)",
        )

        headers = self.server.requests[0]["headers"]
        self.assertEqual("198.51.100.23", headers["x-forwarded-for"])
        self.assertEqual("Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)", headers["user-agent"])


class RequiredVisitorTest(FeasibleTestCase):
    """The two mistakes this SDK exists to make impossible."""

    def test_missing_client_ip_is_refused_by_name(self):
        """The error names the parameter so the fix is the next thing typed."""
        with self.assertRaises(MissingClientIPError) as caught:
            self.client.pageview(url="https://example.com/", client_ip="  ", user_agent="curl/8.4.0")

        self.assertIn("client_ip", str(caught.exception))
        self.assertEqual([], self.server.requests)

    def test_missing_user_agent_is_refused_by_name(self):
        """Same rule for the user agent, and nothing leaves the process."""
        with self.assertRaises(MissingUserAgentError) as caught:
            self.client.event(name="Signup", url="https://example.com/", client_ip="203.0.113.9", user_agent="")

        self.assertIn("user_agent", str(caught.exception))
        self.assertEqual([], self.server.requests)


class VisitorTest(unittest.TestCase):
    """Resolving the visitor from an incoming request."""

    def test_from_wsgi_takes_the_first_forwarded_entry(self):
        """Every proxy appends itself to X-Forwarded-For, so the last entry is
        the nearest proxy: taking it reports the load balancer as the visitor."""
        visitor = Visitor.from_wsgi(
            {
                "HTTP_X_FORWARDED_FOR": "198.51.100.23, 10.0.0.7, 10.0.0.8",
                "REMOTE_ADDR": "10.0.0.8",
                "HTTP_USER_AGENT": "Mozilla/5.0",
            }
        )

        self.assertEqual("198.51.100.23", visitor.client_ip)
        self.assertEqual("Mozilla/5.0", visitor.user_agent)

    def test_cf_connecting_ip_wins(self):
        """Cloudflare sets its own header and it is the more trustworthy of the
        two, so it is preferred exactly as the ingest server prefers it."""
        visitor = Visitor.from_headers(
            {
                "CF-Connecting-IP": "198.51.100.5",
                "X-Forwarded-For": "192.0.2.5",
                "User-Agent": "Mozilla/5.0",
            },
            remote_addr="10.0.0.8",
        )

        self.assertEqual("198.51.100.5", visitor.client_ip)

    def test_falls_back_to_the_socket_address(self):
        """With no forwarding headers the socket address is the visitor, which is
        the ordinary case for an application with no proxy in front."""
        visitor = Visitor.from_request(headers={"User-Agent": "Mozilla/5.0"}, remote_addr="203.0.113.77")

        self.assertEqual("203.0.113.77", visitor.client_ip)

    def test_a_request_with_no_address_is_refused(self):
        """A half-filled visitor is rejected at construction, where the traceback
        points at the code that lost the value."""
        with self.assertRaises(MissingClientIPError):
            Visitor.from_wsgi({"HTTP_USER_AGENT": "Mozilla/5.0"})

    def test_as_kwargs_spreads_into_a_call(self):
        """Spreading the pair is what keeps a call site from dropping one of
        them or transposing the two."""
        visitor = Visitor(client_ip="203.0.113.9", user_agent="curl/8.4.0")

        self.assertEqual({"client_ip": "203.0.113.9", "user_agent": "curl/8.4.0"}, visitor.as_kwargs())


class NoOpTest(FeasibleTestCase):
    """The supported way to test analytics."""

    def test_disabled_client_records_instead_of_sending(self):
        """Nothing is sent, the call succeeds, and the event is kept in memory so
        a test can assert on what the application reported."""
        client = Client(domain="example.com", host=self.server.host, disabled=True)

        result = client.event(
            name="Purchase",
            url="https://example.com/checkout/complete",
            client_ip="203.0.113.9",
            user_agent="curl/8.4.0",
            props={"plan": "pro"},
            revenue=Revenue(amount=49.5, currency="USD"),
        )

        self.assertEqual([], self.server.requests)
        self.assertFalse(result.sent)
        self.assertEqual(0, result.attempts)

        recorded = client.recorded_events
        self.assertEqual(1, len(recorded))
        self.assertEqual("Purchase", recorded[0].name)
        self.assertEqual({"plan": "pro"}, recorded[0].props)
        self.assertEqual({"amount": 49.5, "currency": "USD"}, recorded[0].payload["$"])
        self.assertEqual("203.0.113.9", recorded[0].client_ip)

        client.clear_recorded_events()
        self.assertEqual([], client.recorded_events)

    def test_disabled_client_still_refuses_a_missing_visitor(self):
        """The mistake gets caught by the test suite rather than by a customer."""
        client = Client(domain="example.com", host=self.server.host, disabled=True)

        with self.assertRaises(MissingUserAgentError):
            client.pageview(url="https://example.com/", client_ip="203.0.113.9", user_agent="")

    def test_environment_variable_disables_the_client(self):
        """One variable in a CI container stops the whole application writing to
        the customer's real numbers."""
        os.environ["FEASIBLE_DISABLED"] = "1"
        try:
            client = Client(domain="example.com", host=self.server.host)
            self.assertTrue(client.disabled)

            client.pageview(url="https://example.com/", client_ip="203.0.113.9", user_agent="curl/8.4.0")
            self.assertEqual([], self.server.requests)
        finally:
            del os.environ["FEASIBLE_DISABLED"]


class RetryTest(FeasibleTestCase):
    """What is retried, and — more importantly — what is not."""

    def test_a_500_is_retried_until_it_succeeds(self):
        """The same bytes may well succeed a moment later, so a 5xx is worth
        another attempt."""
        self.server.script = [
            (500, {}, "upstream is unhappy"),
            (503, {}, "still unhappy"),
            (202, {}, ""),
        ]

        result = self.client.pageview(url="https://example.com/", client_ip="203.0.113.9", user_agent="curl/8.4.0")

        self.assertEqual(3, len(self.server.requests))
        self.assertEqual(3, result.attempts)
        self.assertEqual(202, result.status)

    def test_retries_stop_at_max_attempts(self):
        """An endpoint that never recovers must not be hammered forever."""
        self.server.script = [(503, {}, "down") for _ in range(5)]
        client = Client(domain="example.com", host=self.server.host, backoff_base=0.0, max_attempts=2)

        with self.assertRaises(APIError):
            client.pageview(url="https://example.com/", client_ip="203.0.113.9", user_agent="curl/8.4.0")

        self.assertEqual(2, len(self.server.requests))

    def test_a_400_is_never_retried(self):
        """A 400 is the caller's bug: the same bytes get the same answer, and the
        server's explanation travels with the error."""
        self.server.script = [
            (400, {}, "this request arrived from a datacentre address with no X-Forwarded-For"),
        ]

        with self.assertRaises(BadRequestError) as caught:
            self.client.pageview(url="https://example.com/", client_ip="203.0.113.9", user_agent="curl/8.4.0")

        self.assertEqual(1, len(self.server.requests))
        self.assertEqual(400, caught.exception.status)
        self.assertIn("datacentre address", str(caught.exception))

    def test_a_dropped_202_is_not_retried_and_surfaces_the_reason(self):
        """A drop is a classification, not a failure. Retrying reaches the same
        classifier, and swallowing the reason is how silent data loss starts."""
        self.server.script = [(202, {"x-feasible-dropped": "datacenter_ip"}, "")]

        result = self.client.pageview(url="https://example.com/", client_ip="203.0.113.9", user_agent="curl/8.4.0")

        self.assertEqual(1, len(self.server.requests))
        self.assertEqual("datacenter_ip", result.dropped)
        self.assertTrue(result.was_dropped)

    def test_the_idempotency_key_survives_a_retry(self):
        """The server dedupes on "k", so a retry after a lost acknowledgement
        must resend the same key — and the next event must get a fresh one."""
        self.server.script = [(500, {}, "upstream is unhappy"), (202, {}, ""), (202, {}, "")]

        self.client.pageview(url="https://example.com/", client_ip="203.0.113.9", user_agent="curl/8.4.0")
        self.client.pageview(url="https://example.com/", client_ip="203.0.113.9", user_agent="curl/8.4.0")

        self.assertEqual(3, len(self.server.requests))
        first, retried, fresh = (self.payload(i)["k"] for i in range(3))
        self.assertRegex(first, UUID_V4)
        self.assertEqual(first, retried)
        self.assertNotEqual(first, fresh)

    def test_a_transport_failure_is_retried_then_reported(self):
        """Nothing came back at all, which is the one case worth trying again —
        and worth its own error type when every attempt fails the same way."""
        # A port nothing is listening on: bound to learn a free number, then
        # closed, so the connection is refused immediately rather than hanging.
        probe = socket.socket()
        probe.bind(("127.0.0.1", 0))
        dead_port = probe.getsockname()[1]
        probe.close()

        client = Client(
            domain="example.com",
            host="http://127.0.0.1:{0}".format(dead_port),
            backoff_base=0.0,
            max_attempts=3,
        )

        with self.assertRaises(TransportError) as caught:
            client.pageview(url="https://example.com/", client_ip="203.0.113.9", user_agent="curl/8.4.0")

        self.assertEqual(3, caught.exception.attempts)


class DebugTest(FeasibleTestCase):
    """The escape hatch that answers "why is this event wrong"."""

    def test_debug_returns_the_derived_event(self):
        """It asks the server what it would have written and writes nothing, so
        it is safe to run against production."""
        self.server.script = [
            (200, {"content-type": "application/json"}, '{"site_id": 7, "country": "US", "bot_reason": ""}'),
        ]

        derived = self.client.debug(
            name="pageview",
            url="https://example.com/",
            client_ip="203.0.113.9",
            user_agent="curl/8.4.0",
        )

        self.assertEqual("true", self.server.requests[0]["headers"]["x-debug-request"])
        self.assertEqual("US", derived["country"])


if __name__ == "__main__":
    unittest.main()
