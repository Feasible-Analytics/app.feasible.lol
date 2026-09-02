//
// conversions.go
// Public API and MCP adapters for the account-backed conversion platform.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/goals"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/publicapi"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/query"
)

// conversionStore bridges site-scoped public contracts to the owning account
// database without moving conversion definitions back into system.db.
type conversionStore struct {
	control  *sql.DB
	accounts *accounts.Manager
	now      func() time.Time
}

// accountFor resolves which account database owns a site.
func (s *conversionStore) accountFor(ctx context.Context, siteID int64) (int64, string, error) {
	var accountID int64
	var timezone string
	if err := s.control.QueryRowContext(ctx,
		"SELECT account_id, timezone FROM sites WHERE id = ?", siteID).Scan(&accountID, &timezone); err != nil {
		return 0, "", fmt.Errorf("conversions: resolve site: %w", err)
	}
	return accountID, timezone, nil
}

// clock returns the injected clock or current UTC time.
func (s *conversionStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// ListGoals returns the account-backed definitions in the public wire shape.
func (s *conversionStore) ListGoals(ctx context.Context, siteID int64) ([]publicapi.Goal, error) {
	accountID, _, err := s.accountFor(ctx, siteID)
	if err != nil {
		return nil, err
	}
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck // the read error is more useful than an unlock error
	list, err := goals.List(ctx, lease.Account.Reader(), siteID)
	if err != nil {
		return nil, err
	}
	answer := make([]publicapi.Goal, 0, len(list))
	for _, goal := range list {
		answer = append(answer, publicGoal(goal))
	}
	return answer, nil
}

// CreateGoal stores one public goal definition.
func (s *conversionStore) CreateGoal(ctx context.Context, siteID int64, input publicapi.Goal) (*publicapi.Goal, error) {
	accountID, _, err := s.accountFor(ctx, siteID)
	if err != nil {
		return nil, err
	}
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck // the write error is more useful than an unlock error
	created, err := goals.Create(ctx, lease.Account.Writer(), internalGoal(siteID, input), s.clock())
	if err != nil {
		return nil, conversionError(err)
	}
	answer := publicGoal(created)
	return &answer, nil
}

// UpdateGoal replaces one site-owned goal in place.
func (s *conversionStore) UpdateGoal(ctx context.Context, siteID, goalID int64, input publicapi.Goal) (*publicapi.Goal, error) {
	accountID, _, err := s.accountFor(ctx, siteID)
	if err != nil {
		return nil, err
	}
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck // the write error is more useful than an unlock error
	goal := internalGoal(siteID, input)
	goal.ID = goalID
	updated, err := goals.Update(ctx, lease.Account.Writer(), goal)
	if err != nil {
		if errors.Is(err, goals.ErrNotFound) {
			return nil, publicapi.ErrNotFound
		}
		return nil, conversionError(err)
	}
	answer := publicGoal(updated)
	return &answer, nil
}

// DeleteGoal removes a site-owned goal.
func (s *conversionStore) DeleteGoal(ctx context.Context, siteID, goalID int64) error {
	accountID, _, err := s.accountFor(ctx, siteID)
	if err != nil {
		return err
	}
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return err
	}
	defer lease.Release() //nolint:errcheck // the delete error is more useful than an unlock error
	goal, err := goals.Get(ctx, lease.Account.Reader(), goalID)
	if errors.Is(err, goals.ErrNotFound) || (err == nil && goal.SiteID != siteID) {
		return publicapi.ErrNotFound
	}
	if err != nil {
		return err
	}
	return conversionError(goals.Delete(ctx, lease.Account.Writer(), goalID))
}

