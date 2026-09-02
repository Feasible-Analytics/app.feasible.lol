//
// sites.go
// The Sites API: full CRUD over sites and everything configured on one.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package publicapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/destructive"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sharing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/webhooks"
)

// The roles the API accepts, checked here rather than left to the database's
// CHECK constraint. A constraint violation comes back as a driver error nobody
// can act on; this comes back naming the roles that exist.
var (
	guestRoles  = []string{"guest_editor", "guest_viewer"}
	memberRoles = []string{"admin", "editor", "billing", "viewer"}
)

// decodeBody reads a JSON request body into a target, refusing anything it does
// not recognise. A misspelt field that is silently ignored is a setting that
// never applied and a support ticket nobody can reproduce.
func (a *API) decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes))
	if err != nil {
		a.fail(w, http.StatusBadRequest, "could not read the request body")
		return false
	}

	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		a.fail(w, http.StatusBadRequest, readableJSONError(err))
		return false
	}

	return true
}

// readableJSONError turns a decoder failure into something a caller can act on.
func readableJSONError(err error) string {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return "the request body is not valid JSON"
	}

	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		return "the field " + mismatch.Field + " has the wrong type"
	}

	if errors.Is(err, io.EOF) {
		return "the request body is empty"
	}

	message := err.Error()
	if strings.Contains(message, "unknown field") {
		return message + " — check the spelling; unknown fields are refused rather than ignored"
	}

	return message
}

// answerStoreError maps a store failure onto a status. It exists so that every
// endpoint answers a missing row and a conflict the same way, rather than each
// one inventing its own.
func (a *API) answerStoreError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, sharing.ErrNotFound), errors.Is(err, webhooks.ErrNotFound):
		a.fail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrConflict), errors.Is(err, sharing.ErrSiteOwnerChanged):
		a.fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrInvalid):
		a.fail(w, http.StatusBadRequest, err.Error())
	default:
		a.internal(w, what, err)
	}
}

// pageParams reads the shared pagination parameters. The default page size is a
// hundred everywhere in this API, so an integration that ignores pagination
// still gets a useful answer rather than one row or ten thousand.
func (a *API) pageParams(w http.ResponseWriter, r *http.Request) (limit, page int, ok bool) {
	values := r.URL.Query()

	limit, err := intParam(values, "limit", DefaultPageSize, 1, MaxPageSize)
	if err != nil {
		return 0, 0, a.refuse(w, err)
	}

	page, err = intParam(values, "page", 1, 1, 1_000_000)
	if err != nil {
		return 0, 0, a.refuse(w, err)
	}

	return limit, page, true
}

// siteFromPath resolves the {site_id} path segment, which carries a domain.
func (a *API) siteFromPath(w http.ResponseWriter, r *http.Request, scope string) (*apikeys.Key, sites.Site, bool) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return nil, sites.Site{}, false
	}

	if !a.requireScope(w, key, scope) {
		return nil, sites.Site{}, false
	}
	if !a.requirePermission(w, key, sitePermission(scope)) {
		return nil, sites.Site{}, false
	}

	site, ok := a.resolveSite(w, key, r.PathValue("site_id"))
	if !ok {
		return nil, sites.Site{}, false
	}

	return key, site, true
}

// siteFromQuery is the same resolution for the endpoints that carry the site in
// the query string, which is the shape the established Sites API uses for the
// collections hung off a site.
func (a *API) siteFromQuery(w http.ResponseWriter, r *http.Request, scope string) (*apikeys.Key, sites.Site, bool) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return nil, sites.Site{}, false
	}

	if !a.requireScope(w, key, scope) {
		return nil, sites.Site{}, false
	}
	if !a.requirePermission(w, key, sitePermission(scope)) {
		return nil, sites.Site{}, false
	}

	site, ok := a.resolveSite(w, key, r.URL.Query().Get("site_id"))
	if !ok {
		return nil, sites.Site{}, false
	}

	return key, site, true
}

// sitePermission maps the two site scopes onto the role permission needed for
// that kind of request. An unknown scope fails closed as a site mutation.
func sitePermission(scope string) teams.Permission {
	if scope == apikeys.ScopeSitesRead {
		return teams.PermViewDashboard
	}

	return teams.PermManageSiteSettings
}

