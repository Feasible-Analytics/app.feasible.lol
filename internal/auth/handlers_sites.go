//
// handlers_sites.go
// The sites list, creating a site, folders and site settings.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/i18n"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/teams"
)

// transferDestination is one team the current source owner may move a site
// into. Only owner and admin memberships are queried, matching TransferSite's
// live destination authorization rather than advertising a choice it refuses.
type transferDestination struct {
	ID   int64
	Name string
}

// siteTransferRequest is the authenticated JSON transfer contract. Carrying
// the expected owner makes a stale confirmation fail instead of moving a site
// that was already transferred by another request.
type siteTransferRequest struct {
	FromTeamID int64  `json:"from_team_id"`
	ToTeamID   int64  `json:"to_team_id"`
	Confirm    string `json:"confirm"`
}

// showSites renders the sites list, grouped into folders, with a sparkline
// against each one.
func (h *Handler) showSites(w http.ResponseWriter, r *http.Request) {
	team, err := h.teamForRequest(r, teams.PermViewDashboard)
	if err != nil {
		h.teamSelectionError(w, r, err)
		return
	}

	order := r.URL.Query().Get("sort")

	list, err := h.Store.ListSites(r.Context(), team.ID, order)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	// A locked account gets the list without the charts. The page itself has to
	// stay open — it is the way to site settings and, two clicks on, to the
	// billing screen that unlocks everything — but a sparkline is thirty days of
	// that account's visitors, and handing those over here would be the report
	// the dashboard just refused.
	locked := h.Access != nil && h.Access(team.ID)

	// The sparkline read opens the account database, which does not exist until
	// a site has been created. A failure here degrades the page rather than
	// breaking it: a list with no little charts is still a usable list.
	if !locked {
		if err := h.Traffic.SparklinesForSites(r.Context(), list, h.Store.Now()); err != nil {
			h.Log.Warn("could not read sparklines", "team", team.ID, "error", err)
		}
	}

	if order == "traffic" {
		SortByTraffic(list)
	}

	folders, err := h.Store.ListFolders(r.Context(), team.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	// The sites are bucketed into their folders here rather than by a query per
	// folder, so the page is two queries whatever the number of folders.
	byFolder := map[int64][]*Site{}
	var loose []*Site

	for _, site := range list {
		if site.FolderID == 0 {
			loose = append(loose, site)
			continue
		}

		byFolder[site.FolderID] = append(byFolder[site.FolderID], site)
	}

	for _, folder := range folders {
		folder.Sites = byFolder[folder.ID]
	}

	p := h.newPage(r, tr(r, "auth.title.sites"), "sites")
	p.Data["Sites"] = loose
	p.Data["Folders"] = folders
	p.Data["Total"] = len(list)
	p.Data["Sort"] = order
	p.Data["Locked"] = locked
	p.Data["TeamID"] = team.ID
	role, _ := h.Teams.RoleOf(r.Context(), team.ID, userFrom(r).ID)
	p.Data["CanManage"] = teams.Can(role, teams.PermManageSites)
	p.Data["CanConfigure"] = teams.Can(role, teams.PermManageSiteSettings)
	p.Data["CanManageBilling"] = role == teams.RoleOwner || role == teams.RoleAdmin || role == teams.RoleBilling

	if r.URL.Query().Get("welcome") == "1" {
		p.Flash = i18n.T(p.Lang, "auth.flash.email_confirmed")
	}

	if r.URL.Query().Get("deleted") == "1" {
		p.Flash = i18n.T(p.Lang, "auth.flash.site_deleted")
	}

	h.render(w, r, "sites", p, http.StatusOK)
}

// showNewSite renders the create-site form.
func (h *Handler) showNewSite(w http.ResponseWriter, r *http.Request) {
	team, err := h.teamForRequest(r, teams.PermManageSites)
	if err != nil {
		h.teamSelectionError(w, r, err)

		return
	}

	p := h.newPage(r, tr(r, "auth.title.site_new"), "sites")
	firstRun := r.URL.Query().Get("welcome") == "1"
	if firstRun {
		p.Nav = ""
		p.Focused = true
	}
	p.Data["Timezones"] = CommonTimezones()
	p.Data["TeamID"] = team.ID
	p.Data["FirstRun"] = firstRun

	h.render(w, r, "site_new", p, http.StatusOK)
}

// doNewSite creates a site and sends the user straight into onboarding.
//
// The timezone arrives from the browser rather than from a dropdown the user
// had to hunt through. Almost nobody tracks a site in a timezone other than
// their own, and the browser already knows which one that is — so the field is
// pre-filled and overridable rather than a list of six hundred entries starting
// at Africa/Abidjan.
func (h *Handler) doNewSite(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	team, err := h.teamForRequest(r, teams.PermManageSites)
	if err != nil {
		h.teamSelectionError(w, r, err)
		return
	}

	domain := r.PostFormValue("domain")
	displayName := r.PostFormValue("display_name")
	timezone := strings.TrimSpace(r.PostFormValue("timezone"))

	// An unknown or missing zone falls back to UTC rather than failing. A
	// browser that reports a zone Go's database does not carry is not a reason
	// to refuse to create the site; it is a reason to pick a sane default and
	// let the settings screen fix it.
	if timezone == "" {
		timezone = "Etc/UTC"
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		h.Log.Warn("browser reported an unknown timezone", "timezone", timezone)
		timezone = "Etc/UTC"
	}

	site, err := h.Store.CreateSite(r.Context(), team.ID, domain, displayName, timezone)
	if err != nil {
		p := h.newPage(r, tr(r, "auth.title.site_new"), "sites")
		firstRun := r.PostFormValue("first_run") == "1"
		if firstRun {
			p.Nav = ""
			p.Focused = true
		}
		p.Data["Timezones"] = CommonTimezones()
		p.Data["Domain"] = domain
		p.Data["DisplayName"] = displayName
		p.Data["Timezone"] = timezone
		p.Data["TeamID"] = team.ID
		p.Data["FirstRun"] = firstRun

		if errors.Is(err, ErrDomainTaken) {
			p.Error = i18n.T(p.Lang, "auth.error.domain_taken")
		} else {
			p.Error = strings.TrimPrefix(err.Error(), "auth: ")
		}

		h.render(w, r, "site_new", p, http.StatusBadRequest)

		return
	}

	h.Log.Info("site created", "site", site.ID, "team", team.ID, "domain", site.Domain, "timezone", site.Timezone)

	// Account-backed defaults are created before onboarding so the first
	// dashboard request sees the same automatic goals as every later request.
	if h.ProvisionSite != nil {
		if err := h.ProvisionSite(r.Context(), site.AccountID, site.ID, h.Store.Now()); err != nil {
			h.Log.Error("site defaults could not be provisioned", "site", site.ID, "team", team.ID, "error", err)
		}
	}

	// The routing map is updated immediately rather than at the next refresh so
	// the snippet works the moment it is pasted in this process. Other serving
	// processes discover the durable control row on their normal refresh.
	h.pushToCache(site, team)

	destination := "/onboarding/" + strconv.FormatInt(site.ID, 10)
	if r.PostFormValue("first_run") == "1" {
		destination += "?first_run=1"
	}
	http.Redirect(w, r, destination, http.StatusFound)
}

// pushToCache inserts a site into the in-memory routing map.
func (h *Handler) pushToCache(site *Site, team *Team) {
	if h.SiteCache == nil {
		return
	}

	h.SiteCache.Set(sites.Site{
		ID:                 site.ID,
		AccountID:          site.AccountID,
		TeamID:             site.TeamID,
		Domain:             site.Domain,
		Timezone:           site.Timezone,
		AcceptTrafficUntil: team.AcceptTrafficUntil,
	})
}

// doPinSite pins or unpins a site from the list.
func (h *Handler) doPinSite(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	site, team, ok := h.siteOr404(w, r, teams.PermManageSiteSettings)
	if !ok {
		return
	}

	if err := h.Store.SetPinned(r.Context(), team.ID, site.ID, !site.Pinned()); err != nil {
		h.fail(w, r, err)
		return
	}

	http.Redirect(w, r, "/sites?team_id="+strconv.FormatInt(team.ID, 10), http.StatusFound)
}

// showSiteSettings renders general, timezone, domain, reset and delete.
func (h *Handler) showSiteSettings(w http.ResponseWriter, r *http.Request) {
	site, team, ok := h.siteOr404(w, r, teams.PermManageSiteSettings)
	if !ok {
		return
	}

	folders, err := h.Store.ListFolders(r.Context(), team.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	p := h.newPage(r, tr(r, "auth.title.site_settings", "site", site.Label()), "sites")
	p.Data["Site"] = site
	p.Data["Folders"] = folders
	p.Data["Timezones"] = CommonTimezones()
	p.Data["Snippet"] = Snippet(h.BaseURL, h.Keyer, site)
	p.Data["DualWrite"] = site.DualWriteActive(h.Store.Now())
	p.Data["DualWriteHours"] = int(DualWriteWindow.Hours())
	p.Data["TransferTeams"] = h.transferDestinations(r.Context(), userFrom(r).ID, team.ID)
	p.Data["CurrentTeamID"] = team.ID
	if problem := strings.TrimSpace(r.URL.Query().Get("transfer_error")); problem != "" {
		p.Error = problem
	}

	switch r.URL.Query().Get("saved") {
	case "general":
		p.Flash = i18n.T(p.Lang, "auth.flash.saved")
	case "domain":
		p.Flash = i18n.N(p.Lang, "auth.flash.domain_changed", int(DualWriteWindow.Hours()))
	case "reset":
		p.Flash = i18n.T(p.Lang, "auth.flash.stats_reset")
	}

	h.render(w, r, "site_settings", p, http.StatusOK)
}

// showSiteSettingsByDomain gives the domain-keyed settings shell a stable way
// back to General without teaching it system-database site ids.
func (h *Handler) showSiteSettingsByDomain(w http.ResponseWriter, r *http.Request) {
	site, ok := h.SiteCache.Lookup(r.PathValue("domain"))
	if !ok {
		h.notFound(w, r)
		return
	}

	r.SetPathValue("id", strconv.FormatInt(site.ID, 10))
	h.showSiteSettings(w, r)
}

// transferDestinations lists teams the user owns or administers, excluding the
// source. The transfer transaction repeats both role checks; this list is an
// ergonomic filter, not an authorization boundary.
func (h *Handler) transferDestinations(ctx context.Context, userID, sourceTeamID int64) []transferDestination {
	role, err := h.Teams.RoleOf(ctx, sourceTeamID, userID)
	if err != nil || role != teams.RoleOwner {
		return nil
	}

	rows, err := h.Store.DB().QueryContext(ctx, `
		SELECT teams.id, teams.name
		FROM teams
		JOIN team_memberships ON team_memberships.team_id = teams.id
		WHERE team_memberships.user_id = ? AND team_memberships.role IN ('owner', 'admin')
		  AND teams.id <> ?
		ORDER BY teams.name COLLATE NOCASE, teams.id
	`, userID, sourceTeamID)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()

	var destinations []transferDestination
	for rows.Next() {
		var destination transferDestination
		if err := rows.Scan(&destination.ID, &destination.Name); err != nil {
			return destinations
		}
		destinations = append(destinations, destination)
	}

	return destinations
}

// doSiteTransfer handles the confirmed browser form. Every decision is repeated
// by TransferSiteFrom under the writer lock, so a stale page cannot authorize a
// second transfer after ownership has already moved.
func (h *Handler) doSiteTransfer(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	site, team, ok := h.siteOr404(w, r, teams.PermManageSites)
	if !ok {
		return
	}

	fromTeamID, _ := strconv.ParseInt(r.PostFormValue("from_team_id"), 10, 64)
	toTeamID, _ := strconv.ParseInt(r.PostFormValue("to_team_id"), 10, 64)
	if fromTeamID != team.ID || toTeamID < 1 || toTeamID == team.ID {
		h.redirectTransferError(w, r, site.ID, "Choose another team and reload this page before trying again.")
		return
	}
	if strings.TrimSpace(r.PostFormValue("confirm")) != site.Domain {
		h.redirectTransferError(w, r, site.ID, "Type the site's domain exactly to confirm the transfer.")
		return
	}

	if err := h.Teams.TransferSiteFrom(r.Context(), userFrom(r).ID, site.ID, fromTeamID, toTeamID); err != nil {
		h.redirectTransferError(w, r, site.ID, transferErrorMessage(err))
		return
	}

	h.refreshCache(r)
	if h.Log != nil {
		h.Log.Warn("site transferred", "site", site.ID, "from_team", fromTeamID, "to_team", toTeamID, "domain", site.Domain)
	}
	http.Redirect(w, r, "/sites?team_id="+strconv.FormatInt(toTeamID, 10)+"&transferred=1", http.StatusFound)
}

// doSiteTransferAPI exposes the same compare-and-swap to signed-in clients.
// Session authentication and CSRF are both required because this endpoint is a
// browser credential surface, not the bearer-key public API.
func (h *Handler) doSiteTransferAPI(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	var request siteTransferRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		h.apiAccessError(w, http.StatusBadRequest, "the request body must be valid transfer JSON")
		return
	}

	site, err := h.Store.SiteByIDAny(r.Context(), pathID(r, "id"))
	if errors.Is(err, ErrNotFound) {
		h.apiAccessError(w, http.StatusNotFound, "no such site")
		return
	}
	if err != nil {
		h.apiAccessError(w, http.StatusInternalServerError, "the site could not be read")
		return
	}
	role, err := h.Teams.AuthoriseSite(r.Context(), site.ID, userFrom(r).ID, teams.PermManageSites)
	if err != nil || role != teams.RoleOwner {
		h.apiAccessError(w, http.StatusNotFound, "no such site")
		return
	}
	if site.TeamID != request.FromTeamID {
		h.apiAccessError(w, http.StatusConflict, transferErrorMessage(teams.ErrStaleTransfer))
		return
	}
	if strings.TrimSpace(request.Confirm) != site.Domain {
		h.apiAccessError(w, http.StatusBadRequest, "confirm must exactly match the site's domain")
		return
	}

	err = h.Teams.TransferSiteFrom(r.Context(), userFrom(r).ID, site.ID, request.FromTeamID, request.ToTeamID)
	if err != nil {
		status := http.StatusForbidden
		switch {
		case errors.Is(err, teams.ErrStaleTransfer), errors.Is(err, teams.ErrOperationInProgress):
			status = http.StatusConflict
		case errors.Is(err, teams.ErrNotFound):
			status = http.StatusNotFound
		}
		h.apiAccessError(w, status, transferErrorMessage(err))
		return
	}

	h.refreshCache(r)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"transferred":  true,
		"site_id":      site.ID,
		"from_team_id": request.FromTeamID,
		"to_team_id":   request.ToTeamID,
	})
}

