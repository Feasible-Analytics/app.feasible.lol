<?php
//
// BadRequestException.php
// Thrown for a 400, which is always the caller's bug and never retried.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Exception;

/**
 * The endpoint refused the request. A 400 describes something wrong with what
 * was sent — a missing key, or the visitor's address and user agent not being
 * forwarded — so it gets its own type and is never retried: the same bytes sent
 * again produce the same 400 and only delay the caller.
 */
final class BadRequestException extends ApiException
{
}
