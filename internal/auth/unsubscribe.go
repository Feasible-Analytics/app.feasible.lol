//
// unsubscribe.go
// Getting off a recurring email without an account.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"net/http"
	"strings"
)

// Unsubscriber removes one address from one recurring email.
//
// It is an interface because the subscriptions live in internal/reports and
// this package renders every signed-out page. The alternative — a second page
// shell in that package — is how a product ends up with two sign-in pages that
// do not look alike.
type Unsubscriber interface {
	// Describe names the site and what is being stopped, and reports false for
	// a token that is unreadable or names a subscription that is gone.
	Describe(ctx context.Context, token string) (site, list string, ok bool)

	// Remove takes the address off the list. A token it cannot read removes
	// nothing and is not an error: somebody clicking twice should see success.
	Remove(ctx context.Context, token string) error
}

// showUnsubscribe asks before it acts.
//
// A GET that unsubscribed would let any link-scanning proxy in a corporate mail
// path remove people who never clicked, and those proxies follow every link in
// every message. The one-click POST below is the mechanism designed for this,
// and this page is for the human who clicked the footer link.
func (h *Handler) showUnsubscribe(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")

	p := h.newPage(r, tr(r, "auth.title.unsubscribe"), "")
	p.Data["Token"] = token

	if h.Unsubscribe == nil {
		p.Data["Known"] = false
		h.render(w, r, "unsubscribe", p, http.StatusOK)

		return
	}

	site, list, ok := h.Unsubscribe.Describe(r.Context(), token)

	p.Data["Known"] = ok
	p.Data["Site"] = site
	p.Data["List"] = list

	h.render(w, r, "unsubscribe", p, http.StatusOK)
}

// doUnsubscribe removes the address and says so.
//
// There is no CSRF token here, and there cannot be: RFC 8058 one-click is a
// POST from the reader's mail provider, which has no session with us and no
// form to have put a token in. The token in the path is the authorisation, and
// the only thing this can do is take one address off one list.
func (h *Handler) doUnsubscribe(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")

	if h.Unsubscribe != nil {
		if err := h.Unsubscribe.Remove(r.Context(), token); err != nil {
			h.fail(w, r, err)

			return
		}
	}

	// One-click posts want a 200 and nothing else; a person wants a page.
	if oneClick(r) {
		w.WriteHeader(http.StatusOK)

		return
	}

	p := h.newPage(r, tr(r, "auth.title.unsubscribe"), "")
	p.Data["Done"] = true

	h.render(w, r, "unsubscribe", p, http.StatusOK)
}

// oneClick reports whether this POST came from a mail provider acting on the
// List-Unsubscribe-Post header rather than from somebody pressing a button.
func oneClick(r *http.Request) bool {
	return strings.Contains(r.PostFormValue("List-Unsubscribe"), "One-Click")
}
