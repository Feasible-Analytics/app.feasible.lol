//
// exports.go
// The surface the MCP server drives this API through.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// The MCP server is a second front end onto this API, not a second
// implementation of it. Everything here exists so a tool call and an HTTP
// request take the same path to the same answer: if a tool could reach the
// database another way, an assistant and a dashboard would eventually disagree
// about a number and nobody could say which was right.

// ErrForbidden means the key may not see the site it named. It is a distinct
// error so the MCP layer can turn it into a tool result the model can read,
// rather than a protocol failure that just ends the conversation.
var ErrForbidden = errors.New("this key cannot see that site")

// SitesFor lists the sites a key may read, in domain order.
func (a *API) SitesFor(key *apikeys.Key) []sites.Site {
	if !KeyCan(key, teams.PermViewDashboard) {
		return nil
	}

	return a.authorisedSites(key)
}

// SiteFor resolves one site a key may read.
func (a *API) SiteFor(key *apikeys.Key, identifier string) (sites.Site, error) {
	if !KeyCan(key, teams.PermViewDashboard) {
		return sites.Site{}, ErrForbidden
	}

	if identifier == "" {
		return sites.Site{}, errors.New("site_id is required — pass the site's domain")
	}

	site, ok := a.Sites.Lookup(identifier)
	if !ok || site.TeamID != key.TeamID {
		return sites.Site{}, ErrForbidden
	}

	return site, nil
}

// Location is the timezone a site's days are bucketed in, falling back to UTC
// for a stored value we cannot parse. Answering in the wrong timezone is far
// better than refusing to answer at all, and the fallback is visible because
// every response echoes the timezone it used.
func Location(site sites.Site) *time.Location {
	location, err := time.LoadLocation(site.Timezone)
	if err != nil {
		return time.UTC
	}

	return location
}

// NewSite registers a domain on behalf of a key, and publishes the event the
// same way the HTTP endpoint does — an assistant creating a site has to look
// identical to a script creating one, or an agency's automation fires for half
// their sites.
func (a *API) NewSite(ctx context.Context, key *apikeys.Key, domain, displayName, timezone string) (*Site, error) {
	if !KeyCan(key, teams.PermManageSites) {
		return nil, ErrForbidden
	}

	clean, err := validateDomain(domain)
	if err != nil {
		return nil, err
	}

	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		timezone = "Etc/UTC"
	}

	if _, err := time.LoadLocation(timezone); err != nil {
		return nil, badParam("timezone must be an IANA name such as America/Los_Angeles, not %q", timezone)
	}

	site, err := a.System.CreateSite(ctx, key.TeamID, clean, displayName, timezone)
	if err != nil {
		return nil, err
	}
	if a.ProvisionSite != nil {
		if err := a.ProvisionSite(ctx, key.TeamID, site.ID); err != nil {
			return nil, err
		}
	}

	a.refreshSites(ctx)

	// A site created programmatically is the event an agency's own automation
	// hangs off — provisioning a client, kicking off onboarding — so it is
	// published rather than left as something only our database knows.
	a.publishEvent(ctx, key.TeamID, site)

	return site, nil
}

// publishEvent sends the site.created webhook without letting a webhook failure
// undo a site that already exists: publishing writes rows and returns, and a
// webhook that could not be queued must not undo a site that was created.
func (a *API) publishEvent(ctx context.Context, teamID int64, site *Site) {
	if a.Dispatcher == nil {
		return
	}

	if _, err := a.Dispatcher.Publish(ctx, teamID, siteCreatedEvent(site)); err != nil && a.Log != nil {
		a.Log.Error("webhook publish failed", "event", "site.created", "error", err)
	}
}

// EditSite changes a site on behalf of a key.
func (a *API) EditSite(ctx context.Context, key *apikeys.Key, site sites.Site, domain, displayName, timezone *string, isPublic *bool) (*Site, error) {
	if !KeyCan(key, teams.PermManageSiteSettings) {
		return nil, ErrForbidden
	}

	if domain != nil {
		clean, err := validateDomain(*domain)
		if err != nil {
			return nil, err
		}
		domain = &clean
	}

	if timezone != nil {
		if _, err := time.LoadLocation(*timezone); err != nil {
			return nil, badParam("timezone must be an IANA name such as America/Los_Angeles, not %q", *timezone)
		}
	}

	record, err := a.System.UpdateSite(ctx, key.TeamID, site.ID, domain, displayName, timezone, isPublic)
	if err != nil {
		return nil, err
	}

	a.refreshSites(ctx)

	return record, nil
}

// AllowedProperties lists a site's custom property allow list, which is part of
// what the MCP schema resource tells a model it may ask for. A build with no
// property registry answers with none, and the schema says why.
func (a *API) AllowedProperties(ctx context.Context, siteID int64) ([]CustomProperty, error) {
	if a.CustomProperties == nil {
		return nil, errors.New(unavailable("custom properties"))
	}

	return a.CustomProperties.ListProperties(ctx, siteID)
}

// ValidateGoalDefinition checks the complete goal shape used by MCP and other
// typed clients.
func ValidateGoalDefinition(goal Goal) (*Goal, error) {
	return validateGoal(goalRequest{
		Kind: goal.Kind, DisplayName: goal.DisplayName, EventName: goal.EventName,
		PagePath: goal.PagePath, ScrollDepth: goal.ScrollDepth, Currency: goal.Currency,
		Properties: goal.Properties,
	})
}

// ValidateGoalRequest checks a goal body without needing the goals feature to
// exist. The MCP tool uses it so that a model gets told its arguments are wrong
// even on a build where nothing is behind the tool.
func ValidateGoalRequest(displayName, eventName, pagePath, currency string) (*Goal, error) {
	return validateGoal(goalRequest{
		DisplayName: displayName,
		EventName:   eventName,
		PagePath:    pagePath,
		Currency:    currency,
	})
}

// Unavailable is the sentence an endpoint or tool gives for a feature this
// build does not carry yet.
func Unavailable(feature string) string {
	return unavailable(feature)
}
