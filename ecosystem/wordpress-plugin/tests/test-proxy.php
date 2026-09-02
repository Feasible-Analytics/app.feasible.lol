<?php
//
// test-proxy.php
// Which upstream answers the event route may echo on this site's own origin.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

require_once __DIR__ . '/bootstrap.php';

feasible_assert(
	Feasible_Proxy::relays_body( 'application/json' ),
	'a debug JSON document is relayed'
);

feasible_assert(
	Feasible_Proxy::relays_body( 'text/plain; charset=utf-8' ),
	'the endpoint\'s own sentence is relayed, parameters and all'
);

feasible_assert(
	Feasible_Proxy::relays_body( 'Application/JSON' ),
	'the media type is matched case-insensitively'
);

feasible_assert(
	! Feasible_Proxy::relays_body( 'text/html' ),
	'an HTML page from a misconfigured host is never echoed on this origin'
);

feasible_assert(
	! Feasible_Proxy::relays_body( 'application/javascript' ),
	'a script from a compromised host is never echoed on this origin'
);

feasible_assert(
	! Feasible_Proxy::relays_body( '' ),
	'a body with no declared type is not echoed for the browser to guess at'
);