// idFromPath reads a numeric path segment, refusing a non-number with a 400.
// This is the same class of bug as the incumbent's `page=foo`: an id straight
// out of a URL and into an integer parse is a 500 waiting for its first typo.
func (a *API) idFromPath(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)

	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		a.fail(w, http.StatusBadRequest, name+" must be a positive whole number, not "+strconv.Quote(raw))
		return 0, false
	}

	return id, true
}

// createSiteRequest is the body of a site creation.
type createSiteRequest struct {
	Domain      string `json:"domain"`
	DisplayName string `json:"display_name"`
	Timezone    string `json:"timezone"`
}

// handleCreateSite registers a domain.
func (a *API) handleCreateSite(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return
	}

	if !a.requireScope(w, key, apikeys.ScopeSitesProvision) {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageSites) {
		return
	}

	var request createSiteRequest
	if !a.decodeBody(w, r, &request) {
		return
	}

	domain, err := validateDomain(request.Domain)
	if err != nil {
		a.refuse(w, err)
		return
	}

	timezone := strings.TrimSpace(request.Timezone)
	if timezone == "" {
		timezone = "Etc/UTC"
	}

	if _, err := time.LoadLocation(timezone); err != nil {
		a.fail(w, http.StatusBadRequest,
			"timezone must be an IANA name such as America/Los_Angeles, not "+strconv.Quote(timezone))
		return
	}

	site, err := a.System.CreateSite(r.Context(), key.TeamID, domain, request.DisplayName, timezone)
	if err != nil {
		a.answerStoreError(w, "create site", err)
		return
	}
	if a.ProvisionSite != nil {
		if err := a.ProvisionSite(r.Context(), key.TeamID, site.ID); err != nil {
			a.answerStoreError(w, "provision site defaults", err)
			return
		}
	}

	// The routing snapshot is rebuilt immediately rather than at the next
	// refresh, so that a script installed the moment after this call returns is
	// already accepting events. Waiting out the refresh interval looks exactly
	// like a broken snippet to whoever just installed it.
	a.refreshSites(r)

	// A site created programmatically is the event an agency's own automation
	// hangs off — provisioning a client, kicking off onboarding — so it is
	// published rather than left as something only our database knows.
	a.publish(r, key.TeamID, siteCreatedEvent(site))

	a.write(w, http.StatusCreated, site)
}

// siteCreatedEvent builds the webhook payload for a new site. It is one
// function so that a site created over HTTP and one created through an MCP tool
// produce byte-identical events — a receiver that has to tell them apart is a
// receiver with two code paths and one of them untested.
func siteCreatedEvent(site *Site) webhooks.Event {
	return webhooks.Event{
		Type:   webhooks.EventSiteCreated,
		SiteID: &site.ID,
		Data:   site,
	}
}

// validateDomain refuses something that is not a hostname.
func validateDomain(raw string) (string, error) {
	domain := sites.Normalise(raw)

	if domain == "" {
		return "", badParam("domain is required, for example example.com")
	}

	if strings.ContainsAny(domain, " /\\?#") {
		return "", badParam("domain must be a bare hostname such as example.com, not a URL")
	}

	if !strings.Contains(domain, ".") {
		return "", badParam("domain %q does not look like a hostname", raw)
	}

	if len(domain) > 253 {
		return "", badParam("domain is longer than a hostname can be")
	}

	return domain, nil
}

// refreshSites rebuilds the routing snapshot after a write that changed it.
//
// A failure is logged rather than returned: the write already succeeded, and
// telling the caller their site was not created because a cache refresh failed
// would be a lie that makes them create it twice.
func (a *API) refreshSites(r *http.Request) {
	if a.Sites == nil {
		return
	}

	if err := a.Sites.Refresh(r.Context()); err != nil && a.Log != nil {
		a.Log.Error("site cache refresh failed after a provisioning write", "error", err)
	}
}

// publish sends a webhook event, if webhooks are wired in.
//
// It never blocks and never fails the request that produced it: publishing
// writes rows and returns, and a webhook that could not be queued must not undo
// a site that was created.
func (a *API) publish(r *http.Request, teamID int64, event webhooks.Event) {
	if a.Dispatcher == nil {
		return
	}

	if _, err := a.Dispatcher.Publish(r.Context(), teamID, event); err != nil && a.Log != nil {
		a.Log.Error("webhook publish failed", "event", event.Type, "error", err)
	}
}

