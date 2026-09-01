//
// conversions.go
// Site settings for goals, custom properties, and funnels.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package settings

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/goals"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/i18n"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
)

const seenPropertyLimit = 100

var funnelStepSlots = []int{1, 2, 3, 4, 5, 6, 7, 8}

// conversionsPath returns the conversion settings URL for one site.
func conversionsPath(domain string) string {
	return SitePrefix + domain + "/conversions"
}

// conversions renders goals, properties, and funnels from the site's account database.
func (h *Handler) conversions(w http.ResponseWriter, r *http.Request, site sites.Site) {
	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		h.conversionError(w, err)
		return
	}
	defer lease.Release() //nolint:errcheck // rendering completes before the account can be retired

	list, err := goals.List(r.Context(), lease.Account.Reader(), site.ID)
	if err != nil {
		h.conversionError(w, err)
		return
	}

	properties, err := goals.Allowed(r.Context(), lease.Account.Reader(), site.ID)
	if err != nil {
		h.conversionError(w, err)
		return
	}

	seen, err := goals.Seen(r.Context(), lease.Account.Reader(), site.ID,
		goals.NewWindow(h.now().AddDate(0, 0, -30), h.now()), seenPropertyLimit)
	if err != nil {
		h.conversionError(w, err)
		return
	}

	funnels, err := goals.ListFunnels(r.Context(), lease.Account.Reader(), site.ID)
	if err != nil {
		h.conversionError(w, err)
		return
	}

	message, failure := flash(r)
	h.render(w, r, "conversions", page{
		TitleID: "settings.conversions.title", Tab: "conversions", Domain: site.Domain,
		Lang: i18n.Negotiate(r), Message: message, Error: failure,
		Goals: list, Properties: properties, SeenProperties: unseenProperties(seen, properties), Funnels: funnels,
		NoBackfillNotice: goals.NoBackfillNotice, PropertyPIINotice: goals.PIINotice,
		FunnelStepSlots: funnelStepSlots,
	})
}

// unseenProperties removes configured names from the recently received property suggestions.
func unseenProperties(seen []string, allowed []goals.Property) []string {
	configured := make(map[string]bool, len(allowed))
	for _, property := range allowed {
		configured[property.Name] = true
	}

	result := make([]string, 0, len(seen))
	for _, name := range seen {
		if !configured[name] {
			result = append(result, name)
		}
	}

	return result
}

// createGoal validates a goal form and stores the new definition.
func (h *Handler) createGoal(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if !h.requireConversionPost(w, r) {
		return
	}

	goal, err := goalFromForm(r, site.ID)
	if err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		h.conversionError(w, err)
		return
	}
	defer lease.Release() //nolint:errcheck // the mutation is finished before the account can be retired

	if _, err := goals.Create(r.Context(), lease.Account.Writer(), goal, h.now()); err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}

	h.conversionRedirect(w, r, site.Domain, "Goal created.", "")
}

// updateGoal validates a full goal edit and preserves the definition's identity.
func (h *Handler) updateGoal(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if !h.requireConversionPost(w, r) {
		return
	}

	id, err := formID(r, "goal_id")
	if err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}

	goal, err := goalFromForm(r, site.ID)
	if err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}
	goal.ID = id

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		h.conversionError(w, err)
		return
	}
	defer lease.Release() //nolint:errcheck // the mutation is finished before the account can be retired

	existing, err := goals.Get(r.Context(), lease.Account.Reader(), id)
	if err != nil || existing.SiteID != site.ID {
		h.conversionRedirect(w, r, site.Domain, "", "That goal does not belong to this site.")
		return
	}

	if _, err := goals.Update(r.Context(), lease.Account.Writer(), goal); err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}

	h.conversionRedirect(w, r, site.Domain, "Goal updated.", "")
}

