<?php
//
// MissingClientIpException.php
// Thrown when a call would forward the server's own address as the visitor's.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Exception;

/**
 * The visitor's address is missing. Sending the event anyway is worse than not
 * sending it: the request leaves from a datacentre address, the server answers
 * 400 or classifies every visit as a bot, and the numbers look believable enough
 * that nobody notices for weeks. Failing here, loudly and by name, is the point
 * of this package.
 */
final class MissingClientIpException extends FeasibleException
{
    /**
     * Names the parameter rather than the concept so the fix is the next thing
     * the reader types, not something they have to go and look up.
     */
    public function __construct(string $parameter = 'clientIp')
    {
        parent::__construct(
            sprintf(
                '%s is required and was empty. Pass the visitor\'s real IP address, not your server\'s: '
                . 'it becomes the X-Forwarded-For header, and without it every event is attributed to your '
                . 'server rather than to the visitor. Feasible\\Visitor::fromRequest() reads it off the '
                . 'incoming request for you.',
                $parameter
            )
        );
    }
}
