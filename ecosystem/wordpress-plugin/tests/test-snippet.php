<?php
//
// test-snippet.php
// Which tag in the printed markup gets the tracker's attributes.
//
// Created: 2026-11-21
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

require_once __DIR__ . '/bootstrap.php';

$extra = 'defer data-domain="example.com" ';

feasible_assert_same(
	"<script id=\"feasible-analytics-js-before\">\nwindow.feasible=1;\n</script>\n<script defer data-domain=\"example.com\" id=\"feasible-analytics-js\" src=\"https://example.com/a/b.js\"></script>\n",
	Feasible_Snippet::add_to_loading_tag(
		"<script id=\"feasible-analytics-js-before\">\nwindow.feasible=1;\n</script>\n<script id=\"feasible-analytics-js\" src=\"https://example.com/a/b.js\"></script>\n",
		$extra
	),
	'only the tag that loads the file gets the attributes, not the inline script before it'
);

feasible_assert_same(
	"<script defer data-domain=\"example.com\" src='https://example.com/a/b.js' id='feasible-analytics-js'></script>\n<script>\nwindow.feasible('404');\n</script>\n",
	Feasible_Snippet::add_to_loading_tag(
		"<script src='https://example.com/a/b.js' id='feasible-analytics-js'></script>\n<script>\nwindow.feasible('404');\n</script>\n",
		$extra
	),
	'older WordPress prints src first, and the tag still gets the attributes'
);

feasible_assert_same(
	'<script data-src="x" id="y"></script>',
	Feasible_Snippet::add_to_loading_tag( '<script data-src="x" id="y"></script>', $extra ),
	'an attribute that merely ends in src is not a src'
);

feasible_assert_same(
	'<script defer data-domain="a$1\\b.example" src="x"></script>',
	Feasible_Snippet::add_to_loading_tag( '<script src="x"></script>', 'defer data-domain="a$1\\b.example" ' ),
	'dollar signs and backslashes in a value are inserted as written'
);
