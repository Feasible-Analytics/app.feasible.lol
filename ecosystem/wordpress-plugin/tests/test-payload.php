<?php
//
// test-payload.php
// What the proxy keeps from an event body, and what it refuses to forward.
//
// Created: 2026-11-21
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

require_once __DIR__ . '/bootstrap.php';

$pageview = Feasible_Payload::clean(
	'{"n":"pageview","u":"https://example.com/blog/?utm_source=news","d":"example.com","r":"https://www.google.com/","t":"Hello","k":"0F8FAD5B-D9CB-469F-A165-70867728950E","v":7,"w":1280,"a":"os","p":{"author":"sam"}}',
	'example.com'
);

feasible_assert_same(
	array(
		'n' => 'pageview',
		'd' => 'example.com',
		'u' => 'https://example.com/blog/?utm_source=news',
		'r' => 'https://www.google.com/',
		't' => 'Hello',
		'k' => '0f8fad5b-d9cb-469f-a165-70867728950e',
		'v' => 7,
		'w' => 1280,
		'a' => 'os',
	),
	array_diff_key( (array) $pageview, array( 'p' => true ) ),
	'a tracker pageview keeps every field the endpoint reads'
);

feasible_assert_same(
	array( 'author' => 'sam' ),
	(array) $pageview['p'],
	'custom properties are kept'
);

feasible_assert_same(
	'{"n":"pageview","d":"example.com","u":"https://example.com/blog/?utm_source=news","r":"https://www.google.com/","t":"Hello","k":"0f8fad5b-d9cb-469f-a165-70867728950e","v":7,"w":1280,"a":"os","p":{"author":"sam"}}',
	Feasible_Payload::encode( $pageview ),
	'the clean event encodes back to the wire shape'
);

feasible_assert_same(
	null,
	Feasible_Payload::clean( 'not json at all', 'example.com' ),
	'a body that is not JSON is refused'
);

feasible_assert_same(
	null,
	Feasible_Payload::clean( '["pageview"]', 'example.com' ),
	'a JSON array is not an event'
);

feasible_assert_same(
	null,
	Feasible_Payload::clean( '{"u":"https://example.com/"}', 'example.com' ),
	'an event with no name is refused'
);

feasible_assert_same(
	null,
	Feasible_Payload::clean( '{"n":{"nested":true}}', 'example.com' ),
	'a name that is not a string is refused'
);

feasible_assert_same(
	null,
	Feasible_Payload::clean( '{"n":"<b></b>"}', 'example.com' ),
	'a name that is nothing but markup is refused'
);

$forged = Feasible_Payload::clean(
	'{"n":"pageview","d":"someone-else.example","referrer":"https://forged.example/","utm_source":"forged","evil":"x","u":"javascript:alert(1)"}',
	'example.com'
);

feasible_assert_same(
	array(
		'n' => 'pageview',
		'd' => 'example.com',
	),
	$forged,
	'unknown keys are dropped, the domain is always this site, and a script URL is not a URL'
);

$padded = Feasible_Payload::clean( '{"n":"  File Download  ","u":"https://example.com/a file.pdf"}', 'example.com' );

feasible_assert_same( 'File Download', $padded['n'], 'a padded name still matches the switch it belongs to' );
feasible_assert_same( 'https://example.com/a%20file.pdf', $padded['u'], 'a space in a URL is encoded, not dropped' );

$query = Feasible_Payload::clean( '{"n":"pageview","u":"https://example.com/search?q={term}"}', 'example.com' );

feasible_assert_same(
	'https://example.com/search?q=%7Bterm%7D',
	$query['u'],
	'braces a browser leaves bare in a query are encoded rather than removed'
);

$typed = Feasible_Payload::clean(
	'{"n":"engagement","sd":"80","e":1500.5,"i":"false","w":true,"k":"not-a-uuid"}',
	'example.com'
);

feasible_assert_same(
	array(
		'n' => 'engagement',
		'd' => 'example.com',
		'e' => 1500.5,
	),
	$typed,
	'numbers must be numbers, the interactive flag must be a boolean, and a key must be a UUID'
);

