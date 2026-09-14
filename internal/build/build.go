//
// build.go
// Build identity injected at link time.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package build holds the version, commit and build date stamped into the
// binary by -ldflags. It is its own package so the Makefile has one stable
// symbol path to write to, and so anything that wants to report a version does
// not have to import package main.
package build

import (
	"fmt"
	"runtime"
)

// These are overwritten at link time. The defaults describe an unstamped local
// build, which is exactly what someone running `go build` by hand has, and
// saying so is more useful than printing an empty string.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// Info is the build identity in the shape the /version endpoint returns.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	BuiltAt string `json:"built_at"`
}

// Current returns the build identity stamped into this binary.
func Current() Info {
	return Info{Version: Version, Commit: Commit, BuiltAt: Date}
}

// String renders the one line `--version` prints. Support questions start with
// "what exactly are you running", so the commit and the Go runtime are on the
// line too — a version number alone never identifies a build from a branch.
func String() string {
	return fmt.Sprintf("feasible %s (commit %s, built %s, %s %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
