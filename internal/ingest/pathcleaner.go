//
// pathcleaner.go
// The interface path mapping rules use while raw paths are stored.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

// PathCleaner calculates the report-facing path for a raw path. It is an
// interface, nil when an install has no rules, because the rules live in the
// account database and this package must not learn to read one.
//
// The shard stores the original path and writes a source-to-target mapping in
// the same transaction. Reports group through that mapping, so changing or
// removing a rule remains reversible for both historical and new events.
type PathCleaner interface {
	Clean(siteID int64, path string) string
}
