<?php
//
// test-client-ip.php
// The visitor-IP precedence, which is the one thing this proxy cannot get wrong quietly.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

require_once __DIR__ . '/bootstrap.php';

feasible_assert_same(
	'198.51.100.7',
	Feasible_Client_IP::resolve(
		array(
			'HTTP_CF_CONNECTING_IP' => '198.51.100.7',
			'HTTP_X_FORWARDED_FOR'  => '203.0.113.5, 70.41.3.18',
			'REMOTE_ADDR'           => '10.0.0.1',
		)
	),
	'CF-Connecting-IP outranks the forwarding chain and the socket peer'
);

$chain = Feasible_Client_IP::resolve(
	array(
		'HTTP_X_FORWARDED_FOR' => '203.0.113.5, 70.41.3.18, 150.172.238.178',
		'REMOTE_ADDR'          => '10.0.0.1',
	)
);

feasible_assert_same(
	'203.0.113.5',
	$chain,
	'the FIRST X-Forwarded-For entry is the visitor'
);

feasible_assert(
	'150.172.238.178' !== $chain,
	'the LAST X-Forwarded-For entry is the nearest proxy and must never be reported as the visitor'
);

feasible_assert_same(
	'10.0.0.1',
	Feasible_Client_IP::resolve( array( 'REMOTE_ADDR' => '10.0.0.1' ) ),
	'with no forwarding headers the socket peer is the visitor'
);

feasible_assert_same(
	'203.0.113.5',
	Feasible_Client_IP::resolve(
		array(
			'HTTP_CF_CONNECTING_IP' => 'not-an-address',
			'HTTP_X_FORWARDED_FOR'  => '203.0.113.5',
			'REMOTE_ADDR'           => '10.0.0.1',
		)
	),
	'an unparseable Cloudflare header falls through instead of poisoning the answer'
);

feasible_assert_same(
	'10.0.0.1',
	Feasible_Client_IP::resolve(
		array(
			'HTTP_X_FORWARDED_FOR' => 'unknown, garbage',
			'REMOTE_ADDR'          => '10.0.0.1',
		)
	),
	'a chain whose first entry is not an address falls through to the socket peer'
);

feasible_assert_same(
	'203.0.113.5',
	Feasible_Client_IP::resolve( array( 'HTTP_X_FORWARDED_FOR' => '203.0.113.5:41234' ) ),
	'a proxy that appended the source port does not break the address'
);

feasible_assert_same(
	'2001:db8::1',
	Feasible_Client_IP::resolve( array( 'HTTP_X_FORWARDED_FOR' => '[2001:db8::1]:41234' ) ),
	'a bracketed IPv6 literal with a port is unwrapped'
);

feasible_assert_same(
	'2001:db8::1',
	Feasible_Client_IP::resolve( array( 'REMOTE_ADDR' => '2001:db8::1' ) ),
	'a bare IPv6 literal is not cut at one of its own colons'
);

feasible_assert_same(
	'fe80::1',
	Feasible_Client_IP::normalise_address( 'fe80::1%eth0' ),
	'a zone identifier is dropped rather than failing validation'
);

feasible_assert_same(
	'',
	Feasible_Client_IP::resolve( array() ),
	'a request with nothing to go on reports nothing rather than guessing'
);

feasible_assert_same(
	'',
	Feasible_Client_IP::resolve( array( 'REMOTE_ADDR' => '999.999.999.999' ) ),
	'an address that is not an address is refused'
);

feasible_assert_same(
	'Mozilla/5.0 (X11; Linux x86_64)',
	Feasible_Client_IP::user_agent( array( 'HTTP_USER_AGENT' => "Mozilla/5.0 (X11; Linux x86_64)\r\nX-Injected: yes" ) ),
	'a user agent carrying line breaks cannot inject a header of its own'
);

feasible_assert_same(
	512,
	strlen( Feasible_Client_IP::user_agent( array( 'HTTP_USER_AGENT' => str_repeat( 'u', 900 ) ) ) ),
	'an absurdly long user agent is capped rather than rejected upstream'
);

feasible_assert_same(
	'',
	Feasible_Client_IP::user_agent( array() ),
	'a missing user agent is an empty string, not a fabricated one'
);
