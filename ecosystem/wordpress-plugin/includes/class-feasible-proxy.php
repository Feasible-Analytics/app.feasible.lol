<?php
//
// class-feasible-proxy.php
// Serving the tracker script and the event endpoint from the site's own domain.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Feasible_Proxy answers the two randomised routes on this site.
 *
 * Both routes are dispatched from a single query variable named after the
 * generated namespace, whose value is the segment being asked for. That is what
 * lets the same dispatcher serve both routing modes: with pretty permalinks a
 * rewrite rule turns a real path into that variable, and with plain permalinks
 * the URL carries it directly. One code path means the two modes cannot drift
 * apart, and the mode a site is using is a display detail rather than a fork.
 *
 * The rules are registered whether or not the proxy is switched on, so that
 * toggling the setting never needs a rewrite flush and a site that turns the
 * proxy off still gets a readable answer on the old routes instead of a 404
 * that looks like the plugin is broken.
 */
class Feasible_Proxy {

	// The largest event body that will be forwarded. A real event is a few
	// hundred bytes; anything approaching this is a mistake or an attempt to
	// use the site as a relay, and both deserve the same refusal.
	const MAX_BODY_BYTES = 65536;

	// How long a fetched copy of the bundle is served without asking upstream
	// again. Ten minutes keeps a tracker fix moving quickly without turning
	// every pageview on a busy site into an outbound request.
	const SCRIPT_TTL = 600;

	// How long the last known good copy is kept as a fallback. Serving a
	// slightly old script through an upstream outage is strictly better than
	// serving nothing, and the response says which one it was.
	const SCRIPT_FALLBACK_TTL = 86400;

	// How long the browser may keep the script before revalidating. The ETag
	// makes that revalidation a 304, so the cost of an hour is one conditional
	// request rather than a download.
	const BROWSER_CACHE_SECONDS = 3600;

	// The header the proxy uses to explain itself. Every refusal carries one,
	// because a proxy that answers a beacon has no other way to be heard.
	const ERROR_HEADER = 'x-feasible-error';

	/**
	 * register wires the proxy into WordPress.
	 *
	 * @return void
	 */
	public static function register() {
		add_action( 'init', array( __CLASS__, 'add_rewrite_rules' ) );
		add_filter( 'query_vars', array( __CLASS__, 'add_query_var' ) );
		add_action( 'parse_request', array( __CLASS__, 'dispatch' ) );
	}

	/**
	 * add_rewrite_rules maps the two real paths onto the dispatch variable.
	 *
	 * The segments are alphanumeric by construction, so they go into the
	 * pattern unescaped; Feasible_Paths refuses anything else precisely so that
	 * this line never has to think about it.
	 *
	 * @return void
	 */
	public static function add_rewrite_rules() {
		$paths = Feasible_Paths::all();

		add_rewrite_rule(
			'^' . $paths['namespace'] . '/' . $paths['script'] . '\.js$',
			'index.php?' . $paths['namespace'] . '=' . $paths['script'],
			'top'
		);

		add_rewrite_rule(
			'^' . $paths['namespace'] . '/' . $paths['event'] . '/?$',
			'index.php?' . $paths['namespace'] . '=' . $paths['event'],
			'top'
		);
	}

	/**
	 * add_query_var makes the namespace a public query variable.
	 *
	 * It has to be public rather than internal because in the plain-permalink
	 * mode the value arrives in the query string, and WordPress only copies
	 * registered public variables out of the request.
	 *
	 * @param array $vars Registered query variables.
	 * @return array
	 */
	public static function add_query_var( $vars ) {
		$vars[] = Feasible_Paths::namespace_segment();

		return $vars;
	}

