//
// disk_other.go
// The free-space reading on platforms that do not expose one to us.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

//go:build !linux && !darwin

package metrics

// diskSpace reports that free space is unknown here.
//
// It returns "not ok" rather than zero so the collector exports no series at
// all. Zero bytes free is what a full disk looks like, and exporting it on a
// platform that simply cannot answer would page somebody about a healthy box.
func diskSpace(string) (total, available uint64, ok bool) {
	return 0, 0, false
}
