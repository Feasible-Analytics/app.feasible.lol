//
// pathcleaner.go
// The seam the path rewrite rules reach the write path through.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

// PathCleaner rewrites a path before it is interned. It is an interface with a
// no-op default because the rules live in the account database and this package
// must not learn to read one.
//
// It runs at the authoritative account writer rather than during request
// derivation. That is where dim_pathname becomes a row and where a live rule
// can share the fact transaction's decision boundary.
//
// Cleaning here does not make the rules retroactive — the query layer does
// that, by grouping through a map from one interned id to another. Both halves
// are needed: this one bounds what gets written from now on, and the query one
// fixes everything already written.
type PathCleaner interface {
	Clean(siteID int64, path string) string
}

// NoPathCleaner leaves paths alone.
type NoPathCleaner struct{}

// Clean returns the path unchanged.
func (NoPathCleaner) Clean(_ int64, path string) string { return path }
