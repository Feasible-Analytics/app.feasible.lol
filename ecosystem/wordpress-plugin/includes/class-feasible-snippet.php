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

		// Through the proxy the script is the copy bundled with this plugin, so
		// the plugin version is its version. Loaded from the Feasible host it
		// changes with that host, and a null version keeps WordPress from
		// appending a ?ver= that would say nothing true about it.
		$version = Feasible_Settings::proxy_enabled() ? FEASIBLE_VERSION : null;

		wp_enqueue_script( self::HANDLE, self::script_src(), array(), $version, $in_footer );

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
	 * With the proxy on this is a path on the customer's own domain, which
	 * serves the script bundled with this plugin. With it off it is the
	 * Feasible host directly, which is a working installation and is said
	 * plainly on the settings screen rather than treated as a fault.
	 *
	 * @return string
	 */
	public static function script_src() {
		if ( Feasible_Settings::proxy_enabled() ) {
			return Feasible_Paths::script_url();
		}

		return Feasible_Settings::direct_script_url();
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
	 * The markup WordPress passes in also holds the inline scripts attached to
	 * the handle, so only the tag that loads the file, the one with a src, is
	 * changed.
	 *
	 * @param string $tag    The script markup for the handle.
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

		return self::add_to_loading_tag( $tag, $extra );
	}

	/**
	 * add_to_loading_tag inserts attributes into the first script tag that has
	 * a src, and leaves any inline script around it alone.
	 *
	 * WordPress puts src first on older versions and after id on newer ones,
	 * so the match does not depend on the order.
	 *
	 * @param string $tag   The script markup for the handle.
	 * @param string $extra Attributes to insert, each followed by a space.
	 * @return string
	 */
	public static function add_to_loading_tag( $tag, $extra ) {
		return (string) preg_replace_callback(
			'/<script(?=\s(?:[^>]*\s)?src=)\s/i',
			function () use ( $extra ) {
				return '<script ' . $extra;
			},
			(string) $tag,
			1
		);
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

		if ( ! Feasible_Settings::tracking_enabled() ) {
			// Nothing reaches a page until an administrator has turned
			// tracking on, and no filter below can change that.
			return false;
		}

		if ( empty( $settings['inject_enabled'] ) ) {
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