// handleListSites returns one page of the key's sites.
func (a *API) handleListSites(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return
	}

	if !a.requireScope(w, key, apikeys.ScopeSitesRead) {
		return
	}
	if !a.requirePermission(w, key, teams.PermViewDashboard) {
		return
	}

	limit, page, ok := a.pageParams(w, r)
	if !ok {
		return
	}

	list, total, err := a.System.ListSites(r.Context(), key.TeamID, limit, (page-1)*limit)
	if err != nil {
		a.answerStoreError(w, "list sites", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{
		"sites": list,
		"meta": map[string]any{
			"page":        page,
			"limit":       limit,
			"total":       total,
			"total_pages": (total + limit - 1) / limit,
		},
	})
}

// handleGetSite reads one site.
func (a *API) handleGetSite(w http.ResponseWriter, r *http.Request) {
	key, site, ok := a.siteFromPath(w, r, apikeys.ScopeSitesRead)
	if !ok {
		return
	}

	record, err := a.System.GetSite(r.Context(), key.TeamID, site.Domain)
	if err != nil {
		a.answerStoreError(w, "get site", err)
		return
	}

	a.write(w, http.StatusOK, record)
}

// updateSiteRequest is the body of a site update. Every field is a pointer so
// that "not mentioned" and "set to the zero value" are different things — with
// plain fields, an update that only renames a site would also switch its public
// dashboard off.
type updateSiteRequest struct {
	Domain      *string `json:"domain"`
	DisplayName *string `json:"display_name"`
	Timezone    *string `json:"timezone"`
	IsPublic    *bool   `json:"is_public"`
}

// handleUpdateSite changes a site's settings.
func (a *API) handleUpdateSite(w http.ResponseWriter, r *http.Request) {
	key, site, ok := a.siteFromPath(w, r, apikeys.ScopeSitesProvision)
	if !ok {
		return
	}

	var request updateSiteRequest
	if !a.decodeBody(w, r, &request) {
		return
	}

	if request.Domain != nil {
		domain, err := validateDomain(*request.Domain)
		if err != nil {
			a.refuse(w, err)
			return
		}
		request.Domain = &domain
	}

	if request.Timezone != nil {
		if _, err := time.LoadLocation(*request.Timezone); err != nil {
			a.fail(w, http.StatusBadRequest,
				"timezone must be an IANA name such as America/Los_Angeles, not "+strconv.Quote(*request.Timezone))
			return
		}
	}

	record, err := a.System.UpdateSite(r.Context(), key.TeamID, site.ID,
		request.Domain, request.DisplayName, request.Timezone, request.IsPublic)
	if err != nil {
		a.answerStoreError(w, "update site", err)
		return
	}

	a.refreshSites(r)

	a.write(w, http.StatusOK, record)
}

// handleDeleteSite runs the same durable analytics-and-control deletion as the
// dashboard, including retryable tombstones and ownership revalidation.
func (a *API) handleDeleteSite(w http.ResponseWriter, r *http.Request) {
	key, site, ok := a.siteFromPath(w, r, apikeys.ScopeSitesProvision)
	if !ok {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageSites) {
		return
	}

	if a.SiteOperations == nil {
		a.internal(w, "delete site", errors.New("site deletion service is unavailable"))
		return
	}
	if err := a.SiteOperations.DeleteSite(r.Context(), key.TeamID, site.ID); err != nil {
		switch {
		case errors.Is(err, destructive.ErrBusy):
			a.fail(w, http.StatusConflict, "site deletion is already in progress; retry after its lease expires")
		case errors.Is(err, destructive.ErrNotFound):
			a.fail(w, http.StatusNotFound, "the site is no longer owned by this credential's team")
		default:
			a.answerStoreError(w, "delete site", err)
		}
		return
	}

	a.refreshSites(r)

	a.write(w, http.StatusOK, map[string]any{"deleted": true, "domain": site.Domain})
}

