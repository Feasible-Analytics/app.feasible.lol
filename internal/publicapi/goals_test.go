//
// goals_test.go
// Public goal and funnel validation and live-role authorization.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"context"
	"net/http"
	"testing"
)

// conversionStoreFixture records destructive calls while implementing both
// conversion store ports used by the HTTP handlers.
type conversionStoreFixture struct {
	deletedGoal   bool
	deletedFunnel bool
}

// ListGoals returns an empty configured-goal list for authorization tests.
func (s *conversionStoreFixture) ListGoals(context.Context, int64) ([]Goal, error) {
	return []Goal{}, nil
}

// CreateGoal echoes the validated goal for authorization tests.
func (s *conversionStoreFixture) CreateGoal(_ context.Context, _ int64, goal Goal) (*Goal, error) {
	return &goal, nil
}

// DeleteGoal records that the handler reached the destructive store method.
func (s *conversionStoreFixture) DeleteGoal(context.Context, int64, int64) error {
	s.deletedGoal = true
	return nil
}

// ListFunnels returns an empty configured-funnel list for authorization tests.
func (s *conversionStoreFixture) ListFunnels(context.Context, int64) ([]Funnel, error) {
	return []Funnel{}, nil
}

// GetFunnel returns an empty report because deletion does not read it first.
func (s *conversionStoreFixture) GetFunnel(context.Context, int64, int64, string, string) (*FunnelReport, error) {
	return &FunnelReport{}, nil
}

// CreateFunnel echoes the validated funnel for authorization tests.
func (s *conversionStoreFixture) CreateFunnel(_ context.Context, _ int64, funnel Funnel) (*Funnel, error) {
	return &funnel, nil
}

// UpdateFunnel echoes the validated funnel for authorization tests.
func (s *conversionStoreFixture) UpdateFunnel(_ context.Context, _ int64, _ int64, funnel Funnel) (*Funnel, error) {
	return &funnel, nil
}

// DeleteFunnel records that the handler reached the destructive store method.
func (s *conversionStoreFixture) DeleteFunnel(context.Context, int64, int64) error {
	s.deletedFunnel = true
	return nil
}

// TestMalformedGoalDefinitionsAreBadRequests checks every explicit kind keeps
// only the fields it understands and never leaks a domain validation error as
// a server failure.
func TestMalformedGoalDefinitionsAreBadRequests(t *testing.T) {
	cases := []Goal{
		{Kind: "page", EventName: "Signup"},
		{Kind: "event", EventName: "Signup", PagePath: "/thanks"},
		{Kind: "scroll", PagePath: "/article", ScrollDepth: 101},
		{Kind: "event", EventName: "Purchase", Currency: "12$"},
		{Kind: "event", EventName: "Signup", Properties: []GoalProperty{{Name: ""}}},
	}
	for _, goal := range cases {
		if _, err := ValidateGoalDefinition(goal); err == nil {
			t.Errorf("malformed goal was accepted: %+v", goal)
		}
	}

	goal, err := ValidateGoalDefinition(Goal{PagePath: "/article", ScrollDepth: 75})
	if err != nil || goal.Kind != "scroll" {
		t.Fatalf("implicit scroll goal = %+v, %v", goal, err)
	}
}

// TestDemotedMembersCannotDeleteConversions proves deletion checks the live
// role behind an existing API key. Otherwise a key minted while somebody was
// an owner could keep destroying settings after their role was reduced.
func TestDemotedMembersCannotDeleteConversions(t *testing.T) {
	h := newHarness(t)
	store := &conversionStoreFixture{}
	h.API.Goals, h.API.Funnels = store, store
	if _, err := h.System.Exec(`UPDATE team_memberships SET role = 'viewer' WHERE team_id = ? AND user_id = 1`, teamID); err != nil {
		t.Fatal(err)
	}

	status, body := h.do(t, http.MethodDelete, "/api/v1/sites/goals/1?site_id=example.com", "", h.Key)
	if status != http.StatusForbidden {
		t.Fatalf("goal delete status = %d, want 403 (%s)", status, body)
	}
	status, body = h.do(t, http.MethodDelete, "/api/v1/sites/funnels/1?site_id=example.com", "", h.Key)
	if status != http.StatusForbidden {
		t.Fatalf("funnel delete status = %d, want 403 (%s)", status, body)
	}
	if store.deletedGoal || store.deletedFunnel {
		t.Fatalf("demoted member reached destructive store: %+v", store)
	}
}
