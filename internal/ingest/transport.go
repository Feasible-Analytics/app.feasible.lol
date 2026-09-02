//
// transport.go
// The batching seam before either a direct write or a durable ingest outbox.
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

// Transport takes durable ownership of derived events. Direct mode commits to
// the account database; hosted ingest commits to its local SQLite outbox.
type Transport interface {
	// Send delivers a batch and returns the ids durably committed. Returning
	// partial identities lets the buffer release successful waiters precisely.
	Send(ctx context.Context, shard int, batch []Event) (committed []uuid.UUID, err error)
}

// ShardResolver supplies the local partition key used to group a buffer flush.
type ShardResolver interface {
	Shard(accountID int64) (int, bool)
}

// DirectShard resolves every account to the app process's local partition zero.
type DirectShard struct{}

// Shard always answers shard zero.
func (DirectShard) Shard(int64) (int, bool) { return 0, true }

// Direct is the in-process transport from the buffer to the account writer.
type Direct struct {
	writer *Writer
}

// NewDirect builds the in-process transport over an account writer.
func NewDirect(writer *Writer) *Direct {
	return &Direct{writer: writer}
}

// Send hands the batch straight to the account writer and returns its precise
// durable outcome.
func (d *Direct) Send(ctx context.Context, shard int, batch []Event) ([]uuid.UUID, error) {
	if shard != 0 {
		return nil, fmt.Errorf("direct transport: shard %d does not exist in this process", shard)
	}

	return d.writer.Write(ctx, batch)
}
