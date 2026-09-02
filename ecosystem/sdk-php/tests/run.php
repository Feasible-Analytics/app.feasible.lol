<?php
//
// run.php
// The whole test suite: `php tests/run.php`, no dependencies, no network.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

declare(strict_types=1);

namespace Feasible\Tests;

use Feasible\Attribution;
use Feasible\Client;
use Feasible\Exception\BadRequestException;
use Feasible\Exception\MissingClientIpException;
use Feasible\Exception\MissingUserAgentException;
use Feasible\Exception\TransportException;
use Feasible\Revenue;
use Feasible\Transport\Response;
use Feasible\Visitor;
use Throwable;

// The package has no dependencies, so there is no vendor directory to autoload
// from. Mapping the two namespaces by hand keeps `php tests/run.php` working on
// a clean checkout with no install step and no network.
spl_autoload_register(static function (string $class): void {
    $roots = ['Feasible\\Tests\\' => __DIR__ . '/', 'Feasible\\' => __DIR__ . '/../src/'];

    foreach ($roots as $prefix => $root) {
        if (str_starts_with($class, $prefix)) {
            $path = $root . str_replace('\\', '/', substr($class, strlen($prefix))) . '.php';

            if (is_file($path)) {
                require_once $path;
            }

            return;
        }
    }
});

/** @var list<string> Failures collected so one broken case does not hide the rest. */
// The only shape the server accepts in the idempotency field.
const UUID_V4 = '/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/';

$failures = [];

/** @var int How many assertions ran, printed so a silent no-op cannot pass. */
$assertions = 0;

/**
 * Records one assertion. Collecting failures rather than dying on the first
 * one means a single run tells you everything that is broken, which is the
 * difference between one fix cycle and five.
 */
function check(string $what, bool $ok): void
{
    global $failures, $assertions;

    $assertions++;

    if (!$ok) {
        $failures[] = $what;
        echo "  FAIL  {$what}\n";

        return;
    }

    echo "  ok    {$what}\n";
}

/**
 * Compares two values structurally. Strict equality is used so that a string
 * "1" arriving where an integer 1 was expected is a failure rather than a
 * surprise on the wire.
 */
function same(string $what, mixed $expected, mixed $actual): void
{
    $ok = $expected === $actual;

    if (!$ok) {
        echo '        expected: ' . json_encode($expected) . "\n";
        echo '        actual:   ' . json_encode($actual) . "\n";
    }

    check($what, $ok);
}

/**
 * Asserts that a call throws a particular type. The exception is returned so a
 * test can also check that the message names the parameter, which is the part
 * that makes the error useful at three in the morning.
 */
function throws(string $what, string $expected, callable $callable): ?Throwable
{
    try {
        $callable();
    } catch (Throwable $error) {
        if ($error instanceof $expected) {
            check($what, true);

            return $error;
        }

        check($what . ' (got ' . $error::class . ': ' . $error->getMessage() . ')', false);

        return $error;
    }

    check($what . ' (nothing was thrown)', false);

    return null;
}

echo "feasible-php\n";

// A minimal pageview sends exactly the three required keys and nothing else,
// because an absent value must be omitted rather than sent as null.
$transport = new RecordingTransport();
$client = new Client(domain: 'example.com', transport: $transport, disabled: false);
$result = $client->pageview(
    url: 'https://example.com/pricing',
    clientIp: '203.0.113.9',
    userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)'
);

same('pageview sends exactly k, n, u, d', ['k', 'n', 'u', 'd'], array_keys($transport->payload()));
check('the idempotency key is a UUID v4', preg_match(UUID_V4, $transport->payload()['k']) === 1);
same('pageview name is "pageview"', 'pageview', $transport->payload()['n']);
same('pageview url is the full URL', 'https://example.com/pricing', $transport->payload()['u']);
same('pageview domain is the registered site', 'example.com', $transport->payload()['d']);
same('pageview posts to /api/event', 'https://app.feasible.lol/api/event', $transport->requests[0]['url']);
same('a 202 with no drop header is not dropped', false, $result->wasDropped());
same('the default timeout is five seconds', 5.0, $transport->requests[0]['timeout']);

