<?php
//
// class-feasible-admin.php
// The settings screen: every field, and the rotate-paths action beside them.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Feasible_Admin builds the whole administration side of the plugin.
 *
 * Everything goes through the Settings API rather than a hand-rolled form, so
 * the nonce, the capability check and the save are WordPress's own and there is
 * exactly one function — the sanitiser — that decides what may be stored.
 */
class Feasible_Admin {

	// The settings group the form posts under.
	const GROUP = 'feasible_settings_group';

	// The menu slug of the settings screen.
	const PAGE = 'feasible-analytics';

	// The menu slug of the framed dashboard.
	const DASHBOARD_PAGE = 'feasible-analytics-dashboard';

	// The admin-post action behind the rotate button.
	const ROTATE_ACTION = 'feasible_rotate_paths';

	/**
	 * register wires the admin screens up.
	 *
	 * @return void
	 */
	public static function register() {
		add_action( 'admin_menu', array( __CLASS__, 'add_menu' ) );
		add_action( 'admin_init', array( __CLASS__, 'register_settings' ) );
		add_action( 'admin_post_' . self::ROTATE_ACTION, array( __CLASS__, 'handle_rotate' ) );
		add_filter( 'plugin_action_links_' . plugin_basename( FEASIBLE_PLUGIN_FILE ), array( __CLASS__, 'action_links' ) );
	}

	/**
	 * add_menu creates the top-level menu and its two pages.
	 *
	 * A top-level menu rather than an entry under Settings, because the second
	 * page is a dashboard somebody opens every day and nobody looks for a
	 * dashboard under Settings.
	 *
	 * @return void
	 */
	public static function add_menu() {
		add_menu_page(
			__( 'feasible.lol Analytics', 'feasible-analytics' ),
			__( 'feasible.lol', 'feasible-analytics' ),
			'manage_options',
			self::PAGE,
			array( __CLASS__, 'render_settings' ),
			'dashicons-chart-bar',
			80
		);

		add_submenu_page(
			self::PAGE,
			__( 'feasible.lol Settings', 'feasible-analytics' ),
			__( 'Settings', 'feasible-analytics' ),
			'manage_options',
			self::PAGE,
			array( __CLASS__, 'render_settings' )
		);

		add_submenu_page(
			self::PAGE,
			__( 'feasible.lol Dashboard', 'feasible-analytics' ),
			__( 'Dashboard', 'feasible-analytics' ),
			'manage_options',
			self::DASHBOARD_PAGE,
			array( 'Feasible_Dashboard', 'render' )
		);
	}

	/**
	 * action_links puts a settings link on the plugins screen.
	 *
	 * @param array $links Existing action links.
	 * @return array
	 */
	public static function action_links( $links ) {
		$settings = '<a href="' . esc_url( admin_url( 'admin.php?page=' . self::PAGE ) ) . '">' . esc_html__( 'Settings', 'feasible-analytics' ) . '</a>';

		array_unshift( $links, $settings );

		return $links;
	}

