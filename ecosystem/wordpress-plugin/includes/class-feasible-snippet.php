<?php
//
// class-feasible-snippet.php
// Putting the tracking script on the page, and deciding when not to.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Feasible_Snippet enqueues the tracker and the small inline calls beside it.
 *
 * The script goes through the enqueue system rather than being echoed into
 * wp_head, so that caching plugins, script concatenation and any theme that
 * moves the footer all see it the way they see every other script on the site.
 */
class Feasible_Snippet {

	// The handle everything else attaches to.
	const HANDLE = 'feasible-analytics';

	/**
	 * register wires the snippet into the front end.
	 *
	 * @return void
	 */
	public static function register() {
		add_action( 'wp_enqueue_scripts', array( __CLASS__, 'enqueue' ) );
		add_filter( 'script_loader_tag', array( __CLASS__, 'script_tag' ), 10, 2 );
	}

	/**
	 * enqueue registers the script and the inline calls for this request.
	 *
	 * @return void
	 */
	public static function enqueue() {
		if ( ! self::should_track() ) {
			return;
		}

		$in_footer = ( 'footer' === Feasible_Settings::get( 'inject_location' ) );

		// A null version keeps WordPress from appending ?ver= to the source.
		// The proxy path is already unique to this site and the ETag decides
		// freshness, so a version string would only fragment the browser cache
		// on every WordPress upgrade.
		wp_enqueue_script( self::HANDLE, self::script_src(), array(), null, $in_footer );

		// The queue stub has to exist before anything calls feasible(). The
		// bundle is deferred, so a call made by the page — or by the inline
		// measurement below — would otherwise throw before the bundle loads.
		wp_add_inline_script( self::HANDLE, self::queue_stub(), 'before' );

		$measurement = Feasible_Measurements::inline_script();

		if ( '' !== $measurement ) {
			wp_add_inline_script( self::HANDLE, $measurement, 'after' );
		}
	}

	/**
	 * script_src is the address the tag loads from.
	 *
	 * With the proxy on this is a path on the customer's own domain; with it
	 * off it is the service directly, which is a working installation and is
	 * said plainly on the settings screen rather than treated as a fault.
	 *
	 * @return string
	 */
	public static function script_src() {
		if ( Feasible_Settings::proxy_enabled() ) {
			return Feasible_Paths::script_url();
		}

		return Feasible_Settings::upstream_script_url();
	}

	/**
	 * queue_stub is the two-line shim that makes feasible() safe to call early.
	 *
	 * @return string
	 */
	public static function queue_stub() {
		return 'window.feasible=window.feasible||function(){(window.feasible.q=window.feasible.q||[]).push(arguments)};';
	}

	/**
	 * attributes returns the data-* attributes the tag carries.
	 *
	 * data-api is only set when the proxy is on, and then it is mandatory:
	 * without it the bundle posts to its own origin on the default path, which
	 * on this site is a URL nothing answers.
	 *
	 * @return array
	 */
	public static function attributes() {
		$settings   = Feasible_Settings::all();
		$attributes = array( 'data-domain' => $settings['domain'] );

		if ( Feasible_Settings::proxy_enabled() ) {
			$attributes['data-api'] = Feasible_Paths::event_url();
		}

		if ( '' !== trim( (string) $settings['file_types'] ) ) {
			$attributes['data-file-types'] = $settings['file_types'];
		}

		return $attributes;
	}

	/**
	 * script_tag adds defer and the data attributes to our tag only.
	 *
	 * The attributes are added here rather than through wp_script_add_data so
	 * that they land on the tag on every WordPress version this plugin
	 * supports, including the ones before loading strategies existed.
	 *
	 * @param string $tag    The full script tag.
	 * @param string $handle The handle being printed.
	 * @return string
	 */
	public static function script_tag( $tag, $handle ) {
		if ( self::HANDLE !== $handle ) {
			return $tag;
		}

		$extra = 'defer ';

		foreach ( self::attributes() as $name => $value ) {
			$extra .= esc_attr( $name ) . '="' . esc_attr( $value ) . '" ';
		}

		return str_replace( '<script ', '<script ' . $extra, $tag );
	}

	/**
	 * should_track decides whether this request gets a tracking script at all.
	 *
	 * The exclusions are applied here rather than in the browser because an
	 * excluded visitor should cost nothing: no request for the bundle, no
	 * beacon, nothing in the network tab to explain.
	 *
	 * @return bool
	 */
	public static function should_track() {
		$settings = Feasible_Settings::all();
		$track    = true;

		if ( empty( $settings['inject_enabled'] ) || '' === trim( (string) $settings['domain'] ) ) {
			$track = false;
		} elseif ( is_preview() || is_customize_preview() ) {
			// A preview is the author looking at their own unpublished draft,
			// and counting it puts traffic on a page the public cannot reach.
			$track = false;
		} elseif ( is_user_logged_in() ) {
			if ( ! empty( $settings['exclude_logged_in'] ) ) {
				$track = false;
			} elseif ( ! empty( $settings['excluded_roles'] ) ) {
				$user = wp_get_current_user();

				if ( $user && ! empty( $user->roles ) && array_intersect( (array) $user->roles, (array) $settings['excluded_roles'] ) ) {
					$track = false;
				}
			}
		}

		/**
		 * Filters whether this request is measured.
		 *
		 * The last word belongs to the site, because no settings screen can
		 * anticipate a staging domain, a headless preview route or a country
		 * the site has decided not to measure.
		 *
		 * @param bool  $track    Whether to enqueue the tracker.
		 * @param array $settings The plugin settings.
		 */
		return (bool) apply_filters( 'feasible_should_track', $track, $settings );
	}
}
