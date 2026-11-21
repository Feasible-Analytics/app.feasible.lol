<?php
//
// class-feasible-payload.php
// Rebuilding an event body from the fields the event endpoint accepts, each one sanitised.
//
// Created: 2026-11-21
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

/**
 * Feasible_Payload turns a raw event body into one that is safe to forward.
 *
 * The proxy never forwards what the browser sent. It decodes the JSON, keeps
 * only the keys the event endpoint reads, sanitises each one for its type,
 * caps its size, and encodes the result again. Anything that is not a JSON
 * object with an event name is refused before it leaves the site.
 *
 * Each cap sits above the endpoint's own limit (30 properties, 300-character
 * property names, 2,000-character values and paths). Anything the endpoint
 * would cut still reaches it, so it is counted on the site's health panel
 * rather than lost here without a trace. The caps below only stop what no
 * browser running the tracker sends.
 */
class Feasible_Payload {

	// The longest event name kept.
	const MAX_NAME_LENGTH = 500;

	// The longest page URL or referrer kept. Browsers already cut a referrer
	// at 4,096 bytes.
	const MAX_URL_LENGTH = 8192;

	// The longest page title kept.
	const MAX_TITLE_LENGTH = 2000;

	// How many custom properties are kept, and how long each name and value may be.
	const MAX_PROPS             = 50;
	const MAX_PROP_NAME_LENGTH  = 500;
	const MAX_PROP_VALUE_LENGTH = 5000;

	// The automation signals are a few single letters.
	const MAX_SIGNALS_LENGTH = 32;

	// A currency is a three-letter code; a little room is left for a mistake
	// the endpoint should see and report.
	const MAX_CURRENCY_LENGTH = 8;

	// How deep the decoded JSON may nest. The tracker sends one object with
	// one level of properties and a revenue object.
	const MAX_DEPTH = 8;

	// The URL schemes a page or a referrer may use. An Android app reports
	// itself as the referrer with its own scheme.
	const URL_PROTOCOLS = array( 'http', 'https', 'android-app' );

	/**
	 * clean decodes a raw event body and rebuilds it from the accepted fields.
	 *
	 * The site's own configured domain replaces whatever domain the body
	 * named, so the proxy only ever forwards events for this site.
	 *
	 * @param string $body   The raw request body.
	 * @param string $domain The site domain configured in the settings.
	 * @return array|null The clean event, or null when the body is not an event.
	 */
	public static function clean( $body, $domain ) {
		$decoded = json_decode( trim( (string) $body ), true, self::MAX_DEPTH );

		if ( ! is_array( $decoded ) || ! isset( $decoded['n'] ) || ! is_string( $decoded['n'] ) ) {
			return null;
		}

		$name = self::limit( sanitize_text_field( $decoded['n'] ), self::MAX_NAME_LENGTH );

		if ( '' === $name ) {
			return null;
		}

		$event = array(
			'n' => $name,
			'd' => (string) $domain,
		);

		$url = self::url( isset( $decoded['u'] ) ? $decoded['u'] : null );

		if ( '' !== $url ) {
			$event['u'] = $url;
		}

		$referrer = self::url( isset( $decoded['r'] ) ? $decoded['r'] : null );

		if ( '' !== $referrer ) {
			$event['r'] = $referrer;
		}

		if ( isset( $decoded['t'] ) && is_string( $decoded['t'] ) ) {
			$title = self::limit( sanitize_text_field( $decoded['t'] ), self::MAX_TITLE_LENGTH );

			if ( '' !== $title ) {
				$event['t'] = $title;
			}
		}

		if ( isset( $decoded['k'] ) && is_string( $decoded['k'] ) ) {
			$key = strtolower( trim( $decoded['k'] ) );

			if ( wp_is_uuid( $key ) ) {
				$event['k'] = $key;
			}
		}

		foreach ( array( 'v', 'w', 'sd', 'e' ) as $field ) {
			if ( isset( $decoded[ $field ] ) && self::is_number( $decoded[ $field ] ) ) {
				$event[ $field ] = $decoded[ $field ];
			}
		}

		if ( isset( $decoded['i'] ) && is_bool( $decoded['i'] ) ) {
			$event['i'] = $decoded['i'];
		}

		if ( isset( $decoded['a'] ) && is_string( $decoded['a'] ) ) {
			$signals = self::limit( (string) preg_replace( '/[^A-Za-z0-9]/', '', $decoded['a'] ), self::MAX_SIGNALS_LENGTH );

			if ( '' !== $signals ) {
				$event['a'] = $signals;
			}
		}

		$props = self::props( isset( $decoded['p'] ) ? $decoded['p'] : null );

		if ( ! empty( $props ) ) {
			// An object rather than an array, so property names that look like
			// numbers still encode as a JSON object.
			$event['p'] = (object) $props;
		}

		$revenue = self::revenue( isset( $decoded['$'] ) ? $decoded['$'] : null );

		if ( null !== $revenue ) {
			$event['$'] = $revenue;
		}

		return $event;
	}

	/**
	 * encode renders a clean event as the JSON body to forward.
	 *
	 * @param array $event An event returned by clean().
	 * @return string
	 */
	public static function encode( array $event ) {
		return (string) wp_json_encode( $event, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE );
	}

