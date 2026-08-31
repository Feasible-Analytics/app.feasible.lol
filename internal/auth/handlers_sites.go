//
// handlers_sites.go
// The sites list, creating a site, folders and site settings.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

// showSites renders the sites list, grouped into folders, with a sparkline
// against each one.
func (h *Handler) showSites(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)

	team, err := h.Store.TeamForUser(r.Context(), user.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	order := r.URL.Query().Get("sort")

	list, err := h.Store.ListSites(r.Context(), team.ID, order)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	// The sparkline read opens the account database, which does not exist until
	// a site has been created. A failure here degrades the page rather than
	// breaking it: a list with no little charts is still a usable list.
	if err := h.Traffic.Sparklines(r.Context(), team.ID, list, h.Store.Now()); err != nil {
		h.Log.Warn("could not read sparklines", "team", team.ID, "error", err)
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

	p := h.newPage(r, "Sites", "sites")
	p.Data["Sites"] = loose
	p.Data["Folders"] = folders
	p.Data["Total"] = len(list)
	p.Data["Sort"] = order

	if r.URL.Query().Get("welcome") == "1" {
		p.Flash = "Your email is confirmed. Add your first site to start collecting."
	}

	if r.URL.Query().Get("deleted") == "1" {
		p.Flash = "That site has been deleted."
	}

	h.render(w, r, "sites", p, http.StatusOK)
}

// showNewSite renders the create-site form.
func (h *Handler) showNewSite(w http.ResponseWriter, r *http.Request) {
	p := h.newPage(r, "Add a site", "sites")
	p.Data["Timezones"] = CommonTimezones()

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
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)

	team, err := h.Store.TeamForUser(r.Context(), user.ID)
	if err != nil {
		h.fail(w, r, err)
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
		p := h.newPage(r, "Add a site", "sites")
		p.Data["Timezones"] = CommonTimezones()
		p.Data["Domain"] = domain
		p.Data["DisplayName"] = displayName
		p.Data["Timezone"] = timezone

		if errors.Is(err, ErrDomainTaken) {
			p.Error = "That domain is already being tracked. If it is yours and you have lost access, get in touch."
		} else {
			p.Error = strings.TrimPrefix(err.Error(), "auth: ")
		}

		h.render(w, r, "site_new", p, http.StatusBadRequest)

		return
	}

	h.Log.Info("site created", "site", site.ID, "team", team.ID, "domain", site.Domain, "timezone", site.Timezone)

	// The routing map is updated immediately rather than at the next refresh.
	// It does not remove the shard-pull delay in a multi-process deployment,
	// but in the single-process one it means the snippet works the moment it is
	// pasted.
	h.pushToCache(site, team)

	http.Redirect(w, r, "/onboarding/"+strconv.FormatInt(site.ID, 10), http.StatusFound)
}

// pushToCache inserts a site into the in-memory routing map.
func (h *Handler) pushToCache(site *Site, team *Team) {
	if h.SiteCache == nil {
		return
	}

	h.SiteCache.Set(sites.Site{
		ID:                 site.ID,
		AccountID:          site.AccountID,
		Domain:             site.Domain,
		Timezone:           site.Timezone,
		AcceptTrafficUntil: team.AcceptTrafficUntil,
	})
}

// doPinSite pins or unpins a site from the list.
func (h *Handler) doPinSite(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	site, team, ok := h.siteOr404(w, r)
	if !ok {
		return
	}

	if err := h.Store.SetPinned(r.Context(), team.ID, site.ID, !site.Pinned()); err != nil {
		h.fail(w, r, err)
		return
	}

	http.Redirect(w, r, "/sites", http.StatusFound)
}

