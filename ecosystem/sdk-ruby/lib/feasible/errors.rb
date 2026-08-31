#
# errors.rb
# Every exception this gem raises, and why each one is its own class.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

# frozen_string_literal: true

module Feasible
  # The one class that contains everything this gem raises. Analytics is never
  # the reason a checkout should fail, so an application that wants to swallow
  # tracking problems needs a single rescue rather than a list that goes stale
  # the moment a new error is added.
  class Error < StandardError; end

  # The visitor's address was not supplied. Sending anyway is worse than not
  # sending: the request leaves from a datacentre address, the server classifies
  # it as a bot or answers 400, and the numbers look believable enough that
  # nobody notices for weeks.
  class MissingClientIPError < Error
    attr_reader :parameter

    # Names the parameter rather than the concept, so the fix is the next thing
    # the reader types instead of something they go and look up.
    def initialize(parameter = "client_ip")
      @parameter = parameter

      super(
        "#{parameter} is required and was empty. Pass the visitor's real IP address, not your " \
        "server's: it becomes the X-Forwarded-For header, and without it every event is attributed " \
        "to your server rather than to the visitor. Feasible::Visitor.from_request(env) reads it " \
        "off the incoming request for you."
      )
    end
  end

  # The visitor's User-Agent was not supplied. An event with no user agent has
  # no browser, no operating system and no device, and a request carrying
  # neither an address nor a user agent is treated as a datacentre bot.
  class MissingUserAgentError < Error
    attr_reader :parameter

    # Names the parameter so the call site can be fixed without reading the
    # ingest contract.
    def initialize(parameter = "user_agent")
      @parameter = parameter

      super(
        "#{parameter} is required and was empty. Pass the visitor's real User-Agent, not your HTTP " \
        "client's: it is what browser, OS and device are derived from, and a request with neither " \
        "an address nor a user agent is treated as a datacentre bot. " \
        "Feasible::Visitor.from_request(env) reads it off the incoming request for you."
      )
    end
  end

  # The event could not be built. Catching a missing name or URL locally saves a
  # round trip to learn the same thing from a 400, and it raises at the call
  # site that is actually wrong.
  class InvalidEventError < Error; end

  # The request never reached the endpoint, on every attempt. Kept apart from an
  # HTTP error because the two need different responses: this one is usually
  # egress, DNS or a firewall rather than the payload.
  class TransportError < Error
    attr_reader :attempts

    # Carries the attempt count so a log line can say whether the endpoint was
    # tried once or three times before the caller gave up.
    def initialize(message, attempts: 1)
      @attempts = attempts
      super(message)
    end
  end

  # The endpoint answered with a status this gem will not treat as accepted. The
  # status and the body both travel with the error because the endpoint explains
  # itself in the body, and hiding that sentence is what turns a two-minute fix
  # into a support ticket.
  class APIError < Error
    attr_reader :status, :body, :attempts

    # Builds the message from the server's own words, falling back to the status
    # when the body is empty.
    def initialize(status, body, attempts: 1)
      @status = status
      @body = body.to_s
      @attempts = attempts

      detail = @body.strip
      detail = "the response body was empty" if detail.empty?

      super("the ingest endpoint answered #{status}: #{detail}")
    end
  end

  # The endpoint refused the request with a 400. It describes something wrong
  # with what was sent — a missing key, or the visitor's address and user agent
  # not being forwarded — so it has its own class and is never retried: the same
  # bytes produce the same answer.
  class BadRequestError < APIError; end
end
