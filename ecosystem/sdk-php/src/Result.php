<?php
//
// Result.php
// What one accepted event came back as, including why it was dropped.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible;

/**
 * The answer to a send. The drop reason is a field rather than something the
 * SDK swallows because the endpoint answers 202 even for events it decided not
 * to count: without this, a filter that silently discards half a customer's
 * traffic looks exactly like success.
 */
final class Result
{
    /**
     * Holds the whole answer, including the raw body, so that a caller logging
     * a surprise has everything the server said and does not have to reproduce
     * the request to find out.
     */
    public function __construct(
        public readonly int $status,
        public readonly ?string $dropped = null,
        public readonly int $attempts = 1,
        public readonly bool $sent = true,
        public readonly string $body = '',
    ) {
    }

    /**
     * Reports whether the event was accepted but classified. It is not a
     * failure and must not be retried — retrying reaches the same classifier
     * and gets the same answer.
     */
    public function wasDropped(): bool
    {
        return $this->dropped !== null && $this->dropped !== '';
    }
}
