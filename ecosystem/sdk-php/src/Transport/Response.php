<?php
//
// Response.php
// One HTTP answer, reduced to the three things the SDK reads.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Transport;

/**
 * The transport-neutral answer. Headers arrive with their names lower-cased so
 * that the cURL path and the stream path — which disagree about capitalisation
 * — cannot produce different behaviour for the same response.
 */
final class Response
{
    /**
     * @param array<string, string> $headers Lower-cased header names to values.
     */
    public function __construct(
        public readonly int $status,
        public readonly array $headers = [],
        public readonly string $body = '',
    ) {
    }

    /**
     * Reads one header without the caller having to know the capitalisation the
     * server chose, which varies between the endpoint and any proxy in front.
     */
    public function header(string $name): ?string
    {
        return $this->headers[strtolower($name)] ?? null;
    }
}
