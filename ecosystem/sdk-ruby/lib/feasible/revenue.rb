#
# revenue.rb
# The money one event reports.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

# frozen_string_literal: true

module Feasible
  # The `$` field of the wire payload. It is a class rather than a loose hash so
  # a currency typo fails at the call site: the server ignores a revenue object
  # with no currency, and revenue that is silently zero is the hardest kind of
  # missing data to notice.
  class Revenue
    attr_reader :amount, :currency

    # Normalises the currency to the upper-case ISO 4217 form the server stores,
    # so that "usd" and "USD" do not become two rows on the same report.
    def initialize(amount:, currency:)
      code = currency.to_s.strip.upcase

      unless code =~ /\A[A-Z]{3}\z/
        raise InvalidEventError,
              "revenue currency #{currency.inspect} is not a three-letter ISO 4217 code, such as USD or GBP"
      end

      @amount = amount
      @currency = code
    end

    # Renders the wire shape. The key names are the server's and are not
    # configurable, so they live in exactly one place.
    def to_wire
      { "amount" => amount, "currency" => currency }
    end
  end
end
