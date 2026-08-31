<?php
//
// RecordedEvent.php
// One event a disabled client kept in memory instead of sending.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible;

/**
 * What a no-op client remembers. A test suite needs to assert that the checkout
 * reported the revenue it charged, and the only alternatives are a network call
 * from the test or a hand-written mock of this SDK — both of which stop
 * catching the mistake the moment the payload changes.
 */
final class RecordedEvent
{
    /**
     * Keeps the forwarded address and user agent beside the payload, because
     * forgetting them is the failure worth asserting against and they are not
     * part of the body.
     *
     * @param array<string, mixed> $payload The exact JSON body that would have been sent.
     */
    public function __construct(
        public readonly array $payload,
        public readonly string $clientIp,
        public readonly string $userAgent,
        public readonly bool $debug = false,
    ) {
    }

    /**
     * The event name, which is what a test asserts on first and is otherwise a
     * single-letter key lookup at every call site.
     */
    public function name(): string
    {
        $name = $this->payload['n'] ?? '';

        return is_string($name) ? $name : '';
    }

    /**
     * The custom properties, defaulting to an empty array so a test can index
     * into them without checking whether any were sent.
     *
     * @return array<string, mixed>
     */
    public function props(): array
    {
        $props = $this->payload['p'] ?? [];

        return is_array($props) ? $props : [];
    }
}
