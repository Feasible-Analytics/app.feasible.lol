//go:build !windows

//
// lock_unix.go
// Advisory account locks on Unix platforms.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package accounts

import (
	"fmt"
	"os"
	"syscall"
)

// lockFile blocks until the requested shared or exclusive advisory lock can be
// held for the lifetime of file.
func lockFile(file *os.File, mode lockMode) error {
	how := syscall.LOCK_SH
	if mode == lockExclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(file.Fd()), how); err != nil {
		return fmt.Errorf("lock file: %w", err)
	}
	return nil
}

// unlockFile releases an advisory lock while its file descriptor is still
// valid; the caller remains responsible for closing the file.
func unlockFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		return fmt.Errorf("unlock file: %w", err)
	}
	return nil
}
