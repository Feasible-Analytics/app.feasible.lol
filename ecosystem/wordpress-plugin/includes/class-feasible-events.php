<?php
//
// class-feasible-events.php
// The three events WordPress can switch off, and how the proxy recognises them.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Feasible_Events decides which events this site has switched off.
 *
 * The browser script measures outbound clicks, downloads and form submissions
 * with no configuration and no way to disable them from outside. The only place
 * a WordPress setting can actually stop one is between the browser and the
 * ingest endpoint, which is the proxy — so these switches work only when the
 * proxy is on, and the settings screen says so plainly rather than presenting
 * three toggles that quietly do nothing.
 */
class Feasible_Events {

	// The names the browser script sends. They are matched exactly because they
	// are a wire contract, not a label.
	const OUTBOUND  = 'Outbound Link: Click';
	const DOWNLOAD  = 'File Download';
	const FORM      = 'Form: Submission';

	// The response header the ingest server uses to say an event was not
	// counted, and the reason the proxy puts in it. A 202 with this header is
	// not a failure and must never be retried.
	const DROPPED_HEADER = 'x-feasible-dropped';
	const DROPPED_REASON = 'disabled-in-wordpress';

	/**
	 * switches maps each suppressible event name to the setting that governs it.
	 *
	 * It is one table rather than three conditionals so that the settings screen
	 * and the proxy can never disagree about which switch controls what.
	 *
	 * @return array Event name => setting key.
	 */
	public static function switches() {
		return array(
			self::OUTBOUND => 'track_outbound',
			self::DOWNLOAD => 'track_downloads',
			self::FORM     => 'track_forms',
		);
	}

	/**
	 * name_from_body reads the event name out of a raw request body.
	 *
	 * A body that will not decode returns an empty name, which suppresses
	 * nothing: forwarding it and letting the server answer 400 is far better
	 * than the proxy inventing its own opinion about a malformed payload.
	 *
	 * @param string $body Raw JSON body.
	 * @return string
	 */
	public static function name_from_body( $body ) {
		$body = trim( (string) $body );

		if ( '' === $body ) {
			return '';
		}

		$decoded = json_decode( $body, true );

		if ( ! is_array( $decoded ) || ! isset( $decoded['n'] ) || ! is_string( $decoded['n'] ) ) {
			return '';
		}

		return trim( $decoded['n'] );
	}

	/**
	 * is_suppressed reports whether this site has switched an event off.
	 *
	 * Only the three names above are suppressible. A custom event, a pageview
	 * or an unknown name is always forwarded — dropping something the customer
	 * sent deliberately would be the worst kind of silent data loss.
	 *
	 * @param string $name     Event name from the body.
	 * @param array  $settings The plugin settings.
	 * @return bool
	 */
	public static function is_suppressed( $name, array $settings ) {
		$switches = self::switches();
		$name     = trim( (string) $name );

		if ( '' === $name || ! isset( $switches[ $name ] ) ) {
			return false;
		}

		$key = $switches[ $name ];

		// An absent setting means the default, and the default is on. Treating
		// a missing key as off would silence three measurements on every site
		// that upgraded from a version without them.
		if ( ! array_key_exists( $key, $settings ) ) {
			return false;
		}

		return empty( $settings[ $key ] );
	}
}