$engagement = Feasible_Payload::clean( '{"n":"engagement","sd":80,"e":1200,"i":false}', 'example.com' );

feasible_assert_same( false, $engagement['i'], 'an explicit false for interactive survives' );
feasible_assert_same( 80, $engagement['sd'], 'scroll depth survives as a number' );

$props = Feasible_Payload::props(
	array(
		'url'      => 'https://example.com/files/report%202026.pdf',
		'term'     => "blue <b>widget</b>\x07",
		'percent'  => '50% off',
		'count'    => 3,
		'ratio'    => 0.5,
		'flag'     => true,
		'nested'   => array( 'a' => 1 ),
		'empty'    => null,
		'<script>' => 'gone',
	)
);

feasible_assert_same(
	array(
		'url'     => 'https://example.com/files/report%202026.pdf',
		'term'    => 'blue widget',
		'percent' => '50% off',
		'count'   => 3,
		'ratio'   => 0.5,
		'flag'    => true,
		'nested'  => null,
		'empty'   => null,
	),
	$props,
	'property values keep percent signs, lose markup and control characters, and an unsupported value is sent as null for the endpoint to count'
);

feasible_assert_same(
	array( 'plan' => 'pro' ),
	Feasible_Payload::props( '{"plan":"pro"}' ),
	'properties sent as a string holding JSON are read too'
);

$numbered = Feasible_Payload::clean( '{"n":"Signup","p":{"0":"first"}}', 'example.com' );

feasible_assert_same(
	'{"n":"Signup","d":"example.com","p":{"0":"first"}}',
	Feasible_Payload::encode( $numbered ),
	'a property named like a number still encodes as an object'
);

$many = array();

for ( $i = 0; $i < 80; $i++ ) {
	$many[ 'prop' . $i ] = 'value';
}

feasible_assert_same(
	Feasible_Payload::MAX_PROPS,
	count( Feasible_Payload::props( $many ) ),
	'the number of properties is capped'
);

feasible_assert(
	Feasible_Payload::MAX_PROPS > 30 && Feasible_Payload::MAX_PROP_NAME_LENGTH > 300 && Feasible_Payload::MAX_PROP_VALUE_LENGTH > 2000,
	'the property caps sit above the endpoint\'s own, so the endpoint still counts what it cuts'
);

feasible_assert_same(
	Feasible_Payload::MAX_PROP_VALUE_LENGTH,
	strlen( Feasible_Payload::prop_value( str_repeat( 'x', 20000 ) ) ),
	'a property value is capped'
);

feasible_assert_same(
	Feasible_Payload::MAX_NAME_LENGTH,
	strlen( Feasible_Payload::clean( '{"n":"' . str_repeat( 'n', 2000 ) . '"}', 'example.com' )['n'] ),
	'an event name is capped'
);

feasible_assert_same(
	Feasible_Payload::MAX_URL_LENGTH,
	strlen( Feasible_Payload::url( 'https://example.com/' . str_repeat( 'a', 20000 ) ) ),
	'a URL is capped'
);

feasible_assert_same(
	array(
		'amount'   => '19.99',
		'currency' => 'USD',
	),
	Feasible_Payload::revenue(
		array(
			'amount'   => '19.99',
			'currency' => 'USD',
		)
	),
	'a revenue amount sent as text is kept for the endpoint to parse'
);

feasible_assert_same(
	array(
		'amount'   => 20,
		'currency' => 'EUR',
	),
	Feasible_Payload::revenue(
		array(
			'amount'   => 20,
			'currency' => 'E-U-R',
		)
	),
	'a revenue amount sent as a number stays a number, and the currency keeps only letters'
);

feasible_assert_same(
	null,
	Feasible_Payload::revenue( array( 'amount' => array( 1 ), 'currency' => 'USD' ) ),
	'a revenue amount that is neither text nor a number is dropped'
);

feasible_assert_same(
	null,
	Feasible_Payload::revenue( '19.99' ),
	'revenue that is not an object is dropped'
);

feasible_assert_same(
	'android-app://com.google.android.gm/',
	Feasible_Payload::url( 'android-app://com.google.android.gm/' ),
	'an Android app referrer is kept so the visit is not counted as direct'
);