	/**
	 * register_settings declares the option, its sections and its fields.
	 *
	 * @return void
	 */
	public static function register_settings() {
		register_setting(
			self::GROUP,
			Feasible_Settings::OPTION,
			array(
				'type'              => 'array',
				'sanitize_callback' => array( 'Feasible_Settings', 'sanitise' ),
				'default'           => Feasible_Settings::defaults(),
			)
		);

		add_settings_section(
			'feasible_connection',
			__( 'Connection', 'feasible-analytics' ),
			array( __CLASS__, 'section_connection' ),
			self::PAGE
		);

		self::add_field( 'domain', __( 'Site domain', 'feasible-analytics' ), 'feasible_connection', 'render_domain' );
		self::add_field( 'host', __( 'feasible.lol host', 'feasible-analytics' ), 'feasible_connection', 'render_host' );
		self::add_field( 'script_token', __( 'Per-site script token', 'feasible-analytics' ), 'feasible_connection', 'render_token' );

		add_settings_section(
			'feasible_proxy',
			__( 'Proxy', 'feasible-analytics' ),
			array( __CLASS__, 'section_proxy' ),
			self::PAGE
		);

		self::add_field( 'proxy_enabled', __( 'Serve from this domain', 'feasible-analytics' ), 'feasible_proxy', 'render_proxy_enabled' );
		self::add_field( 'proxy_routes', __( 'Live routes', 'feasible-analytics' ), 'feasible_proxy', 'render_routes' );

		add_settings_section(
			'feasible_snippet',
			__( 'Snippet', 'feasible-analytics' ),
			array( __CLASS__, 'section_snippet' ),
			self::PAGE
		);

		self::add_field( 'inject_enabled', __( 'Add the snippet', 'feasible-analytics' ), 'feasible_snippet', 'render_inject_enabled' );
		self::add_field( 'inject_location', __( 'Where', 'feasible-analytics' ), 'feasible_snippet', 'render_inject_location' );
		self::add_field( 'exclude_logged_in', __( 'Logged-in users', 'feasible-analytics' ), 'feasible_snippet', 'render_exclude_logged_in' );
		self::add_field( 'excluded_roles', __( 'Excluded roles', 'feasible-analytics' ), 'feasible_snippet', 'render_excluded_roles' );
		self::add_field( 'file_types', __( 'Download file types', 'feasible-analytics' ), 'feasible_snippet', 'render_file_types' );

		add_settings_section(
			'feasible_measurements',
			__( 'Measurements', 'feasible-analytics' ),
			array( __CLASS__, 'section_measurements' ),
			self::PAGE
		);

		self::add_field( 'track_search', __( 'Site search', 'feasible-analytics' ), 'feasible_measurements', 'render_track_search' );
		self::add_field( 'track_404', __( '404 errors', 'feasible-analytics' ), 'feasible_measurements', 'render_track_404' );
		self::add_field( 'native_switches', __( 'Measured by the script', 'feasible-analytics' ), 'feasible_measurements', 'render_native_switches' );

		add_settings_section(
			'feasible_dashboard',
			__( 'Dashboard', 'feasible-analytics' ),
			array( __CLASS__, 'section_dashboard' ),
			self::PAGE
		);

		self::add_field( 'shared_dashboard_url', __( 'Shared dashboard link', 'feasible-analytics' ), 'feasible_dashboard', 'render_shared_url' );
	}

	/**
	 * add_field is a shorthand for the five arguments every field repeats.
	 *
	 * @param string $id       Field id.
	 * @param string $title    Field label.
	 * @param string $section  Section id.
	 * @param string $callback Method on this class that renders the control.
	 * @return void
	 */
	private static function add_field( $id, $title, $section, $callback ) {
		add_settings_field( 'feasible_' . $id, $title, array( __CLASS__, $callback ), self::PAGE, $section );
	}

	/**
	 * name builds the form field name for one setting.
	 *
	 * @param string $key Setting key.
	 * @return string
	 */
	private static function name( $key ) {
		return Feasible_Settings::OPTION . '[' . $key . ']';
	}

