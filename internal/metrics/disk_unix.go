//
// disk_unix.go
// Free space on the filesystem holding the data directory.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

//go:build linux || darwin

package metrics

import "syscall"

// diskSpace reports the total and available bytes of the filesystem a path is
// on, and whether the answer is real.
//
// Available rather than free: the two differ by the reserved blocks only root
// may use, and an alert built on free space fires after the process has already
// started failing to write. The one this system cares about is the one it can
// actually use.
func diskSpace(path string) (total, available uint64, ok bool) {
	var fs syscall.Statfs_t

	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0, false
	}

	// Bsize is a signed 64-bit value on Linux and an unsigned 32-bit one on
	// macOS, so both sides are widened rather than assuming either shape.
	size := uint64(fs.Bsize) //nolint:gosec,unconvert // widened deliberately, the type differs by platform

	return uint64(fs.Blocks) * size, uint64(fs.Bavail) * size, true
}
