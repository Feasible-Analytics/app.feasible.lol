//
// oauth.go
// Google sign-in: authorization code with PKCE, linked by verified email only.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Google's endpoints. They are constants rather than fetched from the discovery
// document because that document has not moved in a decade and a network call
// on the sign-in path is a network call that can fail.
const (
	googleAuthURL     = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL    = "https://oauth2.googleapis.com/token"
	googleUserInfoURL = "https://openidconnect.googleapis.com/v1/userinfo"
)

// SignupScopes are the only scopes requested at sign-in.
//
// They are the minimum that identifies a person, and nothing else. The same
// OAuth application is reused later for Search Console and analytics imports,
// and asking for those at signup would put a consent screen full of alarming
// permissions in front of somebody who has not yet seen the product. Scopes are
// requested incrementally, when the feature that needs them is turned on.
var SignupScopes = []string{"openid", "email", "profile"}

// oauthStateCookie carries the PKCE verifier and the CSRF state between the
// redirect out and the callback in. It is a signed cookie rather than a
// database row because the whole thing is worthless ninety seconds later, and a
// table of abandoned redirects is a table that only ever needs cleaning.
const oauthStateCookie = "feasible_oauth"

// oauthStateWindow is how long a redirect may take. Ten minutes covers somebody
// who has to log into Google and pass their own second factor.
const oauthStateWindow = 10 * time.Minute

// oauthTimeout caps the token and userinfo calls. Without it a hung Google
// endpoint holds the request until the browser gives up, which reads to the
// user as our bug.
const oauthTimeout = 15 * time.Second

// Google holds the configured OAuth client.
//
// A zero value is valid and means "not configured": credentials are not
// available yet, and the product has to work without them rather than refusing
// to boot. Every method checks Configured, and the sign-in template hides the
// button, so the only visible effect of missing credentials is a missing
// button and one line in the log at start-up.
type Google struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string

	// HTTPClient is swappable so the tests can answer Google's endpoints
	// without a network.
	HTTPClient *http.Client
}

// NewGoogle builds the client from configuration. It returns a usable zero
// value rather than an error when the credentials are absent, because "we have
// not got the credentials yet" must not be a start-up failure.
func NewGoogle(clientID, clientSecret, baseURL string) *Google {
	return &Google{
		ClientID:     strings.TrimSpace(clientID),
		ClientSecret: strings.TrimSpace(clientSecret),
		RedirectURL:  strings.TrimRight(baseURL, "/") + "/auth/google/callback",
		HTTPClient:   &http.Client{Timeout: oauthTimeout},
	}
}

// Configured reports whether Google sign-in can be offered at all.
func (g *Google) Configured() bool {
	return g != nil && g.ClientID != "" && g.ClientSecret != ""
}

// DisabledReason explains in one line why the button is hidden. It is what the
// process logs at start-up: a feature that is silently absent is a support
// ticket, and "which variable do I set" is the only question worth answering.
func (g *Google) DisabledReason() string {
	switch {
	case g == nil:
		return "Google sign-in is off: no client was configured"
	case g.ClientID == "" && g.ClientSecret == "":
		return "Google sign-in is off: set FEASIBLE_GOOGLE_CLIENT_ID and FEASIBLE_GOOGLE_CLIENT_SECRET"
	case g.ClientID == "":
		return "Google sign-in is off: set FEASIBLE_GOOGLE_CLIENT_ID"
	case g.ClientSecret == "":
		return "Google sign-in is off: set FEASIBLE_GOOGLE_CLIENT_SECRET"
	default:
		return ""
	}
}

// PKCE is one authorization attempt's proof-key pair plus its CSRF state.
type PKCE struct {
	Verifier  string
	Challenge string
	State     string
}

