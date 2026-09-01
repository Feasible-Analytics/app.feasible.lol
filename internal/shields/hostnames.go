//
// hostnames.go
// Counting what a site rejected, without letting the counter become the attack.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package shields

import (
	"context"
	"fmt"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// MaxRejectedHostnames is how many distinct hostnames are counted per site per
// day before the rest are folded into one bucket.
const MaxRejectedHostnames = ingest.MaxRejectedHostnames

// OtherHostname is the aggregate bucket for distinct hostnames past the cap.
const OtherHostname = ingest.OtherRejectedHostname

// rejectedDaySeconds is the UTC day divisor used by the durable counter.
const rejectedDaySeconds int64 = 86400

// Rejections reads hostname facts committed atomically with event UUID
// ownership. It has no Record method because only the writer may create facts.
type Rejections struct {
	accounts *accounts.Manager

	// Now is injectable so day ranges can be tested without waiting for UTC
	// midnight.
	Now func() time.Time
}

// NewRejections builds the durable hostname rejection read view.
func NewRejections(manager *accounts.Manager) *Rejections {
	return &Rejections{
		accounts: manager,
		Now:      func() time.Time { return time.Now().UTC() },
	}
}

// RejectedHostname is one named hostname and its rejected event count.
type RejectedHostname struct {
	Hostname string
	Events   int64
}

// ListRejected reads one site's rejected hostnames over the last few days,
// busiest first.
func (r *Rejections) ListRejected(ctx context.Context, accountID, siteID int64, days int) ([]RejectedHostname, error) {
	account, err := r.accounts.Open(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("shields: rejections: open account %d: %w", accountID, err)
	}
	if days <= 0 {
		days = 1
	}

	cutoff := (r.Now().Unix() / rejectedDaySeconds) - int64(days-1)
	rows, err := account.Reader().QueryContext(ctx, `
		SELECT hostname, SUM(events)
		FROM hostname_rejections
		WHERE site_id = ? AND day >= ?
		GROUP BY hostname
		ORDER BY SUM(events) DESC, hostname`, siteID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("shields: rejections: read: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RejectedHostname
	for rows.Next() {
		var entry RejectedHostname
		if err := rows.Scan(&entry.Hostname, &entry.Events); err != nil {
			return nil, fmt.Errorf("shields: rejections: read: %w", err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shields: rejections: read: %w", err)
	}

	return out, nil
}