// deleteGoal removes one goal after proving it belongs to the requested site.
func (h *Handler) deleteGoal(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if !h.requireConversionPost(w, r) {
		return
	}

	id, err := formID(r, "goal_id")
	if err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		h.conversionError(w, err)
		return
	}
	defer lease.Release() //nolint:errcheck // the mutation is finished before the account can be retired

	goal, err := goals.Get(r.Context(), lease.Account.Reader(), id)
	if err != nil || goal.SiteID != site.ID {
		h.conversionRedirect(w, r, site.Domain, "", "That goal does not belong to this site.")
		return
	}

	if err := goals.Delete(r.Context(), lease.Account.Writer(), id); err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}

	h.conversionRedirect(w, r, site.Domain, "Goal removed.", "")
}

// goalFromForm turns repeated constraint fields and the selected goal type into a domain definition.
func goalFromForm(r *http.Request, siteID int64) (goals.Goal, error) {
	if err := r.ParseForm(); err != nil {
		return goals.Goal{}, errors.New("the goal form could not be read")
	}

	goal := goals.Goal{
		SiteID: siteID, Kind: goals.Kind(r.PostFormValue("kind")), DisplayName: r.PostFormValue("display_name"),
		PagePattern: r.PostFormValue("page_pattern"), EventName: r.PostFormValue("event_name"),
		IsRevenue: r.PostFormValue("is_revenue") == "1", Currency: r.PostFormValue("currency"),
	}
	goal.ScrollDepth, _ = strconv.Atoi(r.PostFormValue("scroll_depth"))

	names := r.PostForm["property_name"]
	values := r.PostForm["property_value"]
	for index, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		value := ""
		if index < len(values) {
			value = values[index]
		}
		goal.Properties = append(goal.Properties, goals.PropertyConstraint{Name: name, Value: value})
	}

	goal.Normalise()
	if err := goal.Validate(); err != nil {
		return goals.Goal{}, err
	}

	return goal, nil
}

// allowProperty adds or re-scopes one custom property.
func (h *Handler) allowProperty(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if !h.requireConversionPost(w, r) {
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		h.conversionError(w, err)
		return
	}
	defer lease.Release() //nolint:errcheck // the mutation is finished before the account can be retired

	if _, err := goals.Allow(r.Context(), lease.Account.Writer(), site.ID,
		r.PostFormValue("name"), goals.Scope(r.PostFormValue("scope")), h.now()); err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}

	h.conversionRedirect(w, r, site.Domain, "Custom property enabled.", "")
}

// allowAllProperties enables every property received during the last thirty days.
func (h *Handler) allowAllProperties(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if !h.requireConversionPost(w, r) {
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		h.conversionError(w, err)
		return
	}
	defer lease.Release() //nolint:errcheck // all writes finish before the account can be retired

	seen, err := goals.Seen(r.Context(), lease.Account.Reader(), site.ID,
		goals.NewWindow(h.now().AddDate(0, 0, -30), h.now()), seenPropertyLimit)
	if err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}

	allowed, err := goals.Allowed(r.Context(), lease.Account.Reader(), site.ID)
	if err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}
	newNames := unseenProperties(seen, allowed)

	for _, name := range newNames {
		if _, err := goals.Allow(r.Context(), lease.Account.Writer(), site.ID, name, goals.ScopeEvent, h.now()); err != nil {
			h.conversionRedirect(w, r, site.Domain, "", err.Error())
			return
		}
	}

	h.conversionRedirect(w, r, site.Domain, strconv.Itoa(len(newNames))+" recently seen properties enabled as event properties.", "")
}

// deleteProperty disables one property without deleting values already stored on events.
func (h *Handler) deleteProperty(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if !h.requireConversionPost(w, r) {
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		h.conversionError(w, err)
		return
	}
	defer lease.Release() //nolint:errcheck // the mutation is finished before the account can be retired

	if err := goals.Disallow(r.Context(), lease.Account.Writer(), site.ID, r.PostFormValue("name")); err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}

	h.conversionRedirect(w, r, site.Domain, "Custom property disabled. Its recorded data was kept.", "")
}

