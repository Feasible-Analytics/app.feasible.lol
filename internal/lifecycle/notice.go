//
// notice.go
// What the machine hands the mailer, and what the mailer promises back.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package lifecycle

import (
	"context"
	"time"
)

// Notice is one email the machine has decided is due. It carries the dates
// already computed rather than the state, so that the mail package cannot do
// its own date arithmetic and reach a different answer from the sweeper that
// will act — every "we delete your account on <date>" in the product comes from
// this struct.
type Notice struct {
	TeamID   int64
	TeamName string

	// MessageKey is stable across leased outbox retries. Mail transports expose
	// it as Message-ID where possible; SMTP still has an unavoidable accepted-
	// before-ack crash window, but retries at least carry the same identity.
	MessageKey string

	// To is the billing contact. It is resolved before the notice is built,
	// because a deletion warning with no recipient is a bug that must fail
	// loudly rather than send nothing quietly.
	To string

	// Template names the message; Trigger decides its wording. The same ten
	// templates cover both paths and differ only in whether they talk about a
	// trial ending or a card being declined.
	Template string
	Trigger  Trigger

	// Phase is where the account is now, and Day is where it is on the clock.
	Phase Phase
	Day   int

	// LocksAt, StopsAt and DeletesAt are the three dates the sequence exists to
	// announce. All three are always present, because "what happens next" is
	// only half the answer somebody wants when they are deciding whether to pay.
	LocksAt   time.Time
	StopsAt   time.Time
	DeletesAt time.Time

	// Announced is the one date this particular message is about — the boundary
	// the template quotes in its first sentence.
	Announced time.Time

	// UpgradeURL is the one-click upgrade link every message carries.
	// PortalURL is the payment provider's card-update page and is only set on
	// the dunning path, where "update your card" is the actual fix.
	// ExportURL is their data, available in every phase.
	UpgradeURL string
	PortalURL  string
	ExportURL  string
}

// Notifier sends one notice and reports what the transport observed. The string
// is stored on the lifecycle_emails row, so that "did they get the warning" is
// answerable from the database rather than from a log rotation ago.
type Notifier interface {
	Notify(ctx context.Context, notice Notice) (string, error)
}

// NotifierFunc adapts a function to the interface, for the callers — mostly
// tests and the sweeper's dry-run mode — that have one behaviour rather than a
// type.
type NotifierFunc func(ctx context.Context, notice Notice) (string, error)

// Notify calls the function.
func (f NotifierFunc) Notify(ctx context.Context, notice Notice) (string, error) {
	return f(ctx, notice)
}
