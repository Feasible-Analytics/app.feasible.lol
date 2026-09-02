//
// headers.go
// The response headers every page on a listener carries by default.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package httpserver

import (
	"net/http"
)

// HSTSMaxAge is one year in seconds, the value browsers require before a
// domain is eligible for preloading. A shorter max-age means a visitor who has
// not been back inside the window is downgradeable again, which is most of the
// point of the header.
const HSTSMaxAge = "max-age=31536000; includeSubDomains"

// SecurityHeaders sets the response headers every page on this listener
// should carry unless a handler deliberately overrides them (the share and
// embed pages relax framing on purpose). requireHTTPS turns on HSTS.
//
// The headers are set before the handler runs rather than after, because Go
// sends them with the first byte of the body: a handler that needs a different
// answer deletes or replaces the header before writing, and the default is
// what a handler gets by doing nothing.
func SecurityHeaders(requireHTTPS bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()

		// Framing is denied by default because most of what this listener
		// serves is one account's data behind a session cookie, and a framed
		// page is how somebody is tricked into pressing Remove.
		header.Set("X-Frame-Options", "DENY")

		// Browsers must not guess a content type; a user-supplied file served
		// as text is otherwise a script when sniffed.
		header.Set("X-Content-Type-Options", "nosniff")

		// A URL here can carry a share slug or a reset token, which must not
		// leave for a third-party site in a Referer.
		header.Set("Referrer-Policy", "same-origin")

		if requireHTTPS {
			header.Set("Strict-Transport-Security", HSTSMaxAge)
		}

		next.ServeHTTP(w, r)
	})
}