// publicGoal maps the full internal definition without losing constraints or
// scroll/revenue metadata needed by management clients.
func publicGoal(goal goals.Goal) publicapi.Goal {
	properties := make([]publicapi.GoalProperty, 0, len(goal.Properties))
	for _, property := range goal.Properties {
		properties = append(properties, publicapi.GoalProperty{Name: property.Name, Value: property.Value})
	}
	return publicapi.Goal{
		ID: goal.ID, SiteID: goal.SiteID, Kind: string(goal.Kind), DisplayName: goal.DisplayName,
		EventName: goal.EventName, PagePath: goal.PagePattern, ScrollDepth: goal.ScrollDepth,
		Currency: goal.Currency, IsRevenue: goal.IsRevenue, IsAutomatic: goal.IsAutomatic,
		CreatedAt: goal.CreatedAt, Properties: properties,
	}
}

// internalGoal maps a public definition into the conversion domain model.
func internalGoal(siteID int64, goal publicapi.Goal) goals.Goal {
	kind := goals.Kind(goal.Kind)
	if kind == "" {
		if goal.EventName != "" {
			kind = goals.KindEvent
		} else {
			kind = goals.KindPage
		}
	}
	properties := make([]goals.PropertyConstraint, 0, len(goal.Properties))
	for _, property := range goal.Properties {
		properties = append(properties, goals.PropertyConstraint{Name: property.Name, Value: property.Value})
	}
	return goals.Goal{
		ID: goal.ID, SiteID: siteID, Kind: kind, DisplayName: goal.DisplayName,
		EventName: goal.EventName, PagePattern: goal.PagePath, ScrollDepth: goal.ScrollDepth,
		IsRevenue: goal.IsRevenue || goal.Currency != "", Currency: goal.Currency, Properties: properties,
	}
}

// ListFunnels returns all configured funnels and their named steps.
func (s *conversionStore) ListFunnels(ctx context.Context, siteID int64) ([]publicapi.Funnel, error) {
	accountID, _, err := s.accountFor(ctx, siteID)
	if err != nil {
		return nil, err
	}
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck // the read error is more useful than an unlock error
	list, err := goals.ListFunnels(ctx, lease.Account.Reader(), siteID)
	if err != nil {
		return nil, err
	}
	answer := make([]publicapi.Funnel, 0, len(list))
	for _, funnel := range list {
		answer = append(answer, publicFunnel(funnel))
	}
	return answer, nil
}

// CreateFunnel stores one funnel definition.
func (s *conversionStore) CreateFunnel(ctx context.Context, siteID int64, input publicapi.Funnel) (*publicapi.Funnel, error) {
	accountID, _, err := s.accountFor(ctx, siteID)
	if err != nil {
		return nil, err
	}
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck // the write error is more useful than an unlock error
	created, err := goals.CreateFunnel(ctx, lease.Account.Writer(), internalFunnel(siteID, input), s.clock())
	if err != nil {
		return nil, conversionError(err)
	}
	answer := publicFunnel(created)
	return &answer, nil
}

// UpdateFunnel replaces one site-owned funnel definition.
func (s *conversionStore) UpdateFunnel(ctx context.Context, siteID, funnelID int64, input publicapi.Funnel) (*publicapi.Funnel, error) {
	accountID, _, err := s.accountFor(ctx, siteID)
	if err != nil {
		return nil, conversionError(err)
	}
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck // the write error is more useful than an unlock error
	funnel := internalFunnel(siteID, input)
	funnel.ID = funnelID
	updated, err := goals.UpdateFunnel(ctx, lease.Account.Writer(), funnel)
	if err != nil {
		if errors.Is(err, goals.ErrNotFound) {
			return nil, publicapi.ErrNotFound
		}
		return nil, err
	}
	answer := publicFunnel(updated)
	return &answer, nil
}

// DeleteFunnel removes one site-owned funnel.
func (s *conversionStore) DeleteFunnel(ctx context.Context, siteID, funnelID int64) error {
	accountID, _, err := s.accountFor(ctx, siteID)
	if err != nil {
		return err
	}
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return err
	}
	defer lease.Release() //nolint:errcheck // the delete error is more useful than an unlock error
	funnel, err := goals.GetFunnel(ctx, lease.Account.Reader(), funnelID)
	if errors.Is(err, goals.ErrNotFound) || (err == nil && funnel.SiteID != siteID) {
		return publicapi.ErrNotFound
	}
	if err != nil {
		return err
	}
	return conversionError(goals.DeleteFunnel(ctx, lease.Account.Writer(), funnelID))
}