// A custom event carries props, revenue and the attribution overrides, each
// under the single-letter key the ingest contract fixes.
$transport = new RecordingTransport();
$client = new Client(domain: 'example.com', transport: $transport, disabled: false);
$client->event(
    name: 'Purchase',
    url: 'https://example.com/checkout/complete',
    clientIp: '203.0.113.9',
    userAgent: 'curl/8.4.0',
    title: 'Order complete',
    referrer: 'https://news.example/story',
    props: ['plan' => 'pro', 'seats' => 4, 'trial' => false],
    revenue: new Revenue(amount: 49.5, currency: 'usd'),
    attribution: new Attribution(utmSource: 'newsletter', utmCampaign: 'august'),
    interactive: false,
    scrollDepth: 80,
    engagementTime: 12000,
    viewportWidth: 1440,
);

$payload = $transport->payload();
same(
    'a full event sends every documented key in wire order',
    ['k', 'n', 'u', 'd', 'r', 't', 'p', '$', 'i', 'sd', 'e', 'w', 'utm_source', 'utm_campaign'],
    array_keys($payload)
);
same('props travel under "p"', ['plan' => 'pro', 'seats' => 4, 'trial' => false], $payload['p']);
same('revenue travels under "$"', ['amount' => 49.5, 'currency' => 'USD'], $payload['$']);
same('a non-interaction event sends i=false', false, $payload['i']);
same('the referrer travels under "r"', 'https://news.example/story', $payload['r']);
same('the attribution override keeps its long name', 'newsletter', $payload['utm_source']);

// The content type is text/plain deliberately: it is what the browser tracker
// sends, and the endpoint reads the body as JSON regardless.
same('Content-Type is text/plain', 'text/plain', $transport->header('Content-Type'));

// The visitor's address and user agent are forwarded verbatim. Anything else
// attributes the event to the server rather than to the visitor.
same('X-Forwarded-For carries the caller\'s IP', '203.0.113.9', $transport->header('X-Forwarded-For'));
same('User-Agent carries the caller\'s agent', 'curl/8.4.0', $transport->header('User-Agent'));

// A call with no address or no user agent is refused before anything leaves
// the process, and the error names the parameter to fix.
$transport = new RecordingTransport();
$client = new Client(domain: 'example.com', transport: $transport, disabled: false);

$error = throws(
    'an empty clientIp throws MissingClientIpException',
    MissingClientIpException::class,
    static fn () => $client->pageview(url: 'https://example.com/', clientIp: '  ', userAgent: 'curl/8.4.0')
);
check('the missing-IP error names clientIp', $error !== null && str_contains($error->getMessage(), 'clientIp'));

$error = throws(
    'an empty userAgent throws MissingUserAgentException',
    MissingUserAgentException::class,
    static fn () => $client->pageview(url: 'https://example.com/', clientIp: '203.0.113.9', userAgent: '')
);
check('the missing-UA error names userAgent', $error !== null && str_contains($error->getMessage(), 'userAgent'));
same('a refused call sends nothing', 0, $transport->count());

// Visitor::fromRequest resolves the address the way the ingest server does.
$visitor = Visitor::fromRequest([
    'HTTP_X_FORWARDED_FOR' => '198.51.100.23, 10.0.0.7, 10.0.0.8',
    'REMOTE_ADDR' => '10.0.0.8',
    'HTTP_USER_AGENT' => 'Mozilla/5.0',
]);
same('fromRequest takes the FIRST X-Forwarded-For entry', '198.51.100.23', $visitor->clientIp);
same('fromRequest reads the User-Agent', 'Mozilla/5.0', $visitor->userAgent);

$visitor = Visitor::fromRequest([
    'HTTP_CF_CONNECTING_IP' => '198.51.100.5',
    'HTTP_X_FORWARDED_FOR' => '192.0.2.5',
    'REMOTE_ADDR' => '10.0.0.8',
    'HTTP_USER_AGENT' => 'Mozilla/5.0',
]);
same('fromRequest prefers CF-Connecting-IP', '198.51.100.5', $visitor->clientIp);