	/**
	 * dispatch answers a proxy request and stops WordPress going any further.
	 *
	 * It runs on parse_request rather than template_redirect so that a request
	 * for a three-kilobyte script never triggers the main query, a theme load
	 * or a canonical redirect. Anything that is not one of the two segments is
	 * returned to WordPress untouched.
	 *
	 * @param WP $wp The WordPress environment object.
	 * @return void
	 */
	public static function dispatch( $wp ) {
		$paths = Feasible_Paths::all();
		$name  = $paths['namespace'];

		if ( ! isset( $wp->query_vars[ $name ] ) || ! is_string( $wp->query_vars[ $name ] ) ) {
			return;
		}

		$segment = $wp->query_vars[ $name ];

		if ( hash_equals( $paths['script'], $segment ) ) {
			self::serve_script();
		}

		if ( hash_equals( $paths['event'], $segment ) ) {
			self::serve_event();
		}
	}

	/**
	 * serve_script answers the script route and exits.
	 *
	 * @return void
	 */
	private static function serve_script() {
		header( 'Content-Type: application/javascript; charset=utf-8' );
		header( 'X-Content-Type-Options: nosniff' );

		if ( ! Feasible_Settings::proxy_enabled() ) {
			self::refuse(
				503,
				'proxy-disabled',
				'/* feasible.lol: the proxy is switched off in WordPress, so this path serves nothing. */',
				__( 'The proxy is switched off, so the script and event routes on this domain answer nothing.', 'feasible-analytics' )
			);
		}

		$url    = Feasible_Settings::upstream_script_url();
		$bundle = self::fetch_script( $url );

		if ( false === $bundle ) {
			self::refuse(
				502,
				'script-fetch-failed',
				'/* feasible.lol: the tracking script could not be fetched from ' . str_replace( '*/', '', $url ) . ' */',
				sprintf(
					/* translators: %s: the upstream script URL. */
					__( 'The tracking script could not be fetched from %s, so no page on this site is being measured.', 'feasible-analytics' ),
					$url
				)
			);
		}

		$etag = '"' . md5( $bundle['body'] ) . '"';

		header( 'Cache-Control: public, max-age=' . self::BROWSER_CACHE_SECONDS );
		header( 'ETag: ' . $etag );

		if ( $bundle['stale'] ) {
			// A stale copy is still a working tracker, but the customer is
			// entitled to know that the upstream fetch is failing.
			self::send_header( self::ERROR_HEADER, 'serving-stale-script' );
		}

		if ( self::etag_matches( $etag ) ) {
			status_header( 304 );
			exit;
		}

		status_header( 200 );

		// The body is JavaScript fetched from our own service, and it is
		// written out byte for byte: escaping it would corrupt the bundle.
		echo $bundle['body']; // phpcs:ignore WordPress.Security.EscapeOutput.OutputNotEscaped
		exit;
	}

	/**
	 * etag_matches reports whether the browser already holds this exact bundle.
	 *
	 * The comparison is a substring test because a conditional request may
	 * carry several tags, and some intermediaries rewrite a strong tag into a
	 * weak one on the way through.
	 *
	 * @param string $etag The tag just computed.
	 * @return bool
	 */
	private static function etag_matches( $etag ) {
		if ( ! isset( $_SERVER['HTTP_IF_NONE_MATCH'] ) ) {
			return false;
		}

		$header = trim( (string) wp_unslash( $_SERVER['HTTP_IF_NONE_MATCH'] ) );

		return '' !== $header && false !== strpos( $header, $etag );
	}

