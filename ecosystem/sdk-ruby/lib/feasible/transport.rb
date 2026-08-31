#
# transport.rb
# The seam between the gem and the network, over the standard library only.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

# frozen_string_literal: true

require "net/http"
require "uri"

module Feasible
  module Transport
    # One HTTP answer, reduced to the three things the gem reads. Header names
    # arrive lower-cased so no part of the gem has to guess the capitalisation
    # the endpoint or a proxy in front of it chose.
    class Response
      attr_reader :status, :headers, :body

      # Takes the pieces already normalised, so every transport produces the
      # same shape and the retry rules cannot behave differently per transport.
      def initialize(status:, headers: {}, body: "")
        @status = status
        @headers = headers
        @body = body
      end

      # Reads one header without the caller knowing its capitalisation.
      def header(name)
        headers[name.to_s.downcase]
      end
    end

    # How a request leaves the process. It is replaceable so an application with
    # its own HTTP client, proxy or instrumentation can hand one in rather than
    # have this gem open its own connections, and so a test can assert on the
    # exact bytes with no socket at all.
    #
    # The method is `send_request` rather than `send`, which every Ruby object
    # already answers to and which would be a surprising thing to override.
    class Base
      # Performs one request and returns a Response. Implementations raise
      # Feasible::TransportError when nothing came back at all, which is the
      # signal the retry loop reads.
      def send_request(_url, _headers, _body, _timeout)
        raise NotImplementedError, "a transport must implement send_request"
      end
    end

    # The default transport, on net/http. The standard library is the whole
    # dependency list on purpose: an analytics gem that drags in an HTTP stack
    # is a gem that causes version conflicts in applications whose actual work
    # has nothing to do with analytics.
    class NetHttp < Base
      # POSTs the body and normalises whatever comes back. An HTTP error status
      # is returned rather than raised, because a 400 carries the endpoint's own
      # explanation in its body and losing that sentence is what turns a
      # two-minute fix into a support ticket.
      def send_request(url, headers, body, timeout)
        uri = URI.parse(url)

        http = Net::HTTP.new(uri.host, uri.port)
        http.use_ssl = uri.scheme == "https"
        http.open_timeout = timeout
        http.read_timeout = timeout

        request = Net::HTTP::Post.new(uri.request_uri)
        headers.each { |name, value| request[name] = value }
        request.body = body

        response = begin
          http.request(request)
        rescue StandardError => e
          # A timeout, a refused connection, a DNS failure: nothing came back,
          # which is the one case worth trying again.
          raise TransportError, "the request to #{url} failed: #{e.message}"
        end

        collected = {}
        response.each_header { |name, value| collected[name.to_s.downcase] = value }

        Response.new(status: response.code.to_i, headers: collected, body: response.body.to_s)
      end
    end
  end
end