// redirectTransferError returns to the settings page without echoing form data.
// The query carries only an explanatory sentence and is escaped before use.
func (h *Handler) redirectTransferError(w http.ResponseWriter, r *http.Request, siteID int64, message string) {
	http.Redirect(w, r, "/sites/"+strconv.FormatInt(siteID, 10)+"/settings?transfer_error="+url.QueryEscape(message), http.StatusSeeOther)
}

// transferErrorMessage turns authorization and compare-and-swap sentinels into
// actionable UI/API text while keeping unknown team ids non-enumerable.
func transferErrorMessage(err error) string {
	switch {
	case errors.Is(err, teams.ErrStaleTransfer):
		return "The site owner changed. Reload the page before transferring it again."
	case errors.Is(err, teams.ErrOperationInProgress):
		return "A reset, deletion, or account purge is in progress. Retry after it finishes."
	case errors.Is(err, teams.ErrForbidden):
		return "Only the source team's Owner can transfer this site, and the destination must be a team they own or administer."
	case errors.Is(err, teams.ErrNotFound):
		return "The site or destination team is not available to this account."
	default:
		return "The site could not be transferred."
	}
}

// doSiteGeneral saves the display name, timezone, folder and public flag.
func (h *Handler) doSiteGeneral(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	site, team, ok := h.siteOr404(w, r, teams.PermManageSiteSettings)
	if !ok {
		return
	}

	timezone := strings.TrimSpace(r.PostFormValue("timezone"))
	if timezone == "" {
		timezone = site.Timezone
	}

	err := h.Store.UpdateSiteGeneral(r.Context(), team.ID, site.ID,
		r.PostFormValue("display_name"), timezone, r.PostFormValue("is_public") == "1")
	if err != nil {
		p := h.newPage(r, tr(r, "auth.title.site_settings", "site", site.Label()), "sites")
		p.Data["Site"] = site
		p.Data["Timezones"] = CommonTimezones()
		p.Data["Snippet"] = Snippet(h.BaseURL, h.Keyer, site)
		p.Data["DualWriteHours"] = int(DualWriteWindow.Hours())
		p.Error = strings.TrimPrefix(err.Error(), "auth: ")

		h.render(w, r, "site_settings", p, http.StatusBadRequest)

		return
	}

	folderID, _ := strconv.ParseInt(r.PostFormValue("folder_id"), 10, 64)

	if folderID != site.FolderID {
		if err := h.Store.MoveSite(r.Context(), team.ID, site.ID, folderID, site.Position); err != nil {
			h.fail(w, r, err)
			return
		}
	}

	h.refreshCache(r)

	http.Redirect(w, r, "/sites/"+strconv.FormatInt(site.ID, 10)+"/settings?saved=general", http.StatusFound)
}