// saveFunnel creates a funnel or atomically updates the selected funnel.
func (h *Handler) saveFunnel(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if !h.requireConversionPost(w, r) {
		return
	}

	funnel := goals.Funnel{SiteID: site.ID, Name: r.PostFormValue("name"), StrictOrder: r.PostFormValue("mode") == "strict"}
	funnel.ID, _ = strconv.ParseInt(r.PostFormValue("funnel_id"), 10, 64)
	for _, rawID := range r.PostForm["goal_id"] {
		id, err := strconv.ParseInt(rawID, 10, 64)
		if err == nil && id > 0 {
			funnel.Steps = append(funnel.Steps, goals.Step{GoalID: id})
		}
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		h.conversionError(w, err)
		return
	}
	defer lease.Release() //nolint:errcheck // the mutation is finished before the account can be retired

	for _, step := range funnel.Steps {
		goal, err := goals.Get(r.Context(), lease.Account.Reader(), step.GoalID)
		if err != nil || goal.SiteID != site.ID {
			h.conversionRedirect(w, r, site.Domain, "", "Every funnel step must be a goal from this site.")
			return
		}
	}

	if funnel.ID > 0 {
		existing, err := goals.GetFunnel(r.Context(), lease.Account.Reader(), funnel.ID)
		if err != nil || existing.SiteID != site.ID {
			h.conversionRedirect(w, r, site.Domain, "", "That funnel does not belong to this site.")
			return
		}
		if _, err := goals.UpdateFunnel(r.Context(), lease.Account.Writer(), funnel); err != nil {
			h.conversionRedirect(w, r, site.Domain, "", err.Error())
			return
		}
	} else if _, err := goals.CreateFunnel(r.Context(), lease.Account.Writer(), funnel, h.now()); err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}

	h.conversionRedirect(w, r, site.Domain, "Funnel saved.", "")
}

// deleteFunnel removes one funnel after proving it belongs to the requested site.
func (h *Handler) deleteFunnel(w http.ResponseWriter, r *http.Request, site sites.Site) {
	if !h.requireConversionPost(w, r) {
		return
	}

	id, err := formID(r, "funnel_id")
	if err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}

	lease, err := h.Accounts.Acquire(r.Context(), site.AccountID)
	if err != nil {
		h.conversionError(w, err)
		return
	}
	defer lease.Release() //nolint:errcheck // the mutation is finished before the account can be retired

	funnel, err := goals.GetFunnel(r.Context(), lease.Account.Reader(), id)
	if err != nil || funnel.SiteID != site.ID {
		h.conversionRedirect(w, r, site.Domain, "", "That funnel does not belong to this site.")
		return
	}

	if err := goals.DeleteFunnel(r.Context(), lease.Account.Writer(), id); err != nil {
		h.conversionRedirect(w, r, site.Domain, "", err.Error())
		return
	}

	h.conversionRedirect(w, r, site.Domain, "Funnel removed.", "")
}

// requireConversionPost keeps every conversion mutation on a CSRF-checked POST route.
func (h *Handler) requireConversionPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST is required", http.StatusMethodNotAllowed)
		return false
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "the form could not be read", http.StatusBadRequest)
		return false
	}

	return true
}

// formID reads a positive integer identifier from a submitted form.
func formID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PostFormValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("the selected item is invalid")
	}

	return id, nil
}

// conversionRedirect returns to conversion settings with one status message.
func (h *Handler) conversionRedirect(w http.ResponseWriter, r *http.Request, domain, message, failure string) {
	h.redirect(w, r, domain, "conversions", message, failure)
}

// conversionError writes an unexpected settings error without leaking database details into a form flash.
func (h *Handler) conversionError(w http.ResponseWriter, err error) {
	if h.Log != nil {
		h.Log.Error("conversion settings failed", "error", err)
	}
	http.Error(w, "conversion settings are temporarily unavailable", http.StatusInternalServerError)
}
