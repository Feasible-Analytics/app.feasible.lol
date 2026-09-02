<?php
//
// test-settings.php
// The throttle that keeps a failing proxy from writing the options table once per beacon.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

require_once __DIR__ . '/bootstrap.php';

$now      = 1_800_000_000;
$recorded = array(
	'route'  => 'event',
	'reason' => 'upstream-503',
	'detail' => 'still unhappy',
	'time'   => $now,
);

feasible_assert(
	Feasible_Settings::error_is_stale( array(), 'event', 'upstream-503', $now ),
	'the first failure is always recorded'
);

feasible_assert(
	Feasible_Settings::error_is_stale( false, 'event', 'upstream-503', $now ),
	'a missing option is treated as nothing recorded'
);

feasible_assert(
	! Feasible_Settings::error_is_stale( $recorded, 'event', 'upstream-503', $now + 1 ),
	'the same failure a second later is not written again'
);

feasible_assert(
	! Feasible_Settings::error_is_stale( $recorded, 'event', 'upstream-503', $now + Feasible_Settings::ERROR_THROTTLE_SECONDS - 1 ),
	'the same failure just inside the window is not written again'
);

feasible_assert(
	Feasible_Settings::error_is_stale( $recorded, 'event', 'upstream-503', $now + Feasible_Settings::ERROR_THROTTLE_SECONDS ),
	'the same failure once the window has passed refreshes the timestamp'
);

feasible_assert(
	Feasible_Settings::error_is_stale( $recorded, 'event', 'upstream-502', $now + 1 ),
	'a different reason on the same route is recorded at once'
);

feasible_assert(
	Feasible_Settings::error_is_stale( $recorded, 'proxy', 'upstream-503', $now + 1 ),
	'the same reason on a different route is recorded at once'
);
