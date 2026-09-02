<?php
//
// Client.php
// The server-side ingest client: one event, the visitor's address, and the visitor's user agent.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible;

use Feasible\Exception\ApiException;
use Feasible\Exception\BadRequestException;
use Feasible\Exception\InvalidEventException;
use Feasible\Exception\MissingClientIpException;
use Feasible\Exception\MissingUserAgentException;
use Feasible\Exception\TransportException;
use Feasible\Transport\CurlTransport;
use Feasible\Transport\StreamTransport;
use Feasible\Transport\Transport;

/**
 * Sends events to `POST /api/event` from your server.
 *
 * Read this before your first call. A server-side event carries two things the
 * browser would have carried by itself, and both are required arguments here:
 *
 *   - `$clientIp` — the visitor's real IP, forwarded as `X-Forwarded-For`.
 *   - `$userAgent` — the visitor's real `User-Agent`, forwarded verbatim.
 *
 * A request that arrives from a datacentre address with neither is classified
 * as a bot and dropped, and nothing about the response says so loudly enough to
 * notice. Passing your own server's address is worse than passing nothing: it
 * looks like data. `Feasible\Visitor::fromRequest()` reads both off the
 * incoming request, resolving the address the same way the ingest server does.
 *
 * In a test environment construct with `disabled: true`, or set
 * `FEASIBLE_DISABLED=1`: nothing is sent, calls succeed, and every event is
 * kept in memory for `recordedEvents()` to assert on.
 */
final class Client
{
    /** The hosted endpoint. Self-hosted installs pass their own host. */
    public const DEFAULT_HOST = 'https://app.feasible.lol';

    /** The response header carrying the reason an accepted event was not counted. */
    public const HEADER_DROPPED = 'x-feasible-dropped';

    /** The path is part of the wire contract and is not configurable. */
    private const EVENT_PATH = '/api/event';

    /** The fully qualified URL every event is posted to. */
    private readonly string $endpoint;

    /** How the request leaves the process. */
    private readonly Transport $transport;

    /** Whether this client sends anything at all. */
    private readonly bool $disabled;

    /** @var list<RecordedEvent> Events a disabled client kept instead of sending. */
    private array $recorded = [];

    /**
     * Builds a client for one site.
     *
     * `$domain` is the site as registered — the value the tracking script would
     * put in `data-domain` — and not the URL of the page.
     *
     * The disabled flag is nullable rather than false by default so that an
     * explicit `disabled: false` in application code still wins over
     * `FEASIBLE_DISABLED=1` in the environment; leaving it unset defers to the
     * environment, which is what a shared test container wants.
     */
    public function __construct(
        public readonly string $domain,
        string $host = self::DEFAULT_HOST,
        public readonly float $timeout = 5.0,
        public readonly int $maxAttempts = 3,
        public readonly float $backoffBase = 0.25,
        public readonly float $backoffCap = 5.0,
        ?bool $disabled = null,
        ?Transport $transport = null,
    ) {
        if (trim($domain) === '') {
            throw new InvalidEventException('domain is required: it is the site as registered, such as "example.com"');
        }

        if ($maxAttempts < 1) {
            throw new InvalidEventException('maxAttempts must be at least 1');
        }

        $this->endpoint = rtrim(trim($host), '/') . self::EVENT_PATH;
        $this->disabled = $disabled ?? self::disabledByEnvironment();

        // cURL is preferred for its real connect timeout, but the SDK has no
        // runtime dependencies and has to work where the extension is absent.
        $this->transport = $transport ?? (CurlTransport::isAvailable() ? new CurlTransport() : new StreamTransport());
    }

    /**
     * Records a pageview. This is the event every report is built from, so it
     * gets its own method rather than leaving callers to remember that the
     * name is the literal string "pageview".
     *
     * @param array<string, mixed> $props
     */
    public function pageview(
        string $url,
        string $clientIp,
        string $userAgent,
        ?string $title = null,
        ?string $referrer = null,
        array $props = [],
        ?Revenue $revenue = null,
        ?Attribution $attribution = null,
        ?bool $interactive = null,
        ?int $scrollDepth = null,
        ?int $engagementTime = null,
        ?int $viewportWidth = null,
    ): Result {
        return $this->event(
            name: 'pageview',
            url: $url,
            clientIp: $clientIp,
            userAgent: $userAgent,
            title: $title,
            referrer: $referrer,
            props: $props,
            revenue: $revenue,
            attribution: $attribution,
            interactive: $interactive,
            scrollDepth: $scrollDepth,
            engagementTime: $engagementTime,
            viewportWidth: $viewportWidth,
        );
    }

