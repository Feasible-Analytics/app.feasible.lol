#
# __init__.py
# Marks the tests directory as a package.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#

"""Test package marker.

`python3 -m unittest discover` only descends into importable directories, so
without this file the suite is silently found to contain no tests at all.
"""
