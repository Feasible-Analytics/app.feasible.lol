//
// index.cjs
// The CommonJS entry point.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

"use strict";

// The core is already CommonJS, so this entry is one line. It exists as its own
// file so the `require` condition in the exports map points at a stable path
// that can gain a shim later without moving the implementation.
module.exports = require("./core.cjs");