    /**
     * Records a custom event — a signup, a purchase, a plan change.
     *
     * `$url` is required even for an event with no page of its own, because
     * every report groups by page and an event without one cannot be found
     * again. For an offline conversion, pass the URL it belongs to and set the
     * campaign through `$attribution` so it is not filed as Direct forever.
     *
     * @param array<string, mixed> $props
     */
    public function event(
        string $name,
        string $url,
        string $clientIp,
        string $userAgent,
        ?string $title = null,
        ?string $referrer = null,
        array $props = [],
        ?Revenue $revenue = null,
        ?Attribution $attribution = null,
        ?bool $interactive = null,
        ?int $scrollDepth = null,
        ?int $engagementTime = null,
        ?int $viewportWidth = null,
    ): Result {
        $payload = $this->buildPayload(
            $name,
            $url,
            $title,
            $referrer,
            $props,
            $revenue,
            $attribution,
            $interactive,
            $scrollDepth,
            $engagementTime,
            $viewportWidth
        );

        return $this->dispatch($payload, $clientIp, $userAgent, false);
    }

    /**
     * Asks the server what it would derive from this event and returns that
     * instead of writing anything. It answers "why is this visit attributed to
     * the wrong country" in one call, against production, with no side effects.
     *
     * @param array<string, mixed> $props
     * @return array<string, mixed> The server's derived event.
     */
    public function debug(
        string $name,
        string $url,
        string $clientIp,
        string $userAgent,
        ?string $title = null,
        ?string $referrer = null,
        array $props = [],
        ?Revenue $revenue = null,
        ?Attribution $attribution = null,
        ?bool $interactive = null,
        ?int $scrollDepth = null,
        ?int $engagementTime = null,
        ?int $viewportWidth = null,
    ): array {
        $payload = $this->buildPayload(
            $name,
            $url,
            $title,
            $referrer,
            $props,
            $revenue,
            $attribution,
            $interactive,
            $scrollDepth,
            $engagementTime,
            $viewportWidth
        );

        $result = $this->dispatch($payload, $clientIp, $userAgent, true);

        if (!$result->sent) {
            return [];
        }

        $decoded = json_decode($result->body, true);
        if (!is_array($decoded)) {
            throw new ApiException($result->status, $result->body, $result->attempts);
        }

        return $decoded;
    }

    /**
     * Reports whether this client is in no-op mode, so an application can log
     * once at boot rather than wonder later why a staging dashboard is empty.
     */
    public function isDisabled(): bool
    {
        return $this->disabled;
    }

    /**
     * The events a disabled client kept. This is the supported way to test
     * analytics: assert on what your code reported without a network, a mock
     * of this SDK, or a test double that stops matching the payload.
     *
     * @return list<RecordedEvent>
     */
    public function recordedEvents(): array
    {
        return $this->recorded;
    }

    /**
     * Empties the recording, so one test case cannot see another's events.
     */
    public function clearRecordedEvents(): void
    {
        $this->recorded = [];
    }

    /**
     * Assembles the wire payload. Absent values are omitted rather than sent as
     * null: the endpoint reads a null as a value and would overwrite what it
     * derived with nothing.
     *
     * @param array<string, mixed> $props
     * @return array<string, mixed>
     */
    private function buildPayload(
        string $name,
        string $url,
        ?string $title,
        ?string $referrer,
        array $props,
        ?Revenue $revenue,
        ?Attribution $attribution,
        ?bool $interactive,
        ?int $scrollDepth,
        ?int $engagementTime,
        ?int $viewportWidth,
    ): array {
        $name = trim($name);
        $url = trim($url);

        if ($name === '') {
            throw new InvalidEventException('name is required: "pageview" for a pageview, or your own event name');
        }

        if ($url === '') {
            throw new InvalidEventException('url is required: the full URL of the page the event happened on');
        }

        // The idempotency key is minted here, once per event, so every retry
        // resends the same one and the server drops the duplicate instead of
        // counting it twice.
        $payload = ['k' => self::newKey(), 'n' => $name, 'u' => $url, 'd' => $this->domain];

        if ($referrer !== null && trim($referrer) !== '') {
            $payload['r'] = $referrer;
        }

        if ($title !== null && trim($title) !== '') {
            $payload['t'] = $title;
        }

        if ($props !== []) {
            $payload['p'] = $props;
        }

        if ($revenue !== null) {
            $payload['$'] = $revenue->toArray();
        }

        // Interactive is only sent when the caller said so, because the server
        // defaults an absent flag to true and that is what an ordinary event is.
        if ($interactive !== null) {
            $payload['i'] = $interactive;
        }

        if ($scrollDepth !== null) {
            $payload['sd'] = $scrollDepth;
        }

        if ($engagementTime !== null) {
            $payload['e'] = $engagementTime;
        }

        if ($viewportWidth !== null) {
            $payload['w'] = $viewportWidth;
        }

        if ($attribution !== null) {
            foreach ($attribution->toArray() as $key => $value) {
                $payload[$key] = $value;
            }
        }

        return $payload;
    }

