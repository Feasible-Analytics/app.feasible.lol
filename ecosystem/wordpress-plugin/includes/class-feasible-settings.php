<?php
//
// class-feasible-settings.php
// Every stored option, its default, and the one place input is sanitised.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Feasible_Settings owns the plugin's configuration.
 *
 * Everything is kept in one option array rather than a row per field, because
 * the proxy reads the whole configuration on every event request and a single
 * autoloaded row is one cache lookup instead of a dozen.
 */
class Feasible_Settings {

	// The option holding the settings array.
	const OPTION = 'feasible_settings';

	// The option holding the last thing that went wrong in the proxy. It is
	// separate from the settings so that recording a failure on the front end
	// can never race a settings save and lose a field.
	const ERROR_OPTION = 'feasible_last_error';

	// Where the hosted service lives. A self-hosted install overrides it.
	const DEFAULT_HOST = 'https://app.feasible.lol';

	// The shape of a per-site script token as the server derives it: sixteen
	// lowercase base32 characters. Validating it here means a mistyped token
	// is caught on the settings screen rather than as a silent 404 for the
	// script on every page of the site.
	const TOKEN_PATTERN = '/^[a-z2-7]{16}$/';

	/**
	 * defaults returns the configuration a fresh install starts with.
	 *
	 * The three native measurements default to on because the browser script
	 * measures them whether or not this plugin exists; presenting them as off
	 * by default would describe a site that is not the one being served.
	 *
	 * @return array
	 */
	public static function defaults() {
		return array(
			'domain'               => '',
			'host'                 => self::DEFAULT_HOST,
			'script_token'         => '',
			'proxy_enabled'        => true,
			'inject_enabled'       => true,
			'inject_location'      => 'head',
			'exclude_logged_in'    => false,
			'excluded_roles'       => array(),
			'file_types'           => '',
			'track_404'            => true,
			'track_search'         => true,
			'track_outbound'       => true,
			'track_downloads'      => true,
			'track_forms'          => true,
			'shared_dashboard_url' => '',
		);
	}

	/**
	 * all returns the stored settings merged over the defaults.
	 *
	 * Merging on every read rather than on save means a version that adds a
	 * setting does not need an upgrade routine, and a site whose option row was
	 * hand-edited still has every key the rest of the plugin expects.
	 *
	 * @return array
	 */
	public static function all() {
		$stored = get_option( self::OPTION, array() );

		if ( ! is_array( $stored ) ) {
			$stored = array();
		}

		return array_merge( self::defaults(), $stored );
	}

	/**
	 * get reads one setting, falling back to its default.
	 *
	 * @param string $key Setting name.
	 * @return mixed
	 */
	public static function get( $key ) {
		$all = self::all();

		return isset( $all[ $key ] ) ? $all[ $key ] : null;
	}

	/**
	 * host returns the configured service origin with no trailing slash.
	 *
	 * Trailing slashes are stripped on the way out rather than on the way in so
	 * that a customer who pasted one, and a customer who did not, produce the
	 * same upstream URL and therefore the same cache key.
	 *
	 * @return string
	 */
	public static function host() {
		$host = trim( (string) self::get( 'host' ) );

		if ( '' === $host ) {
			$host = self::DEFAULT_HOST;
		}

		return untrailingslashit( $host );
	}

	/**
	 * upstream_script_url is the URL the proxy fetches the browser bundle from.
	 *
	 * A configured token selects the per-site path, whose configuration is
	 * baked in by the server. Without one the shared path is used and the
	 * snippet's data attributes carry the configuration instead.
	 *
	 * @return string
	 */
	public static function upstream_script_url() {
		$token = (string) self::get( 'script_token' );

		if ( '' !== $token ) {
			return self::host() . '/js/fs-' . $token . '.js';
		}

		return self::host() . '/js/script.js';
	}

	/**
	 * upstream_event_url is the ingest endpoint the proxy forwards events to.
	 *
	 * @return string
	 */
	public static function upstream_event_url() {
		return self::host() . '/api/event';
	}

	/**
	 * proxy_enabled reports whether requests should be served from this domain.
	 *
	 * @return bool
	 */
	public static function proxy_enabled() {
		return (bool) self::get( 'proxy_enabled' );
	}

