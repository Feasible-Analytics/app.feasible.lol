<?php
//
// ApiException.php
// Thrown when the ingest endpoint answered, and the answer was not a success.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Exception;

/**
 * The endpoint answered with a status this SDK will not treat as accepted. The
 * status and the body both travel with the exception because the ingest
 * endpoint explains itself in the body, and hiding that sentence is what turns
 * a two-minute fix into a support ticket.
 */
class ApiException extends FeasibleException
{
    /**
     * Builds the message from the server's own words, falling back to the
     * status when the body is empty.
     */
    public function __construct(
        public readonly int $status,
        public readonly string $body,
        public readonly int $attempts = 1
    ) {
        $detail = trim($body);
        if ($detail === '') {
            $detail = 'the response body was empty';
        }

        parent::__construct(sprintf('the ingest endpoint answered %d: %s', $status, $detail));
    }
}
