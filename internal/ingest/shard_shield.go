//
// shard_shield.go
// The half of the shield rules that is evaluated where the settings are live.
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
// in the ingest tier, because that is the only place the raw address still
// exists — by the time an event reaches here the address has been geolocated,
// hashed into a fingerprint and thrown away. Everything else is evaluated here,
// at the shard, where the rule list is the live table rather than a snapshot
// that has been forwarded across a network.
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
