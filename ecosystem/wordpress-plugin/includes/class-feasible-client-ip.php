<?php
//
// class-feasible-client-ip.php
// Working out which address belongs to the visitor rather than to a proxy.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Feasible_Client_IP resolves the visitor's address for the forwarded header.
 *
 * The proxy is a server-side caller: to the ingest endpoint every event looks
 * like it came from the web host unless this value is right. Getting it wrong
 * does not raise an error anywhere — it collapses every visitor in the country
 * into one row and nobody notices for a month — which is why the precedence
 * below matches the server's own resolution exactly.
 */
class Feasible_Client_IP {

	// Cloudflare's header, which is present and correct whenever the site sits
	// behind Cloudflare and absent otherwise.
	const HEADER_CLOUDFLARE = 'HTTP_CF_CONNECTING_IP';

	// The forwarding chain. The first entry is the client.
	const HEADER_FORWARDED_FOR = 'HTTP_X_FORWARDED_FOR';

	// The socket peer, which is the visitor only when nothing sits in front.
	const HEADER_REMOTE_ADDR = 'REMOTE_ADDR';

	/**
	 * resolve returns the visitor's address, or an empty string.
	 *
	 * The precedence is CF-Connecting-IP, then the first entry of
	 * X-Forwarded-For, then the socket peer. This assumes the WordPress edge
	 * stripped client-supplied forwarding headers; unlike the ingest service,
	 * this helper has no proxy allow-list.
	 *
	 * The server array is passed in rather than read from the superglobal so
	 * that this decision can be tested without a web server in front of it.
	 *
	 * @param array $server A $_SERVER-shaped array.
	 * @return string
	 */
	public static function resolve( array $server ) {
		if ( isset( $server[ self::HEADER_CLOUDFLARE ] ) ) {
			$address = self::normalise_address( $server[ self::HEADER_CLOUDFLARE ] );

			if ( '' !== $address ) {
				return $address;
			}
		}

		if ( isset( $server[ self::HEADER_FORWARDED_FOR ] ) ) {
			// The first entry is the client; everything after it is a proxy
			// that appended itself. Taking the last entry — which several
			// frameworks do, on the theory that it is the most trustworthy —
			// reports the nearest proxy as the visitor, so every visitor
			// behind the same CDN edge becomes one person from one city.
			$chain = explode( ',', (string) $server[ self::HEADER_FORWARDED_FOR ] );
			$first = self::normalise_address( $chain[0] );

			if ( '' !== $first ) {
				return $first;
			}
		}

		if ( isset( $server[ self::HEADER_REMOTE_ADDR ] ) ) {
			return self::normalise_address( $server[ self::HEADER_REMOTE_ADDR ] );
		}

		return '';
	}

	/**
	 * normalise_address strips the decoration around an address and validates
	 * what is left.
	 *
	 * Proxies append a source port often enough, and a bracketed IPv6 literal
	 * is the normal form for one, that both shapes have to be handled here. An
	 * address that fails validation returns empty so the caller falls through
	 * to the next header instead of forwarding rubbish upstream.
	 *
	 * @param mixed $value Raw header value.
	 * @return string
	 */
	public static function normalise_address( $value ) {
		$value = trim( (string) $value );

		if ( '' === $value ) {
			return '';
		}

		// A bracketed literal is either "[::1]" or "[::1]:443".
		if ( '[' === $value[0] ) {
			$close = strpos( $value, ']' );
			$value = ( false === $close ) ? ltrim( $value, '[' ) : substr( $value, 1, $close - 1 );
		} elseif ( substr_count( $value, ':' ) === 1 ) {
			// Exactly one colon is IPv4 with a port. More than one is a bare
			// IPv6 literal, and cutting it at a colon would make it a
			// different address entirely.
			$value = substr( $value, 0, strpos( $value, ':' ) );
		}

		// A zone identifier is valid in a link-local address and rejected by
		// the validator, so it is dropped rather than allowed to fail.
		$percent = strpos( $value, '%' );

		if ( false !== $percent && $percent > 0 ) {
			$value = substr( $value, 0, $percent );
		}

		$value = trim( $value );

		if ( false === filter_var( $value, FILTER_VALIDATE_IP ) ) {
			return '';
		}

		return $value;
	}

	/**
	 * user_agent returns the visitor's user agent string, capped in length.
	 *
	 * The cap is here because the value is copied into an outgoing header and
	 * an absurdly long one would be rejected by the upstream web server, which
	 * would look like the proxy failing rather than like the request it was.
	 *
	 * @param array $server A $_SERVER-shaped array.
	 * @return string
	 */
	public static function user_agent( array $server ) {
		if ( ! isset( $server['HTTP_USER_AGENT'] ) ) {
			return '';
		}

		$agent = (string) $server['HTTP_USER_AGENT'];

		// A user agent containing a line break is an injection attempt, not a
		// browser. Everything from the break onwards is discarded rather than
		// spliced back together, because whatever follows it was written to be
		// read as a header of its own.
		$break = strcspn( $agent, "\r\n\0" );
		$agent = substr( $agent, 0, $break );

		return substr( trim( $agent ), 0, 512 );
	}
}