// doSiteDomain changes the domain and opens the dual-write window.
func (h *Handler) doSiteDomain(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	site, team, ok := h.siteOr404(w, r, teams.PermManageSiteSettings)
	if !ok {
		return
	}

	err := h.Store.ChangeDomain(r.Context(), team.ID, site.ID, r.PostFormValue("domain"))
	if err != nil {
		p := h.newPage(r, tr(r, "auth.title.site_settings", "site", site.Label()), "sites")
		p.Data["Site"] = site
		p.Data["Timezones"] = CommonTimezones()
		p.Data["Snippet"] = Snippet(h.BaseURL, h.Keyer, site)
		p.Data["DualWriteHours"] = int(DualWriteWindow.Hours())

		if errors.Is(err, ErrDomainTaken) {
			p.Error = i18n.T(p.Lang, "auth.error.domain_taken_elsewhere")
		} else {
			p.Error = strings.TrimPrefix(err.Error(), "auth: ")
		}

		h.render(w, r, "site_settings", p, http.StatusBadRequest)

		return
	}

	h.Log.Info("site domain changed", "site", site.ID, "from", site.Domain,
		"to", CleanDomain(r.PostFormValue("domain")), "dual_write_hours", int(DualWriteWindow.Hours()))

	h.refreshCache(r)

	http.Redirect(w, r, "/sites/"+strconv.FormatInt(site.ID, 10)+"/settings?saved=domain", http.StatusFound)
}

