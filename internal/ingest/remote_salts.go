//
// remote_salts.go
// The ingester's memory-only cache of fingerprint salts fetched from the app.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/salts"
)

// RemoteSalts fetches the two live salts over the private app listener and
// retains them only in memory.
type RemoteSalts struct {
	URL    string
	Client *http.Client
	Signer *InternalSigner
	Now    func() time.Time

	mu     sync.RWMutex
	cached salts.Pair
}

// Pair returns an isolated current snapshot, refreshing synchronously when the
// UTC day has changed.
func (s *RemoteSalts) Pair(ctx context.Context) (salts.Pair, error) {
	today := salts.Day(s.clock())
	s.mu.RLock()
	if s.cached.Day == today && len(s.cached.Current) == salts.Size {
		pair := cloneRemotePair(s.cached)
		s.mu.RUnlock()
		return pair, nil
	}
	s.mu.RUnlock()

	return s.Refresh(ctx)
}

// Refresh replaces the memory snapshot only after a complete, current response
// has been authenticated and decoded.
func (s *RemoteSalts) Refresh(ctx context.Context) (salts.Pair, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(s.URL, "/")+InternalSaltsPath, nil)
	if err != nil {
		return salts.Pair{}, fmt.Errorf("remote salts: build request: %w", err)
	}
	if err := s.Signer.Sign(request, nil); err != nil {
		return salts.Pair{}, fmt.Errorf("remote salts: %w", err)
	}

	response, err := s.client().Do(request)
	if err != nil {
		return salts.Pair{}, fmt.Errorf("remote salts: fetch: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return salts.Pair{}, fmt.Errorf("remote salts: app returned %s", response.Status)
	}

	var payload SaltResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return salts.Pair{}, fmt.Errorf("remote salts: decode: %w", err)
	}
	if len(payload.Current) != salts.Size || payload.Day != salts.Day(s.clock()) {
		return salts.Pair{}, fmt.Errorf("remote salts: response is not current")
	}
	if len(payload.Previous) != 0 && len(payload.Previous) != salts.Size {
		return salts.Pair{}, fmt.Errorf("remote salts: previous salt has invalid length")
	}

	pair := salts.Pair{Current: payload.Current, Previous: payload.Previous, Day: payload.Day}
	s.mu.Lock()
	s.cached.Erase()
	s.cached = cloneRemotePair(pair)
	answer := cloneRemotePair(s.cached)
	s.mu.Unlock()
	pair.Erase()

	return answer, nil
}

// Run refreshes on the same cadence as the app authority and erases the cache
// when the process stops.
func (s *RemoteSalts) Run(ctx context.Context, onError func(error)) {
	ticker := time.NewTicker(salts.RefreshInterval)
	defer ticker.Stop()
	defer func() {
		s.mu.Lock()
		s.cached.Erase()
		s.mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pair, err := s.Refresh(ctx)
			pair.Erase()
			if err != nil && onError != nil {
				onError(err)
			}
		}
	}
}

// client returns the configured client or a bounded default.
func (s *RemoteSalts) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}

	return &http.Client{Timeout: 10 * time.Second}
}

// clock returns the injected UTC clock or wall time.
func (s *RemoteSalts) clock() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}

	return time.Now().UTC()
}

// cloneRemotePair ensures request-local erasure never touches the shared
// in-memory snapshot.
func cloneRemotePair(pair salts.Pair) salts.Pair {
	return salts.Pair{
		Current: append([]byte(nil), pair.Current...), Previous: append([]byte(nil), pair.Previous...), Day: pair.Day,
	}
}