	/**
	 * fetch_script returns the bundle, from cache when it can.
	 *
	 * Two caches are kept: a short one that decides how often upstream is
	 * asked, and a long one that exists purely so an upstream outage degrades
	 * to an old script rather than to no analytics at all.
	 *
	 * @param string $url Upstream script URL.
	 * @return array|false array( 'body' => string, 'stale' => bool ), or false.
	 */
	private static function fetch_script( $url ) {
		$key      = 'feasible_js_' . md5( $url );
		$fallback = 'feasible_js_last_' . md5( $url );

		$cached = get_transient( $key );

		if ( is_array( $cached ) && ! empty( $cached['body'] ) ) {
			return array(
				'body'  => $cached['body'],
				'stale' => false,
			);
		}

		$response = wp_remote_get(
			$url,
			array(
				'timeout'     => 10,
				'redirection' => 2,
				'user-agent'  => 'feasible-analytics-wordpress/' . FEASIBLE_VERSION,
			)
		);

		$problem = '';

		if ( is_wp_error( $response ) ) {
			$problem = $response->get_error_message();
		} else {
			$code = (int) wp_remote_retrieve_response_code( $response );
			$body = wp_remote_retrieve_body( $response );

			if ( 200 !== $code ) {
				/* translators: %d: an HTTP status code. */
				$problem = sprintf( __( 'the server answered %d', 'feasible-analytics' ), $code );
			} elseif ( '' === trim( $body ) ) {
				$problem = __( 'the server answered with an empty body', 'feasible-analytics' );
			} else {
				set_transient( $key, array( 'body' => $body ), self::SCRIPT_TTL );
				set_transient( $fallback, array( 'body' => $body ), self::SCRIPT_FALLBACK_TTL );
				Feasible_Settings::clear_error();

				return array(
					'body'  => $body,
					'stale' => false,
				);
			}
		}

		Feasible_Settings::record_error(
			'script',
			'script-fetch-failed',
			sprintf(
				/* translators: 1: the upstream script URL, 2: the reason the fetch failed. */
				__( 'Fetching %1$s failed: %2$s', 'feasible-analytics' ),
				$url,
				$problem
			)
		);

		$stale = get_transient( $fallback );

		if ( is_array( $stale ) && ! empty( $stale['body'] ) ) {
			return array(
				'body'  => $stale['body'],
				'stale' => true,
			);
		}

		return false;
	}

	/**
	 * serve_event forwards one event upstream and relays the answer.
	 *
	 * @return void
	 */
	private static function serve_event() {
		nocache_headers();
		header( 'Content-Type: text/plain; charset=utf-8' );

		$method = isset( $_SERVER['REQUEST_METHOD'] ) ? strtoupper( (string) wp_unslash( $_SERVER['REQUEST_METHOD'] ) ) : 'GET';

		if ( 'OPTIONS' === $method ) {
			header( 'Allow: POST, OPTIONS' );
			status_header( 204 );
			exit;
		}

		if ( 'POST' !== $method ) {
			header( 'Allow: POST, OPTIONS' );
			self::refuse( 405, 'method-not-allowed', __( 'POST an event to this endpoint.', 'feasible-analytics' ), '' );
		}

		$settings = Feasible_Settings::all();

		if ( empty( $settings['proxy_enabled'] ) ) {
			self::refuse(
				503,
				'proxy-disabled',
				__( 'The proxy is switched off in WordPress, so this endpoint forwards nothing.', 'feasible-analytics' ),
				__( 'An event reached the proxy endpoint while the proxy was switched off. Either turn the proxy back on or remove the old snippet.', 'feasible-analytics' )
			);
		}

		if ( '' === trim( (string) $settings['domain'] ) ) {
			self::refuse(
				503,
				'no-site-domain',
				__( 'No site domain is configured in WordPress, so events cannot be attributed to a site.', 'feasible-analytics' ),
				__( 'Events are arriving but no site domain is set, so none of them can be attributed. Set the site domain on the settings screen.', 'feasible-analytics' )
			);
		}

		$body = self::read_body();

		if ( null === $body ) {
			self::refuse(
				413,
				'body-too-large',
				sprintf(
					/* translators: %d: the maximum body size in bytes. */
					__( 'An event body may not be larger than %d bytes.', 'feasible-analytics' ),
					self::MAX_BODY_BYTES
				),
				''
			);
		}

		if ( '' === trim( $body ) ) {
			self::refuse( 400, 'empty-body', __( 'There was no event in the request body.', 'feasible-analytics' ), '' );
		}

		$name = Feasible_Events::name_from_body( $body );

		if ( Feasible_Events::is_suppressed( $name, $settings ) ) {
			// The status stays 202 because the browser script treats anything
			// else as a failure worth reporting, and this is not a failure —
			// it is this site's own choice, said out loud in the header.
			self::send_header( Feasible_Events::DROPPED_HEADER, Feasible_Events::DROPPED_REASON );
			status_header( 202 );
			exit;
		}

		self::forward( $body );
	}