	/**
	 * sanitise takes whatever the settings form posted and returns a value safe
	 * to store.
	 *
	 * Every field is rebuilt from the defaults rather than merged over the
	 * submitted array, so a hand-crafted POST cannot introduce a key the rest of
	 * the plugin never validates.
	 *
	 * @param mixed $input Raw submitted value.
	 * @return array
	 */
	public static function sanitise( $input ) {
		$current = self::all();
		$clean   = self::defaults();

		if ( ! is_array( $input ) ) {
			return $current;
		}

		$clean['domain']       = self::sanitise_domain( isset( $input['domain'] ) ? $input['domain'] : '' );
		$clean['host']         = self::sanitise_host( isset( $input['host'] ) ? $input['host'] : '' );
		$clean['script_token'] = self::sanitise_token( isset( $input['script_token'] ) ? $input['script_token'] : '' );
		$clean['file_types']   = self::sanitise_file_types( isset( $input['file_types'] ) ? $input['file_types'] : '' );

		foreach ( array( 'proxy_enabled', 'inject_enabled', 'exclude_logged_in', 'track_404', 'track_search', 'track_outbound', 'track_downloads', 'track_forms' ) as $flag ) {
			$clean[ $flag ] = ! empty( $input[ $flag ] );
		}

		$clean['inject_location'] = ( isset( $input['inject_location'] ) && 'footer' === $input['inject_location'] ) ? 'footer' : 'head';
		$clean['excluded_roles']  = self::sanitise_roles( isset( $input['excluded_roles'] ) ? $input['excluded_roles'] : array() );

		$clean['shared_dashboard_url'] = self::sanitise_share_url(
			isset( $input['shared_dashboard_url'] ) ? $input['shared_dashboard_url'] : '',
			$clean['host']
		);

		if ( '' === $clean['domain'] ) {
			add_settings_error(
				self::OPTION,
				'feasible_domain_missing',
				esc_html__( 'No site domain is set, so nothing will be measured. Enter the domain exactly as you registered it in feasible.lol.', 'feasible-analytics' ),
				'error'
			);
		}

		return $clean;
	}

	/**
	 * sanitise_domain reduces whatever was pasted to a bare hostname.
	 *
	 * Customers paste a full URL as often as a hostname, and a site id of
	 * "https://example.com/" matches no site on the server — an error that
	 * shows up only as an empty dashboard days later.
	 *
	 * @param string $value Raw domain.
	 * @return string
	 */
	public static function sanitise_domain( $value ) {
		$value = strtolower( trim( (string) $value ) );

		if ( '' === $value ) {
			return '';
		}

		if ( false !== strpos( $value, '//' ) ) {
			$host  = wp_parse_url( $value, PHP_URL_HOST );
			$value = is_string( $host ) ? $host : $value;
		}

		// Anything after the authority is not part of a site id, and a port is
		// not either — the server registers "example.com", never
		// "example.com:8443/blog".
		$value = preg_replace( '#[/?].*$#', '', $value );
		$value = preg_replace( '#:\d+$#', '', (string) $value );
		$value = trim( (string) $value, '.' );

		return preg_replace( '/[^a-z0-9.\-]/', '', (string) $value );
	}

	/**
	 * sanitise_host validates the service origin.
	 *
	 * Only the scheme and host survive. A path here would be silently appended
	 * to every upstream URL the proxy builds, which turns one mistyped setting
	 * into a site that reports nowhere.
	 *
	 * @param string $value Raw host.
	 * @return string
	 */
	public static function sanitise_host( $value ) {
		$value = trim( (string) $value );

		if ( '' === $value ) {
			return self::DEFAULT_HOST;
		}

		if ( false === strpos( $value, '//' ) ) {
			$value = 'https://' . ltrim( $value, '/' );
		}

		$parts  = wp_parse_url( $value );
		$scheme = isset( $parts['scheme'] ) ? strtolower( $parts['scheme'] ) : 'https';
		$host   = isset( $parts['host'] ) ? strtolower( $parts['host'] ) : '';

		if ( '' === $host || ! in_array( $scheme, array( 'http', 'https' ), true ) ) {
			add_settings_error(
				self::OPTION,
				'feasible_host_invalid',
				esc_html__( 'The feasible.lol host must be a full http:// or https:// address. The previous value has been kept.', 'feasible-analytics' ),
				'error'
			);

			return self::host();
		}

		$port = isset( $parts['port'] ) ? ':' . (int) $parts['port'] : '';

		return $scheme . '://' . $host . $port;
	}

	/**
	 * sanitise_token accepts either a bare token or the filename it appears in.
	 *
	 * The dashboard shows the token inside a path, so that is what gets copied.
	 * Pulling it back out here is the difference between a setting that works
	 * first time and a 404 nobody can explain.
	 *
	 * @param string $value Raw token.
	 * @return string
	 */
	public static function sanitise_token( $value ) {
		$value = strtolower( trim( (string) $value ) );

		if ( '' === $value ) {
			return '';
		}

		if ( preg_match( '#fs-([a-z2-7]{16})(?:\.js)?#', $value, $matches ) ) {
			return $matches[1];
		}

		if ( preg_match( self::TOKEN_PATTERN, $value ) ) {
			return $value;
		}

		add_settings_error(
			self::OPTION,
			'feasible_token_invalid',
			esc_html__( 'The script token was not recognised. Copy it from the snippet on your feasible.lol site settings, or leave the field empty to use the shared script path.', 'feasible-analytics' ),
			'error'
		);

		return '';
	}

