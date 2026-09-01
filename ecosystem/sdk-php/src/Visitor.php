<?php
//
// Visitor.php
// The visitor's address and user agent, resolved the same way the server does.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible;

use Feasible\Exception\MissingClientIpException;
use Feasible\Exception\MissingUserAgentException;

/**
 * The two things a server-side call must forward. They travel together in one
 * object because they are always needed together, and because the resolution
 * rules — which header wins, and which entry of it — are worth writing down
 * once rather than at every call site.
 */
final class Visitor
{
    /** The visitor's IP address, sent as X-Forwarded-For. */
    public readonly string $clientIp;

    /** The visitor's User-Agent, sent verbatim. */
    public readonly string $userAgent;

    /**
     * Refuses to hold a half-filled visitor. An empty address or user agent is
     * exactly the mistake this package exists to prevent, so it is rejected at
     * construction rather than at send time, where the stack trace would point
     * at the SDK instead of at the code that lost the value.
     */
    public function __construct(string $clientIp, string $userAgent)
    {
        $ip = trim($clientIp);
        $agent = trim($userAgent);

        if ($ip === '') {
            throw new MissingClientIpException('clientIp');
        }

        if ($agent === '') {
            throw new MissingUserAgentException('userAgent');
        }

        $this->clientIp = $ip;
        $this->userAgent = $agent;
    }

    /**
     * Reads the visitor off the incoming request. The array is injectable so
     * this is testable and usable from a framework that keeps its own copy of
     * the server variables rather than touching the superglobal.
     *
     * @param array<string, mixed>|null $server Defaults to $_SERVER.
     */
    public static function fromRequest(?array $server = null): self
    {
        $server ??= $_SERVER;

        return new self(self::resolveClientIp($server), self::header($server, 'HTTP_USER_AGENT'));
    }

    /**
     * Resolves the address from forwarding headers and then the socket. This
     * assumes the application edge stripped client-supplied forwarding
     * headers; unlike the ingest service, this helper has no proxy allow-list.
     *
     * The first entry is the one that matters. X-Forwarded-For is appended to
     * by each hop, so the last entry is the nearest proxy — taking it, as
     * several frameworks do, reports your own load balancer as the visitor and
     * collapses every visit in the world into one.
     *
     * @param array<string, mixed> $server
     */
    public static function resolveClientIp(array $server): string
    {
        $cloudflare = self::header($server, 'HTTP_CF_CONNECTING_IP');
        if ($cloudflare !== '') {
            return $cloudflare;
        }

        $forwarded = self::header($server, 'HTTP_X_FORWARDED_FOR');
        if ($forwarded !== '') {
            $first = trim(explode(',', $forwarded)[0]);
            if ($first !== '') {
                return $first;
            }
        }

        return self::header($server, 'REMOTE_ADDR');
    }

    /**
     * Reads one server variable as a trimmed string. Anything that is not a
     * string — a framework that stored an array of values, say — is treated as
     * absent, because guessing which entry was meant is how the wrong address
     * ends up on every event.
     *
     * @param array<string, mixed> $server
     */
    private static function header(array $server, string $key): string
    {
        $value = $server[$key] ?? '';

        return is_string($value) ? trim($value) : '';
    }

    /**
     * Returns the pair as named arguments, so a call reads
     * `$client->pageview(...$visitor->args(), url: $url)` and neither value can
     * be forgotten or transposed.
     *
     * @return array{clientIp: string, userAgent: string}
     */
    public function args(): array
    {
        return ['clientIp' => $this->clientIp, 'userAgent' => $this->userAgent];
    }
}