$visitor = Visitor::fromRequest(['REMOTE_ADDR' => '203.0.113.77', 'HTTP_USER_AGENT' => 'Mozilla/5.0']);
same('fromRequest falls back to the socket address', '203.0.113.77', $visitor->clientIp);

// The visitor spreads into a call as named arguments, so neither value can be
// dropped or transposed at the call site.
$transport = new RecordingTransport();
$client = new Client(domain: 'example.com', transport: $transport, disabled: false);
$client->pageview(...$visitor->args(), url: 'https://example.com/');
same('a spread visitor forwards its IP', '203.0.113.77', $transport->header('X-Forwarded-For'));

// A visitor built from stored data rather than a parsed request can carry a
// line break, and the transports write these values as raw header lines.
$transport = new RecordingTransport();
$client = new Client(domain: 'example.com', transport: $transport, disabled: false);
$client->pageview(
    url: 'https://example.com/',
    clientIp: "203.0.113.9\r\nX-Injected: yes",
    userAgent: "Mozilla/5.0\nX-Injected: yes\0"
);
same('a line break in the IP cannot start a new header', '203.0.113.9X-Injected: yes', $transport->header('X-Forwarded-For'));
same('a line break in the agent cannot start a new header', 'Mozilla/5.0X-Injected: yes', $transport->header('User-Agent'));
check('no injected header reached the transport', $transport->header('X-Injected') === null);

// No-op mode: nothing is sent, the call succeeds, and the event is kept for a
// test suite to assert on.
$transport = new RecordingTransport();
$client = new Client(domain: 'example.com', transport: $transport, disabled: true);
$result = $client->event(
    name: 'Signup',
    url: 'https://example.com/welcome',
    clientIp: '203.0.113.9',
    userAgent: 'curl/8.4.0',
    props: ['plan' => 'free']
);

same('no-op mode sends nothing', 0, $transport->count());
same('no-op mode reports success', false, $result->sent);
same('no-op mode makes no attempt', 0, $result->attempts);
same('no-op mode records the event', 1, count($client->recordedEvents()));
same('the recorded event keeps its name', 'Signup', $client->recordedEvents()[0]->name());
same('the recorded event keeps its props', ['plan' => 'free'], $client->recordedEvents()[0]->props());
same('the recorded event keeps the visitor IP', '203.0.113.9', $client->recordedEvents()[0]->clientIp);
$client->clearRecordedEvents();
same('the recording can be cleared between tests', 0, count($client->recordedEvents()));

// A disabled client still refuses a call with no visitor, so the mistake is
// caught in the test suite rather than in production.
throws(
    'no-op mode still refuses a call with no IP',
    MissingClientIpException::class,
    static fn () => $client->pageview(url: 'https://example.com/', clientIp: '', userAgent: 'curl/8.4.0')
);

// A 500 is worth retrying: the same bytes may well succeed a moment later.
$transport = new RecordingTransport([
    new Response(500, [], 'upstream is unhappy'),
    new TransportException('connection reset'),
    new Response(202, [], ''),
]);
$client = new Client(domain: 'example.com', transport: $transport, disabled: false, backoffBase: 0.0);
$result = $client->pageview(url: 'https://example.com/', clientIp: '203.0.113.9', userAgent: 'curl/8.4.0');

same('a 500 and a dropped connection are both retried', 3, $transport->count());
same('the successful attempt is reported', 3, $result->attempts);
same('the retried event ends up accepted', 202, $result->status);

// The server dedupes on "k", so a retry after a lost acknowledgement must
// resend the same key — and the next event must get a fresh one.
$transport = new RecordingTransport([new Response(500, [], 'upstream is unhappy')]);
$client = new Client(domain: 'example.com', transport: $transport, disabled: false, backoffBase: 0.0);
$client->pageview(url: 'https://example.com/', clientIp: '203.0.113.9', userAgent: 'curl/8.4.0');
$client->pageview(url: 'https://example.com/', clientIp: '203.0.113.9', userAgent: 'curl/8.4.0');
[$first, $retried, $fresh] = array_map(static fn (int $i): string => $transport->payload($i)['k'], [0, 1, 2]);
same('a retry resends the same idempotency key', $first, $retried);
check('the next event gets a fresh idempotency key', $first !== $fresh);