// doSiteReset erases every datum scoped to a site while retaining the site row
// itself so the domain can begin collecting again under the same owner.
func (h *Handler) doSiteReset(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	site, team, ok := h.siteOr404(w, r, teams.PermManageSites)
	if !ok {
		return
	}

	if strings.TrimSpace(r.PostFormValue("confirm")) != site.Domain {
		p := h.newPage(r, tr(r, "auth.title.site_settings", "site", site.Label()), "sites")
		p.Data["Site"] = site
		p.Data["Timezones"] = CommonTimezones()
		p.Data["Snippet"] = Snippet(h.BaseURL, h.Keyer, site)
		p.Data["DualWriteHours"] = int(DualWriteWindow.Hours())
		p.Error = i18n.T(p.Lang, "auth.error.confirm_reset")

		h.render(w, r, "site_settings", p, http.StatusBadRequest)

		return
	}

	if h.Destructive == nil {
		h.fail(w, r, errors.New("auth: destructive site operations are unavailable"))
		return
	}
	if err := h.Destructive.ResetSite(r.Context(), team.ID, site.ID); err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Warn("site stats reset", "site", site.ID, "team", team.ID, "domain", site.Domain)

	http.Redirect(w, r, "/sites/"+strconv.FormatInt(site.ID, 10)+"/settings?saved=reset", http.StatusFound)
}

