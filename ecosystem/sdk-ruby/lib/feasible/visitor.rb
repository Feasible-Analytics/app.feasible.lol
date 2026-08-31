#
# visitor.rb
# Reading the visitor's address and user agent off an incoming request.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

# frozen_string_literal: true

module Feasible
  # The two things a server-side call must forward. They travel together because
  # they are always needed together, and because the resolution rules — which
  # header wins, and which entry of it — are worth writing down once rather than
  # at every call site in an application.
  class Visitor
    attr_reader :client_ip, :user_agent

    # Refuses to hold a half-filled visitor. An empty address or user agent is
    # exactly the mistake this gem exists to prevent, so it is rejected at
    # construction, where the backtrace points at the code that lost the value
    # rather than at the gem.
    def initialize(client_ip:, user_agent:)
      ip = client_ip.to_s.strip
      agent = user_agent.to_s.strip

      raise MissingClientIPError, "client_ip" if ip.empty?
      raise MissingUserAgentError, "user_agent" if agent.empty?

      @client_ip = ip
      @user_agent = agent
    end

    class << self
      # Reads the visitor from a Rack env — the hash every Rails controller,
      # Sinatra route and Rack middleware already has in front of it.
      def from_rack(env)
        headers = {
          "cf-connecting-ip" => text(env["HTTP_CF_CONNECTING_IP"]),
          "x-forwarded-for" => text(env["HTTP_X_FORWARDED_FOR"]),
          "user-agent" => text(env["HTTP_USER_AGENT"])
        }

        from_headers(headers, remote_addr: text(env["REMOTE_ADDR"]))
      end

      # The name to remember. Rack is what a Ruby web application speaks, so
      # this is the same thing as from_rack and exists so the documentation does
      # not have to fork per web stack.
      def from_request(env)
        from_rack(env)
      end

      # Reads the visitor from a plain headers hash plus the socket address, for
      # a caller holding headers rather than a Rack env — a background job
      # replaying a stored request, or a non-Rack HTTP library.
      def from_headers(headers, remote_addr: nil)
        lookup = {}
        headers.each { |name, value| lookup[name.to_s.downcase] = text(value) }

        new(client_ip: resolve_client_ip(lookup, remote_addr), user_agent: lookup["user-agent"].to_s)
      end

      # Resolves the address with the same precedence the ingest server uses:
      # CF-Connecting-IP, then the FIRST entry of X-Forwarded-For, then the
      # socket address.
      #
      # The first entry is the one that matters. Every proxy appends itself to
      # X-Forwarded-For, so the last entry is the nearest proxy — taking it, as
      # several frameworks do, reports your own load balancer as the visitor and
      # collapses every visit in the world into one.
      #
      # The hash's keys must already be lower-cased; from_headers does that.
      def resolve_client_ip(headers, remote_addr = nil)
        cloudflare = headers["cf-connecting-ip"].to_s.strip
        return cloudflare unless cloudflare.empty?

        forwarded = headers["x-forwarded-for"].to_s.strip
        unless forwarded.empty?
          first = forwarded.split(",").first.to_s.strip
          return first unless first.empty?
        end

        remote_addr.to_s.strip
      end

      private

      # Coerces one header value to a trimmed string. Anything that is not a
      # string — a framework storing an array of values, say — is treated as
      # absent, because guessing which entry was meant is how the wrong address
      # ends up on every event.
      def text(value)
        value.is_a?(String) ? value.strip : ""
      end
    end

    # Returns the pair as keyword arguments, so a call reads
    # `client.pageview(url: url, **visitor.to_h)` and neither value can be
    # forgotten or transposed.
    def to_h
      { client_ip: client_ip, user_agent: user_agent }
    end
  end
end
