//
// google.go
// The one OAuth client, and the per-site tokens it hands out.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package google is the Google half of the data features: importing history
// from Analytics 4, and reading Search Console. Both run on one OAuth
// application — the same one signing in with Google uses — with scopes
// requested when the customer first asks for the feature rather than all at
// once at sign-up.
//
// Two decisions here are worth more than the rest of the package.
//
// Tokens are stored per site and account, never per Google account. On an
// incumbent's self-hosted build, connecting a second site with the same Google
// account invalidated the first site's refresh token and it was never root
// caused; the shape that makes that possible is one token row shared by every
// site the Google account touches. Here the row is keyed (site, provider), so
// two sites hold two independent grants and revoking one cannot reach the
// other.
//
// The credentials are optional. With none configured every Google feature hides
// itself and the process says so once at start-up, because a button that sends
// somebody to Google and comes back with invalid_client is worse than no button.
package google

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The OAuth endpoints. They are variables rather than constants so a test can
// point the whole package at a local server without a network.
var (
	AuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	TokenURL     = "https://oauth2.googleapis.com/token"
	AnalyticsAPI = "https://analyticsdata.googleapis.com"
	SearchAPI    = "https://searchconsole.googleapis.com"
)

// Providers. They are the two halves of the integration and they are stored
// separately, because a customer may want their history imported without giving
// us their search data, and the scopes differ.
const (
	ProviderGA4           = "ga4"
	ProviderSearchConsole = "search_console"
)

// Connection statuses.
const (
	StatusConnected      = "connected"
	StatusNeedsReconnect = "needs_reconnect"
)

// Scopes, one per provider. Both are read-only: nothing in this product has any
// business writing to a customer's Analytics property.
const (
	ScopeAnalytics     = "https://www.googleapis.com/auth/analytics.readonly"
	ScopeSearchConsole = "https://www.googleapis.com/auth/webmasters.readonly"
)

// SearchConsoleDelay is how far behind Search Console runs. Google's own data
// is a day to a day and a half old, so "today" and usually "yesterday" are
// legitimately empty — and if the interface does not say so, every new customer
// files the same bug.
const SearchConsoleDelay = 36 * time.Hour

// SearchConsoleDelayNotice names the sentence the settings and report pages
// show. It is a catalogue id rather than the sentence itself, because the copy
// a customer reads lives with every other translated string.
const SearchConsoleDelayNotice = "auth.imports.search_console_delay"

// ErrInvalidGrant is a refresh token Google will not honour any more: the
// customer revoked access, changed their password, or the grant expired. It is
// a named error because the only correct response is to ask them to reconnect,
// and a job that retried it would fail identically every night with nobody
// watching.
var ErrInvalidGrant = errors.New("google: the refresh token is no longer valid — reconnect the account")

// ErrNotConfigured is returned when no OAuth client is set. Callers turn it
// into a hidden feature rather than an error page.
var ErrNotConfigured = errors.New("google: no OAuth client is configured")

// App is the OAuth client. It is a value rather than a package variable so a
// test can construct one without touching the process environment.
type App struct {
	ClientID     string
	ClientSecret string

	// RedirectURL is derived from the base URL, so it moves when the app's
	// hostname does. Google rejects a redirect URI that is not registered
	// exactly, which is why this is built in one place.
	RedirectURL string

	// HTTP is the client every call goes through, injectable for tests.
	HTTP *http.Client
}

// NewApp builds the OAuth client from configuration. It returns ok=false when
// either half is missing, which is the signal callers use to hide the feature.
func NewApp(clientID, clientSecret, baseURL string) (*App, bool) {
	if clientID == "" || clientSecret == "" {
		return nil, false
	}

	return &App{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  strings.TrimRight(baseURL, "/") + CallbackPath,
		HTTP:         &http.Client{Timeout: 30 * time.Second},
	}, true
}

// CallbackPath is where Google returns the customer. It is a constant because
// the redirect URI has to match what is registered on the Google application
// character for character.
const CallbackPath = "/settings/google/callback"

// client returns the HTTP client to use.
func (a *App) client() *http.Client {
	if a.HTTP == nil {
		return http.DefaultClient
	}

	return a.HTTP
}

