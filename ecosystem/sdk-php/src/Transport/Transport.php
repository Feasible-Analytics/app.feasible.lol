<?php
//
// Transport.php
// The seam between the SDK and the network.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Transport;

/**
 * How a request leaves the process. It is an interface so that a test can
 * assert on the exact bytes and headers the SDK would send without a socket,
 * and so that an application with its own HTTP client, proxy or instrumentation
 * can hand one in rather than have this package open its own connections.
 */
interface Transport
{
    /**
     * Performs one request and returns what came back. Implementations throw
     * Feasible\Exception\TransportException when nothing came back at all,
     * which is the signal the retry loop reads.
     *
     * @param array<string, string> $headers
     */
    public function send(string $url, array $headers, string $body, float $timeout): Response;
}
