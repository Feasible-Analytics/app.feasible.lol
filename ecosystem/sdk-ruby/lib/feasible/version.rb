#
# version.rb
# The gem version, in one place the gemspec and the code can both read.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

# frozen_string_literal: true

module Feasible
  # Kept in its own file so the gemspec can require it without loading the rest
  # of the gem, which would otherwise drag net/http into every `gem build`.
  VERSION = "1.0.0"
end
