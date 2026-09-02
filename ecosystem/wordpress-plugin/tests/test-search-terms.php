<?php
//
// test-search-terms.php
// The search-term normaliser and the results bucket.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

require_once __DIR__ . '/bootstrap.php';

feasible_assert_same(
	'blue widget',
	Feasible_Measurements::normalise_search_term( '  Blue   Widget ' ),
	'case, padding and repeated spaces all collapse to one grouping key'
);

feasible_assert_same(
	'blue widget',
	Feasible_Measurements::normalise_search_term( "Blue\tWidget" ),
	'a tab is whitespace like any other'
);

feasible_assert_same(
	'blue widget',
	Feasible_Measurements::normalise_search_term( "blue\nwidget" ),
	'a newline pasted into the search box does not create a second row'
);

feasible_assert_same(
	'blue widget',
	Feasible_Measurements::normalise_search_term( "blue\x00\x07widget" ),
	'control characters become a space rather than joining two words'
);

feasible_assert_same(
	'',
	Feasible_Measurements::normalise_search_term( '   ' ),
	'a search box submitted empty normalises to nothing at all'
);

feasible_assert_same(
	'',
	Feasible_Measurements::normalise_search_term( null ),
	'a missing query is not an error'
);

$long = str_repeat( 'a', 250 );

feasible_assert_same(
	Feasible_Measurements::SEARCH_TERM_MAX_LENGTH,
	strlen( Feasible_Measurements::normalise_search_term( $long ) ),
	'a pasted wall of text is cut to the cap'
);

feasible_assert_same(
	'blue widget',
	Feasible_Measurements::normalise_search_term( 'blue widget' ),
	'an already-normal term is left exactly as it is'
);

if ( function_exists( 'mb_strtolower' ) ) {
	feasible_assert_same(
		'café',
		Feasible_Measurements::normalise_search_term( 'CAFÉ' ),
		'accented characters lowercase correctly rather than being left alone'
	);

	feasible_assert_same(
		Feasible_Measurements::SEARCH_TERM_MAX_LENGTH,
		mb_strlen( Feasible_Measurements::normalise_search_term( str_repeat( 'é', 250 ) ), 'UTF-8' ),
		'the cap counts characters, so a multibyte term is not cut mid-character'
	);
}

feasible_assert_same(
	'none',
	Feasible_Measurements::bucket_results( 0 ),
	'a search that found nothing is bucketed as none'
);

feasible_assert_same(
	'none',
	Feasible_Measurements::bucket_results( -1 ),
	'a nonsense count is treated as nothing found rather than as something'
);

feasible_assert_same(
	'some',
	Feasible_Measurements::bucket_results( 1 ),
	'one result is some'
);

feasible_assert_same(
	'some',
	Feasible_Measurements::bucket_results( '42' ),
	'a numeric string from the query object still buckets'
);

// A search term is whatever the visitor typed, and it is emitted between
// <script> and </script>. Plain JSON leaves the HTML characters alone, and
// "<!--<script>" is the sequence that puts the parser into its double-escaped
// state and swallows the rest of the page.
feasible_assert(
	false === strpos( Feasible_Measurements::encode( array( 'search_term' => '<!--<script>' ) ), '<' ),
	'no raw < survives into the inline script'
);

feasible_assert(
	false === strpos( Feasible_Measurements::encode( array( 'search_term' => '</script><img src=x onerror=alert(1)>' ) ), '>' ),
	'no raw > survives into the inline script'
);

feasible_assert_same(
	array( 'search_term' => '<!--<script>' ),
	json_decode( Feasible_Measurements::encode( array( 'search_term' => '<!--<script>' ) ), true ),
	'the escaping changes the bytes on the page, never the value the browser reads'
);