	/**
	 * read_body returns the raw request body, or null when it is too large.
	 *
	 * One byte more than the cap is read so that an oversized body is refused
	 * outright rather than truncated into a payload the server would reject as
	 * malformed for reasons nobody could work out.
	 *
	 * @return string|null
	 */
	private static function read_body() {
		if ( isset( $_SERVER['CONTENT_LENGTH'] ) && (int) $_SERVER['CONTENT_LENGTH'] > self::MAX_BODY_BYTES ) {
			return null;
		}

		// phpcs:ignore WordPress.WP.AlternativeFunctions.file_get_contents_file_get_contents
		$body = file_get_contents( 'php://input', false, null, 0, self::MAX_BODY_BYTES + 1 );

		if ( false === $body ) {
			return '';
		}

		if ( strlen( $body ) > self::MAX_BODY_BYTES ) {
			return null;
		}

		return $body;
	}

	/**
	 * forward posts the event upstream and relays what came back.
	 *
	 * The visitor's address and user agent are the whole reason this route
	 * exists. Without them the ingest server sees the web host, and every
	 * visitor to the site becomes one person sitting in the datacentre — a
	 * failure that produces no error anywhere and is usually noticed weeks
	 * later, if at all.
	 *
	 * @param string $body Raw event body.
	 * @return void
	 */
	private static function forward( $body ) {
		$server = isset( $_SERVER ) && is_array( $_SERVER ) ? $_SERVER : array();
		$ip     = Feasible_Client_IP::resolve( $server );
		$agent  = Feasible_Client_IP::user_agent( $server );

		$headers = array(
			// text/plain is not a nicety: it is what keeps the browser's own
			// request free of a CORS preflight, and the ingest server accepts
			// it deliberately. Forwarding it unchanged keeps the two ends
			// describing the same request.
			'Content-Type'    => 'text/plain',
			'X-Forwarded-For' => $ip,
			'User-Agent'      => $agent,
		);

		if ( isset( $server['HTTP_X_DEBUG_REQUEST'] ) && 'true' === strtolower( trim( (string) $server['HTTP_X_DEBUG_REQUEST'] ) ) ) {
			// Passing the debug flag through is what lets the person who owns
			// this site answer "why was that event not counted" with one curl,
			// instead of asking us to read logs they cannot see.
			$headers['X-Debug-Request'] = 'true';
		}

		if ( '' === $ip ) {
			self::send_header( self::ERROR_HEADER, 'no-client-ip' );
		}

		if ( '' === $agent ) {
			self::send_header( self::ERROR_HEADER, 'no-user-agent' );
		}

		$response = wp_remote_post(
			Feasible_Settings::upstream_event_url(),
			array(
				'timeout'     => 5,
				'redirection' => 0,
				'blocking'    => true,
				'headers'     => $headers,
				'body'        => $body,
			)
		);

		if ( is_wp_error( $response ) ) {
			self::refuse(
				502,
				'upstream-unreachable',
				$response->get_error_message(),
				sprintf(
					/* translators: %s: the reason the request failed. */
					__( 'Events could not be forwarded to feasible.lol: %s', 'feasible-analytics' ),
					$response->get_error_message()
				)
			);
		}

		$code    = (int) wp_remote_retrieve_response_code( $response );
		$dropped = self::first_header( wp_remote_retrieve_header( $response, Feasible_Events::DROPPED_HEADER ) );
		$type    = self::first_header( wp_remote_retrieve_header( $response, 'content-type' ) );
		$answer  = wp_remote_retrieve_body( $response );

		if ( '' !== $dropped ) {
			// Relayed unchanged. A dropped event is information the browser
			// script and the site owner are both entitled to, and rewriting it
			// here would hide a classification decision made upstream.
			self::send_header( Feasible_Events::DROPPED_HEADER, $dropped );
		}

		// The upstream answer is echoed on this site's own origin, so only the
		// two types the ingest endpoint legitimately speaks are relayed, and
		// the browser is told not to guess at what anything else might be.
		self::send_header( 'X-Content-Type-Options', 'nosniff' );

		$relay = self::relays_body( $type );

		if ( $relay ) {
			self::send_header( 'Content-Type', $type );
		}

		if ( $code >= 400 ) {
			// A 400 from the ingest endpoint carries a sentence naming exactly
			// what was missing, and that sentence is the most useful thing the
			// settings screen can show. It is kept rather than logged nowhere.
			Feasible_Settings::record_error( 'event', 'upstream-' . $code, is_string( $answer ) ? $answer : '' );
		} elseif ( $code >= 200 && $code < 300 ) {
			Feasible_Settings::clear_error();
		}

		status_header( $code > 0 ? $code : 502 );

		if ( $relay && is_string( $answer ) && '' !== $answer ) {
			// A debug JSON document or the endpoint's own sentence, relayed as
			// it arrived so whoever is debugging reads what the server said.
			echo $answer; // phpcs:ignore WordPress.Security.EscapeOutput.OutputNotEscaped
		}

		exit;
	}

