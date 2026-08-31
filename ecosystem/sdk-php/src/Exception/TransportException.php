<?php
//
// TransportException.php
// Thrown when the request never reached the ingest endpoint.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Exception;

/**
 * The request failed at the network layer and every retry failed the same way.
 * It is kept separate from an HTTP error because the two need different
 * responses: this one is usually the caller's egress or DNS, not the payload.
 */
final class TransportException extends FeasibleException
{
    /**
     * Carries the attempt count so a log line can say whether the endpoint was
     * tried once or three times before the caller gave up.
     */
    public function __construct(string $message, public readonly int $attempts = 1)
    {
        parent::__construct($message);
    }
}
