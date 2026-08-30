//
// main.go
// Entry point for the feasible binary.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Command feasible is the whole product in one binary: the dashboard, the API,
// the tracker, the ingest tier and the database tooling.
package main

import (
	"os"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/cli"
)

// main hands straight off to the cli package and does nothing else. Keeping
// os.Exit as the only thing that lives in package main is what lets the entire
// command surface be tested in-process, without spawning a binary.
func main() {
	os.Exit(cli.Main())
}
