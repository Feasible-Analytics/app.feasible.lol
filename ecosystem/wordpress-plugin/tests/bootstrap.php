<?php
//
// bootstrap.php
// The handful of WordPress functions the pure logic touches, stubbed so the tests run under plain php.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// The plugin's files refuse to load outside WordPress. Defining the constant is
// the whole of what they need: none of the logic under test reaches a database,
// an HTTP client or a hook.
if ( ! defined( 'ABSPATH' ) ) {
	define( 'ABSPATH', dirname( __DIR__ ) . '/' );
}

if ( ! function_exists( 'wp_generate_password' ) ) {
	/**
	 * wp_generate_password mirrors the alphabet WordPress uses with special
	 * characters switched off, which is the only call shape the plugin makes.
	 *
	 * @param int  $length              How many characters to return.
	 * @param bool $special_chars       Unused here; the plugin always passes false.
	 * @param bool $extra_special_chars Unused here; the plugin always passes false.
	 * @return string
	 */
	function wp_generate_password( $length = 12, $special_chars = true, $extra_special_chars = false ) {
		unset( $special_chars, $extra_special_chars );

		$alphabet = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789';
		$password = '';

		for ( $i = 0; $i < $length; $i++ ) {
			$password .= $alphabet[ random_int( 0, strlen( $alphabet ) - 1 ) ];
		}

		return $password;
	}
}

require_once dirname( __DIR__ ) . '/includes/class-feasible-client-ip.php';
require_once dirname( __DIR__ ) . '/includes/class-feasible-events.php';
require_once dirname( __DIR__ ) . '/includes/class-feasible-measurements.php';
require_once dirname( __DIR__ ) . '/includes/class-feasible-paths.php';

$GLOBALS['feasible_test_passed']   = 0;
$GLOBALS['feasible_test_failures'] = array();

/**
 * feasible_assert records one boolean expectation.
 *
 * Failures are collected rather than thrown so that one broken expectation does
 * not hide the twenty after it, which is the difference between one fix and
 * twenty runs.
 *
 * @param bool   $condition What was expected to hold.
 * @param string $message   What it was expected to prove.
 * @return void
 */
function feasible_assert( $condition, $message ) {
	if ( $condition ) {
		$GLOBALS['feasible_test_passed']++;

		return;
	}

	$GLOBALS['feasible_test_failures'][] = $message;
}

/**
 * feasible_assert_same records one equality expectation, showing both values
 * when it fails.
 *
 * @param mixed  $expected What should have come back.
 * @param mixed  $actual   What did.
 * @param string $message  What it was expected to prove.
 * @return void
 */
function feasible_assert_same( $expected, $actual, $message ) {
	if ( $expected === $actual ) {
		$GLOBALS['feasible_test_passed']++;

		return;
	}

	$GLOBALS['feasible_test_failures'][] = sprintf(
		"%s\n      expected: %s\n      actual:   %s",
		$message,
		var_export( $expected, true ),
		var_export( $actual, true )
	);
}

/**
 * feasible_test_summary prints the result and sets the exit status.
 *
 * It runs on shutdown so that a single test file and the whole suite report the
 * same way, and so a fatal error still produces a non-zero exit rather than a
 * silent pass.
 *
 * @return void
 */
function feasible_test_summary() {
	$failures = $GLOBALS['feasible_test_failures'];
	$passed   = $GLOBALS['feasible_test_passed'];

	$fatal = error_get_last();

	if ( is_array( $fatal ) && in_array( $fatal['type'], array( E_ERROR, E_PARSE, E_CORE_ERROR, E_COMPILE_ERROR ), true ) ) {
		// A file that died halfway through has run some of its assertions and
		// passed all of them, which without this check reads as a clean run.
		echo 'FAILED: ' . $fatal['message'] . ' in ' . $fatal['file'] . ':' . $fatal['line'] . "\n";
		exit( 1 );
	}

	if ( empty( $failures ) ) {
		echo 'ok: ' . $passed . " assertions passed\n";

		return;
	}

	echo 'FAILED: ' . count( $failures ) . ' of ' . ( $passed + count( $failures ) ) . " assertions\n";

	foreach ( $failures as $failure ) {
		echo '  - ' . $failure . "\n";
	}

	exit( 1 );
}

register_shutdown_function( 'feasible_test_summary' );
