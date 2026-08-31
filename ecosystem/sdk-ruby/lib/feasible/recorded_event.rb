#
# recorded_event.rb
# One event a disabled client kept in memory instead of sending.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

# frozen_string_literal: true

module Feasible
  # What a no-op client remembers. A test suite needs to assert that the
  # checkout reported the revenue it charged, and the alternatives are a network
  # call from the test or a hand-written mock of this gem — both of which stop
  # catching the mistake the moment the payload changes.
  class RecordedEvent
    attr_reader :payload, :client_ip, :user_agent

    # Keeps the forwarded address and user agent beside the payload, because
    # forgetting them is the failure worth asserting against and they are not
    # part of the body.
    def initialize(payload:, client_ip:, user_agent:, debug: false)
      @payload = payload
      @client_ip = client_ip
      @user_agent = user_agent
      @debug = debug
    end

    # Whether this was a debug call rather than a write, so a test can tell the
    # two apart in one recording.
    def debug?
      @debug
    end

    # The event name, which is what a test asserts on first and is otherwise a
    # single-letter key lookup at every call site.
    def name
      payload["n"].to_s
    end

    # The page the event happened on, for the same reason as name.
    def url
      payload["u"].to_s
    end

    # The custom properties, defaulting to an empty hash so a test can index
    # into them without first checking whether any were sent.
    def props
      payload["p"].is_a?(Hash) ? payload["p"] : {}
    end
  end
end