// AuthorizeURL builds the URL to send a customer to.
//
// The state carries the site and the provider, because the callback has no
// other way to know which site the customer was configuring — and guessing from
// a session would connect the wrong site for anybody with two tabs open.
func (a *App) AuthorizeURL(state, scope string) string {
	values := url.Values{}
	values.Set("client_id", a.ClientID)
	values.Set("redirect_uri", a.RedirectURL)
	values.Set("response_type", "code")
	values.Set("scope", scope)
	values.Set("state", state)

	// A refresh token is only issued with consent forced and offline access
	// asked for. Without both, a reconnect silently returns an access token
	// that expires in an hour and the nightly import stops working tomorrow.
	values.Set("access_type", "offline")
	values.Set("prompt", "consent")
	values.Set("include_granted_scopes", "true")

	return AuthorizeURL + "?" + values.Encode()
}

// Token is what Google returns from either token call.
type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Scope        string
}

// tokenResponse is the wire form.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Exchange turns an authorisation code into tokens.
func (a *App) Exchange(ctx context.Context, code string, now time.Time) (*Token, error) {
	values := url.Values{}
	values.Set("code", code)
	values.Set("client_id", a.ClientID)
	values.Set("client_secret", a.ClientSecret)
	values.Set("redirect_uri", a.RedirectURL)
	values.Set("grant_type", "authorization_code")

	return a.token(ctx, values, now)
}

// Refresh exchanges a refresh token for a fresh access token. An invalid_grant
// here is the one failure that must not be retried: the customer has to
// reconnect, and only they can.
func (a *App) Refresh(ctx context.Context, refreshToken string, now time.Time) (*Token, error) {
	values := url.Values{}
	values.Set("refresh_token", refreshToken)
	values.Set("client_id", a.ClientID)
	values.Set("client_secret", a.ClientSecret)
	values.Set("grant_type", "refresh_token")

	token, err := a.token(ctx, values, now)
	if err != nil {
		return nil, err
	}

	// A refresh response usually omits the refresh token, and overwriting the
	// stored one with an empty string would disconnect the site on the first
	// successful refresh.
	if token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}

	return token, nil
}

// token performs one token request.
func (a *App) token(ctx context.Context, values url.Values, now time.Time) (token *Token, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, fmt.Errorf("google: token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := a.client().Do(request)
	if err != nil {
		return nil, fmt.Errorf("google: token request: %w", err)
	}
	defer closeResource(response.Body, &err, "token response")

	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("google: token response: %w", err)
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("google: token response was not JSON: %w", err)
	}

	if parsed.Error == "invalid_grant" {
		return nil, ErrInvalidGrant
	}

	if parsed.Error != "" {
		return nil, fmt.Errorf("google: %s: %s", parsed.Error, parsed.ErrorDescription)
	}

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google: the token endpoint answered %d", response.StatusCode)
	}

	return &Token{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresAt:    now.Add(time.Duration(parsed.ExpiresIn) * time.Second),
		Scope:        parsed.Scope,
	}, nil
}

// closeResource closes a Google API resource and preserves both the request
// failure and a transport or statement cleanup failure when they coincide.
func closeResource(resource io.Closer, err *error, operation string) {
	if closeErr := resource.Close(); closeErr != nil {
		*err = errors.Join(*err, fmt.Errorf("google: close %s: %w", operation, closeErr))
	}
}

// Connection is one site's grant for one provider.
type Connection struct {
	ID           int64
	SiteID       int64
	AccountID    int64
	Provider     string
	GoogleEmail  string
	Property     string
	RefreshToken string
	AccessToken  string
	ExpiresAt    int64
	Scopes       string
	Status       string
	Failure      string
}

// NeedsReconnect reports whether the customer has to authorise again.
func (c Connection) NeedsReconnect() bool {
	return c.Status == StatusNeedsReconnect
}

