//
// lock.go
// Platform-neutral advisory lock modes used by account lifecycle fences.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package accounts

// lockMode distinguishes shared account-use leases from exclusive deletion
// fences without leaking an operating system's constants into common code.
type lockMode uint8

const (
	lockShared lockMode = iota
	lockExclusive
)
