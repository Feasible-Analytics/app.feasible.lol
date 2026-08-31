<?php
//
// class-feasible-paths.php
// The randomised paths the proxy answers on, and how they are rotated.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Feasible_Paths generates and stores the two paths this site serves on.
 *
 * Filter lists name files individually. A script served from a memorable path
 * such as /analytics/script.js on every WordPress site in the world is one
 * entry away from being blocked everywhere at once, and the only remedy is a
 * rename. Random per-site segments mean a listing costs one site, and rotating
 * them renames both paths in a single click.
 *
 * None of this is an escape from blocklists and it must not be sold as one. It
 * raises the cost of listing; it does not end the game.
 */
class Feasible_Paths {

	// The option holding the generated namespace and the two segments.
	const OPTION = 'feasible_paths';

	// How long a generated segment is. Twelve alphanumeric characters is about
	// seventy bits, which is far past guessing, and short enough to read out.
	const SEGMENT_LENGTH = 12;

	// Real paths under a rewrite rule.
	const MODE_REWRITE = 'rewrite';

	// A query string on the site index, for sites with plain permalinks.
	const MODE_QUERY = 'query';

	/**
	 * generate returns a fresh namespace and the two segments under it.
	 *
	 * The script and event segments are forced to differ because the dispatcher
	 * tells the two routes apart by comparing the segment, and a collision would
	 * serve the script for an event POST — a failure that looks like the tracker
	 * silently not working.
	 *
	 * @return array
	 */
	public static function generate() {
		$namespace = wp_generate_password( self::SEGMENT_LENGTH, false, false );
		$script    = wp_generate_password( self::SEGMENT_LENGTH, false, false );
		$event     = wp_generate_password( self::SEGMENT_LENGTH, false, false );

		$guard = 0;

		while ( ( $event === $script || $event === $namespace || $script === $namespace ) && $guard < 10 ) {
			$event = wp_generate_password( self::SEGMENT_LENGTH, false, false );
			$guard++;
		}

		return array(
			'namespace' => $namespace,
			'script'    => $script,
			'event'     => $event,
		);
	}

	/**
	 * is_valid_segment reports whether a stored value is usable in a path.
	 *
	 * It is deliberately strict: the segments are interpolated straight into a
	 * rewrite regex and into a query variable name, so anything but letters and
	 * digits is refused rather than escaped.
	 *
	 * @param mixed $value Candidate segment.
	 * @return bool
	 */
	public static function is_valid_segment( $value ) {
		return is_string( $value ) && 1 === preg_match( '/^[A-Za-z0-9]{8,32}$/', $value );
	}

	/**
	 * all returns the stored paths, generating them if they are missing.
	 *
	 * Regenerating a missing or corrupt set here rather than failing means a
	 * site whose activation hook never ran — a plugin restored from a backup,
	 * a manual copy into mu-plugins — still serves.
	 *
	 * @return array
	 */
	public static function all() {
		$stored = get_option( self::OPTION, array() );

		if ( self::is_complete( $stored ) ) {
			return $stored;
		}

		$fresh = self::generate();
		update_option( self::OPTION, $fresh, true );

		return $fresh;
	}

	/**
	 * is_complete reports whether a stored value has all three usable segments.
	 *
	 * @param mixed $stored Stored option value.
	 * @return bool
	 */
	public static function is_complete( $stored ) {
		if ( ! is_array( $stored ) ) {
			return false;
		}

		foreach ( array( 'namespace', 'script', 'event' ) as $key ) {
			if ( ! isset( $stored[ $key ] ) || ! self::is_valid_segment( $stored[ $key ] ) ) {
				return false;
			}
		}

		return $stored['script'] !== $stored['event'];
	}

	/**
	 * rotate replaces every segment and rewrites the rules that point at them.
	 *
	 * This is the documented remedy when a path lands on a filter list. The
	 * flush happens here, with the new rules registered first, because a
	 * rotation that leaves the old rewrite rules in place is a site serving
	 * 404s on both routes.
	 *
	 * @return array The new paths.
	 */
	public static function rotate() {
		$fresh = self::generate();
		update_option( self::OPTION, $fresh, true );

		Feasible_Proxy::add_rewrite_rules();
		flush_rewrite_rules( false );

		return $fresh;
	}

	/**
	 * namespace_segment is the first path segment, and the query variable name
	 * in the fallback mode.
	 *
	 * @return string
	 */
	public static function namespace_segment() {
		$paths = self::all();

		return $paths['namespace'];
	}

	/**
	 * script_segment names the route that serves the browser bundle.
	 *
	 * @return string
	 */
	public static function script_segment() {
		$paths = self::all();

		return $paths['script'];
	}

	/**
	 * event_segment names the route that forwards events upstream.
	 *
	 * @return string
	 */
	public static function event_segment() {
		$paths = self::all();

		return $paths['event'];
	}

	/**
	 * mode reports how the routes are reachable on this site.
	 *
	 * With plain permalinks there are no rewrite rules in front of WordPress at
	 * all, so a bare path never reaches index.php and would be answered by the
	 * web server as a 404 before any PHP runs. The query-string route is the
	 * only shape that works there, so it is chosen automatically rather than
	 * left as a setting nobody would know to change.
	 *
	 * @return string
	 */
	public static function mode() {
		$structure = get_option( 'permalink_structure' );

		return ( is_string( $structure ) && '' !== $structure ) ? self::MODE_REWRITE : self::MODE_QUERY;
	}

	/**
	 * script_url is the address the snippet's src points at.
	 *
	 * @return string
	 */
	public static function script_url() {
		$paths = self::all();

		if ( self::MODE_REWRITE === self::mode() ) {
			return home_url( '/' . $paths['namespace'] . '/' . $paths['script'] . '.js' );
		}

		return add_query_arg( $paths['namespace'], $paths['script'], home_url( '/' ) );
	}

	/**
	 * event_url is the address the snippet's data-api points at.
	 *
	 * @return string
	 */
	public static function event_url() {
		$paths = self::all();

		if ( self::MODE_REWRITE === self::mode() ) {
			return home_url( '/' . $paths['namespace'] . '/' . $paths['event'] );
		}

		return add_query_arg( $paths['namespace'], $paths['event'], home_url( '/' ) );
	}
}