// showSiteSettings renders general, timezone, domain, reset and delete.
func (h *Handler) showSiteSettings(w http.ResponseWriter, r *http.Request) {
	site, team, ok := h.siteOr404(w, r)
	if !ok {
		return
	}

	folders, err := h.Store.ListFolders(r.Context(), team.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	p := h.newPage(r, site.Label()+" settings", "sites")
	p.Data["Site"] = site
	p.Data["Folders"] = folders
	p.Data["Timezones"] = CommonTimezones()
	p.Data["Snippet"] = Snippet(h.BaseURL, h.Keyer, site)
	p.Data["DualWrite"] = site.DualWriteActive(h.Store.Now())
	p.Data["DualWriteHours"] = int(DualWriteWindow.Hours())

	switch r.URL.Query().Get("saved") {
	case "general":
		p.Flash = "Saved."
	case "domain":
		p.Flash = "The domain has been changed. The old one keeps collecting for the next " +
			strconv.Itoa(int(DualWriteWindow.Hours())) + " hours, so update your snippet before then."
	case "reset":
		p.Flash = "Every recorded pageview for this site has been deleted. Your goals and settings are untouched."
	}

	h.render(w, r, "site_settings", p, http.StatusOK)
}

// doSiteGeneral saves the display name, timezone, folder and public flag.
func (h *Handler) doSiteGeneral(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	site, team, ok := h.siteOr404(w, r)
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
		p := h.newPage(r, site.Label()+" settings", "sites")
		p.Data["Site"] = site
		p.Data["Timezones"] = CommonTimezones()
		p.Data["Snippet"] = Snippet(h.BaseURL, h.Keyer, site)
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
	if !h.checkCSRF(w, r) {
		return
	}

	site, team, ok := h.siteOr404(w, r)
	if !ok {
		return
	}

	err := h.Store.ChangeDomain(r.Context(), team.ID, site.ID, r.PostFormValue("domain"))
	if err != nil {
		p := h.newPage(r, site.Label()+" settings", "sites")
		p.Data["Site"] = site
		p.Data["Timezones"] = CommonTimezones()
		p.Data["Snippet"] = Snippet(h.BaseURL, h.Keyer, site)

		if errors.Is(err, ErrDomainTaken) {
			p.Error = "That domain is already being tracked by another site."
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

// doSiteReset deletes a site's recorded traffic and nothing else.
func (h *Handler) doSiteReset(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	site, team, ok := h.siteOr404(w, r)
	if !ok {
		return
	}

	if strings.TrimSpace(r.PostFormValue("confirm")) != site.Domain {
		p := h.newPage(r, site.Label()+" settings", "sites")
		p.Data["Site"] = site
		p.Data["Timezones"] = CommonTimezones()
		p.Data["Snippet"] = Snippet(h.BaseURL, h.Keyer, site)
		p.Error = "Type the domain exactly to confirm the reset."

		h.render(w, r, "site_settings", p, http.StatusBadRequest)

		return
	}

	if err := h.Traffic.ResetStats(r.Context(), team.ID, site.ID); err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Warn("site stats reset", "site", site.ID, "team", team.ID, "domain", site.Domain)

	http.Redirect(w, r, "/sites/"+strconv.FormatInt(site.ID, 10)+"/settings?saved=reset", http.StatusFound)
}

// doSiteDelete removes a site, its routing entry and its recorded traffic.
func (h *Handler) doSiteDelete(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	site, team, ok := h.siteOr404(w, r)
	if !ok {
		return
	}

	if strings.TrimSpace(r.PostFormValue("confirm")) != site.Domain {
		p := h.newPage(r, site.Label()+" settings", "sites")
		p.Data["Site"] = site
		p.Data["Timezones"] = CommonTimezones()
		p.Data["Snippet"] = Snippet(h.BaseURL, h.Keyer, site)
		p.Error = "Type the domain exactly to confirm the deletion."

		h.render(w, r, "site_settings", p, http.StatusBadRequest)

		return
	}

	// The events go first. Deleting the routing row first would leave the
	// account database holding rows for a site id nothing can name any more.
	if err := h.Traffic.ResetStats(r.Context(), team.ID, site.ID); err != nil {
		h.fail(w, r, err)
		return
	}

	if err := h.Store.DeleteSite(r.Context(), team.ID, site.ID); err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Warn("site deleted", "site", site.ID, "team", team.ID, "domain", site.Domain)

	h.refreshCache(r)

	http.Redirect(w, r, "/sites?deleted=1", http.StatusFound)
}

// doCreateFolder adds a folder.
func (h *Handler) doCreateFolder(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)

	team, err := h.Store.TeamForUser(r.Context(), user.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	if _, err := h.Store.CreateFolder(r.Context(), team.ID, r.PostFormValue("name")); err != nil {
		h.Log.Warn("could not create a folder", "team", team.ID, "error", err)
	}

	http.Redirect(w, r, "/sites", http.StatusFound)
}

// doRenameFolder renames a folder.
func (h *Handler) doRenameFolder(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)

	team, err := h.Store.TeamForUser(r.Context(), user.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	if err := h.Store.RenameFolder(r.Context(), team.ID, pathID(r, "id"), r.PostFormValue("name")); err != nil {
		h.Log.Warn("could not rename a folder", "team", team.ID, "error", err)
	}

	http.Redirect(w, r, "/sites", http.StatusFound)
}

// doDeleteFolder removes a folder and leaves its sites at the top level.
func (h *Handler) doDeleteFolder(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)

	team, err := h.Store.TeamForUser(r.Context(), user.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	if err := h.Store.DeleteFolder(r.Context(), team.ID, pathID(r, "id")); err != nil {
		h.fail(w, r, err)
		return
	}

	http.Redirect(w, r, "/sites", http.StatusFound)
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
	if !h.checkCSRF(w, r) {
		return
	}

	user := userFrom(r)

	team, err := h.Store.TeamForUser(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "not signed in", http.StatusUnauthorized)
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
