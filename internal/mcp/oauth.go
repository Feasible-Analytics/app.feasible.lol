//
// oauth.go
// OAuth 2.1 with dynamic client registration, for remote MCP clients.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
)

// A remote MCP client is a program somebody pasted a URL into. There is nobody
// to fill in a developer portal, no client secret anybody could keep, and no
// second chance to ask — so registration is dynamic, PKCE is mandatory on every
// authorisation, and the only thing the person has to do is prove they hold a
// key for the team they are connecting.
//
// The authorisation step asks for an API key rather than a password. That is
// deliberate while sign-in lives elsewhere in the product: a key is already the
// credential this API is built around, it is revocable on its own, and it means
// this package never touches a password.

// Token lifetimes. An hour is short enough that a leaked access token is not
// worth much and long enough that a working session is not interrupted; the
// refresh token is what keeps a connection alive across days.
const (
	AccessTokenLifetime  = time.Hour
	RefreshTokenLifetime = 30 * 24 * time.Hour

	// authorizationCodeLifetime is deliberately tiny. A code is exchanged
	// within a second of being issued by every real client, and a minute is
	// generous for a slow redirect.
	authorizationCodeLifetime = time.Minute
)

// oauthScopes is the complete grant vocabulary for MCP. It deliberately uses
// the same names as API keys so authorization is an intersection, not a second
// policy language that can drift away from the API it represents.
var oauthScopes = []string{
	apikeys.ScopeStatsRead,
	apikeys.ScopeSitesRead,
	apikeys.ScopeSitesProvision,
	apikeys.ScopeWebhooks,
}

// The OAuth routes, relative to the base URL.
const (
	PathAuthorizationServerMetadata = "/.well-known/oauth-authorization-server"
	PathProtectedResourceMetadata   = "/.well-known/oauth-protected-resource"
	PathRegister                    = "/oauth/register"
	PathAuthorize                   = "/oauth/authorize"
	PathToken                       = "/oauth/token"
)

// OAuth serves the authorisation endpoints and issues the tokens the MCP
// endpoint accepts.
type OAuth struct {
	// DB is control.db, where clients, codes and tokens live.
	DB *sql.DB

	// Keys authenticates the API key somebody proves themselves with at the
	// authorisation step, and is what an issued token ultimately stands for.
	Keys *apikeys.Store

	// BaseURL is the public address. Every URL in the metadata documents is
	// built from it, so a wrong value here produces metadata that sends clients
	// somewhere they cannot reach.
	BaseURL string

	Now func() time.Time
}

// now reads the clock.
func (o *OAuth) now() time.Time {
	if o.Now == nil {
		return time.Now().UTC()
	}

	return o.Now()
}

// Routes mounts the OAuth endpoints and the two metadata documents.
func (o *OAuth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+PathAuthorizationServerMetadata, o.authorizationServerMetadata)
	mux.HandleFunc("GET "+PathProtectedResourceMetadata, o.protectedResourceMetadata)

	// The path-suffixed form is what a client derives from a resource URL of
	// /mcp. Both are served because clients differ on which they try, and a
	// 404 on discovery is a connection that never starts.
	mux.HandleFunc("GET "+PathProtectedResourceMetadata+Path, o.protectedResourceMetadata)

	mux.HandleFunc("POST "+PathRegister, o.register)
	mux.HandleFunc("GET "+PathAuthorize, o.authorizeForm)
	mux.HandleFunc("POST "+PathAuthorize, o.authorizeSubmit)
	mux.HandleFunc("POST "+PathToken, o.token)
}

// authorizationServerMetadata describes this authorisation server.
func (o *OAuth) authorizationServerMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                o.BaseURL,
		"authorization_endpoint":                o.BaseURL + PathAuthorize,
		"token_endpoint":                        o.BaseURL + PathToken,
		"registration_endpoint":                 o.BaseURL + PathRegister,
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post"},

		// S256 only. OAuth 2.1 drops the "plain" challenge method, and offering
		// it would let a client opt out of the one protection that makes an
		// intercepted authorisation code useless.
		"code_challenge_methods_supported": []string{"S256"},
		"scopes_supported":                 oauthScopes,
	})
}

