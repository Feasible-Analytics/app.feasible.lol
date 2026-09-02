//
// security.go
// HTTPS redirects and framing policy for the pages anybody can reach.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package sharing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Security decides how public pages are protected in transit. It is resolved
// from the configured base URL rather than guessed per request: a local
// development install runs over plain HTTP and must not redirect itself into a
// loop trying to reach an HTTPS listener that does not exist.
type Security struct {
	// RequireHTTPS turns on the redirect. It is true when the install's base
	// URL is an https one; the listener sets HSTS from the same fact.
	RequireHTTPS bool

	// Host is the hostname redirects are built against, taken from the base
	// URL. Using the request's own Host would let anybody with a DNS record
	// pointing at us produce a redirect to a domain they control.
	Host string
}

// NewSecurity derives the policy from a base URL.
func NewSecurity(baseURL string) Security {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return Security{}
	}

	return Security{RequireHTTPS: parsed.Scheme == "https", Host: parsed.Host}
}

// IsHTTPS reports whether a request arrived over TLS, believing the proxy
// header only because every deployment of this product has a reverse proxy in
// front of it — the app listens on loopback and never terminates TLS itself, so
// r.TLS is nil on requests that were HTTPS the whole way to the proxy.
func IsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}

	proto := r.Header.Get("X-Forwarded-Proto")
	first, _, _ := strings.Cut(proto, ",")

	return strings.EqualFold(strings.TrimSpace(first), "https")
}

// Apply enforces the transport policy on one request, returning true when it
// has already answered and the caller must stop.
//
// A public dashboard and a shared link are pages somebody sends to other
// people, often over chat, often on a network they do not control. Serving one
// over plain HTTP puts a set of business metrics on the wire in clear text and
// leaves the URL — which for a shared link *is* the credential — readable by
// anything between the two ends.
func (s Security) Apply(w http.ResponseWriter, r *http.Request) bool {
	if !s.RequireHTTPS {
		return false
	}

	if !IsHTTPS(r) {
		target := "https://" + s.Host + r.URL.RequestURI()

		// 308 rather than 301: a permanent redirect that preserves the method,
		// so a POST to the password form is not silently turned into a GET that
		// throws the password away and looks like a wrong password.
		http.Redirect(w, r, target, http.StatusPermanentRedirect)

		return true
	}

	return false
}

// AllowFraming opens a response up to being embedded. It removes the framing
// header rather than never setting it, so the safe default is what a handler
// gets by forgetting to call anything.
func AllowFraming(w http.ResponseWriter) {
	w.Header().Del("X-Frame-Options")

	// frame-ancestors is the modern control and the only one that can express
	// "anybody". An embeddable dashboard is embeddable by definition: the
	// customer chose to publish it, and enumerating the sites allowed to frame
	// it would be a list they have to maintain forever.
	w.Header().Set("Content-Security-Policy", "frame-ancestors *")
}

// DenyFraming closes a response to being embedded.
func DenyFraming(w http.ResponseWriter) {
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
}

// colourPattern is the only background value that is accepted.
//
// The parameter ends up as a colour the page paints itself with, so anything
// that is not provably a colour is a way to put attacker-chosen text into the
// document. A three- or six-digit hex value and the word "transparent" cover
// every real use of the parameter and cannot express anything else.
var colourPattern = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// NormaliseBackground validates the background parameter, returning the empty
// string for anything it will not accept. A rejected value is dropped silently
// rather than erroring, because the alternative is an embed that refuses to
// load at all over a cosmetic parameter somebody mistyped.
func NormaliseBackground(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if strings.EqualFold(value, "transparent") {
		return "transparent"
	}

	// A bare hex value with no hash is the commonest way to write this in a
	// query string, because a '#' has to be escaped there and usually is not.
	if !strings.HasPrefix(value, "#") {
		value = "#" + value
	}

	if !colourPattern.MatchString(value) {
		return ""
	}

	return strings.ToLower(value)
}

// NormaliseTheme validates the theme parameter.
func NormaliseTheme(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "light":
		return "light"
	case "dark":
		return "dark"
	case "system":
		return "system"
	}

	return ""
}

// CookieName is the per-link cookie a solved password sets.
func CookieName(slug string) string {
	return "feasible_share_" + slug
}

// SignSlug is the cookie value proving a password was solved.
//
// It is an HMAC over the slug rather than a random session id because there is
// no session to store: the cookie is scoped to one link, carries no identity,
// and needs no server-side state to check. Rotating the secret invalidates
// every outstanding cookie, which is the remedy if one is ever leaked.
func SignSlug(secret []byte, slug string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("share-cookie:" + slug))

	return hex.EncodeToString(mac.Sum(nil))
}

// ValidSignature checks a cookie in constant time.
func ValidSignature(secret []byte, slug, value string) bool {
	return hmac.Equal([]byte(SignSlug(secret, slug)), []byte(value))
}

// DeriveSecret produces this package's cookie key from the install's script
// secret. Deriving rather than adding a second key file means one thing to back
// up and one thing to rotate; the label keeps the two uses of the same root
// secret from being interchangeable.
func DeriveSecret(root []byte) []byte {
	mac := hmac.New(sha256.New, root)
	mac.Write([]byte("feasible/share-cookie"))

	return mac.Sum(nil)
}

// PasswordSourceKey pseudonymizes the client address for durable throttling.
// The first forwarded address is the browser address supplied by the app's
// loopback reverse proxy; RemoteAddr is the direct-development fallback.
func PasswordSourceKey(secret []byte, r *http.Request) string {
	source := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if first, _, found := strings.Cut(source, ","); found {
		source = strings.TrimSpace(first)
	}
	if source == "" {
		source = strings.TrimSpace(r.RemoteAddr)
		if host, _, err := net.SplitHostPort(source); err == nil {
			source = host
		}
	}
	if source == "" {
		source = "unknown"
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("share-password-source:" + source))

	return hex.EncodeToString(mac.Sum(nil))
}
