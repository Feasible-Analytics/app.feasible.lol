<?php
//
// InvalidEventException.php
// Thrown for an event the ingest endpoint would reject as malformed.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Exception;

/**
 * The event could not be built. Catching a missing name or URL locally saves a
 * network round trip to learn the same thing from a 400, and it puts the stack
 * trace at the call site that is actually wrong.
 */
final class InvalidEventException extends FeasibleException
{
}
