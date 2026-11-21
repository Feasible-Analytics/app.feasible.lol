<?php
//
// class-feasible-plugin.php
// Boot, activation and deactivation: the small amount of wiring everything else hangs from.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Feasible_Plugin starts the plugin and owns the lifecycle hooks.
 */
class Feasible_Plugin {

	// The version the rewrite rules were last built for. It exists so that an
	// update which changes the routes rebuilds them once, rather than leaving a
	// site serving 404s until somebody happens to re-save their permalinks.
	const VERSION_OPTION = 'feasible_version';

	/**
	 * boot registers everything the plugin does.
	 *
	 * The proxy and the snippet are registered on every request because both
	 * run on the front end; the admin screens are not, because loading them on
	 * a pageview would be work nobody sees.
	 *
	 * @return void
	 */
	public static function boot() {
		Feasible_Proxy::register();
		Feasible_Snippet::register();

		if ( is_admin() ) {
			Feasible_Admin::register();
			Feasible_Site_Health::register();

			add_action( 'admin_init', array( __CLASS__, 'maybe_upgrade' ) );
		}
	}

	/**
	 * activate prepares a fresh installation without turning anything on.
	 *
	 * Activation stores no domain, enables no tracking and makes no request to
	 * any server. It only generates the random path segments and registers
	 * their rewrite rules, both local to this site. Tracking starts when an
	 * administrator enters the site domain and switches it on in the settings.
	 *
	 * The rules are added by hand before the flush because init has already
	 * fired by the time an activation hook runs, so the rules this plugin adds
	 * on init are not in the rewrite object yet.
	 *
	 * @return void
	 */
	public static function activate() {
		Feasible_Paths::all();
		Feasible_Proxy::add_rewrite_rules();
		flush_rewrite_rules( false );

		update_option( self::VERSION_OPTION, FEASIBLE_VERSION );
	}

	/**
	 * deactivate takes the routes back out of the rewrite table.
	 *
	 * The stored rules are deleted rather than flushed, because a flush during
	 * deactivation regenerates them from a request in which this plugin is
	 * still loaded — and writes our own rules straight back in. Deleting them
	 * makes WordPress rebuild on the next request, by which time the plugin is
	 * gone and its rules with it.
	 *
	 * @return void
	 */
	public static function deactivate() {
		delete_option( 'rewrite_rules' );
	}

	/**
	 * maybe_upgrade rebuilds the rewrite rules after a plugin update.
	 *
	 * @return void
	 */
	public static function maybe_upgrade() {
		if ( get_option( self::VERSION_OPTION ) === FEASIBLE_VERSION ) {
			return;
		}

		Feasible_Proxy::add_rewrite_rules();
		flush_rewrite_rules( false );

		update_option( self::VERSION_OPTION, FEASIBLE_VERSION );
	}
}
