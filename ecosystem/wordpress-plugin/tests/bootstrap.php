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

if ( ! function_exists( 'wp_json_encode' ) ) {
	/**
	 * wp_json_encode stands in for the WordPress wrapper, which for the depth
	 * and the flags this plugin passes is json_encode with the same arguments.
	 *
	 * @param mixed $data    Value to encode.
	 * @param int   $options Encoding flags.
	 * @param int   $depth   Maximum depth.
	 * @return string|false
	 */
	function wp_json_encode( $data, $options = 0, $depth = 512 ) {
		return json_encode( $data, $options, $depth ); // phpcs:ignore WordPress.WP.AlternativeFunctions.json_encode_json_encode
	}
}

if ( ! function_exists( 'sanitize_text_field' ) ) {
	/**
	 * sanitize_text_field follows the core function's steps that matter for an
	 * event body: tags stripped, whitespace collapsed, percent octets removed,
	 * ends trimmed. The real function runs in the WordPress checks.
	 *
	 * @param mixed $str Value to sanitise.
	 * @return string
	 */
	function sanitize_text_field( $str ) {
		$filtered = strip_tags( (string) $str ); // phpcs:ignore WordPress.WP.AlternativeFunctions.strip_tags_strip_tags
		$filtered = preg_replace( '/[\r\n\t ]+/', ' ', $filtered );

		while ( preg_match( '/%[a-f0-9]{2}/i', $filtered, $match ) ) {
			$filtered = str_replace( $match[0], '', $filtered );
		}

		return trim( $filtered );
	}
}

if ( ! function_exists( 'wp_strip_all_tags' ) ) {
	/**
	 * wp_strip_all_tags is the core function, which is short enough to copy.
	 *
	 * @param string $text          Text to strip.
	 * @param bool   $remove_breaks Whether to collapse line breaks too.
	 * @return string
	 */
	function wp_strip_all_tags( $text, $remove_breaks = false ) {
		$text = preg_replace( '@<(script|style)[^>]*?>.*?</\\1>@si', '', (string) $text );
		$text = strip_tags( $text ); // phpcs:ignore WordPress.WP.AlternativeFunctions.strip_tags_strip_tags

		if ( $remove_breaks ) {
			$text = preg_replace( '/[\r\n\t ]+/', ' ', $text );
		}

		return trim( $text );
	}
}

if ( ! function_exists( 'esc_url_raw' ) ) {
	/**
	 * esc_url_raw keeps the core function's protocol check and its allowed
	 * character set, which is what the payload tests depend on.
	 *
	 * @param string $url       URL to sanitise.
	 * @param array  $protocols Allowed schemes.
	 * @return string
	 */
	function esc_url_raw( $url, $protocols = null ) {
		$url = str_replace( ' ', '%20', ltrim( (string) $url ) );
		$url = preg_replace( '|[^a-z0-9-~+_.?#=!&;,/:%@$\|*\'()\[\]\\x80-\\xff]|i', '', $url );

		if ( '' === $url ) {
			return '';
		}

		$colon = strpos( $url, ':' );

		if ( false === $colon ) {
			return '';
		}

		$scheme = strtolower( substr( $url, 0, $colon ) );

		return in_array( $scheme, (array) $protocols, true ) ? $url : '';
	}
}

if ( ! function_exists( 'wp_is_uuid' ) ) {
	/**
	 * wp_is_uuid is the core function's pattern for any UUID version.
	 *
	 * @param mixed $uuid Value to check.
	 * @return bool
	 */
	function wp_is_uuid( $uuid ) {
		return is_string( $uuid ) && 1 === preg_match( '/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/', $uuid );
	}
}

if ( ! function_exists( 'wp_parse_url' ) ) {
	/**
	 * wp_parse_url is the core wrapper around parse_url, which on the PHP
	 * versions the plugin supports behaves the same for absolute URLs.
	 *
	 * @param string $url       URL to parse.
	 * @param int    $component One PHP_URL_* component, or -1 for all of them.
	 * @return mixed
	 */
	function wp_parse_url( $url, $component = -1 ) {
		return parse_url( $url, $component ); // phpcs:ignore WordPress.WP.AlternativeFunctions.parse_url_parse_url
	}
}

if ( ! function_exists( 'add_query_arg' ) ) {
	/**
	 * add_query_arg sets one query parameter the way the core function does for
	 * the three-argument form: an existing value is replaced, the other
	 * parameters and any fragment are kept.
	 *
	 * @param string $key   Parameter name.
	 * @param string $value Parameter value.
	 * @param string $url   URL to change.
	 * @return string
	 */
	function add_query_arg( $key, $value, $url ) {
		$fragment = '';

		if ( false !== strpos( $url, '#' ) ) {
			list( $url, $fragment ) = explode( '#', $url, 2 );
			$fragment               = '#' . $fragment;
		}

		$query = array();

		if ( false !== strpos( $url, '?' ) ) {
			list( $url, $existing ) = explode( '?', $url, 2 );
			parse_str( $existing, $query );
		}

		$query[ $key ] = $value;

		return $url . '?' . http_build_query( $query ) . $fragment;
	}
}

require_once dirname( __DIR__ ) . '/includes/class-feasible-client-ip.php';
require_once dirname( __DIR__ ) . '/includes/class-feasible-dashboard.php';
require_once dirname( __DIR__ ) . '/includes/class-feasible-events.php';
require_once dirname( __DIR__ ) . '/includes/class-feasible-payload.php';
require_once dirname( __DIR__ ) . '/includes/class-feasible-measurements.php';
require_once dirname( __DIR__ ) . '/includes/class-feasible-paths.php';
require_once dirname( __DIR__ ) . '/includes/class-feasible-proxy.php';
require_once dirname( __DIR__ ) . '/includes/class-feasible-settings.php';
require_once dirname( __DIR__ ) . '/includes/class-feasible-snippet.php';

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
