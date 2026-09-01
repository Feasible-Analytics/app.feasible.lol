//go:build windows

//
// lock_windows.go
// Advisory account locks on Windows.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package accounts

import (
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile blocks until Windows grants a shared or exclusive lock over the
// complete lock file. A zeroed OVERLAPPED selects the range beginning at byte
// zero, and the two maximum lengths cover the entire 64-bit file range.
func lockFile(file *os.File, mode lockMode) error {
	var flags uint32
	if mode == lockExclusive {
		flags = windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	overlapped := &windows.Overlapped{}
	if err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		flags,
		0,
		math.MaxUint32,
		math.MaxUint32,
		overlapped,
	); err != nil {
		return fmt.Errorf("lock file: %w", err)
	}
	return nil
}

// unlockFile releases the complete range acquired by lockFile while the
// underlying Windows handle remains valid.
func unlockFile(file *os.File) error {
	overlapped := &windows.Overlapped{}
	if err := windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		math.MaxUint32,
		math.MaxUint32,
		overlapped,
	); err != nil {
		return fmt.Errorf("unlock file: %w", err)
	}
	return nil
}
