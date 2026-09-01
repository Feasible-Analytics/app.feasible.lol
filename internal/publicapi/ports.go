//
// ports.go
// The seams where this API meets features it does not own.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"context"
	"errors"
)

// Goals, funnels, shields and annotations are owned elsewhere in the product and
// do not exist yet. Every one of them has an endpoint and an MCP tool here
// anyway, because the alternative — leaving the route out until the feature
// lands — is worse in a specific way: a 404 tells an integrator their URL is
// wrong and sends them to check their code, where a 501 naming the feature tells
// them the truth and lets them come back later.
//
// Each is a narrow interface rather than a direct dependency so that wiring the
// real implementation in is one line in `serve`, and so this package's tests do
// not need the feature to exist.

// ErrNotAvailable is what an unimplemented dependency answers with.
var ErrNotAvailable = errors.New("not available yet")

// ErrInvalid marks a domain validation failure from an account-backed adapter.
var ErrInvalid = errors.New("invalid conversion definition")

// Goal is one conversion a site counts.
type Goal struct {
	ID          int64  `json:"id"`
	SiteID      int64  `json:"-"`
	Kind        string `json:"kind,omitempty"`
	DisplayName string `json:"display_name"`

	// EventName and PagePath are the two kinds of goal, and exactly one of them
	// is set. They are separate fields rather than one polymorphic value because
	// a caller has to know which kind it is to render it.
	EventName string `json:"event_name,omitempty"`
	PagePath  string `json:"page_path,omitempty"`

	Currency    string         `json:"currency,omitempty"`
	ScrollDepth int            `json:"scroll_depth,omitempty"`
	IsRevenue   bool           `json:"is_revenue,omitempty"`
	IsAutomatic bool           `json:"is_automatic,omitempty"`
	CreatedAt   int64          `json:"created_at,omitempty"`
	Properties  []GoalProperty `json:"properties,omitempty"`
}

// GoalProperty is one equality constraint on a conversion definition.
type GoalProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// GoalStore is the goals feature as this API needs it.
type GoalStore interface {
	ListGoals(ctx context.Context, siteID int64) ([]Goal, error)
	CreateGoal(ctx context.Context, siteID int64, goal Goal) (*Goal, error)
	DeleteGoal(ctx context.Context, siteID, goalID int64) error
}

// GoalUpdater is the optional management extension for builds that support
// changing a definition in place.
type GoalUpdater interface {
	UpdateGoal(ctx context.Context, siteID, goalID int64, goal Goal) (*Goal, error)
}

// FunnelManager is the optional write surface behind funnel settings and API
// automation.
type FunnelManager interface {
	CreateFunnel(ctx context.Context, siteID int64, funnel Funnel) (*Funnel, error)
	UpdateFunnel(ctx context.Context, siteID, funnelID int64, funnel Funnel) (*Funnel, error)
	DeleteFunnel(ctx context.Context, siteID, funnelID int64) error
}

// FunnelStep is one stage of a funnel.
type FunnelStep struct {
	GoalID      int64  `json:"goal_id"`
	DisplayName string `json:"display_name"`
}

// Funnel is an ordered set of goals people are expected to move through.
type Funnel struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	StrictOrder bool         `json:"strict_order"`
	Steps       []FunnelStep `json:"steps"`
}

// FunnelReport is a funnel with the numbers filled in.
type FunnelReport struct {
	Funnel        Funnel    `json:"funnel"`
	EntryVisitors int64     `json:"entry_visitors"`
	StepVisitors  []int64   `json:"step_visitors"`
	StepRates     []float64 `json:"step_conversion_rates"`
}

// FunnelStore is the funnels feature as this API needs it.
type FunnelStore interface {
	ListFunnels(ctx context.Context, siteID int64) ([]Funnel, error)
	GetFunnel(ctx context.Context, siteID, funnelID int64, from, to string) (*FunnelReport, error)
}

// CustomPropertyStore manages the one property registry analytics queries use.
type CustomPropertyStore interface {
	ListProperties(ctx context.Context, siteID int64) ([]CustomProperty, error)
	CreateProperty(ctx context.Context, siteID int64, name, scope string) (*CustomProperty, error)
	DeleteProperty(ctx context.Context, siteID, propertyID int64) error
}

// ShieldRule is one thing that is blocked from being counted.
type ShieldRule struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// ShieldStore is the traffic-filtering feature as this API needs it.
type ShieldStore interface {
	ListShields(ctx context.Context, siteID int64) ([]ShieldRule, error)
	AddShieldRule(ctx context.Context, siteID int64, rule ShieldRule) (*ShieldRule, error)
}

// Annotation is a note pinned to a date on a site's charts.
type Annotation struct {
	ID     int64  `json:"id"`
	Date   string `json:"date"`
	Note   string `json:"note"`
	SiteID int64  `json:"-"`
}

// AnnotationStore is the annotations feature as this API needs it.
type AnnotationStore interface {
	ListAnnotations(ctx context.Context, siteID int64, from, to string) ([]Annotation, error)
	CreateAnnotation(ctx context.Context, siteID int64, annotation Annotation) (*Annotation, error)
}

// unavailable is the message an endpoint gives when its feature is not wired in
// yet. It names the feature so the answer is actionable rather than merely
// discouraging.
func unavailable(feature string) string {
	return feature + " are not available on this build yet — the endpoint exists and its shape is final, but nothing is behind it"
}
