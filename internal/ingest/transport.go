//
// transport.go
// The seam between an ingestor and the shard that owns an account's data.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Transport moves derived events from an ingestor to the shard that owns them.
//
// There is one implementation today and there will be three. What is expensive
// to retrofit is not the shard count — that is configuration — it is the seam
// itself: adding it later means touching every write path in the codebase.
// Building it now means the production topology becomes a config change.
//
//	direct   every self-hoster, and our dev machines. Ingestor and shard are
//	         the same process and the write happens in one transaction.
//	http     our production. Store-and-forward over the network.
//	queue    an escape hatch we do not expect to need.
type Transport interface {
	// Send delivers a batch and returns the ids the shard has durably
	// committed. Returning the committed ids rather than a bare error is what
	// lets a store-and-forward sender delete exactly what landed and retry
	// exactly what did not, with no double-counting either way.
	Send(ctx context.Context, shard int, batch []Event) (committed []uuid.UUID, err error)
}

// ShardResolver turns an account into the shard that holds it. Routing is
// data-driven rather than a hash function: there are no hash ranges to
// rebalance later, so moving an account becomes "move the file, update two
// lists" instead of a migration.
type ShardResolver interface {
	Shard(accountID int64) (int, bool)
}

// DirectShard is the resolver every single-process install uses. There is
// exactly one shard and the answer is always zero, but the call still happens
// so the seam is exercised from the first event rather than the day it is
// needed.
type DirectShard struct{}

// Shard always answers shard zero.
func (DirectShard) Shard(int64) (int, bool) { return 0, true }

// Direct is the in-process Transport. The "network hop" is a function call, and
// that is the whole point: the pipeline runs accept → derive → buffer → forward
// → write in every deployment, so the path a self-hoster runs is the path we
// run, minus one HTTP round trip.
type Direct struct {
	writer *Writer
}

// NewDirect builds the in-process transport over a shard writer.
func NewDirect(writer *Writer) *Direct {
	return &Direct{writer: writer}
}

// Send hands the batch straight to the shard writer. It still returns the
// committed ids, even though nothing can be lost between here and there,
// because the caller's bookkeeping must not care which transport it is talking
// to.
func (d *Direct) Send(ctx context.Context, shard int, batch []Event) ([]uuid.UUID, error) {
	if shard != 0 {
		return nil, fmt.Errorf("direct transport: shard %d does not exist in this process", shard)
	}

	return d.writer.Write(ctx, batch)
}
