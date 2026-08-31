//
// index.js
// The ESM entry point: named exports over the shared core.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

import core from "./core.cjs";

// Every binding is re-exported by hand rather than with a star. A star export
// over a CommonJS module leans on static analysis of the module's shape, which
// works until it quietly does not; naming them means an `import { track }` that
// would break is a failure here, in this package's own test run, rather than in
// somebody's bundler.
export const FeasibleClient = core.FeasibleClient;
export const FeasibleValidationError = core.FeasibleValidationError;
export const FeasibleApiError = core.FeasibleApiError;
export const FeasibleTransportError = core.FeasibleTransportError;
export const createClient = core.createClient;
export const visitorFromNodeRequest = core.visitorFromNodeRequest;
export const visitorFromWebRequest = core.visitorFromWebRequest;
export const DEFAULT_HOST = core.DEFAULT_HOST;
export const DISABLED_ENV = core.DISABLED_ENV;
export const HEADER_DROPPED = core.HEADER_DROPPED;
export const HEADER_DEBUG = core.HEADER_DEBUG;

export default core;
