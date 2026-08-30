//
// build_test.go
// Tests for the build identity string.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package build

import (
	"runtime"
	"strings"
	"testing"
)

// TestString checks every stamped field reaches the output. The release
// Makefile writes these symbols by name, so a rename that quietly stops the
// injection working would otherwise only show up in a shipped binary.
func TestString(t *testing.T) {
	original := []string{Version, Commit, Date}
	defer func() { Version, Commit, Date = original[0], original[1], original[2] }()

	Version, Commit, Date = "v1.2.3", "abc1234", "2026-08-30T00:00:00Z"

	got := String()

	for _, want := range []string{"feasible", "v1.2.3", "abc1234", "2026-08-30T00:00:00Z", runtime.Version(), runtime.GOOS} {
		if !strings.Contains(got, want) {
			t.Errorf("version line %q is missing %q", got, want)
		}
	}
}
