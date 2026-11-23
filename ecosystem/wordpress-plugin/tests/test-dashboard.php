<?php
//
// test-dashboard.php
// Which links the wp-admin dashboard page will frame, and the address it frames them at.
//
// Created: 2026-11-23
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

require_once __DIR__ . '/bootstrap.php';

$host = 'https://app.feasible.lol';

feasible_assert(
	Feasible_Dashboard::is_valid_share_url( 'https://app.feasible.lol/share/aNu5WoaVG5cPRTHZYmQmWA', $host ),
	'a share link on the configured host is framed'
);

feasible_assert(
	Feasible_Dashboard::is_valid_share_url( 'https://app.feasible.lol/share/aNu5WoaVG5cPRTHZYmQmWA?embed=true&theme=light', $host ),
	'a share link that already carries embed parameters is still a share link'
);

feasible_assert(
	! Feasible_Dashboard::is_valid_share_url( 'https://evil.example/share/aNu5WoaVG5cPRTHZYmQmWA', $host ),
	'a share path on another host is refused'
);

feasible_assert(
	! Feasible_Dashboard::is_valid_share_url( 'https://app.feasible.lol/login', $host ),
	'a page on the right host that is not a share link is refused'
);

feasible_assert(
	! Feasible_Dashboard::is_valid_share_url( 'https://app.feasible.lol/share/', $host ),
	'a bare /share/ with no link after it is refused'
);

feasible_assert(
	! Feasible_Dashboard::is_valid_share_url( 'javascript://app.feasible.lol/share/x', $host ),
	'a scheme other than http or https is refused'
);

feasible_assert_same(
	'https://app.feasible.lol/share/aNu5WoaVG5cPRTHZYmQmWA?embed=true',
	Feasible_Dashboard::frame_url( 'https://app.feasible.lol/share/aNu5WoaVG5cPRTHZYmQmWA' ),
	'a link as copied from the dashboard is framed with embed=true, or the service refuses the frame'
);

feasible_assert_same(
	'https://app.feasible.lol/share/aNu5WoaVG5cPRTHZYmQmWA?embed=true',
	Feasible_Dashboard::frame_url( ' https://app.feasible.lol/share/aNu5WoaVG5cPRTHZYmQmWA ' ),
	'surrounding whitespace from a paste does not end up in the frame address'
);

feasible_assert_same(
	'https://app.feasible.lol/share/aNu5WoaVG5cPRTHZYmQmWA?embed=true&theme=dark',
	Feasible_Dashboard::frame_url( 'https://app.feasible.lol/share/aNu5WoaVG5cPRTHZYmQmWA?embed=false&theme=dark' ),
	'an existing embed value is replaced once and the other parameters are kept'
);