	/**
	 * refuse answers with a status, a reason header and a readable body, then
	 * stops.
	 *
	 * Nothing in this proxy is allowed to fail quietly. The sender is a beacon
	 * that cannot read English, so the machine-readable reason goes in a header
	 * and, when a person needs to act, the sentence is recorded for the
	 * settings screen and the site health panel to show.
	 *
	 * @param int    $status  HTTP status to send.
	 * @param string $reason  Short machine-readable reason.
	 * @param string $message Body to write.
	 * @param string $detail  Sentence to record for an administrator, if any.
	 * @return void
	 */
	private static function refuse( $status, $reason, $message, $detail = '' ) {
		self::send_header( self::ERROR_HEADER, $reason );

		if ( '' !== $detail ) {
			Feasible_Settings::record_error( 'proxy', $reason, $detail );
		}

		status_header( (int) $status );

		if ( '' !== $message ) {
			echo esc_html( $message );
		}

		exit;
	}

	/**
	 * relays_body decides whether an upstream response body may be echoed.
	 *
	 * The ingest endpoint answers empty, as text/plain, or as JSON. Anything
	 * else comes from a misconfigured or compromised host, and echoing it
	 * under its own type on this site's origin would let it run as this site.
	 *
	 * @param string $type The upstream Content-Type header, parameters included.
	 * @return bool
	 */
	public static function relays_body( $type ) {
		$parts = explode( ';', (string) $type, 2 );
		$media = strtolower( trim( $parts[0] ) );

		return 'application/json' === $media || 'text/plain' === $media;
	}

	/**
	 * first_header flattens a retrieved header to a single string.
	 *
	 * The HTTP API hands back an array when a header appeared more than once,
	 * and concatenating that into a response would produce a header value of
	 * the word "Array".
	 *
	 * @param mixed $value Retrieved header value.
	 * @return string
	 */
	private static function first_header( $value ) {
		if ( is_array( $value ) ) {
			$value = count( $value ) > 0 ? reset( $value ) : '';
		}

		return is_string( $value ) ? trim( $value ) : '';
	}

	/**
	 * send_header writes one response header with the value made safe.
	 *
	 * Line breaks are removed rather than escaped because a relayed header
	 * carrying one would let an upstream response inject headers of its own.
	 *
	 * @param string $name  Header name.
	 * @param string $value Header value.
	 * @return void
	 */
	private static function send_header( $name, $value ) {
		$value = str_replace( array( "\r", "\n", "\0" ), '', (string) $value );

		header( $name . ': ' . substr( trim( $value ), 0, 512 ) );
	}
}