// handleGetTracker reads a site's script configuration and the snippet it
// implies. The snippet is returned alongside the settings because the settings
// on their own are not what anybody wants: they want the line to paste.
func (a *API) handleGetTracker(w http.ResponseWriter, r *http.Request) {
	key, site, ok := a.siteFromPath(w, r, apikeys.ScopeSitesRead)
	if !ok {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}

	config, err := a.System.TrackerConfig(r.Context(), site.ID)
	if err != nil {
		a.answerStoreError(w, "tracker config", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{
		"site_id": site.Domain,
		"config":  config,
		"snippet": trackerSnippet(a.BaseURL, site.Domain, config),
	})
}

// handleUpdateTracker writes a site's script configuration.
func (a *API) handleUpdateTracker(w http.ResponseWriter, r *http.Request) {
	_, site, ok := a.siteFromPath(w, r, apikeys.ScopeSitesProvision)
	if !ok {
		return
	}

	var config TrackerConfig
	if !a.decodeBody(w, r, &config) {
		return
	}

	if config.APIEndpoint != "" && !strings.HasPrefix(config.APIEndpoint, "http://") && !strings.HasPrefix(config.APIEndpoint, "https://") {
		a.fail(w, http.StatusBadRequest, "api_endpoint must be an absolute URL, or empty to post to the origin the script was loaded from")
		return
	}

	if err := a.System.SaveTrackerConfig(r.Context(), site.ID, &config); err != nil {
		a.answerStoreError(w, "save tracker config", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{
		"site_id": site.Domain,
		"config":  config,
		"snippet": trackerSnippet(a.BaseURL, site.Domain, &config),
	})
}

// trackerSnippet renders the script tag for a site.
//
// The options are carried as data attributes rather than baked into a per-site
// URL, because a snippet somebody can read and edit is a snippet they can debug.
func trackerSnippet(baseURL, domain string, config *TrackerConfig) string {
	if baseURL == "" {
		baseURL = "https://feasible.lol"
	}

	attributes := []string{
		`defer`,
		`data-domain="` + domain + `"`,
	}

	if config.APIEndpoint != "" {
		attributes = append(attributes, `data-api="`+config.APIEndpoint+`"`)
	}
	// Each flag carries an explicit value, and the localhost one carries the
	// hyphenated name. The script reads a flag with `getAttribute`, which hands
	// back an empty string for a bare attribute, so a valueless `data-hash` is
	// indistinguishable from one that is not there at all — a snippet that looks
	// configured and behaves as though it is not.
	if config.HashRouting {
		attributes = append(attributes, `data-hash="true"`)
	}
	if config.ManualTagging {
		attributes = append(attributes, `data-manual="true"`)
	}
	if config.TrackLocalhost {
		attributes = append(attributes, `data-capture-on-localhost="true"`)
	}
	if config.ExcludedPages != "" {
		attributes = append(attributes, `data-exclude="`+config.ExcludedPages+`"`)
	}
	if config.FileTypes != "" {
		attributes = append(attributes, `data-file-types="`+config.FileTypes+`"`)
	}

	return `<script ` + strings.Join(attributes, " ") + ` src="` +
		strings.TrimRight(baseURL, "/") + `/js/script.js"></script>`
}

// handleListCustomProps lists a site's allowed properties.
func (a *API) handleListCustomProps(w http.ResponseWriter, r *http.Request) {
	key, site, ok := a.siteFromQuery(w, r, apikeys.ScopeSitesRead)
	if !ok {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}

	var properties any
	var err error
	if a.CustomProperties != nil {
		properties, err = a.CustomProperties.ListProperties(r.Context(), site.ID)
	} else {
		properties, err = a.System.CustomProperties(r.Context(), site.ID)
	}
	if err != nil {
		a.answerStoreError(w, "custom properties", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{"custom_props": properties})
}

// customPropRequest is the body of an allow-list addition.
type customPropRequest struct {
	SiteID string `json:"site_id"`
	Key    string `json:"key"`
	Scope  string `json:"scope"`
}

// handleCreateCustomProp allows a property on a site.
func (a *API) handleCreateCustomProp(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return
	}

	if !a.requireScope(w, key, apikeys.ScopeSitesProvision) {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}

	var request customPropRequest
	if !a.decodeBody(w, r, &request) {
		return
	}

	site, ok := a.resolveSite(w, key, request.SiteID)
	if !ok {
		return
	}

	name := strings.TrimSpace(request.Key)
	if name == "" {
		a.fail(w, http.StatusBadRequest, "key is required — it is the property name the tracker sends")
		return
	}

	if len(name) > 300 {
		a.fail(w, http.StatusBadRequest, "key is longer than the 300 characters a property name may have")
		return
	}
	scope := strings.TrimSpace(request.Scope)
	if scope == "" {
		scope = "event"
	}
	var property any
	var err error
	if a.CustomProperties != nil {
		property, err = a.CustomProperties.CreateProperty(r.Context(), site.ID, name, scope)
	} else {
		property, err = a.System.AddCustomProperty(r.Context(), site.ID, name)
	}
	if err != nil {
		a.answerStoreError(w, "add custom property", err)
		return
	}

	a.write(w, http.StatusCreated, property)
}

// handleDeleteCustomProp stops allowing a property.
func (a *API) handleDeleteCustomProp(w http.ResponseWriter, r *http.Request) {
	_, site, ok := a.siteFromQuery(w, r, apikeys.ScopeSitesProvision)
	if !ok {
		return
	}

	id, ok := a.idFromPath(w, r, "prop_id")
	if !ok {
		return
	}

	var err error
	if a.CustomProperties != nil {
		err = a.CustomProperties.DeleteProperty(r.Context(), site.ID, id)
	} else {
		err = a.System.DeleteCustomProperty(r.Context(), site.ID, id)
	}
	if err != nil {
		a.answerStoreError(w, "delete custom property", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleListSharedLinks lists a site's public dashboard links.
func (a *API) handleListSharedLinks(w http.ResponseWriter, r *http.Request) {
	key, site, ok := a.siteFromQuery(w, r, apikeys.ScopeSitesRead)
	if !ok {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}

	links, err := a.System.SharedLinks(r.Context(), site.ID)
	if err != nil {
		a.answerStoreError(w, "shared links", err)
		return
	}

	for i := range links {
		links[i].URL = a.sharedLinkURL(links[i].Slug)
	}

	a.write(w, http.StatusOK, map[string]any{"shared_links": links})
}

// sharedLinkURL is where a shared link is opened.
func (a *API) sharedLinkURL(slug string) string {
	base := a.BaseURL
	if base == "" {
		base = "https://feasible.lol"
	}

	return strings.TrimRight(base, "/") + "/share/" + slug
}

// sharedLinkRequest is the body of a shared-link creation.
type sharedLinkRequest struct {
	SiteID   string `json:"site_id"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// handleCreateSharedLink mints a public dashboard URL.
func (a *API) handleCreateSharedLink(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return
	}

	if !a.requireScope(w, key, apikeys.ScopeSitesProvision) {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}

	var request sharedLinkRequest
	if !a.decodeBody(w, r, &request) {
		return
	}

	site, ok := a.resolveSite(w, key, request.SiteID)
	if !ok {
		return
	}

	if strings.TrimSpace(request.Name) == "" {
		a.fail(w, http.StatusBadRequest, "name is required — it is how the link is identified in the dashboard")
		return
	}

	if a.Sharing == nil {
		a.internal(w, "create shared link", errors.New("shared-link service is unavailable"))
		return
	}

	created, err := a.Sharing.CreateLinkForOwner(r.Context(), site.ID, key.TeamID,
		request.Name, request.Password, 0, key.UserID)
	if err != nil {
		a.answerStoreError(w, "create shared link", err)
		return
	}
	link := SharedLink{
		ID: created.ID, Name: created.Name, Slug: created.Slug,
		HasPassword: created.HasPassword, CreatedAt: created.CreatedAt,
	}

	link.URL = a.sharedLinkURL(link.Slug)

	a.write(w, http.StatusCreated, link)
}

// handleDeleteSharedLink revokes a link.
func (a *API) handleDeleteSharedLink(w http.ResponseWriter, r *http.Request) {
	key, site, ok := a.siteFromQuery(w, r, apikeys.ScopeSitesProvision)
	if !ok {
		return
	}

	id, ok := a.idFromPath(w, r, "link_id")
	if !ok {
		return
	}

	if a.Sharing == nil {
		a.internal(w, "delete shared link", errors.New("shared-link service is unavailable"))
		return
	}

	if err := a.Sharing.RevokeLinkForOwner(r.Context(), site.ID, key.TeamID, id); err != nil {
		a.answerStoreError(w, "delete shared link", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleListGuests lists the people with access to one site only.
func (a *API) handleListGuests(w http.ResponseWriter, r *http.Request) {
	key, site, ok := a.siteFromQuery(w, r, apikeys.ScopeSitesRead)
	if !ok {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}

	guests, err := a.System.Guests(r.Context(), site.ID)
	if err != nil {
		a.answerStoreError(w, "guests", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{"guests": guests})
}

// guestRequest is the body of a guest addition.
type guestRequest struct {
	SiteID string `json:"site_id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
}

// handleCreateGuest creates a 48-hour invitation for any email address. The
// established route name remains for compatibility, but no membership exists
// until the verified recipient accepts through the normal invitation flow.
func (a *API) handleCreateGuest(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return
	}

	if !a.requireScope(w, key, apikeys.ScopeSitesProvision) {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageSiteSettings) {
		return
	}

	var request guestRequest
	if !a.decodeBody(w, r, &request) {
		return
	}

	site, ok := a.resolveSite(w, key, request.SiteID)
	if !ok {
		return
	}

	email := strings.TrimSpace(request.Email)
	if email == "" || !strings.Contains(email, "@") {
		a.fail(w, http.StatusBadRequest, "email must be an address")
		return
	}

	role := request.Role
	if role == "" {
		role = "guest_viewer"
	}

	if !contains(guestRoles, role) {
		a.fail(w, http.StatusBadRequest, "role must be one of "+strings.Join(guestRoles, ", ")+", not "+strconv.Quote(role))
		return
	}

	if a.Teams == nil {
		a.internal(w, "invite guest", errors.New("team invitation service is unavailable"))
		return
	}
	token, invitation, err := a.Teams.Invite(r.Context(), key.UserID, teams.Invitation{
		TeamID: key.TeamID,
		SiteID: site.ID,
		Email:  email,
		Role:   teams.Role(role),
	})
	if err != nil {
		switch {
		case errors.Is(err, teams.ErrForbidden), errors.Is(err, teams.ErrInvalidRole):
			a.fail(w, http.StatusForbidden, err.Error())
		case errors.Is(err, teams.ErrNotFound):
			a.fail(w, http.StatusNotFound, "the site is not available to this credential")
		default:
			a.internal(w, "invite guest", err)
		}
		return
	}

	a.write(w, http.StatusCreated, map[string]any{
		"invitation_id": invitation.ID,
		"email":         invitation.Email,
		"role":          invitation.Role,
		"expires_at":    invitation.ExpiresAt,
		"token":         token,
	})
}

// handleRevokeGuestInvitation withdraws an unaccepted invitation and scopes
// the id to the site named in the query so one site's integration cannot use a
// guessed id to revoke another site's offer.
func (a *API) handleRevokeGuestInvitation(w http.ResponseWriter, r *http.Request) {
	key, site, ok := a.siteFromQuery(w, r, apikeys.ScopeSitesProvision)
	if !ok {
		return
	}

	id, ok := a.idFromPath(w, r, "invitation_id")
	if !ok {
		return
	}
	if a.Teams == nil {
		a.internal(w, "revoke guest invitation", errors.New("team invitation service is unavailable"))
		return
	}
	if err := a.Teams.RevokeSiteInvitation(r.Context(), key.UserID, key.TeamID, site.ID, id); err != nil {
		switch {
		case errors.Is(err, teams.ErrForbidden):
			a.fail(w, http.StatusForbidden, err.Error())
		case errors.Is(err, teams.ErrNotFound):
			a.fail(w, http.StatusNotFound, "the invitation is not available for this site")
		default:
			a.internal(w, "revoke guest invitation", err)
		}
		return
	}

	a.write(w, http.StatusOK, map[string]any{"revoked": true})
}

// handleDeleteGuest removes a guest.
func (a *API) handleDeleteGuest(w http.ResponseWriter, r *http.Request) {
	_, site, ok := a.siteFromQuery(w, r, apikeys.ScopeSitesProvision)
	if !ok {
		return
	}

	id, ok := a.idFromPath(w, r, "guest_id")
	if !ok {
		return
	}

	if err := a.System.DeleteGuest(r.Context(), site.ID, id); err != nil {
		a.answerStoreError(w, "delete guest", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleListMemberships lists the team.
func (a *API) handleListMemberships(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return
	}

	if !a.requireScope(w, key, apikeys.ScopeSitesRead) {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageMembers) {
		return
	}

	members, err := a.System.Members(r.Context(), key.TeamID)
	if err != nil {
		a.answerStoreError(w, "memberships", err)
		return
	}

	a.write(w, http.StatusOK, map[string]any{"memberships": members})
}

// membershipRequest is the body of a team invitation.
type membershipRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// handleCreateMembership creates a 48-hour invitation. Keeping the established
// route name preserves client compatibility while removing direct membership
// insertion and its account-enumeration side channel.
func (a *API) handleCreateMembership(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return
	}

	if !a.requireScope(w, key, apikeys.ScopeSitesProvision) {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageMembers) {
		return
	}

	var request membershipRequest
	if !a.decodeBody(w, r, &request) {
		return
	}

	email := strings.TrimSpace(request.Email)
	if email == "" || !strings.Contains(email, "@") {
		a.fail(w, http.StatusBadRequest, "email must be an address")
		return
	}

	role := request.Role
	if role == "" {
		role = "viewer"
	}
	if role == string(teams.RoleOwner) {
		a.fail(w, http.StatusForbidden, "ownership only changes through the ownership transfer workflow")
		return
	}

	if !contains(memberRoles, role) {
		a.fail(w, http.StatusBadRequest, "role must be one of "+strings.Join(memberRoles, ", ")+", not "+strconv.Quote(role))
		return
	}
	requestedRole := teams.Role(role)
	actorRole := teams.Role(key.Role)
	if teams.Rank(requestedRole) > teams.Rank(actorRole) {
		a.fail(w, http.StatusForbidden, "this API key's owner may not grant that team role")
		return
	}

	if a.Teams == nil {
		a.internal(w, "invite member", errors.New("team invitation service is unavailable"))
		return
	}
	token, invitation, err := a.Teams.Invite(r.Context(), key.UserID, teams.Invitation{
		TeamID: key.TeamID,
		Email:  email,
		Role:   requestedRole,
	})
	if err != nil {
		switch {
		case errors.Is(err, teams.ErrForbidden), errors.Is(err, teams.ErrInvalidRole):
			a.fail(w, http.StatusForbidden, err.Error())
		case errors.Is(err, teams.ErrNotFound):
			a.fail(w, http.StatusNotFound, "the team is not available to this credential")
		default:
			a.internal(w, "invite member", err)
		}
		return
	}

	a.write(w, http.StatusCreated, map[string]any{
		"invitation_id": invitation.ID,
		"email":         invitation.Email,
		"role":          invitation.Role,
		"expires_at":    invitation.ExpiresAt,
		"token":         token,
	})
}

// handleDeleteMembership takes somebody out of the team.
func (a *API) handleDeleteMembership(w http.ResponseWriter, r *http.Request) {
	key, ok := KeyFrom(r.Context())
	if !ok {
		a.unauthorised(w, "this endpoint needs an API key")
		return
	}

	if !a.requireScope(w, key, apikeys.ScopeSitesProvision) {
		return
	}
	if !a.requirePermission(w, key, teams.PermManageMembers) {
		return
	}

	id, ok := a.idFromPath(w, r, "membership_id")
	if !ok {
		return
	}

	targetUserID, targetRole, err := a.System.MembershipTarget(r.Context(), key.TeamID, id)
	if err != nil {
		a.answerStoreError(w, "read membership", err)
		return
	}
	if teams.Rank(teams.Role(targetRole)) > teams.Rank(teams.Role(key.Role)) {
		a.fail(w, http.StatusForbidden, "this API key's owner may not remove that team role")
		return
	}

	if a.Teams == nil {
		a.internal(w, "remove member", errors.New("team membership service is unavailable"))
		return
	}
	err = a.Teams.RemoveMember(r.Context(), key.UserID, key.TeamID, targetUserID)

	switch {
	case err == nil:
		a.write(w, http.StatusOK, map[string]any{"deleted": true})
	case errors.Is(err, ErrNotFound), errors.Is(err, teams.ErrNotFound):
		a.fail(w, http.StatusNotFound, "no such membership in this team")
	case errors.Is(err, teams.ErrLastOwner):
		a.fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, teams.ErrForbidden):
		a.fail(w, http.StatusForbidden, err.Error())
	default:
		a.internal(w, "remove member", err)
	}
}

// contains reports membership in a small fixed list.
func contains(list []string, value string) bool {
	for _, entry := range list {
		if entry == value {
			return true
		}
	}

	return false
}