// NewPKCE mints a verifier, its S256 challenge and a state value.
//
// PKCE is used even though this is a confidential client with a secret. The
// authorization code travels back through the user's browser, and a code
// intercepted there — a malicious extension, a logged referrer, a shared
// machine — is useless without the verifier, which never leaves the server.
// Adding it costs one hash.
func NewPKCE() (*PKCE, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("auth: pkce: %w", err)
	}

	verifier := base64.RawURLEncoding.EncodeToString(raw)

	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	stateRaw := make([]byte, 16)
	if _, err := rand.Read(stateRaw); err != nil {
		return nil, fmt.Errorf("auth: pkce: %w", err)
	}

	return &PKCE{
		Verifier:  verifier,
		Challenge: challenge,
		State:     base64.RawURLEncoding.EncodeToString(stateRaw),
	}, nil
}

// AuthURL builds the URL the browser is sent to.
//
// prompt=select_account rather than the default is deliberate: without it,
// someone signed into one Google account is silently taken straight through
// with that identity, which is the wrong one often enough — a work machine, a
// shared browser — to be worth one extra click.
func (g *Google) AuthURL(p *PKCE) string {
	q := url.Values{}
	q.Set("client_id", g.ClientID)
	q.Set("redirect_uri", g.RedirectURL)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(SignupScopes, " "))
	q.Set("state", p.State)
	q.Set("code_challenge", p.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("access_type", "offline")
	q.Set("prompt", "select_account")

	return googleAuthURL + "?" + q.Encode()
}

// Profile is the subset of Google's userinfo response we act on. The subject
// is the identity; the email is only ever used to find an existing account to
// link to, and only when Google says it is verified.
type Profile struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

// tokenResponse is Google's token endpoint reply.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// Exchange trades an authorization code for tokens and the caller's profile.
//
// An `invalid_grant` reply is turned into a message that says to reconnect
// rather than a generic failure. It is what Google returns for a code that was
// already used, a revoked consent and an expired refresh token alike, and
// treating it as an unexplained error is how a broken connection stays broken:
// the incumbent's self-hosted build hit exactly this when a second site was
// connected with the same Google account, and it was never root-caused.
func (g *Google) Exchange(ctx context.Context, code, verifier string) (*Profile, error) {
	if !g.Configured() {
		return nil, fmt.Errorf("auth: %s", g.DisabledReason())
	}

	form := url.Values{}
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", g.RedirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("auth: google token: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := g.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: google token: %w", err)
	}
	defer resp.Body.Close()

	var token tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return nil, fmt.Errorf("auth: google token: %w", err)
	}

	if token.Error == "invalid_grant" {
		return nil, fmt.Errorf("auth: Google rejected the sign-in as expired or already used — start again from the sign-in page")
	}

	if token.Error != "" {
		return nil, fmt.Errorf("auth: google token: %s: %s", token.Error, token.ErrorDesc)
	}

	if token.AccessToken == "" {
		return nil, fmt.Errorf("auth: google returned no access token")
	}

	return g.profile(ctx, token.AccessToken)
}

// profile reads the userinfo endpoint.
func (g *Google) profile(ctx context.Context, accessToken string) (*Profile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleUserInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: google profile: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := g.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: google profile: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: google profile: unexpected status %d", resp.StatusCode)
	}

	var profile Profile
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		return nil, fmt.Errorf("auth: google profile: %w", err)
	}

	if profile.Sub == "" {
		return nil, fmt.Errorf("auth: google returned no subject id")
	}

	return &profile, nil
}

// client returns the HTTP client, defaulting to one with a timeout so a nil
// field cannot turn into an unbounded request.
func (g *Google) client() *http.Client {
	if g.HTTPClient != nil {
		return g.HTTPClient
	}

	return &http.Client{Timeout: oauthTimeout}
}

