<?php
//
// StreamTransport.php
// The dependency-free fallback for hosts without the cURL extension.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Transport;

use Feasible\Exception\TransportException;

/**
 * Sends through a stream context. It exists so the package has no runtime
 * dependencies at all and still works on shared hosting where cURL is not
 * compiled in — the alternative is telling that customer to install an
 * extension they have no control over.
 */
final class StreamTransport implements Transport
{
    /**
     * Performs the POST. `ignore_errors` is on because without it a 400 or a
     * 500 comes back as a false with a warning and no body, which would hide
     * the very sentence the endpoint wrote to explain itself.
     *
     * @param array<string, string> $headers
     */
    public function send(string $url, array $headers, string $body, float $timeout): Response
    {
        $lines = '';
        foreach ($headers as $name => $value) {
            $lines .= $name . ': ' . $value . "\r\n";
        }

        $context = stream_context_create([
            'http' => [
                'method' => 'POST',
                'header' => $lines,
                'content' => $body,
                'timeout' => $timeout,
                'ignore_errors' => true,
                'follow_location' => 0,
            ],
        ]);

        // The response headers arrive in a local variable the stream wrapper
        // defines, so it is initialised first and read straight afterwards.
        $http_response_header = [];

        $response = @file_get_contents($url, false, $context);

        if ($response === false) {
            $error = error_get_last();
            throw new TransportException(sprintf(
                'the request to %s failed: %s',
                $url,
                $error['message'] ?? 'no response'
            ));
        }

        [$status, $collected] = $this->parseHeaders($http_response_header);

        if ($status === 0) {
            throw new TransportException('no HTTP status came back from ' . $url);
        }

        return new Response($status, $collected, $response);
    }

    /**
     * Splits the raw header block into a status and a lower-cased map. Only the
     * last status line is kept: a proxy answering "100 Continue" first would
     * otherwise be reported as the result.
     *
     * @param array<int, string> $raw
     * @return array{0: int, 1: array<string, string>}
     */
    private function parseHeaders(array $raw): array
    {
        $status = 0;
        $collected = [];

        foreach ($raw as $line) {
            if (preg_match('#^HTTP/\d(?:\.\d)?\s+(\d{3})#i', $line, $matches) === 1) {
                $status = (int) $matches[1];
                $collected = [];
                continue;
            }

            $parts = explode(':', $line, 2);
            if (count($parts) === 2) {
                $collected[strtolower(trim($parts[0]))] = trim($parts[1]);
            }
        }

        return [$status, $collected];
    }
}