	/**
	 * section_connection explains what the connection fields are for.
	 *
	 * @return void
	 */
	public static function section_connection() {
		echo '<p>' . esc_html__( 'The domain has to match the site exactly as you registered it in feasible.lol. Everything else on this screen depends on it.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * section_proxy explains what the proxy does and what it does not do.
	 *
	 * @return void
	 */
	public static function section_proxy() {
		echo '<p>' . esc_html__( 'With the proxy on, the tracking script and the event endpoint are served from paths on this site rather than from feasible.lol, and the visitor\'s real address is forwarded upstream so the numbers stay correct.', 'feasible-analytics' ) . '</p>';
		echo '<p>' . esc_html__( 'The paths are random and specific to this site. That means a filter list naming one path costs one site rather than every site using this plugin. It raises the cost of being blocked; it does not end it.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * section_snippet introduces the injection controls.
	 *
	 * @return void
	 */
	public static function section_snippet() {
		echo '<p>' . esc_html__( 'The snippet is added to the front end of the site. Turn it off if you paste the script into your theme yourself.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * section_measurements draws the line between what this plugin measures and
	 * what the browser script measures on its own.
	 *
	 * @return void
	 */
	public static function section_measurements() {
		echo '<p>' . esc_html__( 'Site searches and 404s are measured by this plugin, because only WordPress knows a page was a 404 and how many results a search returned.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * section_dashboard explains the framed dashboard.
	 *
	 * @return void
	 */
	public static function section_dashboard() {
		echo '<p>' . esc_html__( 'Create a shared link for this site in feasible.lol and paste it here to see the dashboard without leaving WordPress.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * render_domain draws the site domain field.
	 *
	 * @return void
	 */
	public static function render_domain() {
		$value = (string) Feasible_Settings::get( 'domain' );
		$guess = Feasible_Settings::sanitise_domain( home_url() );

		echo '<input type="text" class="regular-text" name="' . esc_attr( self::name( 'domain' ) ) . '" value="' . esc_attr( $value ) . '" placeholder="' . esc_attr( $guess ) . '" />';
		echo '<p class="description">';
		printf(
			/* translators: %s: the domain of this WordPress site. */
			esc_html__( 'This site looks like %s.', 'feasible-analytics' ),
			'<code>' . esc_html( $guess ) . '</code>'
		);
		echo '</p>';
	}

	/**
	 * render_host draws the service host field.
	 *
	 * @return void
	 */
	public static function render_host() {
		echo '<input type="url" class="regular-text" name="' . esc_attr( self::name( 'host' ) ) . '" value="' . esc_attr( Feasible_Settings::host() ) . '" />';
		echo '<p class="description">' . esc_html__( 'Leave this alone unless you run feasible.lol yourself.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * render_token draws the per-site script token field.
	 *
	 * @return void
	 */
	public static function render_token() {
		$value = (string) Feasible_Settings::get( 'script_token' );

		echo '<input type="text" class="regular-text code" name="' . esc_attr( self::name( 'script_token' ) ) . '" value="' . esc_attr( $value ) . '" placeholder="' . esc_attr__( 'optional', 'feasible-analytics' ) . '" />';
		echo '<p class="description">' . esc_html__( 'Optional. If your site settings in feasible.lol show a script path like /js/fs-abcdefghijklmnop.js, paste it here and the proxy will fetch that one instead of the shared script.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * render_proxy_enabled draws the proxy switch.
	 *
	 * @return void
	 */
	public static function render_proxy_enabled() {
		self::checkbox( 'proxy_enabled', __( 'Serve the script and the event endpoint from this domain', 'feasible-analytics' ) );
		echo '<p class="description">' . esc_html__( 'With this off, the snippet loads the script from feasible.lol directly. That works; it is simply easier to block.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * render_routes shows the live URLs, the routing mode and the rotate button.
	 *
	 * The mode is stated rather than hidden because it changes the shape of
	 * every URL on this screen, and somebody comparing a support article to
	 * their own site needs to know which of the two they are looking at.
	 *
	 * @return void
	 */
	public static function render_routes() {
		$mode = Feasible_Paths::mode();

		echo '<p><strong>' . esc_html__( 'Script', 'feasible-analytics' ) . '</strong><br /><code>' . esc_html( Feasible_Paths::script_url() ) . '</code></p>';
		echo '<p><strong>' . esc_html__( 'Events', 'feasible-analytics' ) . '</strong><br /><code>' . esc_html( Feasible_Paths::event_url() ) . '</code></p>';

		echo '<p><strong>' . esc_html__( 'Routing mode', 'feasible-analytics' ) . ':</strong> ';

		if ( Feasible_Paths::MODE_REWRITE === $mode ) {
			echo esc_html__( 'real paths, using WordPress rewrite rules.', 'feasible-analytics' );
		} else {
			echo esc_html__( 'query string on the site index.', 'feasible-analytics' );
			echo '</p><p class="description">';
			echo esc_html__( 'This site uses plain permalinks, so nothing rewrites a bare path to WordPress and a path route would be a 404 before any PHP runs. Switch to any other permalink setting and these become real paths, which look less like tracking.', 'feasible-analytics' );
		}

		echo '</p>';

		echo '<p><a class="button" href="' . esc_url( wp_nonce_url( admin_url( 'admin-post.php?action=' . self::ROTATE_ACTION ), self::ROTATE_ACTION ) ) . '">' . esc_html__( 'Rotate paths', 'feasible-analytics' ) . '</a></p>';
		echo '<p class="description">' . esc_html__( 'Rotating generates new random paths and rewrites the rules behind them. It is the remedy when a path ends up on a filter list. Anything caching the old script URL will fetch the new one on its next page load.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * render_inject_enabled draws the injection switch.
	 *
	 * @return void
	 */
	public static function render_inject_enabled() {
		self::checkbox( 'inject_enabled', __( 'Add the tracking snippet to the front end', 'feasible-analytics' ) );
	}

	/**
	 * render_inject_location draws the head or footer choice.
	 *
	 * @return void
	 */
	public static function render_inject_location() {
		$current = (string) Feasible_Settings::get( 'inject_location' );

		foreach ( array(
			'head'   => __( 'In the head (recommended)', 'feasible-analytics' ),
			'footer' => __( 'In the footer', 'feasible-analytics' ),
		) as $value => $label ) {
			echo '<label style="display:block;margin-bottom:4px;"><input type="radio" name="' . esc_attr( self::name( 'inject_location' ) ) . '" value="' . esc_attr( $value ) . '" ' . checked( $current, $value, false ) . ' /> ' . esc_html( $label ) . '</label>';
		}

		echo '<p class="description">' . esc_html__( 'The script is deferred either way, so the head costs nothing and catches visitors who leave before the footer renders.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * render_exclude_logged_in draws the logged-in exclusion.
	 *
	 * @return void
	 */
	public static function render_exclude_logged_in() {
		self::checkbox( 'exclude_logged_in', __( 'Do not measure anyone who is signed in to WordPress', 'feasible-analytics' ) );
	}

	/**
	 * render_excluded_roles draws one checkbox per role on the site.
	 *
	 * @return void
	 */
	public static function render_excluded_roles() {
		$selected = (array) Feasible_Settings::get( 'excluded_roles' );
		$roles    = Feasible_Settings::roles();

		if ( empty( $roles ) ) {
			echo '<p class="description">' . esc_html__( 'No roles were found on this site.', 'feasible-analytics' ) . '</p>';

			return;
		}

		foreach ( $roles as $slug => $label ) {
			echo '<label style="display:block;margin-bottom:4px;"><input type="checkbox" name="' . esc_attr( self::name( 'excluded_roles' ) ) . '[]" value="' . esc_attr( $slug ) . '" ' . checked( in_array( $slug, $selected, true ), true, false ) . ' /> ' . esc_html( translate_user_role( $label ) ) . '</label>';
		}

		echo '<p class="description">' . esc_html__( 'Useful when the whole editorial team should be invisible but subscribers should still count.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * render_file_types draws the download extension override.
	 *
	 * @return void
	 */
	public static function render_file_types() {
		$value = (string) Feasible_Settings::get( 'file_types' );

		echo '<input type="text" class="regular-text code" name="' . esc_attr( self::name( 'file_types' ) ) . '" value="' . esc_attr( $value ) . '" placeholder="pdf,zip,docx" />';
		echo '<p class="description">' . esc_html__( 'Extensions, separated by commas. Leave it empty to use the script\'s own list. Setting this replaces that list rather than adding to it.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * render_track_search draws the site search switch.
	 *
	 * @return void
	 */
	public static function render_track_search() {
		self::checkbox( 'track_search', __( 'Record what people search for on this site', 'feasible-analytics' ) );
		echo '<p class="description">' . esc_html__( 'Sends a Search event with the search term, the number of results, and whether anything was found at all. Terms are lowercased and their spacing collapsed so the same question asked three ways is one row rather than three.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * render_track_404 draws the 404 switch.
	 *
	 * @return void
	 */
	public static function render_track_404() {
		self::checkbox( 'track_404', __( 'Record pages that were not found', 'feasible-analytics' ) );
		echo '<p class="description">' . esc_html__( 'Sends a 404 event with the path and the referrer, which is what turns a broken link into something you can find and fix.', 'feasible-analytics' ) . '</p>';
	}

	/**
	 * render_native_switches draws the three switches the proxy enforces.
	 *
	 * They are grouped and labelled honestly: the browser script measures all
	 * three whether or not this plugin exists, so the only place a WordPress
	 * setting can stop one is on the way past the proxy.
	 *
	 * @return void
	 */
	public static function render_native_switches() {
		self::checkbox( 'track_outbound', __( 'Outbound link clicks', 'feasible-analytics' ) );
		self::checkbox( 'track_downloads', __( 'File downloads', 'feasible-analytics' ) );
		self::checkbox( 'track_forms', __( 'Form submissions', 'feasible-analytics' ) );

		echo '<p class="description">';

		if ( Feasible_Settings::proxy_enabled() ) {
			echo esc_html__( 'These three are measured by the browser script itself and cannot be switched off inside it. Turning one off here makes the proxy recognise the event and answer the browser without forwarding it, so nothing is recorded.', 'feasible-analytics' );
		} else {
			echo '<strong>' . esc_html__( 'These three switches do nothing while the proxy is off.', 'feasible-analytics' ) . '</strong> ';
			echo esc_html__( 'The browser script measures them regardless, and with no proxy in the way the events go straight to feasible.lol. Turn the proxy on if you need to stop one.', 'feasible-analytics' );
		}

		echo '</p>';
	}

	/**
	 * render_shared_url draws the shared dashboard link field.
	 *
	 * @return void
	 */
	public static function render_shared_url() {
		$value = (string) Feasible_Settings::get( 'shared_dashboard_url' );

		echo '<input type="url" class="large-text code" name="' . esc_attr( self::name( 'shared_dashboard_url' ) ) . '" value="' . esc_attr( $value ) . '" placeholder="' . esc_attr( Feasible_Settings::host() . '/share/example.com' ) . '" />';
		echo '<p class="description">';
		printf(
			/* translators: %s: the configured feasible.lol host. */
			esc_html__( 'Only a /share/ link on %s is accepted, because anything else would be framed next to your admin session.', 'feasible-analytics' ),
			'<code>' . esc_html( Feasible_Settings::host() ) . '</code>'
		);
		echo '</p>';
	}

	/**
	 * checkbox draws one boolean setting.
	 *
	 * @param string $key   Setting key.
	 * @param string $label Label beside the box.
	 * @return void
	 */
	private static function checkbox( $key, $label ) {
		$checked = (bool) Feasible_Settings::get( $key );

		echo '<label style="display:block;margin-bottom:4px;"><input type="checkbox" name="' . esc_attr( self::name( $key ) ) . '" value="1" ' . checked( $checked, true, false ) . ' /> ' . esc_html( $label ) . '</label>';
	}

	/**
	 * render_settings draws the settings page.
	 *
	 * @return void
	 */
	public static function render_settings() {
		if ( ! current_user_can( 'manage_options' ) ) {
			wp_die( esc_html__( 'You do not have permission to view this page.', 'feasible-analytics' ) );
		}

		echo '<div class="wrap">';
		echo '<h1>' . esc_html__( 'feasible.lol Analytics', 'feasible-analytics' ) . '</h1>';

		self::rotation_notice();
		settings_errors( Feasible_Settings::OPTION );
		settings_errors( 'general' );
		self::error_notice();

		echo '<form method="post" action="' . esc_url( admin_url( 'options.php' ) ) . '">';
		settings_fields( self::GROUP );
		do_settings_sections( self::PAGE );
		submit_button();
		echo '</form>';

		self::snippet_preview();

		echo '</div>';
	}

	/**
	 * rotation_notice confirms a rotation after the redirect back.
	 *
	 * @return void
	 */
	private static function rotation_notice() {
		// phpcs:ignore WordPress.Security.NonceVerification.Recommended
		if ( empty( $_GET['feasible_rotated'] ) ) {
			return;
		}

		echo '<div class="notice notice-success is-dismissible"><p>' . esc_html__( 'New paths generated and the rewrite rules rebuilt.', 'feasible-analytics' ) . '</p></div>';
	}

	/**
	 * error_notice shows the last thing that went wrong in the proxy.
	 *
	 * The proxy answers machines, so a failure there is invisible unless it is
	 * repeated to a person somewhere. This is that somewhere.
	 *
	 * @return void
	 */
	private static function error_notice() {
		$error = Feasible_Settings::last_error();

		if ( empty( $error['detail'] ) ) {
			return;
		}

		$when = isset( $error['time'] ) ? (int) $error['time'] : 0;

		echo '<div class="notice notice-error"><p><strong>' . esc_html__( 'The last request through the proxy failed.', 'feasible-analytics' ) . '</strong></p>';
		echo '<p><code>' . esc_html( $error['detail'] ) . '</code></p>';

		if ( $when > 0 ) {
			echo '<p>' . esc_html(
				sprintf(
					/* translators: %s: a human-readable interval, for example "5 minutes". */
					__( '%s ago.', 'feasible-analytics' ),
					human_time_diff( $when, time() )
				)
			) . '</p>';
		}

		echo '</div>';
	}

	/**
	 * snippet_preview shows the exact tag the front end will render.
	 *
	 * Somebody debugging a blocked script needs the real tag, not a
	 * description of it, and reading it here is faster than viewing source on
	 * a page that may be cached.
	 *
	 * @return void
	 */
	private static function snippet_preview() {
		$tag = '<script defer';

		foreach ( Feasible_Snippet::attributes() as $name => $value ) {
			$tag .= ' ' . $name . '="' . $value . '"';
		}

		$tag .= ' src="' . Feasible_Snippet::script_src() . '"></script>';

		echo '<h2>' . esc_html__( 'The snippet on your pages', 'feasible-analytics' ) . '</h2>';
		echo '<textarea class="large-text code" rows="3" readonly>' . esc_textarea( $tag ) . '</textarea>';

		if ( ! Feasible_Settings::get( 'inject_enabled' ) ) {
			echo '<p class="description">' . esc_html__( 'Injection is switched off, so this is the tag to paste into your theme yourself.', 'feasible-analytics' ) . '</p>';
		}
	}

	/**
	 * handle_rotate regenerates the paths and sends the administrator back.
	 *
	 * @return void
	 */
	public static function handle_rotate() {
		if ( ! current_user_can( 'manage_options' ) ) {
			wp_die( esc_html__( 'You do not have permission to rotate the paths.', 'feasible-analytics' ) );
		}

		check_admin_referer( self::ROTATE_ACTION );

		Feasible_Paths::rotate();

		wp_safe_redirect(
			add_query_arg(
				array(
					'page'             => self::PAGE,
					'feasible_rotated' => '1',
				),
				admin_url( 'admin.php' )
			)
		);

		exit;
	}
}
