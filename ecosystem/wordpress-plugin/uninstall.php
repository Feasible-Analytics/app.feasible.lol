<?php
//
// uninstall.php
// Removing everything this plugin stored, on every site of a network.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Only WordPress may run this, and only while it is uninstalling the plugin.
if ( ! defined( 'WP_UNINSTALL_PLUGIN' ) ) {
	exit;
}

/**
 * feasible_uninstall_site removes the plugin's rows from the current site.
 *
 * The cached script bodies are deleted with a query rather than by name
 * because their transient keys contain a hash of the upstream URL, so a site
 * that changed hosts has more than one and none of them is predictable from
 * here.
 *
 * @return void
 */
function feasible_uninstall_site() {
	global $wpdb;

	delete_option( 'feasible_settings' );
	delete_option( 'feasible_paths' );
	delete_option( 'feasible_last_error' );
	delete_option( 'feasible_version' );

	// The rewrite rules hold the two routes this plugin registered, and they
	// are rebuilt without them on the next request.
	delete_option( 'rewrite_rules' );

	// phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching
	$wpdb->query(
		$wpdb->prepare(
			"DELETE FROM {$wpdb->options} WHERE option_name LIKE %s OR option_name LIKE %s",
			$wpdb->esc_like( '_transient_feasible_js_' ) . '%',
			$wpdb->esc_like( '_transient_timeout_feasible_js_' ) . '%'
		)
	);
}

if ( is_multisite() ) {
	$feasible_sites = get_sites(
		array(
			'fields' => 'ids',
			'number' => 0,
		)
	);

	foreach ( $feasible_sites as $feasible_site_id ) {
		switch_to_blog( $feasible_site_id );
		feasible_uninstall_site();
		restore_current_blog();
	}
} else {
	feasible_uninstall_site();
}
