#
# feasible_test.rb
# The whole suite: a real socket on an ephemeral port, started and stopped in-process.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

# frozen_string_literal: true

require "minitest/autorun"
require "json"
require "socket"
require "feasible"

# The only shape the server accepts in the idempotency field.
UUID_V4 = /\A[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\z/

# A tiny HTTP endpoint that records every request and answers from a script.
#
# It speaks the two lines of HTTP the SDK needs rather than pulling in a real
# server, so the suite runs on the system Ruby with nothing installed. A real
# socket is used rather than a stub because the point of the assertions is the
# bytes and headers that actually leave the process.
class ScriptedServer
  # The status lines the tests use. Net::HTTP wants a reason phrase, and an
  # empty one turns a clear assertion failure into a parse error.
  REASONS = {
    200 => "OK",
    202 => "Accepted",
    400 => "Bad Request",
    429 => "Too Many Requests",
    500 => "Internal Server Error",
    503 => "Service Unavailable"
  }.freeze

  attr_reader :requests
  attr_accessor :script

  # Binds port 0 so a suite can never collide with a running service or with
  # another copy of itself in CI.
  def initialize
    @server = TCPServer.new("127.0.0.1", 0)
    @requests = []
    @script = []
    @thread = Thread.new { serve }
  end

  # The base URL the SDK should be pointed at.
  def host
    "http://127.0.0.1:#{@server.addr[1]}"
  end

  # Stops the listener and closes the socket, so a failing test cannot leave a
  # port bound behind it.
  def stop
    @thread.kill
    @server.close unless @server.closed?
  end

  private

  # Accepts connections one at a time. The SDK opens a fresh connection per
  # attempt, which is also what a retry does in production, so a serial loop is
  # enough and keeps the recorded order meaningful.
  def serve
    loop { handle(@server.accept) }
  rescue IOError, Errno::EBADF
    nil
  end

  # Reads one request, records it, and writes the next scripted answer.
  def handle(socket)
    request_line = socket.gets.to_s
    headers = {}

    while (line = socket.gets) && line.strip != ""
      name, value = line.split(":", 2)
      headers[name.to_s.downcase.strip] = value.to_s.strip
    end

    length = headers["content-length"].to_i
    body = length > 0 ? socket.read(length).to_s : ""

    @requests << { path: request_line.split(" ")[1], headers: headers, body: body }

    status, extra, payload = next_response
    socket.print("HTTP/1.1 #{status} #{REASONS.fetch(status, 'Status')}\r\n")
    extra.each { |name, value| socket.print("#{name}: #{value}\r\n") }
    socket.print("Content-Length: #{payload.bytesize}\r\n")
    socket.print("Connection: close\r\n\r\n")
    socket.print(payload)
  ensure
    socket.close if socket && !socket.closed?
  end

  # Hands out the next scripted answer, falling back to a plain 202 once the
  # script runs out.
  def next_response
    @script.empty? ? [202, {}, ""] : @script.shift
  end
end

# Everything the SDK promises, asserted against what actually goes over a socket.
class FeasibleTest < Minitest::Test
  # Starts one endpoint per test so no test can see another's requests.
  def setup
    @server = ScriptedServer.new

    # backoff_base is zero so the retry tests assert on the rule rather than
    # sleeping through a real exponential delay.
    @client = Feasible::Client.new(domain: "example.com", host: @server.host, backoff_base: 0.0)
  end

  # Shuts the endpoint down whatever the test did.
  def teardown
    @server.stop
  end

  # The decoded body of one recorded request, so assertions are about JSON keys
  # rather than about a string whitespace could break.
  def payload(index = 0)
    JSON.parse(@server.requests[index][:body])
  end

  # An absent value must be omitted, never sent as null: the endpoint reads a
  # null as a value and overwrites what it derived.
  def test_pageview_sends_only_the_required_keys
    @client.pageview(
      url: "https://example.com/pricing",
      client_ip: "203.0.113.9",
      user_agent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"
    )

    assert_equal %w[k n u d], payload.keys
    assert_match UUID_V4, payload["k"]
    assert_equal "pageview", payload["n"]
    assert_equal "https://example.com/pricing", payload["u"]
    assert_equal "example.com", payload["d"]
    assert_equal "/api/event", @server.requests[0][:path]
  end

  # Every optional field travels under the single-letter key the ingest contract
  # fixes, and the attribution overrides keep their long names.
  def test_custom_event_sends_props_revenue_and_overrides
    @client.event(
      name: "Purchase",
      url: "https://example.com/checkout/complete",
      client_ip: "203.0.113.9",
      user_agent: "curl/8.4.0",
      title: "Order complete",
      referrer: "https://news.example/story",
      props: { "plan" => "pro", "seats" => 4, "trial" => false },
      revenue: Feasible::Revenue.new(amount: 49.5, currency: "usd"),
      attribution: Feasible::Attribution.new(utm_source: "newsletter", utm_campaign: "august"),
      interactive: false,
      scroll_depth: 80,
      engagement_time: 12_000,
      viewport_width: 1440
    )

    assert_equal %w[k n u d r t p $ i sd e w utm_source utm_campaign], payload.keys
    assert_equal({ "plan" => "pro", "seats" => 4, "trial" => false }, payload["p"])
    assert_equal({ "amount" => 49.5, "currency" => "USD" }, payload["$"])
    assert_equal false, payload["i"]
    assert_equal "https://news.example/story", payload["r"]
    assert_equal "newsletter", payload["utm_source"]
  end

  # text/plain is what the browser tracker sends; it avoids a CORS preflight and
  # the endpoint reads the body as JSON regardless.
  def test_content_type_is_text_plain
    @client.pageview(url: "https://example.com/", client_ip: "203.0.113.9", user_agent: "curl/8.4.0")

    assert_equal "text/plain", @server.requests[0][:headers]["content-type"]
  end

  # Anything other than the visitor's own values attributes the event to the
  # server rather than to the visitor.
  def test_visitor_headers_are_forwarded_verbatim
    @client.pageview(
      url: "https://example.com/",
      client_ip: "198.51.100.23",
      user_agent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)"
    )

    headers = @server.requests[0][:headers]
    assert_equal "198.51.100.23", headers["x-forwarded-for"]
    assert_equal "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)", headers["user-agent"]
  end

  # The error names the parameter so the fix is the next thing typed, and
  # nothing leaves the process.
  def test_missing_client_ip_is_refused_by_name
    error = assert_raises(Feasible::MissingClientIPError) do
      @client.pageview(url: "https://example.com/", client_ip: "  ", user_agent: "curl/8.4.0")
    end

    assert_includes error.message, "client_ip"
    assert_empty @server.requests
  end

  # Same rule for the user agent.
  def test_missing_user_agent_is_refused_by_name
    error = assert_raises(Feasible::MissingUserAgentError) do
      @client.event(name: "Signup", url: "https://example.com/", client_ip: "203.0.113.9", user_agent: nil)
    end

    assert_includes error.message, "user_agent"
    assert_empty @server.requests
  end

  # Every proxy appends itself to X-Forwarded-For, so the last entry is the
  # nearest proxy: taking it reports the load balancer as the visitor.
  def test_from_request_takes_the_first_forwarded_entry
    visitor = Feasible::Visitor.from_request(
      "HTTP_X_FORWARDED_FOR" => "198.51.100.23, 10.0.0.7, 10.0.0.8",
      "REMOTE_ADDR" => "10.0.0.8",
      "HTTP_USER_AGENT" => "Mozilla/5.0"
    )

    assert_equal "198.51.100.23", visitor.client_ip
    assert_equal "Mozilla/5.0", visitor.user_agent
  end

  # Cloudflare sets its own header and it is the more trustworthy of the two, so
  # it is preferred exactly as the ingest server prefers it.
  def test_cf_connecting_ip_wins
    visitor = Feasible::Visitor.from_rack(
      "HTTP_CF_CONNECTING_IP" => "198.51.100.5",
      "HTTP_X_FORWARDED_FOR" => "192.0.2.5",
      "REMOTE_ADDR" => "10.0.0.8",
      "HTTP_USER_AGENT" => "Mozilla/5.0"
    )

    assert_equal "198.51.100.5", visitor.client_ip
  end

  # With no forwarding headers the socket address is the visitor, which is the
  # ordinary case for an application with no proxy in front.
  def test_falls_back_to_the_socket_address
    visitor = Feasible::Visitor.from_request("REMOTE_ADDR" => "203.0.113.77", "HTTP_USER_AGENT" => "Mozilla/5.0")

    assert_equal "203.0.113.77", visitor.client_ip
  end

  # A half-filled visitor is rejected at construction, where the backtrace points
  # at the code that lost the value.
  def test_a_request_with_no_address_is_refused
    assert_raises(Feasible::MissingClientIPError) do
      Feasible::Visitor.from_request("HTTP_USER_AGENT" => "Mozilla/5.0")
    end
  end

  # Splatting the pair is what keeps a call site from dropping one of them or
  # transposing the two.
  def test_the_visitor_splats_into_a_call
    visitor = Feasible::Visitor.from_request("REMOTE_ADDR" => "203.0.113.77", "HTTP_USER_AGENT" => "Mozilla/5.0")
    @client.pageview(url: "https://example.com/", **visitor.to_h)

    assert_equal "203.0.113.77", @server.requests[0][:headers]["x-forwarded-for"]
  end

  # Nothing is sent, the call succeeds, and the event is kept in memory so a test
  # can assert on what the application reported.
  def test_disabled_client_records_instead_of_sending
    client = Feasible::Client.new(domain: "example.com", host: @server.host, disabled: true)

    result = client.event(
      name: "Purchase",
      url: "https://example.com/checkout/complete",
      client_ip: "203.0.113.9",
      user_agent: "curl/8.4.0",
      props: { "plan" => "pro" },
      revenue: Feasible::Revenue.new(amount: 49.5, currency: "USD")
    )

    assert_empty @server.requests
    refute result.sent?
    assert_equal 0, result.attempts

    recorded = client.recorded_events
    assert_equal 1, recorded.length
    assert_equal "Purchase", recorded[0].name
    assert_equal({ "plan" => "pro" }, recorded[0].props)
    assert_equal({ "amount" => 49.5, "currency" => "USD" }, recorded[0].payload["$"])
    assert_equal "203.0.113.9", recorded[0].client_ip

    client.clear_recorded_events
    assert_empty client.recorded_events
  end

  # The mistake gets caught by the test suite rather than by a customer.
  def test_disabled_client_still_refuses_a_missing_visitor
    client = Feasible::Client.new(domain: "example.com", host: @server.host, disabled: true)

    assert_raises(Feasible::MissingUserAgentError) do
      client.pageview(url: "https://example.com/", client_ip: "203.0.113.9", user_agent: "")
    end
  end

  # One variable in a CI container stops the whole application writing to the
  # customer's real numbers.
  def test_environment_variable_disables_the_client
    ENV["FEASIBLE_DISABLED"] = "1"

    begin
      client = Feasible::Client.new(domain: "example.com", host: @server.host)
      assert client.disabled?

      client.pageview(url: "https://example.com/", client_ip: "203.0.113.9", user_agent: "curl/8.4.0")
      assert_empty @server.requests
    ensure
      ENV.delete("FEASIBLE_DISABLED")
    end
  end

  # The same bytes may well succeed a moment later, so a 5xx is worth another
  # attempt.
  def test_a_500_is_retried_until_it_succeeds
    @server.script = [[500, {}, "upstream is unhappy"], [503, {}, "still unhappy"], [202, {}, ""]]

    result = @client.pageview(url: "https://example.com/", client_ip: "203.0.113.9", user_agent: "curl/8.4.0")

    assert_equal 3, @server.requests.length
    assert_equal 3, result.attempts
    assert_equal 202, result.status
  end

  # An endpoint that never recovers must not be hammered forever.
  def test_retries_stop_at_max_attempts
    @server.script = Array.new(5) { [503, {}, "down"] }
    client = Feasible::Client.new(domain: "example.com", host: @server.host, backoff_base: 0.0, max_attempts: 2)

    assert_raises(Feasible::APIError) do
      client.pageview(url: "https://example.com/", client_ip: "203.0.113.9", user_agent: "curl/8.4.0")
    end

    assert_equal 2, @server.requests.length
  end

  # A 400 is the caller's bug: the same bytes get the same answer, and the
  # server's explanation travels with the error.
  def test_a_400_is_never_retried
    @server.script = [[400, {}, "this request arrived from a datacentre address with no X-Forwarded-For"]]

    error = assert_raises(Feasible::BadRequestError) do
      @client.pageview(url: "https://example.com/", client_ip: "203.0.113.9", user_agent: "curl/8.4.0")
    end

    assert_equal 1, @server.requests.length
    assert_equal 400, error.status
    assert_includes error.message, "datacentre address"
  end

  # A drop is a classification, not a failure. Retrying reaches the same
  # classifier, and swallowing the reason is how silent data loss starts.
  def test_a_dropped_202_is_not_retried_and_surfaces_the_reason
    @server.script = [[202, { "x-feasible-dropped" => "datacenter_ip" }, ""]]

    result = @client.pageview(url: "https://example.com/", client_ip: "203.0.113.9", user_agent: "curl/8.4.0")

    assert_equal 1, @server.requests.length
    assert_equal "datacenter_ip", result.dropped
    assert result.dropped?
  end

  # Nothing came back at all, which is the one case worth trying again — and
  # worth its own error class when every attempt fails the same way.
  # The server dedupes on "k", so a retry after a lost acknowledgement must
  # resend the same key — and the next event must get a fresh one.
  def test_the_idempotency_key_survives_a_retry
    @server.script = [[500, {}, "upstream is unhappy"], [202, {}, ""], [202, {}, ""]]

    @client.pageview(url: "https://example.com/", client_ip: "203.0.113.9", user_agent: "curl/8.4.0")
    @client.pageview(url: "https://example.com/", client_ip: "203.0.113.9", user_agent: "curl/8.4.0")

    assert_equal 3, @server.requests.length
    first, retried, fresh = (0..2).map { |i| payload(i)["k"] }
    assert_match UUID_V4, first
    assert_equal first, retried
    refute_equal first, fresh
  end

  def test_a_transport_failure_is_retried_then_reported
    # A port nothing is listening on: bound to learn a free number, then closed,
    # so the connection is refused immediately rather than hanging.
    probe = TCPServer.new("127.0.0.1", 0)
    dead_port = probe.addr[1]
    probe.close

    client = Feasible::Client.new(
      domain: "example.com",
      host: "http://127.0.0.1:#{dead_port}",
      backoff_base: 0.0,
      max_attempts: 3
    )

    error = assert_raises(Feasible::TransportError) do
      client.pageview(url: "https://example.com/", client_ip: "203.0.113.9", user_agent: "curl/8.4.0")
    end

    assert_equal 3, error.attempts
  end

  # It asks the server what it would have written and writes nothing, so it is
  # safe to run against production.
  def test_debug_returns_the_derived_event
    @server.script = [
      [200, { "content-type" => "application/json" }, '{"site_id":7,"country":"US","bot_reason":""}']
    ]

    derived = @client.debug(
      name: "pageview",
      url: "https://example.com/",
      client_ip: "203.0.113.9",
      user_agent: "curl/8.4.0"
    )

    assert_equal "true", @server.requests[0][:headers]["x-debug-request"]
    assert_equal "US", derived["country"]
  end

  # A bad currency fails at the call site, because the server ignores a revenue
  # object it cannot read and silently zero revenue is the worst kind of bug.
  def test_a_bad_currency_is_refused
    assert_raises(Feasible::InvalidEventError) do
      Feasible::Revenue.new(amount: 10, currency: "dollars")
    end
  end

  # A self-hosted install posts to its own host, with any trailing slash removed
  # so the path does not double up.
  def test_a_custom_host_is_honoured
    client = Feasible::Client.new(domain: "example.com", host: "#{@server.host}/")
    client.pageview(url: "https://example.com/", client_ip: "203.0.113.9", user_agent: "curl/8.4.0")

    assert_equal "/api/event", @server.requests[0][:path]
  end
end