    /**
     * Mints the idempotency key an event carries on every attempt. It is a
     * random UUID v4 because that is the only shape the server accepts in
     * this field, and it is built by hand so the package keeps its zero
     * dependencies.
     */
    private static function newKey(): string
    {
        $raw = random_bytes(16);
        $raw[6] = chr((ord($raw[6]) & 0x0f) | 0x40);
        $raw[8] = chr((ord($raw[8]) & 0x3f) | 0x80);

        $hex = bin2hex($raw);

        return substr($hex, 0, 8) . '-' . substr($hex, 8, 4) . '-' . substr($hex, 12, 4) . '-'
            . substr($hex, 16, 4) . '-' . substr($hex, 20, 12);
    }

    /**
     * Validates the visitor, then either records or sends. The validation runs
     * in no-op mode too, on purpose: a test suite that never exercises the
     * check would let a call with no address ship to production unnoticed,
     * which is the exact failure this package exists to prevent.
     *
     * @param array<string, mixed> $payload
     */
    private function dispatch(array $payload, string $clientIp, string $userAgent, bool $debug): Result
    {
        // Both values become raw header lines in the transports, and PHP's
        // stream wrapper does not refuse a line break in one. Stripping here
        // keeps a visitor built from stored data from injecting a header.
        $ip = trim(str_replace(["\r", "\n", "\0"], '', $clientIp));
        $agent = trim(str_replace(["\r", "\n", "\0"], '', $userAgent));

        if ($ip === '') {
            throw new MissingClientIpException('clientIp');
        }

        if ($agent === '') {
            throw new MissingUserAgentException('userAgent');
        }

        if ($this->disabled) {
            $this->recorded[] = new RecordedEvent($payload, $ip, $agent, $debug);

            return new Result(status: 0, dropped: null, attempts: 0, sent: false, body: '');
        }

        $headers = [
            // text/plain is deliberate. It is what the browser tracker sends,
            // it avoids a CORS preflight, and the endpoint reads the body as
            // JSON regardless of the declared type.
            'Content-Type' => 'text/plain',
            'X-Forwarded-For' => $ip,
            'User-Agent' => $agent,
        ];

        if ($debug) {
            $headers['X-Debug-Request'] = 'true';
        }

        // PRESERVE_ZERO_FRACTION keeps a revenue amount of 10.0 from becoming
        // the integer 10, which changes what a currency looks like in a log.
        $body = json_encode($payload, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_PRESERVE_ZERO_FRACTION);

        if ($body === false) {
            throw new InvalidEventException('the event could not be encoded as JSON: ' . json_last_error_msg());
        }

        return $this->post($headers, $body);
    }

    /**
     * Sends with retries. What is retried is deliberately narrow: a transport
     * failure, a 429 and a 5xx are all conditions that a second attempt can
     * genuinely fix, and nothing else is. A 400 is the caller's bug and would
     * fail identically; a 202 carrying a drop reason is a classification, not a
     * failure, and retrying it reaches the same classifier.
     *
     * @param array<string, string> $headers
     */
    private function post(array $headers, string $body): Result
    {
        $attempt = 0;

        while (true) {
            $attempt++;

            try {
                $response = $this->transport->send($this->endpoint, $headers, $body, $this->timeout);
            } catch (TransportException $error) {
                if ($attempt >= $this->maxAttempts) {
                    throw new TransportException($error->getMessage(), $attempt);
                }

                $this->pause($attempt);
                continue;
            }

            $status = $response->status;

            if ($status === 400) {
                throw new BadRequestException($status, $response->body, $attempt);
            }

            if ($status === 429 || $status >= 500) {
                if ($attempt >= $this->maxAttempts) {
                    throw new ApiException($status, $response->body, $attempt);
                }

                $this->pause($attempt);
                continue;
            }

            if ($status < 200 || $status >= 300) {
                throw new ApiException($status, $response->body, $attempt);
            }

            return new Result(
                status: $status,
                dropped: $response->header(self::HEADER_DROPPED),
                attempts: $attempt,
                sent: true,
                body: $response->body,
            );
        }
    }

    /**
     * Waits before the next attempt. The delay is exponential so a struggling
     * endpoint is not hammered, capped so a background job does not sleep for
     * minutes, and jittered so a fleet of servers that all failed at the same
     * moment does not retry in lockstep and repeat the outage.
     */
    private function pause(int $attempt): void
    {
        $delay = min($this->backoffCap, $this->backoffBase * (2 ** ($attempt - 1)));

        if ($delay <= 0) {
            return;
        }

        $jittered = ($delay / 2) + (mt_rand() / mt_getrandmax()) * ($delay / 2);

        usleep((int) round($jittered * 1_000_000));
    }

    /**
     * Reads the environment switch. A test container, a CI job or a local
     * development machine sets one variable and the whole application stops
     * writing to the customer's real numbers.
     */
    private static function disabledByEnvironment(): bool
    {
        $value = getenv('FEASIBLE_DISABLED');

        if (!is_string($value)) {
            return false;
        }

        return in_array(strtolower(trim($value)), ['1', 'true', 'yes', 'on'], true);
    }
}