// protectedResourceMetadata tells a client which authorisation server guards
// the MCP endpoint. It is the document the WWW-Authenticate challenge points
// at, and the whole of automatic discovery hangs off it.
func (o *OAuth) protectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 o.BaseURL + Path,
		"authorization_servers":    []string{o.BaseURL},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         oauthScopes,
	})
}

// registrationRequest is a dynamic client registration.
type registrationRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope"`
}

// register creates a client on demand.
//
// Registration is open, which is the point: an assistant registers itself the
// first time somebody connects, and a gate here would mean a human step in the
// middle of what should be pasting a URL. What keeps it safe is that
// registering grants nothing — a client cannot read a single number until
// somebody has completed the authorisation step with a key of their own.
func (o *OAuth) register(w http.ResponseWriter, r *http.Request) {
	var request registrationRequest

	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes)).Decode(&request); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "the registration body is not valid JSON")
		return
	}

	if len(request.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}

	for _, uri := range request.RedirectURIs {
		if err := validateRedirectURI(uri); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}

	clientID, err := randomToken()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the client could not be registered")
		return
	}

	method := request.TokenEndpointAuthMethod
	if method == "" {
		method = "none"
	}
	if method != "none" && method != "client_secret_post" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata",
			"token_endpoint_auth_method must be none or client_secret_post")
		return
	}
	registeredScope, err := canonicalScopes(request.Scope)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}

	// A secret is only issued to a client that asked to authenticate with one.
	// A desktop assistant cannot keep a secret, and handing it one anyway would
	// be security theatre that clients then ship in a config file.
	secret := ""
	secretHash := ""

	if method == "client_secret_post" {
		secret, err = randomToken()
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "the client could not be registered")
			return
		}
		secretHash = hashToken(secret)
	}

	redirects, _ := json.Marshal(request.RedirectURIs)

	grants := request.GrantTypes
	if len(grants) == 0 {
		grants = []string{"authorization_code", "refresh_token"}
	}
	grantList, _ := json.Marshal(grants)

	if _, err := o.DB.ExecContext(r.Context(), `
		INSERT INTO mcp_oauth_clients
			(client_id, client_secret_hash, client_name, redirect_uris, grant_types, token_endpoint_auth_method, scope, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		clientID, secretHash, request.ClientName, string(redirects), string(grantList), method, registeredScope,
		o.now().Unix()); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the client could not be registered")
		return
	}

	response := map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        o.now().Unix(),
		"client_name":                request.ClientName,
		"redirect_uris":              request.RedirectURIs,
		"grant_types":                grants,
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
		"scope":                      registeredScope,
	}

	if secret != "" {
		response["client_secret"] = secret

		// Zero means it does not expire. A client that has to re-register to
		// keep working is a client that stops working while nobody is watching.
		response["client_secret_expires_at"] = 0
	}

	writeJSON(w, http.StatusCreated, response)
}

// validateRedirectURI refuses a callback we should not send a code to.
//
// http is allowed only on loopback, because that is how a desktop client
// receives its callback and there is no realistic way to intercept it there.
// Anything else must be https: a code delivered over plain http to a remote
// host is a code on the wire in the clear.
func validateRedirectURI(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a URL", raw)
	}

	if parsed.Fragment != "" {
		return errors.New("a redirect URI may not carry a fragment")
	}

	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		host := parsed.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return errors.New("http redirect URIs are only allowed on loopback")
	case "":
		return errors.New("a redirect URI must be absolute")
	default:
		// A private-use scheme is how a native app receives its callback, and
		// refusing them would lock out every desktop client.
		if strings.Contains(parsed.Scheme, ".") {
			return nil
		}

		return fmt.Errorf("redirect URI scheme %q is not supported", parsed.Scheme)
	}
}

// authorizeRequest is what the authorisation endpoint reads from the query.
type authorizeRequest struct {
	ClientID            string
	RedirectURI         string
	State               string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// parseAuthorize validates the authorisation parameters.
//
// The redirect URI is checked against what the client registered *before*
// anything is redirected. An authorisation server that redirects to an
// unregistered URI is one that can be used to deliver somebody else's code
// wherever an attacker likes.
func (o *OAuth) parseAuthorize(ctx context.Context, values url.Values) (*authorizeRequest, error) {
	request := &authorizeRequest{
		ClientID:            values.Get("client_id"),
		RedirectURI:         values.Get("redirect_uri"),
		State:               values.Get("state"),
		Scope:               values.Get("scope"),
		CodeChallenge:       values.Get("code_challenge"),
		CodeChallengeMethod: values.Get("code_challenge_method"),
	}

	if values.Get("response_type") != "code" {
		return nil, errors.New("response_type must be code")
	}

	if request.ClientID == "" {
		return nil, errors.New("client_id is required")
	}

	if request.CodeChallenge == "" {
		return nil, errors.New("code_challenge is required — this server requires PKCE")
	}

	if request.CodeChallengeMethod == "" {
		request.CodeChallengeMethod = "S256"
	}

	if request.CodeChallengeMethod != "S256" {
		return nil, errors.New("code_challenge_method must be S256")
	}

	registered, err := o.redirectURIs(ctx, request.ClientID)
	if err != nil {
		return nil, err
	}

	if request.RedirectURI == "" && len(registered) == 1 {
		request.RedirectURI = registered[0]
	}

	if !containsString(registered, request.RedirectURI) {
		return nil, errors.New("redirect_uri does not match anything this client registered")
	}

	var registeredScope string
	if err := o.DB.QueryRowContext(ctx, `SELECT scope FROM mcp_oauth_clients WHERE client_id = ?`, request.ClientID).
		Scan(&registeredScope); err != nil {
		return nil, errors.New("the client is not registered")
	}
	if request.Scope == "" {
		request.Scope = registeredScope
	}
	canonical, err := canonicalScopes(request.Scope)
	if err != nil {
		return nil, err
	}
	if registeredScope != "" && !scopeSubset(canonical, registeredScope) {
		return nil, errors.New("requested scope exceeds the client's registered scope")
	}
	request.Scope = canonical

	return request, nil
}

// scopeSubset reports whether every requested scope is in a canonical
// registered grant. Empty registration scope means the server's default set
// and is handled by the caller rather than as an empty grant.
func scopeSubset(requested, registered string) bool {
	for _, scope := range strings.Fields(requested) {
		if !containsString(strings.Fields(registered), scope) {
			return false
		}
	}

	return true
}

// canonicalScopes validates, deduplicates and stabilizes a requested grant.
func canonicalScopes(raw string) (string, error) {
	seen := map[string]bool{}
	for _, scope := range strings.Fields(raw) {
		if !containsString(oauthScopes, scope) {
			return "", fmt.Errorf("scope %q is not supported", scope)
		}
		seen[scope] = true
	}

	ordered := make([]string, 0, len(seen))
	for _, scope := range oauthScopes {
		if seen[scope] {
			ordered = append(ordered, scope)
		}
	}

	return strings.Join(ordered, " "), nil
}

// grantedScopes resolves an omitted grant to every scope the key carries and
// refuses a request that tries to widen a restricted API key.
func grantedScopes(key *apikeys.Key, requested string) (string, error) {
	if requested == "" {
		allowed := make([]string, 0, len(oauthScopes))
		for _, scope := range oauthScopes {
			if key.Allows(scope) {
				allowed = append(allowed, scope)
			}
		}

		return strings.Join(allowed, " "), nil
	}

	for _, scope := range strings.Fields(requested) {
		if !key.Allows(scope) {
			return "", fmt.Errorf("that API key does not grant the %s scope", scope)
		}
	}

	return requested, nil
}

// redirectURIs reads what a client registered.
func (o *OAuth) redirectURIs(ctx context.Context, clientID string) ([]string, error) {
	var encoded string

	err := o.DB.QueryRowContext(ctx, `SELECT redirect_uris FROM mcp_oauth_clients WHERE client_id = ?`, clientID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("no client is registered with that client_id")
	}
	if err != nil {
		return nil, errors.New("the client could not be read")
	}

	var uris []string
	if err := json.Unmarshal([]byte(encoded), &uris); err != nil {
		return nil, errors.New("the client's redirect URIs could not be read")
	}

	return uris, nil
}

// consentPage is what somebody sees when a client sends them here.
//
// It asks for an API key rather than a password. Sign-in belongs to the rest of
// the product; a key is already this API's credential, is revocable on its own,
// and means this page never handles a password — so a mistake here cannot cost
// somebody their account.
var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect to Feasible</title>
<style>
  body { font: 16px/1.5 system-ui, sans-serif; max-width: 34rem; margin: 4rem auto; padding: 0 1rem; }
  label { display: block; margin: 1.5rem 0 .5rem; font-weight: 600; }
  input[type=password] { width: 100%; padding: .6rem; font: inherit; box-sizing: border-box; }
  button { margin-top: 1.5rem; padding: .6rem 1.2rem; font: inherit; cursor: pointer; }
  .who { background: #f4f4f5; padding: 1rem; border-radius: .4rem; }
  .error { color: #b00020; font-weight: 600; }
</style></head><body>
<h1>Connect to Feasible</h1>
<p class="who"><strong>{{.ClientName}}</strong> is asking to read the analytics for one of your teams.</p>
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
<form method="post" action="{{.Action}}">
  <input type="hidden" name="client_id" value="{{.ClientID}}">
  <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
  <input type="hidden" name="state" value="{{.State}}">
  <input type="hidden" name="scope" value="{{.Scope}}">
  <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
  <input type="hidden" name="code_challenge_method" value="{{.CodeChallengeMethod}}">
  <input type="hidden" name="response_type" value="code">
  <label for="key">Your API key</label>
  <input id="key" name="api_key" type="password" autocomplete="off" spellcheck="false"
         placeholder="feas_…" required>
  <p>Create one in your dashboard under API keys. The connection can do whatever that key can do,
     and revoking the key ends it.</p>
  <button type="submit">Allow access</button>
</form>
</body></html>`))