	/**
	 * url sanitises a page URL or a referrer.
	 *
	 * Characters a browser leaves bare in a query string but that
	 * esc_url_raw() removes are percent-encoded first, so a query such as
	 * ?q={term} keeps its value rather than losing the braces.
	 *
	 * @param mixed $value Raw value from the body.
	 * @return string The URL, or an empty string when it is not one.
	 */
	public static function url( $value ) {
		if ( ! is_string( $value ) ) {
			return '';
		}

		$value = self::limit( trim( $value ), self::MAX_URL_LENGTH );

		if ( '' === $value ) {
			return '';
		}

		$value = strtr(
			$value,
			array(
				' '  => '%20',
				'"'  => '%22',
				'<'  => '%3C',
				'>'  => '%3E',
				'^'  => '%5E',
				'`'  => '%60',
				'{'  => '%7B',
				'}'  => '%7D',
				'\\' => '%5C',
			)
		);

		return esc_url_raw( $value, self::URL_PROTOCOLS );
	}

	/**
	 * props sanitises the custom properties.
	 *
	 * They arrive as an object, or as a string holding one from older
	 * trackers. Names are plain text. String values keep their spacing and
	 * percent signs, because a property is often a URL or a file name, and
	 * lose any HTML and control characters. A URL value is sanitised as a URL.
	 * Numbers and booleans pass as they are. A null, an object or an array is
	 * sent as null, which the endpoint counts as a property it could not keep.
	 *
	 * @param mixed $value Raw value from the body.
	 * @return array Property name => sanitised value.
	 */
	public static function props( $value ) {
		if ( is_string( $value ) ) {
			$value = json_decode( $value, true, self::MAX_DEPTH );
		}

		if ( ! is_array( $value ) ) {
			return array();
		}

		$clean = array();

		foreach ( $value as $name => $item ) {
			if ( count( $clean ) >= self::MAX_PROPS ) {
				break;
			}

			$name = self::limit( sanitize_text_field( (string) $name ), self::MAX_PROP_NAME_LENGTH );

			if ( '' === $name ) {
				continue;
			}

			$clean[ $name ] = self::prop_value( $item );
		}

		return $clean;
	}

	/**
	 * prop_value sanitises one property value.
	 *
	 * @param mixed $value Raw property value.
	 * @return string|int|float|bool|null
	 */
	public static function prop_value( $value ) {
		if ( is_bool( $value ) || self::is_number( $value ) ) {
			return $value;
		}

		if ( ! is_string( $value ) ) {
			return null;
		}

		if ( 1 === preg_match( '#^https?://#i', $value ) ) {
			$url = self::url( $value );

			if ( '' !== $url ) {
				return self::limit( $url, self::MAX_PROP_VALUE_LENGTH );
			}
		}

		return self::limit( self::text( $value ), self::MAX_PROP_VALUE_LENGTH );
	}

	/**
	 * revenue sanitises the revenue object.
	 *
	 * The amount stays a number or becomes plain text. A text amount that is
	 * not a number is still forwarded, so the endpoint refuses it with a
	 * reason the settings screen can show.
	 *
	 * @param mixed $value Raw value from the body.
	 * @return array|null
	 */
	public static function revenue( $value ) {
		if ( ! is_array( $value ) || ! isset( $value['amount'], $value['currency'] ) || ! is_string( $value['currency'] ) ) {
			return null;
		}

		$currency = self::limit( (string) preg_replace( '/[^A-Za-z]/', '', $value['currency'] ), self::MAX_CURRENCY_LENGTH );
		$amount   = $value['amount'];

		if ( is_string( $amount ) ) {
			$amount = self::limit( sanitize_text_field( $amount ), 64 );
		} elseif ( ! self::is_number( $amount ) ) {
			return null;
		}

		if ( '' === $currency || '' === $amount ) {
			return null;
		}

		return array(
			'amount'   => $amount,
			'currency' => $currency,
		);
	}

	/**
	 * text strips HTML and control characters from free text.
	 *
	 * @param string $value Raw text.
	 * @return string
	 */
	public static function text( $value ) {
		$value = wp_strip_all_tags( (string) $value );

		return (string) preg_replace( '/[\x00-\x08\x0B\x0C\x0E-\x1F\x7F]/', '', $value );
	}

	/**
	 * is_number reports whether a decoded JSON value is a finite number.
	 *
	 * @param mixed $value Decoded value.
	 * @return bool
	 */
	public static function is_number( $value ) {
		return is_int( $value ) || ( is_float( $value ) && is_finite( $value ) );
	}

	/**
	 * limit cuts a string to a number of characters.
	 *
	 * @param string $value The string.
	 * @param int    $max   The most characters to keep.
	 * @return string
	 */
	public static function limit( $value, $max ) {
		$value = (string) $value;

		if ( function_exists( 'mb_substr' ) ) {
			return mb_strlen( $value, 'UTF-8' ) > $max ? mb_substr( $value, 0, $max, 'UTF-8' ) : $value;
		}

		return strlen( $value ) > $max ? substr( $value, 0, $max ) : $value;
	}
}
