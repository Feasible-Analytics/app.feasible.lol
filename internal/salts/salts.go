//
// salts.go
// Derives daily visitor-fingerprint salts from one shared deployment value.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package salts derives matching daily fingerprint material in every ingest
// process without storage, rotation jobs, or an app-side authority.
package salts

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"
)

// Size is the 128-bit key size required by the SipHash fingerprint function.
const Size = 16

// daySeconds maps a Unix timestamp to its UTC day without involving local time,
// daylight-saving transitions, or a timezone database.
const daySeconds int64 = 86400

// Pair contains today's derived salt and yesterday's session fallback.
type Pair struct {
	Current  []byte
	Previous []byte
	Day      int64
}

// Source derives salts from one shared deployment value and an injectable UTC
// clock. It owns no database or background lifecycle.
type Source struct {
	shared []byte
	now    func() time.Time
}

// New builds a daily salt source. An empty shared value would make every
// installation derive the same public fingerprint keys, so it is rejected.
func New(shared string) (*Source, error) {
	if shared == "" {
		return nil, fmt.Errorf("salts: shared value cannot be empty")
	}

	return &Source{shared: []byte(shared), now: time.Now}, nil
}

// SetClock replaces the source clock for deterministic replay and rollover
// tests. Serving processes leave the UTC system clock in place.
func (s *Source) SetClock(now func() time.Time) {
	s.now = now
}

// Day returns the UTC day number containing a timestamp.
func Day(t time.Time) int64 {
	return t.UTC().Unix() / daySeconds
}

// Pair derives today's and yesterday's material locally. The context is part
// of the ingest SaltSource contract but no I/O occurs and it cannot expire.
func (s *Source) Pair(_ context.Context) (Pair, error) {
	today := Day(s.now())
	return Pair{
		Current:  s.derive(today),
		Previous: s.derive(today - 1),
		Day:      today,
	}, nil
}

// derive uses HMAC for domain-separated, deterministic expansion of the shared
// value and UTC day into the 16 bytes consumed by SipHash.
func (s *Source) derive(day int64) []byte {
	mac := hmac.New(sha256.New, s.shared)
	_, _ = mac.Write([]byte("feasible-visitor-salt-v1"))

	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(day))
	_, _ = mac.Write(encoded[:])

	return append([]byte(nil), mac.Sum(nil)[:Size]...)
}
