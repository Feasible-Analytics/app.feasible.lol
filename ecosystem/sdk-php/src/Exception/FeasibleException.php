<?php
//
// FeasibleException.php
// The base every exception this package throws extends.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Exception;

use RuntimeException;

/**
 * The single type a caller can catch to contain everything this SDK throws.
 * Analytics is never the reason a checkout should fail, so an application that
 * wants to swallow tracking problems needs one catch block rather than a list
 * that goes stale the moment a new exception is added.
 */
abstract class FeasibleException extends RuntimeException
{
}
