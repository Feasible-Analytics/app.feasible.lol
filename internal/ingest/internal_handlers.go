//
// internal_handlers.go
// The private app-shard endpoints used by store-and-forward ingesters.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

const (
	// InternalDomainsPath returns the domains owned by one app shard.
	InternalDomainsPath = "/internal/domains"

	// InternalIngestPath accepts derived batches from the store-and-forward tier.
	InternalIngestPath = "/internal/ingest"
)

// RoutedSite is one domain record returned to ingesters. Blocked IP rules are
// the sole account-specific policy copied forward because they must run before
// the raw address is discarded.
type RoutedSite struct {
	Site       sites.Site `json:"site"`
	BlockedIPs []string   `json:"blocked_ips,omitempty"`
}

// DomainsResponse is one versioned routing snapshot from an app shard.
type DomainsResponse struct {
	Shard int          `json:"shard"`
	Sites []RoutedSite `json:"sites"`
}

// IngestBatch is the private delivery envelope.
type IngestBatch struct {
	Events []Event `json:"events"`
}

// IngestResponse names exactly what committed and what this shard does not
// own. A sender can therefore delete, reroute, or retain each row independently.
type IngestResponse struct {
	Committed []uuid.UUID `json:"committed,omitempty"`
	NotMine   []string    `json:"not_mine,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// RoutingShields exposes only the IP rules that must cross into the ingest
// tier, avoiding any dependency on account-side policy implementation.
type RoutingShields interface {
	BlockedIPPrefixes(siteID int64) []string
}

// BatchWriter is the app shard's durable account writer. Keeping the protocol
// against this narrow boundary makes ownership checks independently testable.
type BatchWriter interface {
	Write(context.Context, []Event) ([]uuid.UUID, error)
}

// InternalShard serves the app half of the private delivery protocol.
type InternalShard struct {
	ID      int
	Sites   *sites.Cache
	Shields RoutingShields
	Writer  BatchWriter
}

// Handler builds the private routes. Authentication and private-interface
// binding are applied by the CLI so tests can exercise the protocol directly.
func (s *InternalShard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+InternalDomainsPath, s.handleDomains)
	mux.HandleFunc("POST "+InternalIngestPath, s.handleIngest)

	return mux
}

// handleDomains publishes only sites this shard currently accepts traffic for.
func (s *InternalShard) handleDomains(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	response := DomainsResponse{Shard: s.ID}
	for _, site := range s.Sites.All() {
		if site.AcceptTrafficUntil > 0 && now > site.AcceptTrafficUntil {
			continue
		}
		routed := RoutedSite{Site: site}
		if s.Shields != nil {
			routed.BlockedIPs = s.Shields.BlockedIPPrefixes(site.ID)
		}
		response.Sites = append(response.Sites, routed)
	}
	sort.Slice(response.Sites, func(i, j int) bool {
		return sites.Normalise(response.Sites[i].Site.Domain) < sites.Normalise(response.Sites[j].Site.Domain)
	})

	body, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "routing snapshot could not be encoded", http.StatusInternalServerError)
		return
	}
	digest := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(digest[:]) + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

// handleIngest validates ownership before committing a batch. A stale sender
// receives not_mine for the affected domains rather than authority to delete.
func (s *InternalShard) handleIngest(w http.ResponseWriter, r *http.Request) {
	var batch IngestBatch
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes*1000))
	if err := decoder.Decode(&batch); err != nil {
		http.Error(w, "internal ingest batch is invalid", http.StatusBadRequest)
		return
	}

	response := IngestResponse{}
	accepted := make([]Event, 0, len(batch.Events))
	notMine := map[string]struct{}{}
	for _, event := range batch.Events {
		site, ok := s.Sites.Lookup(event.Domain)
		if !ok || site.ID != event.SiteID || site.AccountID != event.AccountID ||
			(site.AcceptTrafficUntil > 0 && time.Now().Unix() > site.AcceptTrafficUntil) {
			notMine[event.Domain] = struct{}{}
			continue
		}
		accepted = append(accepted, event)
	}
	for domain := range notMine {
		response.NotMine = append(response.NotMine, domain)
	}
	sort.Strings(response.NotMine)

	if len(accepted) > 0 {
		if s.Writer == nil {
			response.Error = "account writer is unavailable"
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		committed, err := s.Writer.Write(r.Context(), accepted)
		response.Committed = committed
		if err != nil {
			response.Error = err.Error()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(response)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