// doSiteDelete removes a site, its routing entry and its recorded traffic.
func (h *Handler) doSiteDelete(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	site, team, ok := h.siteOr404(w, r, teams.PermManageSites)
	if !ok {
		return
	}

	if strings.TrimSpace(r.PostFormValue("confirm")) != site.Domain {
		p := h.newPage(r, tr(r, "auth.title.site_settings", "site", site.Label()), "sites")
		p.Data["Site"] = site
		p.Data["Timezones"] = CommonTimezones()
		p.Data["Snippet"] = Snippet(h.BaseURL, h.Keyer, site)
		p.Data["DualWriteHours"] = int(DualWriteWindow.Hours())
		p.Error = i18n.T(p.Lang, "auth.error.confirm_delete")

		h.render(w, r, "site_settings", p, http.StatusBadRequest)

		return
	}

	if h.Destructive == nil {
		h.fail(w, r, errors.New("auth: destructive site operations are unavailable"))
		return
	}
	if err := h.Destructive.DeleteSite(r.Context(), team.ID, site.ID); err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Warn("site deleted", "site", site.ID, "team", team.ID, "domain", site.Domain)

	h.refreshCache(r)

	http.Redirect(w, r, "/sites?team_id="+strconv.FormatInt(team.ID, 10)+"&deleted=1", http.StatusFound)
}

