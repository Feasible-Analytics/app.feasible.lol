//
// shard_shield.go
// The shield rules evaluated in the authoritative account transaction.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

// ShardShield decides whether a customer has blocked an event on something
// other than its IP address. It is an interface with a no-op default for the
// same reason IPShield is: the rules belong to the account database, and this
// package must not learn how to read one.
//
// The split between the two is not arbitrary. An IP rule can only be evaluated
// at the event endpoint, because that is the only place the raw address still
// exists. Everything else is evaluated by the writer against the live account
// rule snapshot in the same process.
type ShardShield interface {
	// Allowed reports whether an event may be written, and the drop reason when
	// it may not. The reason is one of the Reason constants, so it lands on the
	// customer's ingestion health panel as a countable value rather than as
	// prose.
	Allowed(siteID int64, hostname, pathname, country string) (bool, string)
}

// NoShardShield allows everything.
type NoShardShield struct{}

// Allowed always allows.
func (NoShardShield) Allowed(int64, string, string, string) (bool, string) { return true, "" }
