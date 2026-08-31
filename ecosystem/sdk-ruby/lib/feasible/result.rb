#
# result.rb
# What one accepted event came back as, including why it was dropped.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

# frozen_string_literal: true

module Feasible
  # The answer to a send. The drop reason is a field rather than something the
  # gem swallows, because the endpoint answers 202 even for events it decided
  # not to count: without it, a filter silently discarding half a customer's
  # traffic looks exactly like success.
  class Result
    attr_reader :status, :dropped, :attempts, :body

    # Holds the whole answer, including the raw body, so a caller logging a
    # surprise has everything the server said and does not have to reproduce the
    # request to find out.
    def initialize(status:, dropped: nil, attempts: 1, sent: true, body: "")
      @status = status
      @dropped = dropped
      @attempts = attempts
      @sent = sent
      @body = body
    end

    # Whether anything actually left the process, which is false in no-op mode
    # and is what a caller checks before logging a send.
    def sent?
      @sent
    end

    # Whether the event was accepted but classified. It is not a failure and
    # must not be retried — the retry reaches the same classifier and gets the
    # same answer.
    def dropped?
      !dropped.nil? && !dropped.to_s.empty?
    end
  end
end
