<?php
//
// class-feasible-dashboard.php
// The shared dashboard, framed inside wp-admin, and the check that decides what may be framed.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Feasible_Dashboard renders the shared dashboard inside the admin.
 *
 * Framing is the only way to put the real dashboard in wp-admin without
 * rebuilding it, and it is the reason the URL is validated rather than trusted:
 * an iframe sits in the same browsing context as the administrator's session,
 * so the address in it has to be one of ours or it is not framed at all.
 */
class Feasible_Dashboard {

	// The path prefix a shared dashboard link always has.
	const SHARE_PATH_PREFIX = '/share/';

	/**
	 * is_valid_share_url reports whether a URL may be framed.
	 *
	 * Two things are checked and neither is optional: the host has to be the
	 * configured service, and the path has to be a share link on it. Accepting
	 * any URL on the right host would still frame a login page, and accepting
	 * any /share/ path on any host would frame anything at all.
	 *
	 * @param string $url  Candidate URL.
	 * @param string $host The configured service origin.
	 * @return bool
	 */
	public static function is_valid_share_url( $url, $host ) {
		$url  = trim( (string) $url );
		$host = trim( (string) $host );

		if ( '' === $url || '' === $host ) {
			return false;
		}

		$target = wp_parse_url( $url );
		$ours   = wp_parse_url( $host );

		if ( ! is_array( $target ) || ! is_array( $ours ) ) {
			return false;
		}

		$scheme = isset( $target['scheme'] ) ? strtolower( $target['scheme'] ) : '';

		if ( ! in_array( $scheme, array( 'http', 'https' ), true ) ) {
			return false;
		}

		$target_host = isset( $target['host'] ) ? strtolower( $target['host'] ) : '';
		$our_host    = isset( $ours['host'] ) ? strtolower( $ours['host'] ) : '';

		if ( '' === $target_host || $target_host !== $our_host ) {
			return false;
		}

		$path = isset( $target['path'] ) ? $target['path'] : '';

		if ( 0 !== strpos( $path, self::SHARE_PATH_PREFIX ) ) {
			return false;
		}

		// A bare /share/ with nothing after it is not a dashboard, and framing
		// it would show an error page inside wp-admin with no clue why.
		return strlen( $path ) > strlen( self::SHARE_PATH_PREFIX );
	}

	/**
	 * render draws the dashboard page.
	 *
	 * @return void
	 */
	public static function render() {
		if ( ! current_user_can( 'manage_options' ) ) {
			wp_die( esc_html__( 'You do not have permission to view this page.', 'feasible-analytics' ) );
		}

		$url      = (string) Feasible_Settings::get( 'shared_dashboard_url' );
		$host     = Feasible_Settings::host();
		$settings = admin_url( 'admin.php?page=feasible-analytics' );

		echo '<div class="wrap">';
		echo '<h1>' . esc_html__( 'feasible.lol Dashboard', 'feasible-analytics' ) . '</h1>';

		if ( '' === $url ) {
			echo '<p>' . esc_html__( 'No shared dashboard link is set yet. Create a shared link for this site in feasible.lol, then paste it into the settings screen and it will appear here.', 'feasible-analytics' ) . '</p>';
			echo '<p><a class="button button-primary" href="' . esc_url( $settings ) . '">' . esc_html__( 'Open the settings screen', 'feasible-analytics' ) . '</a></p>';
			echo '</div>';

			return;
		}

		if ( ! self::is_valid_share_url( $url, $host ) ) {
			echo '<div class="notice notice-error"><p>';
			printf(
				/* translators: %s: the configured feasible.lol host. */
				esc_html__( 'The saved dashboard link is not a /share/ link on %s, so it has not been loaded.', 'feasible-analytics' ),
				esc_html( $host )
			);
			echo '</p></div>';
			echo '</div>';

			return;
		}

		echo '<p><a href="' . esc_url( $url ) . '" target="_blank" rel="noopener noreferrer">' . esc_html__( 'Open the dashboard in a new tab', 'feasible-analytics' ) . '</a></p>';
		echo '<iframe title="' . esc_attr__( 'feasible.lol dashboard', 'feasible-analytics' ) . '"';
		echo ' src="' . esc_url( $url ) . '"';
		echo ' referrerpolicy="no-referrer"';
		echo ' loading="lazy"';
		echo ' style="width:100%;min-height:80vh;border:1px solid #c3c4c7;background:#fff;"></iframe>';
		echo '</div>';
	}
}
