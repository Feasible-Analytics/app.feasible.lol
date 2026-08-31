<?php
//
// RecordingTransport.php
// A transport that answers from a script and remembers every request.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Tests;

use Feasible\Exception\TransportException;
use Feasible\Transport\Response;
use Feasible\Transport\Transport;

/**
 * Stands in for the network. The tests assert on the exact bytes and headers
 * the SDK would send, which is both faster and stricter than a local server:
 * a real socket can only prove the request was accepted, not that the retry
 * rules stopped where they should have.
 */
final class RecordingTransport implements Transport
{
    /** @var list<array{url: string, headers: array<string, string>, body: string, timeout: float}> */
    public array $requests = [];

    /** @var list<Response|TransportException> Answers handed out in order. */
    private array $script;

    /** The answer given once the script runs out. */
    private Response $fallback;

    /**
     * Takes the answers up front so a test reads as a story: these are the
     * three things the server says, and this is what the SDK does about them.
     *
     * @param list<Response|TransportException> $script
     */
    public function __construct(array $script = [], ?Response $fallback = null)
    {
        $this->script = $script;
        $this->fallback = $fallback ?? new Response(202, [], '');
    }

    /**
     * Records the request, then answers with the next scripted response. A
     * scripted exception is thrown rather than returned so the retry loop sees
     * the same failure a dead socket produces.
     *
     * @param array<string, string> $headers
     */
    public function send(string $url, array $headers, string $body, float $timeout): Response
    {
        $this->requests[] = ['url' => $url, 'headers' => $headers, 'body' => $body, 'timeout' => $timeout];

        $next = array_shift($this->script);

        if ($next instanceof TransportException) {
            throw $next;
        }

        return $next ?? $this->fallback;
    }

    /**
     * How many requests actually left the SDK, which is the only way to prove
     * a retry happened or, more importantly, did not.
     */
    public function count(): int
    {
        return count($this->requests);
    }

    /**
     * The decoded body of one request, so a test asserts on the JSON keys
     * rather than on a string that whitespace could break.
     *
     * @return array<string, mixed>
     */
    public function payload(int $index = 0): array
    {
        $decoded = json_decode($this->requests[$index]['body'] ?? '', true);

        return is_array($decoded) ? $decoded : [];
    }

    /**
     * One request header, so a test can name the header it cares about instead
     * of indexing into the recorded array.
     */
    public function header(string $name, int $index = 0): ?string
    {
        return $this->requests[$index]['headers'][$name] ?? null;
    }
}
