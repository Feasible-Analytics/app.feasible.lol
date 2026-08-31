<?php
//
// class-feasible-measurements.php
// The two measurements WordPress knows about and the browser script cannot: 404s and site search.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Feasible_Measurements builds the inline script for a 404 or a search page.
 *
 * These two are the plugin's own work rather than the browser script's. Only
 * WordPress knows that a URL was a 404 rather than a thin page, and only
 * WordPress knows how many results a search returned — the browser cannot see
 * either without guessing from the markup.
 */
class Feasible_Measurements {

	// The event names sent for each. Short and stable, because they become
	// dashboard rows and renaming one loses the history behind it.
	const EVENT_NOT_FOUND = '404';
	const EVENT_SEARCH    = 'Search';

	// How long a search term may be before it is cut.
	//
	// The ingest server accepts two thousand characters, but a property that
	// long is not a search term — it is a paste, or a scanner probing the
	// search form. A hundred keeps the breakdown readable and still holds every
	// real query a person types.
	const SEARCH_TERM_MAX_LENGTH = 100;

	// The two buckets for "did this search find anything".
	const RESULTS_NONE = 'none';
	const RESULTS_SOME = 'some';

	/**
	 * normalise_search_term reduces a query to the form it should be grouped by.
	 *
	 * Without this the breakdown is a thousand near-duplicates: "Blue Widget",
	 * "blue  widget" and "blue widget " are one question asked three ways, and
	 * splitting them across three rows hides the fact that it is the most
	 * common search on the site. Case and whitespace carry no meaning in a
	 * search box, so they are removed rather than reported.
	 *
	 * @param mixed $term Raw search query.
	 * @return string
	 */
	public static function normalise_search_term( $term ) {
		$term = (string) $term;

		// Control characters reach here from pasted content and from probes,
		// and they would travel into a JSON property that no dashboard can
		// render. A space keeps word boundaries that a deletion would join.
		$stripped = preg_replace( '/[\x00-\x1F\x7F]+/u', ' ', $term );

		if ( null === $stripped ) {
			// The subject was not valid UTF-8, so the unicode pattern refused
			// it. Falling back byte-wise still produces something groupable
			// rather than dropping the search entirely.
			$stripped = preg_replace( '/[\x00-\x1F\x7F]+/', ' ', $term );
		}

		$term = is_string( $stripped ) ? $stripped : '';

		$collapsed = preg_replace( '/\s+/u', ' ', $term );

		if ( null === $collapsed ) {
			$collapsed = preg_replace( '/\s+/', ' ', $term );
		}

		$term = trim( is_string( $collapsed ) ? $collapsed : '' );

		if ( '' === $term ) {
			return '';
		}

		if ( function_exists( 'mb_strtolower' ) ) {
			$term = mb_strtolower( $term, 'UTF-8' );
		} else {
			$term = strtolower( $term );
		}

		if ( function_exists( 'mb_substr' ) ) {
			if ( mb_strlen( $term, 'UTF-8' ) > self::SEARCH_TERM_MAX_LENGTH ) {
				$term = mb_substr( $term, 0, self::SEARCH_TERM_MAX_LENGTH, 'UTF-8' );
			}
		} elseif ( strlen( $term ) > self::SEARCH_TERM_MAX_LENGTH ) {
			$term = substr( $term, 0, self::SEARCH_TERM_MAX_LENGTH );
		}

		return trim( $term );
	}

	/**
	 * bucket_results answers "did this search find anything" in one word.
	 *
	 * A raw count is a poor filter: nobody wants the searches that returned
	 * exactly seven results. The question worth asking of a site search is
	 * which queries came back empty, and a two-value property puts that one
	 * click away instead of behind a range filter no dashboard offers.
	 *
	 * @param mixed $count Number of results.
	 * @return string
	 */
	public static function bucket_results( $count ) {
		return ( (int) $count > 0 ) ? self::RESULTS_SOME : self::RESULTS_NONE;
	}

	/**
	 * search_props builds the properties for the current search request.
	 *
	 * @return array
	 */
	public static function search_props() {
		global $wp_query;

		$found = 0;

		if ( isset( $wp_query ) && is_object( $wp_query ) && isset( $wp_query->found_posts ) ) {
			$found = (int) $wp_query->found_posts;
		}

		return array(
			'search_term'   => self::normalise_search_term( get_search_query( false ) ),
			'results'       => $found,
			'results_found' => self::bucket_results( $found ),
		);
	}

	/**
	 * not_found_props builds the properties for the current 404.
	 *
	 * Only the path is reported, never the query string. A 404 with a tracking
	 * parameter on it is the same broken link as one without, and keeping the
	 * query would split a single broken link across hundreds of rows.
	 *
	 * @return array
	 */
	public static function not_found_props() {
		$request = isset( $_SERVER['REQUEST_URI'] ) ? wp_unslash( $_SERVER['REQUEST_URI'] ) : '';
		$request = is_string( $request ) ? $request : '';

		$path = wp_parse_url( esc_url_raw( $request ), PHP_URL_PATH );
		$path = is_string( $path ) ? $path : '/';

		return array(
			'path' => substr( $path, 0, 500 ),
		);
	}

	/**
	 * inline_script returns the JavaScript for this request, or an empty string.
	 *
	 * It is emitted as an inline script attached to the tracker handle rather
	 * than a separate file, because a second request for four lines of code
	 * would cost more than the measurement is worth. The referrer is read in
	 * the browser rather than from the request headers, so it matches what the
	 * rest of the tracker reports.
	 *
	 * @return string
	 */
	public static function inline_script() {
		$settings = Feasible_Settings::all();

		if ( is_404() && ! empty( $settings['track_404'] ) ) {
			$props = self::not_found_props();

			return 'window.feasible(' . wp_json_encode( self::EVENT_NOT_FOUND ) . ',{props:(function(){'
				. 'var p=' . wp_json_encode( $props ) . ';'
				. 'if(document.referrer)p.referrer=document.referrer;'
				. 'return p;})()});';
		}

		if ( is_search() && ! empty( $settings['track_search'] ) ) {
			$props = self::search_props();

			if ( '' === $props['search_term'] ) {
				// An empty search box submitted by hand is not a search, and
				// counting it would put a blank row at the top of the report.
				return '';
			}

			return 'window.feasible(' . wp_json_encode( self::EVENT_SEARCH ) . ',{props:' . wp_json_encode( $props ) . '});';
		}

		return '';
	}
}