	/**
	 * sanitise_file_types normalises the download extension override.
	 *
	 * The browser script matches a comma-separated list with no spaces and no
	 * dots, so anything else is accepted here and rewritten rather than handed
	 * to the script to fail on quietly.
	 *
	 * @param string $value Raw list.
	 * @return string
	 */
	public static function sanitise_file_types( $value ) {
		$value = strtolower( (string) $value );
		$parts = preg_split( '/[\s,]+/', $value );
		$clean = array();

		if ( ! is_array( $parts ) ) {
			return '';
		}

		foreach ( $parts as $part ) {
			$part = ltrim( trim( $part ), '.' );
			$part = preg_replace( '/[^a-z0-9]/', '', $part );

			if ( '' === $part || strlen( $part ) > 10 ) {
				continue;
			}

			$clean[ $part ] = $part;
		}

		// A list long enough to bloat every page of the site is a mistake, not
		// a configuration, and the script only ever needs a handful.
		$clean = array_slice( $clean, 0, 50 );

		return implode( ',', $clean );
	}

	/**
	 * sanitise_roles keeps only role slugs that exist on this site.
	 *
	 * Storing a role that was removed by another plugin would exclude nobody
	 * while still reading as an active exclusion on the settings screen.
	 *
	 * @param mixed $value Raw role list.
	 * @return array
	 */
	public static function sanitise_roles( $value ) {
		if ( ! is_array( $value ) ) {
			return array();
		}

		$known = array_keys( self::roles() );
		$clean = array();

		foreach ( $value as $role ) {
			$role = sanitize_key( (string) $role );

			if ( '' !== $role && in_array( $role, $known, true ) ) {
				$clean[ $role ] = $role;
			}
		}

		return array_values( $clean );
	}

	/**
	 * roles lists the roles a site has, as slug => display name.
	 *
	 * @return array
	 */
	public static function roles() {
		if ( ! function_exists( 'wp_roles' ) ) {
			return array();
		}

		$roles = wp_roles();

		return ( $roles && is_array( $roles->role_names ) ) ? $roles->role_names : array();
	}

	/**
	 * sanitise_share_url keeps a dashboard link only when it is one of ours.
	 *
	 * The value ends up in an iframe inside wp-admin, so an arbitrary URL here
	 * would let anyone who can edit settings frame anything at all next to the
	 * administrator's own session. Requiring the configured host and a /share/
	 * path is the whole check.
	 *
	 * @param string $value Raw URL.
	 * @param string $host  The host the settings are being saved with.
	 * @return string
	 */
	public static function sanitise_share_url( $value, $host ) {
		$value = trim( (string) $value );

		if ( '' === $value ) {
			return '';
		}

		$value = esc_url_raw( $value );

		if ( '' !== $value && Feasible_Dashboard::is_valid_share_url( $value, $host ) ) {
			return $value;
		}

		add_settings_error(
			self::OPTION,
			'feasible_share_url_invalid',
			esc_html__( 'The shared dashboard link has to be a /share/ link on the feasible.lol host configured above, so it has been discarded.', 'feasible-analytics' ),
			'error'
		);

		return '';
	}

	/**
	 * record_error stores the last problem the proxy hit.
	 *
	 * The proxy answers a machine, and a machine does not read English, so the
	 * reason is kept here for the settings screen and the site health panel to
	 * show a person. Autoloading is off because only wp-admin reads it.
	 *
	 * @param string $route  Which route failed.
	 * @param string $reason A short machine-readable reason.
	 * @param string $detail A longer explanation, already translated.
	 * @return void
	 */
	public static function record_error( $route, $reason, $detail = '' ) {
		update_option(
			self::ERROR_OPTION,
			array(
				'route'  => (string) $route,
				'reason' => (string) $reason,
				'detail' => (string) $detail,
				'time'   => time(),
			),
			false
		);
	}

	/**
	 * clear_error forgets a recorded failure once a request has succeeded.
	 *
	 * @return void
	 */
	public static function clear_error() {
		if ( false !== get_option( self::ERROR_OPTION, false ) ) {
			delete_option( self::ERROR_OPTION );
		}
	}

	/**
	 * last_error returns the recorded failure, or an empty array.
	 *
	 * @return array
	 */
	public static function last_error() {
		$error = get_option( self::ERROR_OPTION, array() );

		return is_array( $error ) ? $error : array();
	}
}