// consentData is what the page renders from.
type consentData struct {
	ClientName          string
	ClientID            string
	RedirectURI         string
	State               string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	Action              string
	Error               string
}

// authorizeForm shows the consent page.
func (o *OAuth) authorizeForm(w http.ResponseWriter, r *http.Request) {
	request, err := o.parseAuthorize(r.Context(), r.URL.Query())
	if err != nil {
		// Nothing is redirected on a parameter failure. Redirecting an
		// unvalidated redirect_uri is the open-redirect this check exists to
		// prevent, so the error is rendered here instead.
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	o.renderConsent(w, request, "")
}

// renderConsent writes the page.
func (o *OAuth) renderConsent(w http.ResponseWriter, request *authorizeRequest, message string) {
	name := o.clientName(request.ClientID)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// The page is never cached: it carries a one-time authorisation context and
	// is submitted with a credential.
	w.Header().Set("Cache-Control", "no-store")

	_ = consentPage.Execute(w, consentData{
		ClientName:          name,
		ClientID:            request.ClientID,
		RedirectURI:         request.RedirectURI,
		State:               request.State,
		Scope:               request.Scope,
		CodeChallenge:       request.CodeChallenge,
		CodeChallengeMethod: request.CodeChallengeMethod,
		Action:              o.BaseURL + PathAuthorize,
		Error:               message,
	})
}

// clientName reads a client's own name for the consent page, falling back to
// something honest rather than to a blank space.
func (o *OAuth) clientName(clientID string) string {
	var name string

	if err := o.DB.QueryRow(`SELECT client_name FROM mcp_oauth_clients WHERE client_id = ?`, clientID).Scan(&name); err != nil {
		return "An application"
	}

	if strings.TrimSpace(name) == "" {
		return "An unnamed application"
	}

	return name
}

// authorizeSubmit checks the key and issues a code.
func (o *OAuth) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the form could not be read")
		return
	}

	request, err := o.parseAuthorize(r.Context(), r.PostForm)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	key, err := o.Keys.Authenticate(r.Context(), r.PostFormValue("api_key"))
	if err != nil {
		// The form comes back rather than redirecting with an error: the person
		// mistyped their key, and sending them back to the client to start
		// again is a worse experience than one more attempt here.
		o.renderConsent(w, request, "That key is not valid. Check it and try again.")
		return
	}

	request.Scope, err = grantedScopes(key, request.Scope)
	if err != nil {
		o.renderConsent(w, request, err.Error())
		return
	}

	code, err := randomToken()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the authorisation could not be completed")
		return
	}

	now := o.now()

	if _, err := o.DB.ExecContext(r.Context(), `
		INSERT INTO mcp_oauth_codes
			(code_hash, client_id, team_id, api_key_id, redirect_uri, scope, code_challenge, code_challenge_method, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hashToken(code), request.ClientID, key.TeamID, key.ID, request.RedirectURI, request.Scope,
		request.CodeChallenge, request.CodeChallengeMethod,
		now.Unix(), now.Add(authorizationCodeLifetime).Unix()); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the authorisation could not be recorded")
		return
	}

	target, err := url.Parse(request.RedirectURI)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the redirect URI could not be built")
		return
	}

	query := target.Query()
	query.Set("code", code)

	if request.State != "" {
		query.Set("state", request.State)
	}

	target.RawQuery = query.Encode()

	http.Redirect(w, r, target.String(), http.StatusFound)
}

// token exchanges a code or a refresh token for an access token.
func (o *OAuth) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the form could not be read")
		return
	}

	clientID, ok := o.authenticateTokenClient(w, r)
	if !ok {
		return
	}

	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		o.exchangeCode(w, r, clientID)
	case "refresh_token":
		o.refresh(w, r, clientID)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type",
			"grant_type must be authorization_code or refresh_token")
	}
}

// authenticateTokenClient enforces the method a dynamically registered client
// selected. Public clients identify themselves; confidential clients must also
// prove the secret issued at registration on every code and refresh exchange.
func (o *OAuth) authenticateTokenClient(w http.ResponseWriter, r *http.Request) (string, bool) {
	clientID := r.PostFormValue("client_id")
	if clientID == "" {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client_id is required")
		return "", false
	}

	var method, storedHash string
	err := o.DB.QueryRowContext(r.Context(), `
		SELECT token_endpoint_auth_method, client_secret_hash
		FROM mcp_oauth_clients WHERE client_id = ?`, clientID).Scan(&method, &storedHash)
	if err != nil {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return "", false
	}

	switch method {
	case "none":
		if r.PostFormValue("client_secret") != "" {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "public clients do not use a client_secret")
			return "", false
		}
	case "client_secret_post":
		presented := r.PostFormValue("client_secret")
		presentedHash := hashToken(presented)
		if presented == "" || subtle.ConstantTimeCompare([]byte(presentedHash), []byte(storedHash)) != 1 {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
			return "", false
		}
	default:
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "the client's authentication method is not supported")
		return "", false
	}

	return clientID, true
}

// exchangeCode turns an authorisation code and its PKCE verifier into tokens.
func (o *OAuth) exchangeCode(w http.ResponseWriter, r *http.Request, authenticatedClientID string) {
	code := r.PostFormValue("code")
	verifier := r.PostFormValue("code_verifier")

	if code == "" || verifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code and code_verifier are both required")
		return
	}

	var (
		id          int64
		clientID    string
		teamID      int64
		apiKeyID    sql.NullInt64
		redirectURI string
		scope       string
		challenge   string
		expiresAt   int64
		consumedAt  sql.NullInt64
	)

	tx, err := o.DB.BeginTx(r.Context(), nil)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the code could not be consumed")
		return
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	// Acquire SQLite's single writer lock before reading the one-time row. Two
	// independent process connections now serialize before either can observe
	// the code as unused.
	if _, err := tx.ExecContext(r.Context(), `UPDATE mcp_oauth_codes SET consumed_at = consumed_at WHERE id = -1`); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the code could not be consumed")
		return
	}

	err = tx.QueryRowContext(r.Context(), `
		SELECT id, client_id, team_id, api_key_id, redirect_uri, scope, code_challenge, expires_at, consumed_at
		FROM mcp_oauth_codes WHERE code_hash = ? AND client_id = ?`, hashToken(code), authenticatedClientID).
		Scan(&id, &clientID, &teamID, &apiKeyID, &redirectURI, &scope, &challenge, &expiresAt, &consumedAt)

	if errors.Is(err, sql.ErrNoRows) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "that authorisation code is not valid")
		return
	}
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the code could not be read")
		return
	}

	if consumedAt.Valid {
		// Release the transaction's writer lock before revoking the replayed
		// grant through the database handle. A replay is evidence that both the
		// intended client and another party may hold the code, so neither keeps
		// tokens minted from it.
		_ = tx.Rollback()
		o.revokeGrant(r.Context(), clientID, teamID)
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "that authorisation code has already been used")
		return
	}

	now := o.now()
	if now.Unix() >= expiresAt {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "that authorisation code has expired")
		return
	}

	if given := r.PostFormValue("redirect_uri"); given != "" && given != redirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the one the code was issued for")
		return
	}

	if !verifyPKCE(challenge, verifier) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the code_verifier does not match the code_challenge")
		return
	}

	result, err := tx.ExecContext(r.Context(), `
		UPDATE mcp_oauth_codes SET consumed_at = ?
		WHERE id = ? AND consumed_at IS NULL AND expires_at > ?`, now.Unix(), id, now.Unix())
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the code could not be consumed")
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "that authorisation code is no longer valid")
		return
	}

	pair, err := o.issueTx(r.Context(), tx, clientID, teamID, apiKeyID, scope, "", 0, now)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the token could not be stored")
		return
	}
	if err := tx.Commit(); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the token could not be stored")
		return
	}

	o.writeTokenPair(w, pair)
}

// refresh exchanges a refresh token for a new pair.
func (o *OAuth) refresh(w http.ResponseWriter, r *http.Request, authenticatedClientID string) {
	presented := r.PostFormValue("refresh_token")
	if presented == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}

	var (
		id               int64
		clientID         string
		teamID           int64
		apiKeyID         sql.NullInt64
		scope            string
		refreshExpiresAt int64
		revokedAt        sql.NullInt64
		familyID         string
	)

	tx, err := o.DB.BeginTx(r.Context(), nil)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the token could not be rotated")
		return
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless

	if _, err := tx.ExecContext(r.Context(), `UPDATE mcp_oauth_tokens SET revoked_at = revoked_at WHERE id = -1`); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the token could not be rotated")
		return
	}

	err = tx.QueryRowContext(r.Context(), `
		SELECT id, client_id, team_id, api_key_id, scope, refresh_expires_at, revoked_at, token_family_id
		FROM mcp_oauth_tokens WHERE refresh_token_hash = ? AND client_id = ?`,
		hashToken(presented), authenticatedClientID).
		Scan(&id, &clientID, &teamID, &apiKeyID, &scope, &refreshExpiresAt, &revokedAt, &familyID)

	if errors.Is(err, sql.ErrNoRows) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "that refresh token is not valid")
		return
	}
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the token could not be read")
		return
	}

	now := o.now()
	if revokedAt.Valid {
		if err := o.revokeFamilyTx(r.Context(), tx, familyID, now); err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "the compromised token family could not be revoked")
			return
		}
		if err := tx.Commit(); err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "the compromised token family could not be revoked")
			return
		}
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "that refresh token has been revoked")
		return
	}
	if now.Unix() >= refreshExpiresAt {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "that refresh token has expired")
		return
	}

	// The old row is revoked as the new one is issued. Rotating on every
	// refresh means a stolen refresh token stops working the moment the real
	// client uses its own, which is the only way the theft becomes visible.
	result, err := tx.ExecContext(r.Context(), `
		UPDATE mcp_oauth_tokens SET revoked_at = ?
		WHERE id = ? AND revoked_at IS NULL AND refresh_expires_at > ?`, now.Unix(), id, now.Unix())
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the token could not be rotated")
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		if err := o.revokeFamilyTx(r.Context(), tx, familyID, now); err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "the compromised token family could not be revoked")
			return
		}
		if err := tx.Commit(); err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "the compromised token family could not be revoked")
			return
		}
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "that refresh token is no longer valid")
		return
	}

	pair, err := o.issueTx(r.Context(), tx, clientID, teamID, apiKeyID, scope, familyID, id, now)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the token could not be stored")
		return
	}
	if err := tx.Commit(); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the token could not be stored")
		return
	}

	o.writeTokenPair(w, pair)
}

// tokenPair carries plaintext tokens only between the transaction that stores
// their hashes and the response written after that transaction commits.
type tokenPair struct {
	Access  string
	Refresh string
	Scope   string
}

// issueTx mints and stores a token pair in its caller's one-time-token
// transaction. Consumption and replacement therefore commit or roll back as a
// unit, including under independent database connections.
func (o *OAuth) issueTx(ctx context.Context, tx *sql.Tx, clientID string, teamID int64,
	apiKeyID sql.NullInt64, scope, familyID string, parentID int64, now time.Time) (tokenPair, error) {
	access, err := randomToken()
	if err != nil {
		return tokenPair{}, err
	}

	refresh, err := randomToken()
	if err != nil {
		return tokenPair{}, err
	}
	if familyID == "" {
		familyToken, err := randomToken()
		if err != nil {
			return tokenPair{}, err
		}
		familyID = hashToken(familyToken)
	}

	var parent any
	if parentID != 0 {
		parent = parentID
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mcp_oauth_tokens
			(token_hash, refresh_token_hash, client_id, team_id, api_key_id, scope,
			 created_at, expires_at, refresh_expires_at, token_family_id, parent_token_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hashToken(access), hashToken(refresh), clientID, teamID, apiKeyID, scope,
		now.Unix(), now.Add(AccessTokenLifetime).Unix(), now.Add(RefreshTokenLifetime).Unix(), familyID, parent); err != nil {
		return tokenPair{}, err
	}

	return tokenPair{Access: access, Refresh: refresh, Scope: scope}, nil
}

// revokeFamilyTx revokes every access and refresh token descended from one
// grant while the caller still owns SQLite's writer reservation. A replaying
// request therefore cannot race the just-issued child into remaining valid.
func (o *OAuth) revokeFamilyTx(ctx context.Context, tx *sql.Tx, familyID string, now time.Time) error {
	if familyID == "" {
		return errors.New("mcp oauth: token has no family lineage")
	}

	_, err := tx.ExecContext(ctx, `
		UPDATE mcp_oauth_tokens
		SET revoked_at = COALESCE(revoked_at, ?)
		WHERE token_family_id = ?
	`, now.Unix(), familyID)
	if err != nil {
		return fmt.Errorf("mcp oauth: revoke token family: %w", err)
	}

	return nil
}

// writeTokenPair writes a committed token pair with cache prevention.
func (o *OAuth) writeTokenPair(w http.ResponseWriter, pair tokenPair) {
	// no-store, because a token in a proxy cache is a token anybody behind that
	// proxy can have.
	w.Header().Set("Cache-Control", "no-store")

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  pair.Access,
		"token_type":    "Bearer",
		"expires_in":    int(AccessTokenLifetime.Seconds()),
		"refresh_token": pair.Refresh,
		"scope":         pair.Scope,
	})
}

// revokeGrant kills every live token for one client and team. It runs when a
// code is replayed, which means somebody other than the real client has it.
func (o *OAuth) revokeGrant(ctx context.Context, clientID string, teamID int64) {
	_, _ = o.DB.ExecContext(ctx,
		`UPDATE mcp_oauth_tokens SET revoked_at = ? WHERE client_id = ? AND team_id = ? AND revoked_at IS NULL`,
		o.now().Unix(), clientID, teamID)
}

// Authenticate turns a bearer token from the MCP endpoint into a credential.
//
// It accepts both an OAuth access token and a plain API key. Two kinds of token
// on one endpoint sounds like a smell, but it is what makes the same URL work
// for a remote assistant that went through the authorisation flow and for a
// script that just has a key — and both end up as the same key underneath.
func (o *OAuth) Authenticate(ctx context.Context, token string) (*apikeys.Key, error) {
	if strings.HasPrefix(token, apikeys.Prefix) {
		return o.Keys.Authenticate(ctx, token)
	}

	var (
		teamID    int64
		apiKeyID  sql.NullInt64
		scope     string
		expiresAt int64
		revokedAt sql.NullInt64
	)

	err := o.DB.QueryRowContext(ctx, `
		SELECT team_id, api_key_id, scope, expires_at, revoked_at
		FROM mcp_oauth_tokens WHERE token_hash = ?`,
		hashToken(token)).Scan(&teamID, &apiKeyID, &scope, &expiresAt, &revokedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("that token is not valid")
	}
	if err != nil {
		return nil, errors.New("the token could not be read")
	}

	if revokedAt.Valid {
		return nil, errors.New("that token has been revoked")
	}

	if o.now().Unix() >= expiresAt {
		return nil, errors.New("that token has expired — refresh it")
	}

	// The token stands for the key it was authorised with, so revoking that key
	// ends every connection made with it. Without this, revoking a key would
	// leave every assistant that had ever used it still connected.
	if apiKeyID.Valid {
		key, err := o.keyByID(ctx, apiKeyID.Int64)
		if err != nil {
			return nil, err
		}
		if key.TeamID != teamID {
			return nil, errors.New("that token is not bound to its API key's team")
		}

		// A non-nil slice distinguishes an intentionally empty OAuth grant from
		// a directly presented API key whose empty scope list means all scopes.
		key.GrantedScopes = append([]string{}, strings.Fields(scope)...)

		return key, nil
	}

	return nil, errors.New("that token is not bound to an active API key")
}

// keyByID reads the key an access token stands for, refusing a revoked key or
// one whose owner is no longer a member of the team it was issued against.
func (o *OAuth) keyByID(ctx context.Context, id int64) (*apikeys.Key, error) {
	var (
		key     apikeys.Key
		scopes  string
		revoked sql.NullInt64
	)

	err := o.DB.QueryRowContext(ctx, `
		SELECT api_keys.id, api_keys.team_id, api_keys.user_id, team_memberships.role, api_keys.name,
		       api_keys.scopes, api_keys.hourly_limit, api_keys.revoked_at
		FROM api_keys
		JOIN team_memberships
		  ON team_memberships.team_id = api_keys.team_id
		 AND team_memberships.user_id = api_keys.user_id
		WHERE api_keys.id = ?`, id).
		Scan(&key.ID, &key.TeamID, &key.UserID, &key.Role, &key.Name, &scopes, &key.HourlyLimit, &revoked)

	if errors.Is(err, sql.ErrNoRows) || (err == nil && revoked.Valid) {
		return nil, errors.New("the key this connection was authorised with has been revoked")
	}
	if err != nil {
		return nil, errors.New("the key could not be read")
	}

	if err := json.Unmarshal([]byte(scopes), &key.Scopes); err != nil {
		return nil, errors.New("the key's scopes could not be read")
	}

	return &key, nil
}

// verifyPKCE checks a verifier against its challenge.
func verifyPKCE(challenge, verifier string) bool {
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])

	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// randomToken mints an unguessable string.
func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// hashToken is the stored form of any bearer credential in this file. Like the
// API keys, these are high-entropy random strings rather than passwords, so a
// plain SHA-256 is the right function: there is no dictionary to slow down.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// containsString reports membership.
func containsString(list []string, value string) bool {
	for _, entry := range list {
		if entry == value {
			return true
		}
	}

	return false
}

// writeJSON writes a response body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(body)
}

// writeOAuthError writes the error shape OAuth clients parse. The description
// is for the developer reading a log, not for the end user, so it says what is
// actually wrong rather than something reassuring.
func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}
