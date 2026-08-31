<?php
//
// test-events.php
// The suppression list: which event names a WordPress switch may stop, and which it may not.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

require_once __DIR__ . '/bootstrap.php';

$all_on = array(
	'track_outbound'  => true,
	'track_downloads' => true,
	'track_forms'     => true,
);

$all_off = array(
	'track_outbound'  => false,
	'track_downloads' => false,
	'track_forms'     => false,
);

feasible_assert_same(
	array(
		'Outbound Link: Click' => 'track_outbound',
		'File Download'        => 'track_downloads',
		'Form: Submit'         => 'track_forms',
	),
	Feasible_Events::switches(),
	'exactly three event names are suppressible, and each maps to one setting'
);

foreach ( Feasible_Events::switches() as $name => $setting ) {
	feasible_assert(
		Feasible_Events::is_suppressed( $name, $all_off ),
		$name . ' is suppressed when its switch is off'
	);

	feasible_assert(
		! Feasible_Events::is_suppressed( $name, $all_on ),
		$name . ' is forwarded when its switch is on'
	);

	$only_this_off            = $all_on;
	$only_this_off[ $setting ] = false;

	foreach ( array_keys( Feasible_Events::switches() ) as $other ) {
		if ( $other === $name ) {
			continue;
		}

		feasible_assert(
			! Feasible_Events::is_suppressed( $other, $only_this_off ),
			'switching off ' . $name . ' leaves ' . $other . ' alone'
		);
	}
}

feasible_assert(
	! Feasible_Events::is_suppressed( 'pageview', $all_off ),
	'a pageview is never suppressed, whatever the three switches say'
);

feasible_assert(
	! Feasible_Events::is_suppressed( 'Signup', $all_off ),
	'an event the site sends deliberately is never suppressed'
);

feasible_assert(
	! Feasible_Events::is_suppressed( 'outbound link: click', $all_off ),
	'the names are a wire contract and are matched exactly, not loosely'
);

feasible_assert(
	! Feasible_Events::is_suppressed( '', $all_off ),
	'a body with no name is forwarded so the server can answer for it'
);

feasible_assert(
	! Feasible_Events::is_suppressed( 'File Download', array() ),
	'a setting that has never been saved means the default, and the default is on'
);

feasible_assert_same(
	'Outbound Link: Click',
	Feasible_Events::name_from_body( '{"n":"Outbound Link: Click","u":"https://example.com/","d":"example.com"}' ),
	'the event name is read out of the wire body'
);

feasible_assert_same(
	'File Download',
	Feasible_Events::name_from_body( '{"n":"  File Download  "}' ),
	'a padded name still matches the switch it belongs to'
);

feasible_assert_same(
	'',
	Feasible_Events::name_from_body( 'not json at all' ),
	'a malformed body yields no name, so nothing is suppressed on a guess'
);

feasible_assert_same(
	'',
	Feasible_Events::name_from_body( '{"n":{"nested":true}}' ),
	'a name that is not a string yields no name'
);

feasible_assert_same(
	'',
	Feasible_Events::name_from_body( '' ),
	'an empty body yields no name'
);

feasible_assert_same(
	'disabled-in-wordpress',
	Feasible_Events::DROPPED_REASON,
	'the reason the proxy reports is the one documented for the response header'
);