// doCreateFolder adds a folder.
func (h *Handler) doCreateFolder(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	team, err := h.teamForRequest(r, teams.PermManageSites)
	if err != nil {
		h.teamSelectionError(w, r, err)
		return
	}

	if _, err := h.Store.CreateFolder(r.Context(), team.ID, r.PostFormValue("name")); err != nil {
		h.Log.Warn("could not create a folder", "team", team.ID, "error", err)
	}

	http.Redirect(w, r, "/sites?team_id="+strconv.FormatInt(team.ID, 10), http.StatusFound)
}

// doRenameFolder renames a folder.
func (h *Handler) doRenameFolder(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	team, err := h.teamForRequest(r, teams.PermManageSites)
	if err != nil {
		h.teamSelectionError(w, r, err)
		return
	}

	if err := h.Store.RenameFolder(r.Context(), team.ID, pathID(r, "id"), r.PostFormValue("name")); err != nil {
		h.Log.Warn("could not rename a folder", "team", team.ID, "error", err)
	}

	http.Redirect(w, r, "/sites?team_id="+strconv.FormatInt(team.ID, 10), http.StatusFound)
}

// doDeleteFolder removes a folder and leaves its sites at the top level.
func (h *Handler) doDeleteFolder(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	team, err := h.teamForRequest(r, teams.PermManageSites)
	if err != nil {
		h.teamSelectionError(w, r, err)
		return
	}

	if err := h.Store.DeleteFolder(r.Context(), team.ID, pathID(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}

	http.Redirect(w, r, "/sites?team_id="+strconv.FormatInt(team.ID, 10), http.StatusFound)
}

// reorderRequest is what the drag handle posts back: the folder order, and the
// site order inside each folder including the top level under id zero.
type reorderRequest struct {
	Folders []int64            `json:"folders"`
	Sites   map[string][]int64 `json:"sites"`
}

// doReorder saves a drag-and-drop rearrangement.
//
// It takes JSON rather than form fields because the browser is sending two
// nested lists, and it answers with a status rather than a redirect because the
// page has already moved the elements — a reload here would undo the animation
// the user just watched.
func (h *Handler) doReorder(w http.ResponseWriter, r *http.Request) {
	if !h.CheckFormToken(w, r) {
		return
	}

	team, err := h.teamForRequest(r, teams.PermManageSites)
	if err != nil {
		http.Error(w, "an explicit authorized team_id is required", http.StatusForbidden)
		return
	}

	var body reorderRequest

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "could not read the new order", http.StatusBadRequest)
		return
	}

	if len(body.Folders) > 0 {
		if err := h.Store.ReorderFolders(r.Context(), team.ID, body.Folders); err != nil {
			h.Log.Warn("could not reorder folders", "team", team.ID, "error", err)
			http.Error(w, "could not save the new order", http.StatusInternalServerError)

			return
		}
	}

	for key, ids := range body.Sites {
		folderID, _ := strconv.ParseInt(key, 10, 64)

		if err := h.Store.ReorderSites(r.Context(), team.ID, folderID, ids); err != nil {
			h.Log.Warn("could not reorder sites", "team", team.ID, "folder", folderID, "error", err)
			http.Error(w, "could not save the new order", http.StatusInternalServerError)

			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// refreshCache rebuilds the in-memory routing map after a change that affects
// which domains this process accepts.
func (h *Handler) refreshCache(r *http.Request) {
	if h.SiteCache == nil {
		return
	}

	if err := h.SiteCache.Refresh(r.Context()); err != nil {
		h.Log.Warn("could not refresh the routing map", "error", err)
	}
}

// CommonTimezones is the shortlist the create form offers as suggestions.
//
// It is not the full IANA database, and that is the point. The field is
// pre-filled from the browser, so this list only has to cover the person who
// wants to change it — and a hundred-entry list they can scroll beats six
// hundred they have to search. Any valid zone name can still be typed in.
func CommonTimezones() []string {
	return []string{
		"Etc/UTC",
		"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles",
		"America/Phoenix", "America/Anchorage", "Pacific/Honolulu",
		"America/Toronto", "America/Vancouver", "America/Mexico_City",
		"America/Bogota", "America/Lima", "America/Sao_Paulo", "America/Argentina/Buenos_Aires",
		"Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Madrid", "Europe/Paris",
		"Europe/Brussels", "Europe/Amsterdam", "Europe/Berlin", "Europe/Zurich", "Europe/Vienna",
		"Europe/Prague", "Europe/Warsaw", "Europe/Stockholm", "Europe/Oslo", "Europe/Copenhagen",
		"Europe/Helsinki", "Europe/Rome", "Europe/Athens", "Europe/Bucharest", "Europe/Kyiv",
		"Europe/Istanbul", "Europe/Moscow",
		"Africa/Casablanca", "Africa/Lagos", "Africa/Cairo", "Africa/Johannesburg", "Africa/Nairobi",
		"Asia/Jerusalem", "Asia/Dubai", "Asia/Karachi", "Asia/Kolkata", "Asia/Dhaka",
		"Asia/Bangkok", "Asia/Jakarta", "Asia/Singapore", "Asia/Hong_Kong", "Asia/Shanghai",
		"Asia/Taipei", "Asia/Seoul", "Asia/Tokyo", "Asia/Manila",
		"Australia/Perth", "Australia/Adelaide", "Australia/Brisbane", "Australia/Sydney",
		"Australia/Melbourne", "Pacific/Auckland",
	}
}
