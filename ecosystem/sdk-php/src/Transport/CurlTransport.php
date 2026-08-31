<?php
//
// CurlTransport.php
// The cURL transport, used whenever the extension is present.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Transport;

use Feasible\Exception\TransportException;

/**
 * Sends over cURL. It is preferred where available because it gives a real
 * connect timeout and a real error string, both of which the stream wrapper
 * only approximates — and a tracking call that hangs is worse than one that
 * fails, since it holds up the request the visitor is waiting on.
 */
final class CurlTransport implements Transport
{
    /**
     * Reports whether this transport can run here. Shared hosting with cURL
     * disabled is common enough that the SDK checks rather than assumes.
     */
    public static function isAvailable(): bool
    {
        return function_exists('curl_init');
    }

    /**
     * Performs the POST. Response headers are collected through a callback
     * rather than parsed out of the body, because a redirect or a proxy can
     * produce more than one header block and only the last one is the answer.
     *
     * @param array<string, string> $headers
     */
    public function send(string $url, array $headers, string $body, float $timeout): Response
    {
        $handle = curl_init($url);
        if ($handle === false) {
            throw new TransportException('could not initialise a cURL handle for ' . $url);
        }

        $collected = [];

        $lines = [];
        foreach ($headers as $name => $value) {
            $lines[] = $name . ': ' . $value;
        }

        $milliseconds = max(1, (int) round($timeout * 1000));

        curl_setopt_array($handle, [
            CURLOPT_POST => true,
            CURLOPT_POSTFIELDS => $body,
            CURLOPT_HTTPHEADER => $lines,
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_TIMEOUT_MS => $milliseconds,
            CURLOPT_CONNECTTIMEOUT_MS => $milliseconds,
            CURLOPT_FOLLOWLOCATION => false,
            CURLOPT_HEADERFUNCTION => static function ($_handle, string $line) use (&$collected): int {
                $length = strlen($line);
                $parts = explode(':', $line, 2);

                if (count($parts) === 2) {
                    $collected[strtolower(trim($parts[0]))] = trim($parts[1]);
                }

                return $length;
            },
        ]);

        $response = curl_exec($handle);
        $error = curl_error($handle);
        $status = (int) curl_getinfo($handle, CURLINFO_RESPONSE_CODE);

        // The handle is freed when it goes out of scope. Calling curl_close on
        // it does nothing on any supported version and is deprecated on the
        // newest ones, which would put a warning in the caller's error log.
        unset($handle);

        if ($response === false || $status === 0) {
            throw new TransportException(
                sprintf('the request to %s failed: %s', $url, $error !== '' ? $error : 'no response')
            );
        }

        return new Response($status, $collected, is_string($response) ? $response : '');
    }
}