// internalFunnel maps a public funnel request into ordered internal steps.
func internalFunnel(siteID int64, funnel publicapi.Funnel) goals.Funnel {
	steps := make([]goals.Step, 0, len(funnel.Steps))
	for _, step := range funnel.Steps {
		steps = append(steps, goals.Step{GoalID: step.GoalID})
	}
	return goals.Funnel{ID: funnel.ID, SiteID: siteID, Name: funnel.Name, StrictOrder: funnel.StrictOrder, Steps: steps}
}

// GetFunnel runs a public funnel report over an explicit UTC range.
func (s *conversionStore) GetFunnel(ctx context.Context, siteID, funnelID int64, from, to string) (*publicapi.FunnelReport, error) {
	accountID, timezone, err := s.accountFor(ctx, siteID)
	if err != nil {
		return nil, err
	}
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck // the report error is more useful than an unlock error
	dateRange := query.DateRange{Preset: query.RangeLast28Days}
	if from != "" || to != "" {
		if from == "" || to == "" {
			return nil, fmt.Errorf("from and to must be supplied together")
		}
		start, startDateOnly, err := parseFunnelDate(from)
		if err != nil {
			return nil, fmt.Errorf("from must be YYYY-MM-DD or RFC3339: %w", err)
		}
		end, endDateOnly, err := parseFunnelDate(to)
		if err != nil {
			return nil, fmt.Errorf("to must be YYYY-MM-DD or RFC3339: %w", err)
		}
		dateRange = query.DateRange{Preset: query.RangeCustom, Start: start, End: end, DateOnly: startDateOnly && endDateOnly}
	}
	engine := query.New(lease.Account.Reader())
	result, err := goals.RunFunnel(ctx, lease.Account.Reader(), engine, goals.FunnelRequest{
		FunnelID: funnelID, Timezone: timezone,
		DateRange: dateRange, Exact: true,
	})
	if err != nil {
		if errors.Is(err, goals.ErrNotFound) {
			return nil, publicapi.ErrNotFound
		}
		return nil, err
	}
	if result.Funnel.SiteID != siteID {
		return nil, publicapi.ErrNotFound
	}
	visitors := make([]int64, len(result.Steps))
	rates := make([]float64, len(result.Steps))
	for i, step := range result.Steps {
		visitors[i], rates[i] = step.Visitors, step.ConversionRate
	}
	entry := int64(0)
	if len(visitors) > 0 {
		entry = visitors[0]
	}
	return &publicapi.FunnelReport{
		Funnel: publicFunnel(result.Funnel), EntryVisitors: entry,
		StepVisitors: visitors, StepRates: rates,
	}, nil
}

// parseFunnelDate accepts the date-only contract advertised by MCP and the
// precise RFC3339 form used by API clients.
func parseFunnelDate(value string) (time.Time, bool, error) {
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed, true, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	return parsed, false, err
}

// conversionError translates domain validation and not-found errors into the
// public API's stable status sentinels without hiding operational failures.
func conversionError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, goals.ErrNotFound) {
		return publicapi.ErrNotFound
	}
	var invalid *goals.Error
	if errors.As(err, &invalid) {
		return fmt.Errorf("%w: %s", publicapi.ErrInvalid, invalid.Error())
	}
	return err
}

// publicFunnel maps one internal funnel into the stable public wire shape.
func publicFunnel(funnel goals.Funnel) publicapi.Funnel {
	steps := make([]publicapi.FunnelStep, 0, len(funnel.Steps))
	for _, step := range funnel.Steps {
		steps = append(steps, publicapi.FunnelStep{GoalID: step.GoalID, DisplayName: step.Goal.Label()})
	}
	return publicapi.Funnel{ID: funnel.ID, Name: funnel.Name, StrictOrder: funnel.StrictOrder, Steps: steps}
}

