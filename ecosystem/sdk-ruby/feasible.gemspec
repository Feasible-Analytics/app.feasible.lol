#
# feasible.gemspec
# Gem metadata for the feasible Ruby SDK.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

# frozen_string_literal: true

require File.expand_path("lib/feasible/version", __dir__)

Gem::Specification.new do |spec|
  spec.name = "feasible"
  spec.version = Feasible::VERSION
  spec.authors = ["Cloudmanic Labs, LLC"]
  spec.summary = "Server-side event tracking for feasible.lol"
  spec.description = "Server-side event tracking for feasible.lol. The visitor's IP address and " \
                     "User-Agent are required arguments, because forgetting either is what makes " \
                     "server-side analytics silently wrong."
  spec.homepage = "https://feasible.lol"
  spec.license = "MIT"

  # The gem is built from an explicit list rather than from git, so it packages
  # identically whether or not the build machine has git or a checkout.
  spec.files = Dir["lib/**/*.rb"] + ["README.md", "LICENSE", "feasible.gemspec"]
  spec.require_paths = ["lib"]

  # 2.6 is the Ruby that ships with macOS, and an analytics gem is not worth
  # forcing an upgrade over.
  spec.required_ruby_version = ">= 2.6.0"

  spec.metadata = {
    "homepage_uri" => spec.homepage,
    "source_code_uri" => spec.homepage
  }

  # No runtime dependencies at all: net/http, json and uri are the whole stack,
  # so this gem cannot conflict with what the application already installs.
end
