<?php
//
// test-paths.php
// The shape of a generated path: what goes into a rewrite regex has to be boring.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

require_once __DIR__ . '/bootstrap.php';

$paths = Feasible_Paths::generate();

feasible_assert_same(
	array( 'namespace', 'script', 'event' ),
	array_keys( $paths ),
	'a generated set is a namespace and the two segments under it'
);

foreach ( $paths as $key => $segment ) {
	feasible_assert_same(
		Feasible_Paths::SEGMENT_LENGTH,
		strlen( $segment ),
		'the ' . $key . ' segment is exactly the configured length'
	);

	feasible_assert(
		1 === preg_match( '/^[A-Za-z0-9]+$/', $segment ),
		'the ' . $key . ' segment is alphanumeric, so it needs no escaping in a rewrite pattern'
	);

	feasible_assert(
		Feasible_Paths::is_valid_segment( $segment ),
		'the ' . $key . ' segment passes the validator that guards the rewrite rules'
	);
}

feasible_assert(
	$paths['script'] !== $paths['event'],
	'the two routes are told apart by their segment, so the two segments must differ'
);

$second = Feasible_Paths::generate();

feasible_assert(
	$second['script'] !== $paths['script'],
	'rotating produces a different path, which is the whole point of the button'
);

feasible_assert(
	! Feasible_Paths::is_valid_segment( '' ),
	'an empty segment is refused'
);

feasible_assert(
	! Feasible_Paths::is_valid_segment( 'abc' ),
	'a segment short enough to guess is refused'
);

feasible_assert(
	! Feasible_Paths::is_valid_segment( 'seg/ment12345' ),
	'a segment containing a slash is refused before it reaches a rewrite pattern'
);

feasible_assert(
	! Feasible_Paths::is_valid_segment( 'segment.js123' ),
	'a segment containing a dot is refused, because the pattern adds its own'
);

feasible_assert(
	! Feasible_Paths::is_valid_segment( 'segment-12345' ),
	'a segment containing a hyphen is refused rather than escaped'
);

feasible_assert(
	! Feasible_Paths::is_valid_segment( str_repeat( 'a', 64 ) ),
	'an absurdly long segment is refused'
);

feasible_assert(
	! Feasible_Paths::is_valid_segment( array( 'abcdefghijkl' ) ),
	'anything that is not a string is refused'
);

feasible_assert(
	Feasible_Paths::is_complete( $paths ),
	'a freshly generated set is complete'
);

feasible_assert(
	! Feasible_Paths::is_complete( array( 'namespace' => 'abcdefghijkl' ) ),
	'a set missing its segments is not complete, so it is regenerated rather than served'
);

feasible_assert(
	! Feasible_Paths::is_complete(
		array(
			'namespace' => 'abcdefghijkl',
			'script'    => 'mnopqrstuvwx',
			'event'     => 'mnopqrstuvwx',
		)
	),
	'a set whose two segments collided is not complete'
);

feasible_assert(
	! Feasible_Paths::is_complete( 'not an array' ),
	'a corrupt option value is not complete'
);
