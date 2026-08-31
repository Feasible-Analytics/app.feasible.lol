#
# attribution.rb
# The server-side attribution overrides for an event with no referrer of its own.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

# frozen_string_literal: true

module Feasible
  # The override fields the ingest endpoint honours for server-side callers. A
  # delayed or offline conversion — a webhook hours later, a phone order, a
  # refund — has no referrer of its own and would be filed as Direct forever, so
  # the campaign that earned it is passed explicitly instead.
  #
  # Ignored on browser traffic, where the real referrer is authoritative.
  class Attribution
    attr_reader :referrer, :utm_source, :utm_medium, :utm_campaign, :utm_content, :utm_term

    # Every field is optional because a caller usually knows one or two of them
    # — the campaign, say — and inventing values for the rest would be worse
    # than leaving them absent.
    def initialize(referrer: nil, utm_source: nil, utm_medium: nil, utm_campaign: nil, utm_content: nil,
                   utm_term: nil)
      @referrer = referrer
      @utm_source = utm_source
      @utm_medium = utm_medium
      @utm_campaign = utm_campaign
      @utm_content = utm_content
      @utm_term = utm_term
    end

    # Renders only the fields that were set. An absent key is omitted rather
    # than sent as null, because the endpoint reads a null as a value and would
    # overwrite what it derived with nothing.
    def to_wire
      pairs = {
        "referrer" => referrer,
        "utm_source" => utm_source,
        "utm_medium" => utm_medium,
        "utm_campaign" => utm_campaign,
        "utm_content" => utm_content,
        "utm_term" => utm_term
      }

      pairs.each_with_object({}) do |(key, value), out|
        out[key] = value unless value.nil? || value.to_s.strip.empty?
      end
    end
  end
end