// SaveConnection writes or replaces one site's grant.
//
// The upsert is keyed on (site, provider) and nothing else. That is the whole
// fix for the incumbent's bug: connecting a second site with the same Google
// account writes a second row, and cannot touch the first site's token.
func SaveConnection(ctx context.Context, db *sql.DB, connection Connection, now time.Time) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO google_connections
			(site_id, account_id, provider, google_email, property, refresh_token, access_token,
			 expires_at, scopes, status, failure, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)
		ON CONFLICT(site_id, provider) DO UPDATE SET
			google_email  = excluded.google_email,
			property      = excluded.property,
			refresh_token = excluded.refresh_token,
			access_token  = excluded.access_token,
			expires_at    = excluded.expires_at,
			scopes        = excluded.scopes,
			status        = excluded.status,
			failure       = '',
			updated_at    = excluded.updated_at`,
		connection.SiteID, connection.AccountID, connection.Provider, connection.GoogleEmail,
		connection.Property, connection.RefreshToken, connection.AccessToken,
		connection.ExpiresAt, connection.Scopes, StatusConnected, now.Unix(), now.Unix())
	if err != nil {
		return fmt.Errorf("google: save connection: %w", err)
	}

	return nil
}

// GetConnection reads one site's grant for a provider.
func GetConnection(ctx context.Context, db *sql.DB, siteID int64, provider string) (*Connection, error) {
	var connection Connection
	var expires sql.NullInt64

	err := db.QueryRowContext(ctx, `
		SELECT id, site_id, account_id, provider, google_email, property,
		       refresh_token, access_token, expires_at, scopes, status, failure
		FROM google_connections WHERE site_id = ? AND provider = ?`, siteID, provider).
		Scan(&connection.ID, &connection.SiteID, &connection.AccountID, &connection.Provider,
			&connection.GoogleEmail, &connection.Property, &connection.RefreshToken,
			&connection.AccessToken, &expires, &connection.Scopes, &connection.Status, &connection.Failure)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("google: read connection: %w", err)
	}

	connection.ExpiresAt = expires.Int64

	return &connection, nil
}

// DeleteConnection removes one site's grant.
func DeleteConnection(ctx context.Context, db *sql.DB, siteID int64, provider string) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM google_connections WHERE site_id = ? AND provider = ?", siteID, provider); err != nil {
		return fmt.Errorf("google: delete connection: %w", err)
	}

	return nil
}

// MarkNeedsReconnect records that a grant has stopped working, with the reason.
// It is what turns a nightly failure into a button the customer can press,
// rather than a job that fails silently until somebody notices the numbers
// stopped moving.
func MarkNeedsReconnect(ctx context.Context, db *sql.DB, siteID int64, provider, reason string, now time.Time) error {
	_, err := db.ExecContext(ctx,
		"UPDATE google_connections SET status = 'needs_reconnect', failure = ?, updated_at = ? WHERE site_id = ? AND provider = ?",
		reason, now.Unix(), siteID, provider)
	if err != nil {
		return fmt.Errorf("google: mark reconnect: %w", err)
	}

	return nil
}

// AccessToken returns a usable access token for a connection, refreshing it
// when it is close to expiry. A grant Google has stopped honouring is recorded
// as needing a reconnect before the error is returned, so the state the
// customer sees and the error the job reports can never disagree.
func (a *App) AccessToken(ctx context.Context, db *sql.DB, connection *Connection, now time.Time) (string, error) {
	// A minute of slack. A token that expires while a request is in flight
	// fails with a 401 that looks like a permissions problem.
	if connection.AccessToken != "" && connection.ExpiresAt > now.Add(time.Minute).Unix() {
		return connection.AccessToken, nil
	}

	if connection.RefreshToken == "" {
		return "", ErrInvalidGrant
	}

	token, err := a.Refresh(ctx, connection.RefreshToken, now)
	if err != nil {
		if errors.Is(err, ErrInvalidGrant) {
			if markErr := MarkNeedsReconnect(ctx, db, connection.SiteID, connection.Provider,
				"Google no longer accepts the stored authorisation for this site — reconnect it to resume importing", now); markErr != nil {
				return "", markErr
			}
		}

		return "", err
	}

	connection.AccessToken = token.AccessToken
	connection.RefreshToken = token.RefreshToken
	connection.ExpiresAt = token.ExpiresAt.Unix()

	if err := SaveConnection(ctx, db, *connection, now); err != nil {
		return "", err
	}

	return token.AccessToken, nil
}