// ResolveProfile turns a Google profile into the account it belongs to,
// creating one if this is a new person.
//
// The rules, in order, and the middle one is the important one:
//
//  1. A stored subject id wins. That is the identity, and it survives the
//     person changing their email address at Google.
//  2. Otherwise, link to an existing account by email *only when both sides are
//     verified* — Google says the address is verified, and we have already
//     proven it ourselves. Linking on an unverified address on either side lets
//     whoever can obtain an address at a provider take over an account that was
//     registered with it.
//  3. Otherwise it is a new signup, and it starts verified because Google has
//     already proven the address.
func (s *Store) ResolveProfile(ctx context.Context, profile *Profile) (*User, bool, error) {
	user, err := s.UserByGoogleSub(ctx, profile.Sub)
	if err == nil {
		return user, false, nil
	}
	if err != ErrNotFound {
		return nil, false, err
	}

	email := NormaliseEmail(profile.Email)

	existing, err := s.UserByEmail(ctx, email)
	switch {
	case err == nil:
		if !profile.EmailVerified || !existing.Verified() {
			return nil, false, fmt.Errorf(
				"auth: an account already uses %s — sign in with your password first, then link Google from settings", email)
		}

		if err := s.LinkGoogle(ctx, existing.ID, profile.Sub); err != nil {
			return nil, false, err
		}

		existing.GoogleSub = profile.Sub

		return existing, false, nil

	case err == ErrNotFound:
		// A new account with no password at all. That is a complete identity:
		// the person signs in with Google, and can set a password later from
		// settings if they want a second way in.
		created, _, err := s.CreateUser(ctx, email, profile.Name, "", profile.Sub)
		if err != nil {
			return nil, false, err
		}

		if profile.EmailVerified {
			if err := s.MarkVerified(ctx, created.ID); err != nil {
				return nil, false, err
			}
			created.EmailVerifiedAt = s.now().Unix()
		}

		return created, true, nil

	default:
		return nil, false, err
	}
}

// SetOAuthStateCookie stores the PKCE verifier and state for the callback. The
// value is signed so a browser cannot hand back a verifier we never issued, and
// it carries an expiry inside the signature so a stale cookie from an abandoned
// attempt cannot be replayed.
func SetOAuthStateCookie(w http.ResponseWriter, sealer *Sealer, p *PKCE, next, baseURL string) error {
	payload, err := json.Marshal(map[string]string{
		"v":   p.Verifier,
		"s":   p.State,
		"n":   next,
		"exp": fmt.Sprint(time.Now().Add(oauthStateWindow).Unix()),
	})
	if err != nil {
		return fmt.Errorf("auth: oauth state: %w", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    sealer.SignedValue(base64.RawURLEncoding.EncodeToString(payload)),
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(baseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthStateWindow.Seconds()),
	})

	return nil
}

// ReadOAuthStateCookie recovers and clears the redirect state. It clears
// unconditionally, including on failure: a cookie that survives a failed
// callback is a verifier sitting in the browser for the next attempt to reuse.
func ReadOAuthStateCookie(w http.ResponseWriter, r *http.Request, sealer *Sealer, baseURL string) (verifier, state, next string, err error) {
	defer http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(baseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	cookie, cookieErr := r.Cookie(oauthStateCookie)
	if cookieErr != nil {
		return "", "", "", fmt.Errorf("auth: the sign-in attempt expired — start again")
	}

	encoded, ok := sealer.VerifySignedValue(cookie.Value)
	if !ok {
		return "", "", "", fmt.Errorf("auth: the sign-in attempt could not be verified — start again")
	}

	raw, decodeErr := base64.RawURLEncoding.DecodeString(encoded)
	if decodeErr != nil {
		return "", "", "", fmt.Errorf("auth: the sign-in attempt could not be read — start again")
	}

	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", "", "", fmt.Errorf("auth: the sign-in attempt could not be read — start again")
	}

	var expires int64
	fmt.Sscanf(payload["exp"], "%d", &expires)

	if expires <= time.Now().Unix() {
		return "", "", "", fmt.Errorf("auth: the sign-in attempt expired — start again")
	}

	return payload["v"], payload["s"], payload["n"], nil
}
