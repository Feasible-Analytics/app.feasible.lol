#
# feasible.rb
# The public surface of the feasible gem.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

# frozen_string_literal: true

require "feasible/version"
require "feasible/errors"
require "feasible/revenue"
require "feasible/attribution"
require "feasible/visitor"
require "feasible/result"
require "feasible/recorded_event"
require "feasible/transport"
require "feasible/client"

# Server-side event tracking for feasible.lol.
#
# The visitor's IP address and User-Agent are required arguments on every call.
# A server-side request that forwards neither is classified as a datacentre bot
# and dropped, so this gem refuses to send rather than let the mistake through:
#
#   client  = Feasible.client(domain: "example.com")
#   visitor = Feasible::Visitor.from_request(request.env)
#
#   client.pageview(url: request.url, **visitor.to_h)
module Feasible
  # Builds a client. It exists so the common case is one line and one name to
  # remember, rather than a constant path an application has to reach through.
  def self.client(**options)
    Client.new(**options)
  end
end
