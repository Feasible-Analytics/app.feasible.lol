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
 * @return void
 */
function feasible_uninstall_site() {
	delete_option( 'feasible_settings' );
	delete_option( 'feasible_paths' );
	delete_option( 'feasible_last_error' );
	delete_option( 'feasible_version' );

	// The rewrite rules hold the two routes this plugin registered, and they
	// are rebuilt without them on the next request.
	delete_option( 'rewrite_rules' );
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
