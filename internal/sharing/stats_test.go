//
// stats_test.go
// Direct stats capability checks: revocation, privacy, passwords and segments.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package sharing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// TestStatsCapabilitiesAreRevalidatedOnEveryRequest drives the authorization
// layer directly so a request cannot bypass a revoked link or private toggle by
// calling /api/stats instead of loading the shared dashboard first.
func TestStatsCapabilitiesAreRevalidatedOnEveryRequest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	secret := DeriveSecret([]byte("test-root"))
	authorizer := StatsAuthorizer{Store: f.store, Secret: secret}

	request := httptest.NewRequest(http.MethodPost, "/api/stats/"+f.domain+"/query", nil)
	if _, presented, err := authorizer.Authorize(request, f.domain); presented || !errors.Is(err, ErrNoStatsCapability) {
		t.Fatalf("request with no capability = presented %v, %v", presented, err)
	}

	request.Header.Set(HeaderPublic, "public")
	if _, _, err := authorizer.Authorize(request, f.domain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("private site accepted public capability: %v", err)
	}
	if err := f.store.SetPublicForOwner(ctx, f.siteID, f.teamID, true); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, _, err := authorizer.Authorize(request, f.domain); err != nil {
		t.Fatalf("public site refused capability: %v", err)
	}
	if err := f.store.SetPublicForOwner(ctx, f.siteID, f.teamID, false); err != nil {
		t.Fatalf("make private: %v", err)
	}
	if _, _, err := authorizer.Authorize(request, f.domain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("private toggle did not revoke direct stats access: %v", err)
	}

	link, err := f.store.CreateLinkForOwner(ctx, f.siteID, f.teamID, "temporary", "", 0, 0)
	if err != nil {
		t.Fatalf("create link: %v", err)
	}
	request.Header.Del(HeaderPublic)
	request.Header.Set(HeaderShare, link.Slug)
	if _, _, err := authorizer.Authorize(request, f.domain); err != nil {
		t.Fatalf("open share refused: %v", err)
	}
	if err := f.store.RevokeLinkForOwner(ctx, f.siteID, f.teamID, link.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, _, err := authorizer.Authorize(request, f.domain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked link retained direct stats access: %v", err)
	}
}

// TestProtectedAndSegmentedStatsCapabilitiesFailClosed checks both restrictions
// that would be lost if the frontend were trusted to enforce them.
func TestProtectedAndSegmentedStatsCapabilitiesFailClosed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	secret := DeriveSecret([]byte("test-root"))
	authorizer := StatsAuthorizer{Store: f.store, Secret: secret}

	filter := query.Filter{Operator: "is", Dimension: "visit:country", Values: []string{"US"}}
	segmentID, err := f.store.CreateSegment(ctx, f.siteID, "US only", []query.Filter{filter})
	if err != nil {
		t.Fatalf("create segment: %v", err)
	}
	link, err := f.store.CreateLinkForOwner(ctx, f.siteID, f.teamID, "client", "hunter2", segmentID, 0)
	if err != nil {
		t.Fatalf("create protected link: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/stats/"+f.domain+"/query", nil)
	request.Header.Set(HeaderShare, link.Slug)
	if _, _, err := authorizer.Authorize(request, f.domain); !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("protected direct request without cookie = %v", err)
	}

	request.AddCookie(&http.Cookie{Name: CookieName(link.Slug), Value: SignSlug(secret, link.Slug)})
	access, _, err := authorizer.Authorize(request, f.domain)
	if err != nil {
		t.Fatalf("solved protected link: %v", err)
	}
	if len(access.Filters) != 1 || access.Filters[0].Dimension != filter.Dimension || access.Filters[0].Values[0] != "US" {
		t.Fatalf("pinned segment was not applied server-side: %+v", access.Filters)
	}

	if _, err := f.db.Exec(`DELETE FROM saved_segments WHERE id = ?`, segmentID); err != nil {
		t.Fatalf("delete segment: %v", err)
	}
	if _, _, err := authorizer.Authorize(request, f.domain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing pinned segment widened access: %v", err)
	}
}
