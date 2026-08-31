//
// handlers_onboarding.go
// The snippet screen, the live waiting poll and the installation check.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// showOnboarding renders the install-and-wait screen for a site.
func (h *Handler) showOnboarding(w http.ResponseWriter, r *http.Request) {
	site, _, ok := h.siteOr404(w, r)
	if !ok {
		return
	}

	p := h.newPage(r, tr(r, "auth.title.onboarding", "site", site.Label()), "sites")
	p.Data["Site"] = site
	p.Data["Snippet"] = Snippet(h.BaseURL, h.Keyer, site)
	p.Data["SnippetLegacy"] = SnippetLegacy(h.BaseURL, site)
	p.Data["Platforms"] = InstallPlatforms()
	p.Data["PollMillis"] = int(FirstEventPollInterval.Milliseconds())
	p.Data["RoutingDelay"] = int(RoutingDelay.Seconds())

	h.render(w, r, "onboarding", p, http.StatusOK)
}

// statusResponse is what the waiting screen polls for.
type statusResponse struct {
	Received bool   `json:"received"`
	At       int64  `json:"at"`
	Message  string `json:"message"`
}

// onboardingStatus answers "has anything arrived yet".
//
// This endpoint is the whole reason the waiting screen works. Shard-pull
// routing means a brand-new domain can take a full poll cycle before an
// ingestor will accept anything for it, so somebody who pastes the snippet and
// immediately reloads sees nothing and concludes the product is broken. A
// screen that says "waiting" and then changes by itself turns that gap into a
// normal-feeling wait.
func (h *Handler) onboardingStatus(w http.ResponseWriter, r *http.Request) {
	site, team, ok := h.siteOr404(w, r)
	if !ok {
		return
	}

	at, err := h.Traffic.FirstEventAt(r.Context(), team.ID, site.ID)
	if err != nil {
		// A site whose account database has never been written to is not an
		// error, it is the normal state of the screen that is waiting for the
		// first write. Reporting it as a failure would put an error banner on
		// the happy path.
		h.Log.Debug("no traffic yet for this site", "site", site.ID, "error", err)
		at = 0
	}

	response := statusResponse{Received: at > 0, At: at}

	if response.Received {
		response.Message = "We have your first pageview."

		// The site is marked as installed the moment traffic arrives, so the
		// wizard does not reappear the next time somebody opens the sites list.
		if site.OnboardedAt == 0 {
			if err := h.Store.MarkOnboarded(r.Context(), team.ID, site.ID); err != nil {
				h.Log.Warn("could not mark the site onboarded", "site", site.ID, "error", err)
			}
		}
	} else {
		response.Message = "Waiting for your first pageview."
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	_ = json.NewEncoder(w).Encode(response)
}

// doVerifyInstall fetches the customer's page and reports what it found.
//
// It is a real fetch rather than a check of whether an event has arrived,
// because those answer different questions. "No traffic yet" cannot tell
// somebody whether they forgot to deploy, pasted another site's snippet, or
// have a Content-Security-Policy blocking the script — and those have three
// completely different fixes.
func (h *Handler) doVerifyInstall(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	site, _, ok := h.siteOr404(w, r)
	if !ok {
		return
	}

	result := VerifyInstallation(r.Context(), h.Verifier, h.BaseURL, site)

	h.Log.Info("installation checked", "site", site.ID, "domain", site.Domain,
		"outcome", string(result.Outcome), "status", result.StatusCode)

	p := h.newPage(r, tr(r, "auth.title.onboarding", "site", site.Label()), "sites")
	p.Data["Site"] = site
	p.Data["Snippet"] = Snippet(h.BaseURL, h.Keyer, site)
	p.Data["SnippetLegacy"] = SnippetLegacy(h.BaseURL, site)
	p.Data["Platforms"] = InstallPlatforms()
	p.Data["PollMillis"] = int(FirstEventPollInterval.Milliseconds())
	p.Data["RoutingDelay"] = int(RoutingDelay.Seconds())
	p.Data["Verify"] = result

	if result.OK() {
		p.Flash = result.Message
	} else {
		p.Error = result.Message
	}

	h.render(w, r, "onboarding", p, http.StatusOK)
}

// doSkipOnboarding leaves the wizard without installing anything.
//
// It exists because somebody adding a site for a colleague to install, or
// coming back to it tomorrow, must not be held on this screen. A wizard with no
// way out is a wizard people escape by closing the tab.
func (h *Handler) doSkipOnboarding(w http.ResponseWriter, r *http.Request) {
	if !h.checkCSRF(w, r) {
		return
	}

	site, team, ok := h.siteOr404(w, r)
	if !ok {
		return
	}

	if err := h.Store.MarkOnboarded(r.Context(), team.ID, site.ID); err != nil {
		h.fail(w, r, err)
		return
	}

	h.Log.Info("onboarding skipped", "site", site.ID, "domain", site.Domain)

	http.Redirect(w, r, "/sites?site="+strconv.FormatInt(site.ID, 10), http.StatusFound)
}