// ListProperties reads the canonical account-backed property registry.
func (s *conversionStore) ListProperties(ctx context.Context, siteID int64) ([]publicapi.CustomProperty, error) {
	accountID, _, err := s.accountFor(ctx, siteID)
	if err != nil {
		return nil, err
	}
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck // the read error is more useful than an unlock error
	list, err := goals.Allowed(ctx, lease.Account.Reader(), siteID)
	if err != nil {
		return nil, err
	}
	answer := make([]publicapi.CustomProperty, 0, len(list))
	for _, property := range list {
		answer = append(answer, publicapi.CustomProperty{ID: property.ID, Key: property.Name, Scope: string(property.Scope), CreatedAt: property.CreatedAt})
	}
	return answer, nil
}

// CreateProperty registers a property with explicit event or session scope.
func (s *conversionStore) CreateProperty(ctx context.Context, siteID int64, name, scope string) (*publicapi.CustomProperty, error) {
	accountID, _, err := s.accountFor(ctx, siteID)
	if err != nil {
		return nil, err
	}
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return nil, err
	}
	defer lease.Release() //nolint:errcheck // the write error is more useful than an unlock error
	property, err := goals.Allow(ctx, lease.Account.Writer(), siteID, name, goals.Scope(scope), s.clock())
	if err != nil {
		return nil, err
	}
	return &publicapi.CustomProperty{ID: property.ID, Key: property.Name, Scope: string(property.Scope), CreatedAt: property.CreatedAt}, nil
}

// DeleteProperty removes one site-owned property registration by id.
func (s *conversionStore) DeleteProperty(ctx context.Context, siteID, propertyID int64) error {
	accountID, _, err := s.accountFor(ctx, siteID)
	if err != nil {
		return err
	}
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return err
	}
	defer lease.Release() //nolint:errcheck // the delete error is more useful than an unlock error
	var name string
	if err := lease.Account.Reader().QueryRowContext(ctx,
		"SELECT name FROM allowed_properties WHERE id = ? AND site_id = ?", propertyID, siteID).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return publicapi.ErrNotFound
		}
		return err
	}
	return goals.Disallow(ctx, lease.Account.Writer(), siteID, name)
}

// ProvisionSite creates the automatic definitions every site receives.
func (s *conversionStore) ProvisionSite(ctx context.Context, accountID, siteID int64) error {
	lease, err := s.accounts.Acquire(ctx, accountID)
	if err != nil {
		return err
	}
	defer lease.Release() //nolint:errcheck // the provisioning error is more useful than an unlock error
	_, err = goals.EnsureAutomatic(ctx, lease.Account.Writer(), siteID, s.clock())
	return err
}

// provisionExistingSites idempotently backfills automatic goals before the
// server begins answering reads, keeping GET endpoints side-effect-free.
func provisionExistingSites(ctx context.Context, control *sql.DB, manager *accounts.Manager, now time.Time) error {
	rows, err := control.QueryContext(ctx, "SELECT id, account_id FROM sites ORDER BY account_id, id")
	if err != nil {
		return fmt.Errorf("conversions: list sites for provisioning: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type siteOwner struct{ siteID, accountID int64 }
	var sites []siteOwner
	for rows.Next() {
		var site siteOwner
		if err := rows.Scan(&site.siteID, &site.accountID); err != nil {
			return fmt.Errorf("conversions: list sites for provisioning: %w", err)
		}
		sites = append(sites, site)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("conversions: list sites for provisioning: %w", err)
	}
	for _, site := range sites {
		lease, err := manager.Acquire(ctx, site.accountID)
		if err != nil {
			return err
		}
		_, provisionErr := goals.EnsureAutomatic(ctx, lease.Account.Writer(), site.siteID, now)
		releaseErr := lease.Release()
		if provisionErr != nil {
			return provisionErr
		}
		if releaseErr != nil {
			return releaseErr
		}
	}
	return nil
}
