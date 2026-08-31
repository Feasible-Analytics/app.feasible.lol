<?php
//
// feasible-analytics.php
// The plugin entry point: constants, class loading, activation and boot.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

/**
 * Plugin Name:       feasible.lol Analytics
 * Plugin URI:        https://feasible.lol/wordpress
 * Description:       Privacy-first analytics for WordPress. Serves the tracking script and the event endpoint from your own domain on randomised paths, tracks site searches and 404s, and puts your dashboard inside wp-admin.
 * Version:           1.0.0
 * Requires at least: 5.9
 * Requires PHP:      7.4
 * Author:            Cloudmanic Labs, LLC
 * Author URI:        https://cloudmanic.com
 * License:           GPL-2.0-or-later
 * License URI:       https://www.gnu.org/licenses/gpl-2.0.html
 * Text Domain:       feasible-analytics
 * Domain Path:       /languages
 */

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

// The version is read by the settings screen and the site health panel. It is a
// constant rather than a call to get_plugin_data() because that function is not
// loaded on the front end, and the proxy runs on every event request.
define( 'FEASIBLE_VERSION', '1.0.0' );

// The absolute path and URL of this plugin folder, both with a trailing slash.
define( 'FEASIBLE_PLUGIN_FILE', __FILE__ );
define( 'FEASIBLE_PLUGIN_DIR', plugin_dir_path( __FILE__ ) );
define( 'FEASIBLE_PLUGIN_URL', plugin_dir_url( __FILE__ ) );

require_once FEASIBLE_PLUGIN_DIR . 'includes/class-feasible-settings.php';
require_once FEASIBLE_PLUGIN_DIR . 'includes/class-feasible-paths.php';
require_once FEASIBLE_PLUGIN_DIR . 'includes/class-feasible-client-ip.php';
require_once FEASIBLE_PLUGIN_DIR . 'includes/class-feasible-events.php';
require_once FEASIBLE_PLUGIN_DIR . 'includes/class-feasible-measurements.php';
require_once FEASIBLE_PLUGIN_DIR . 'includes/class-feasible-proxy.php';
require_once FEASIBLE_PLUGIN_DIR . 'includes/class-feasible-snippet.php';
require_once FEASIBLE_PLUGIN_DIR . 'includes/class-feasible-dashboard.php';
require_once FEASIBLE_PLUGIN_DIR . 'includes/class-feasible-admin.php';
require_once FEASIBLE_PLUGIN_DIR . 'includes/class-feasible-site-health.php';
require_once FEASIBLE_PLUGIN_DIR . 'includes/class-feasible-plugin.php';

register_activation_hook( __FILE__, array( 'Feasible_Plugin', 'activate' ) );
register_deactivation_hook( __FILE__, array( 'Feasible_Plugin', 'deactivate' ) );

Feasible_Plugin::boot();
