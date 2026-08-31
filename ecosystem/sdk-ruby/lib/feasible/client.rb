#
# client.rb
# The server-side ingest client: one event, the visitor's address, and the visitor's user agent.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

# frozen_string_literal: true

require "json"

module Feasible
  # Sends events to POST /api/event from your server.
  #
  # Read this before your first call. A server-side event carries two things the
  # browser would have carried by itself, and both are required arguments here:
  #
  #   client_ip:  the visitor's real IP, forwarded as X-Forwarded-For.
  #   user_agent: the visitor's real User-Agent, forwarded verbatim.
  #
  # A request that arrives from a datacentre address with neither is classified
  # as a bot and dropped, and nothing in the response says so loudly enough to
  # notice. Passing your own server's address is worse than passing nothing: it
  # looks like data. Feasible::Visitor.from_request(env) reads both off the
  # incoming Rack request, resolving the address the way the ingest server does.
  #
  # In a test environment pass `disabled: true`, or set FEASIBLE_DISABLED=1:
  # nothing is sent, calls succeed, and every event is kept in memory for
  # #recorded_events to assert on.
  class Client
    # The hosted endpoint. Self-hosted installs pass their own host.
    DEFAULT_HOST = "https://app.feasible.lol"

    # The response header carrying the reason an accepted event was not counted.
    HEADER_DROPPED = "x-feasible-dropped"

    # The path is part of the wire contract and is not configurable.
    EVENT_PATH = "/api/event"

    # Values of FEASIBLE_DISABLED that mean "do not send anything".
    TRUTHY = %w[1 true yes on].freeze

    attr_reader :domain, :endpoint, :timeout, :max_attempts, :backoff_base, :backoff_cap, :transport

    # Builds a client for one site.
    #
    # `domain` is the site as registered — what the tracking script would put in
    # data-domain — and not the URL of a page.
    #
    # `disabled` is tri-state rather than a plain false so an explicit
    # `disabled: false` in application code still wins over FEASIBLE_DISABLED=1
    # in the environment, while leaving it unset defers to the environment,
    # which is what a shared test container wants.
    def initialize(domain:, host: DEFAULT_HOST, timeout: 5.0, max_attempts: 3, backoff_base: 0.25,
                   backoff_cap: 5.0, disabled: nil, transport: nil)
      raise InvalidEventError, 'domain is required: it is the site as registered, such as "example.com"' if
        domain.to_s.strip.empty?

      raise InvalidEventError, "max_attempts must be at least 1" if max_attempts < 1

      @domain = domain.to_s.strip
      @endpoint = "#{host.to_s.strip.sub(%r{/+\z}, '')}#{EVENT_PATH}"
      @timeout = timeout
      @max_attempts = max_attempts
      @backoff_base = backoff_base
      @backoff_cap = backoff_cap
      @disabled = disabled.nil? ? self.class.disabled_by_environment? : disabled
      @transport = transport || Transport::NetHttp.new
      @recorded = []
    end

    # Reports whether this client is in no-op mode, so an application can log
    # once at boot rather than wonder later why a staging dashboard is empty.
    def disabled?
      @disabled
    end

    # Records a pageview. Every report is built from pageviews, so it gets its
    # own method rather than leaving callers to remember that the name is the
    # literal string "pageview".
    def pageview(url:, client_ip:, user_agent:, title: nil, referrer: nil, props: nil, revenue: nil,
                 attribution: nil, interactive: nil, scroll_depth: nil, engagement_time: nil,
                 viewport_width: nil)
      event(
        name: "pageview",
        url: url,
        client_ip: client_ip,
        user_agent: user_agent,
        title: title,
        referrer: referrer,
        props: props,
        revenue: revenue,
        attribution: attribution,
        interactive: interactive,
        scroll_depth: scroll_depth,
        engagement_time: engagement_time,
        viewport_width: viewport_width
      )
    end

    # Records a custom event — a signup, a purchase, a plan change.
    #
    # `url` is required even for an event with no page of its own, because every
    # report groups by page and an event without one cannot be found again. For
    # an offline conversion, pass the URL it belongs to and set `attribution` so
    # it is not filed as Direct forever.
    def event(name:, url:, client_ip:, user_agent:, title: nil, referrer: nil, props: nil, revenue: nil,
              attribution: nil, interactive: nil, scroll_depth: nil, engagement_time: nil,
              viewport_width: nil)
      payload = build_payload(
        name: name, url: url, title: title, referrer: referrer, props: props, revenue: revenue,
        attribution: attribution, interactive: interactive, scroll_depth: scroll_depth,
        engagement_time: engagement_time, viewport_width: viewport_width
      )

      dispatch(payload, client_ip, user_agent, false)
    end

    # Asks the server what it would derive from this event and returns that
    # instead of writing anything. It answers "why is this visit attributed to
    # the wrong country" in one call, against production, with no side effects.
    def debug(name:, url:, client_ip:, user_agent:, title: nil, referrer: nil, props: nil, revenue: nil,
              attribution: nil, interactive: nil, scroll_depth: nil, engagement_time: nil,
              viewport_width: nil)
      payload = build_payload(
        name: name, url: url, title: title, referrer: referrer, props: props, revenue: revenue,
        attribution: attribution, interactive: interactive, scroll_depth: scroll_depth,
        engagement_time: engagement_time, viewport_width: viewport_width
      )

      result = dispatch(payload, client_ip, user_agent, true)
      return {} unless result.sent?

      derived = begin
        JSON.parse(result.body)
      rescue JSON::ParserError
        raise APIError.new(result.status, result.body, attempts: result.attempts)
      end

      raise APIError.new(result.status, result.body, attempts: result.attempts) unless derived.is_a?(Hash)

      derived
    end

    # The events a disabled client kept. This is the supported way to test
    # analytics: assert on what your code reported without a network, a mock of
    # this gem, or a test double that stops matching the payload.
    def recorded_events
      @recorded.dup
    end

    # Empties the recording, so one test case cannot see another's events.
    def clear_recorded_events
      @recorded = []
    end

    # Reads the environment switch. A test container, a CI job or a local
    # development machine sets one variable and the whole application stops
    # writing to the customer's real numbers.
    def self.disabled_by_environment?
      TRUTHY.include?(ENV["FEASIBLE_DISABLED"].to_s.strip.downcase)
    end

    private

    # Assembles the wire payload. An absent value is omitted rather than sent as
    # null: the endpoint reads a null as a value and would overwrite what it
    # derived with nothing.
    def build_payload(name:, url:, title:, referrer:, props:, revenue:, attribution:, interactive:,
                      scroll_depth:, engagement_time:, viewport_width:)
      name = name.to_s.strip
      url = url.to_s.strip

      raise InvalidEventError, 'name is required: "pageview" for a pageview, or your own event name' if name.empty?

      if url.empty?
        raise InvalidEventError, "url is required: the full URL of the page the event happened on"
      end

      payload = { "n" => name, "u" => url, "d" => domain }

      payload["r"] = referrer unless referrer.nil? || referrer.to_s.strip.empty?
      payload["t"] = title unless title.nil? || title.to_s.strip.empty?
      payload["p"] = props unless props.nil? || props.empty?
      payload["$"] = revenue.to_wire unless revenue.nil?

      # Interactive is only sent when the caller said so, because the server
      # defaults an absent flag to true and that is what an ordinary event is.
      payload["i"] = interactive unless interactive.nil?

      payload["sd"] = scroll_depth unless scroll_depth.nil?
      payload["e"] = engagement_time unless engagement_time.nil?
      payload["w"] = viewport_width unless viewport_width.nil?

      payload.merge!(attribution.to_wire) unless attribution.nil?

      payload
    end

    # Validates the visitor, then either records or sends. The validation runs
    # in no-op mode too, on purpose: a test suite that never exercised the check
    # would let a call with no address ship to production unnoticed, which is
    # the exact failure this gem prevents.
    def dispatch(payload, client_ip, user_agent, debug)
      ip = client_ip.to_s.strip
      agent = user_agent.to_s.strip

      raise MissingClientIPError, "client_ip" if ip.empty?
      raise MissingUserAgentError, "user_agent" if agent.empty?

      if disabled?
        @recorded << RecordedEvent.new(payload: payload, client_ip: ip, user_agent: agent, debug: debug)

        return Result.new(status: 0, dropped: nil, attempts: 0, sent: false, body: "")
      end

      headers = {
        # text/plain is deliberate. It is what the browser tracker sends, it
        # avoids a CORS preflight, and the endpoint reads the body as JSON
        # regardless of the declared type.
        "Content-Type" => "text/plain",
        "X-Forwarded-For" => ip,
        "User-Agent" => agent
      }

      headers["X-Debug-Request"] = "true" if debug

      post(headers, JSON.generate(payload))
    end

    # Sends with retries. What is retried is deliberately narrow: a transport
    # failure, a 429 and a 5xx are conditions a second attempt can genuinely
    # fix, and nothing else is. A 400 is the caller's bug and would fail
    # identically; a 202 carrying a drop reason is a classification, not a
    # failure, and retrying reaches the same classifier.
    def post(headers, body)
      attempt = 0

      loop do
        attempt += 1

        begin
          response = transport.send_request(endpoint, headers, body, timeout)
        rescue TransportError => e
          raise TransportError.new(e.message, attempts: attempt) if attempt >= max_attempts

          pause(attempt)
          next
        end

        status = response.status

        raise BadRequestError.new(status, response.body, attempts: attempt) if status == 400

        if status == 429 || status >= 500
          raise APIError.new(status, response.body, attempts: attempt) if attempt >= max_attempts

          pause(attempt)
          next
        end

        raise APIError.new(status, response.body, attempts: attempt) if status < 200 || status >= 300

        return Result.new(
          status: status,
          dropped: response.header(HEADER_DROPPED),
          attempts: attempt,
          sent: true,
          body: response.body
        )
      end
    end

    # Waits before the next attempt. The delay is exponential so a struggling
    # endpoint is not hammered, capped so a background job does not sleep for
    # minutes, and jittered so a fleet of servers that all failed at the same
    # moment does not retry in lockstep and repeat the outage.
    def pause(attempt)
      delay = [backoff_cap, backoff_base * (2**(attempt - 1))].min
      return if delay <= 0

      sleep(delay / 2 + rand * (delay / 2))
    end
  end
end