// Retries stop at maxAttempts rather than looping until the endpoint recovers.
$transport = new RecordingTransport([], new Response(503, [], 'still unhappy'));
$client = new Client(domain: 'example.com', transport: $transport, disabled: false, backoffBase: 0.0, maxAttempts: 2);
throws(
    'an endpoint that never recovers gives up at maxAttempts',
    \Feasible\Exception\ApiException::class,
    static fn () => $client->pageview(url: 'https://example.com/', clientIp: '203.0.113.9', userAgent: 'curl/8.4.0')
);
same('maxAttempts is honoured exactly', 2, $transport->count());

// A 400 is the caller's bug. Retrying sends the same bytes and gets the same
// answer, so it is thrown immediately.
$transport = new RecordingTransport([
    new Response(400, [], 'this request arrived from a datacentre address with no X-Forwarded-For'),
]);
$client = new Client(domain: 'example.com', transport: $transport, disabled: false, backoffBase: 0.0);
$error = throws(
    'a 400 throws BadRequestException',
    BadRequestException::class,
    static fn () => $client->pageview(url: 'https://example.com/', clientIp: '203.0.113.9', userAgent: 'curl/8.4.0')
);
same('a 400 is never retried', 1, $transport->count());
check(
    'a 400 carries the server\'s own explanation',
    $error !== null && str_contains($error->getMessage(), 'datacentre address')
);

// A 202 with a drop reason is a classification, not a failure. It is returned
// with the reason attached and never retried.
$transport = new RecordingTransport([
    new Response(202, ['x-feasible-dropped' => 'datacenter_ip'], ''),
]);
$client = new Client(domain: 'example.com', transport: $transport, disabled: false, backoffBase: 0.0);
$result = $client->pageview(url: 'https://example.com/', clientIp: '203.0.113.9', userAgent: 'curl/8.4.0');

same('a dropped 202 is not retried', 1, $transport->count());
same('the drop reason is surfaced, never swallowed', 'datacenter_ip', $result->dropped);
same('a dropped result says so', true, $result->wasDropped());

// The debug escape hatch returns what the server derived instead of writing.
$transport = new RecordingTransport([
    new Response(200, ['content-type' => 'application/json'], '{"site_id":7,"country":"US","bot_reason":""}'),
]);
$client = new Client(domain: 'example.com', transport: $transport, disabled: false);
$derived = $client->debug(
    name: 'pageview',
    url: 'https://example.com/',
    clientIp: '203.0.113.9',
    userAgent: 'curl/8.4.0'
);

same('debug asks for the derived event', 'true', $transport->header('X-Debug-Request'));
same('debug returns the derived event as an array', 'US', $derived['country']);

// A self-hosted install posts to its own host, with any trailing slash removed
// so the path does not double up.
$transport = new RecordingTransport();
$client = new Client(domain: 'example.com', host: 'https://stats.example.org/', transport: $transport, disabled: false);
$client->pageview(url: 'https://example.com/', clientIp: '203.0.113.9', userAgent: 'curl/8.4.0');
same('a custom host is honoured', 'https://stats.example.org/api/event', $transport->requests[0]['url']);

// The environment switch is what a CI container sets, and it is what makes the
// no-op mode usable without touching application code.
putenv('FEASIBLE_DISABLED=1');
$client = new Client(domain: 'example.com', transport: new RecordingTransport());
same('FEASIBLE_DISABLED=1 disables the client', true, $client->isDisabled());
putenv('FEASIBLE_DISABLED');

echo "\n";

if ($failures !== []) {
    echo count($failures) . ' of ' . $assertions . " assertions failed\n";
    exit(1);
}

echo $assertions . " assertions passed\n";
exit(0);
