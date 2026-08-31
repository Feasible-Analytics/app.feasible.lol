<?php
//
// class-feasible-site-health.php
// Saying out loud, in the place people already look, when the measurement is broken.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Feasible_Site_Health reports configuration problems to Site Health.
 *
 * Analytics fails silently by nature: a broken installation looks exactly like
 * a quiet week. Site Health is the one screen an administrator opens when
 * something feels wrong, so the plugin's own problems are stated there rather
 * than only on a settings page nobody visits once it is set up.
 *
 * The test makes no outbound request. It reads what the proxy already recorded,
 * because a health page that fetches a remote script every time it loads is a
 * health page that times out.
 */
class Feasible_Site_Health {

	/**
	 * register hooks the test and the debug section in.
	 *
	 * @return void
	 */
	public static function register() {
		add_filter( 'site_status_tests', array( __CLASS__, 'add_test' ) );
		add_filter( 'debug_information', array( __CLASS__, 'add_debug_information' ) );
	}

	/**
	 * add_test registers a direct test.
	 *
	 * @param array $tests The registered tests.
	 * @return array
	 */
	public static function add_test( $tests ) {
		$tests['direct']['feasible_analytics'] = array(
			'label' => __( 'feasible.lol analytics', 'feasible-analytics' ),
			'test'  => array( __CLASS__, 'run_test' ),
		);

		return $tests;
	}

	/**
	 * run_test reports the first thing standing between this site and a number
	 * on the dashboard.
	 *
	 * @return array
	 */
	public static function run_test() {
		$result = array(
			'label'       => __( 'feasible.lol is measuring this site', 'feasible-analytics' ),
			'status'      => 'good',
			'badge'       => array(
				'label' => __( 'Analytics', 'feasible-analytics' ),
				'color' => 'blue',
			),
			'description' => '<p>' . esc_html__( 'The tracking snippet is being added to the front end and the proxy has not reported a failure.', 'feasible-analytics' ) . '</p>',
			'actions'     => '',
			'test'        => 'feasible_analytics',
		);

		$settings = Feasible_Settings::all();
		$problems = array();

		if ( '' === trim( (string) $settings['domain'] ) ) {
			$problems[] = __( 'No site domain is set, so no event can be attributed to a site.', 'feasible-analytics' );
		}

		if ( empty( $settings['inject_enabled'] ) ) {
			$problems[] = __( 'The snippet is not being added to the front end. If you paste it into your theme yourself this is fine; otherwise nothing is being measured.', 'feasible-analytics' );
		}

		$error = Feasible_Settings::last_error();

		if ( ! empty( $error['detail'] ) ) {
			$problems[] = $error['detail'];
		}

		if ( empty( $problems ) ) {
			return $result;
		}

		$result['status'] = 'critical';
		$result['label']  = __( 'feasible.lol is not measuring this site correctly', 'feasible-analytics' );

		$list = '';

		foreach ( $problems as $problem ) {
			$list .= '<li>' . esc_html( $problem ) . '</li>';
		}

		$result['description'] = '<ul>' . $list . '</ul>';
		$result['actions']     = sprintf(
			'<p><a href="%s">%s</a></p>',
			esc_url( admin_url( 'admin.php?page=' . Feasible_Admin::PAGE ) ),
			esc_html__( 'Open the feasible.lol settings', 'feasible-analytics' )
		);

		return $result;
	}

	/**
	 * add_debug_information puts the live configuration in the debug report.
	 *
	 * The two route URLs are the first thing anybody needs when a site is not
	 * reporting, and pasting a support report is easier than describing them.
	 *
	 * @param array $info The debug sections.
	 * @return array
	 */
	public static function add_debug_information( $info ) {
		$settings = Feasible_Settings::all();

		$info['feasible-analytics'] = array(
			'label'  => __( 'feasible.lol Analytics', 'feasible-analytics' ),
			'fields' => array(
				'version'      => array(
					'label' => __( 'Plugin version', 'feasible-analytics' ),
					'value' => FEASIBLE_VERSION,
				),
				'domain'       => array(
					'label' => __( 'Site domain', 'feasible-analytics' ),
					'value' => '' !== $settings['domain'] ? $settings['domain'] : __( 'not set', 'feasible-analytics' ),
				),
				'host'         => array(
					'label' => __( 'Host', 'feasible-analytics' ),
					'value' => Feasible_Settings::host(),
				),
				'proxy'        => array(
					'label' => __( 'Proxy', 'feasible-analytics' ),
					'value' => Feasible_Settings::proxy_enabled() ? __( 'on', 'feasible-analytics' ) : __( 'off', 'feasible-analytics' ),
				),
				'routing_mode' => array(
					'label' => __( 'Routing mode', 'feasible-analytics' ),
					'value' => Feasible_Paths::mode(),
				),
				'script_route' => array(
					'label' => __( 'Script route', 'feasible-analytics' ),
					'value' => Feasible_Paths::script_url(),
				),
				'event_route'  => array(
					'label' => __( 'Event route', 'feasible-analytics' ),
					'value' => Feasible_Paths::event_url(),
				),
			),
		);

		return $info;
	}
}
