<?php
//
// run.php
// Runs every test file in this folder under plain php, with no WordPress and no PHPUnit.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

require_once __DIR__ . '/bootstrap.php';

// The files are discovered rather than listed so that adding one is adding a
// file, and a test that was written but never wired up cannot sit there passing
// nothing.
$files = glob( __DIR__ . '/test-*.php' );

if ( empty( $files ) ) {
	echo "FAILED: no test files found\n";
	exit( 1 );
}

sort( $files );

foreach ( $files as $file ) {
	echo 'running ' . basename( $file ) . "\n";

	require_once $file;
}

// The summary and the exit status are printed by the shutdown handler that
// bootstrap.php registered, so a fatal error in any file still fails the run
// instead of ending it quietly.
