<?php
//
// MissingUserAgentException.php
// Thrown when a call would report the visitor's browser as unknown.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Exception;

/**
 * The visitor's User-Agent is missing. An event with no user agent has no
 * browser, no operating system and no device, and a request carrying neither an
 * address nor a user agent is classified as a datacentre bot and dropped, so
 * the call is refused before it can quietly cost the customer a dimension.
 */
final class MissingUserAgentException extends FeasibleException
{
    /**
     * Names the parameter so the caller can fix the call site without reading
     * the ingest contract.
     */
    public function __construct(string $parameter = 'userAgent')
    {
        parent::__construct(
            sprintf(
                '%s is required and was empty. Pass the visitor\'s real User-Agent, not your HTTP client\'s: '
                . 'it is what browser, OS and device are derived from, and a request with neither an address '
                . 'nor a user agent is treated as a datacentre bot. Feasible\\Visitor::fromRequest() reads it '
                . 'off the incoming request for you.',
                $parameter
            )
        );
    }
}
