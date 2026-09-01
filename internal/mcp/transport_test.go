//
// transport_test.go
// Streamable HTTP, stdio, and the OAuth flow a remote client walks on its own.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// checkClose runs one test cleanup and reports a failure against the test that
// owns the resource instead of silently discarding it.
func checkClose(t testing.TB, name string, close func() error) {
	t.Helper()
	if err := close(); err != nil {
		t.Errorf("close %s: %v", name, err)
	}
}

// closeResponse closes a test HTTP response through the shared cleanup check.
func closeResponse(t testing.TB, response *http.Response) {
	t.Helper()
	checkClose(t, "response body", response.Body.Close)
}

// httpFixture wraps the shared fixture with a mounted HTTP endpoint and the
// OAuth server in front of it.
type httpFixture struct {
	*fixture

	Server *httptest.Server
	OAuth  *OAuth
}

// newHTTPFixture mounts the endpoint the way `serve` mounts it.
func newHTTPFixture(t *testing.T) *httpFixture {
	t.Helper()

	f := newFixture(t)

	mux := http.NewServeMux()

	// The base URL is only known once the test server is listening, and the
	// metadata documents are built from it, so the OAuth server is created here
	// and its base filled in below.
	oauth := &OAuth{DB: f.Control, Keys: f.API.Keys, Now: func() time.Time { return testNow }}

	mux.Handle(Path, &Handler{
		Server:              f.Server,
		Authenticate:        oauth.Authenticate,
		ResourceMetadataURL: "http://replaced" + PathProtectedResourceMetadata,
	})

	oauth.Routes(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	oauth.BaseURL = server.URL

	return &httpFixture{fixture: f, Server: server, OAuth: oauth}
}

// rpc posts one JSON-RPC message with a bearer token.
func (h *httpFixture) rpc(t *testing.T, token, body string) (int, map[string]any) {
	t.Helper()

	request, err := http.NewRequest(http.MethodPost, h.Server.URL+Path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	decoded := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}

	return response.StatusCode, decoded
}

// TestHTTPRequiresABearerToken checks the refusal, including the header that
// makes automatic discovery work. A client that reads the resource-metadata
// parameter finds the authorisation server, registers itself and comes back with
// a token, with nobody pasting anything.
func TestHTTPRequiresABearerToken(t *testing.T) {
	h := newHTTPFixture(t)

	request, err := http.NewRequest(http.MethodPost, h.Server.URL+Path,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)

	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}

	challenge := response.Header.Get("WWW-Authenticate")

	if !strings.Contains(challenge, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", challenge)
	}

	if !strings.Contains(challenge, "resource_metadata=") {
		t.Errorf("the challenge does not point at the metadata a client discovers from: %q", challenge)
	}
}

// TestInitializeIsAllowedWithoutAToken checks the one exception. A client has to
// be able to find out what this server is and which authorisation server it uses
// before it has a token; refusing the handshake outright leaves it with no way
// to start.
func TestInitializeIsAllowedWithoutAToken(t *testing.T) {
	h := newHTTPFixture(t)

	status, body := h.rpc(t, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+ProtocolVersion+`","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want the handshake to be allowed", status)
	}

	if _, ok := body["result"]; !ok {
		t.Fatalf("no result: %+v", body)
	}
}

// TestHTTPCallsToolsWithAnAPIKey checks the plain-key path, which is what a
// script or a self-hoster uses without going near OAuth.
func TestHTTPCallsToolsWithAnAPIKey(t *testing.T) {
	h := newHTTPFixture(t)

	status, body := h.rpc(t, h.Raw,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sites","arguments":{}}}`)

	if status != http.StatusOK {
		t.Fatalf("status = %d (%+v)", status, body)
	}

	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %+v", body)
	}

	if !strings.Contains(marshal(t, result), "example.com") {
		t.Errorf("list_sites did not name the site: %+v", result)
	}
}

// TestLeavingTheTeamEndsPlainAndOAuthMCPAccess proves both remote credential
// forms consult live membership. OAuth tokens stand for the API key that
// authorized them and may not outlive that key owner's place on the team.
func TestLeavingTheTeamEndsPlainAndOAuthMCPAccess(t *testing.T) {
	h := newHTTPFixture(t)
	clientID, code, verifier := h.authorize(t)
	token := postForm(t, h.Server.URL+PathToken, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})
	access := token["access_token"].(string)

	for name, credential := range map[string]string{"plain": h.Raw, "oauth": access} {
		if status, _ := h.rpc(t, credential, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); status != http.StatusOK {
			t.Fatalf("%s credential did not work before leaving: %d", name, status)
		}
	}

	if _, err := h.Control.Exec(`DELETE FROM team_memberships WHERE team_id = ? AND user_id = 1`, teamID); err != nil {
		t.Fatal(err)
	}

	for name, credential := range map[string]string{"plain": h.Raw, "oauth": access} {
		if status, _ := h.rpc(t, credential, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); status != http.StatusUnauthorized {
			t.Fatalf("%s credential answered %d after leaving, want 401", name, status)
		}
	}
}

// TestRoleDemotionRestrictsPlainAndOAuthMCPTools proves both remote token
// forms reload the membership role before dispatch. A still-valid credential
// must lose administrative tools immediately when its owner becomes billing.
func TestRoleDemotionRestrictsPlainAndOAuthMCPTools(t *testing.T) {
	h := newHTTPFixture(t)
	clientID, code, verifier := h.authorize(t)
	token := postForm(t, h.Server.URL+PathToken, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})

	if _, err := h.Control.Exec(`
		UPDATE team_memberships SET role = 'billing' WHERE team_id = ? AND user_id = 1
	`, teamID); err != nil {
		t.Fatal(err)
	}

	credentials := map[string]string{"plain": h.Raw, "oauth": token["access_token"].(string)}
	for name, credential := range credentials {
		status, body := h.rpc(t, credential,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_site","arguments":{"domain":"denied.example.com"}}}`)
		if status != http.StatusOK {
			t.Fatalf("%s status = %d, want a tool refusal (%+v)", name, status, body)
		}
		if !strings.Contains(marshal(t, body), "does not permit manage_sites") {
			t.Fatalf("%s mutation was not refused by role: %+v", name, body)
		}

		status, body = h.rpc(t, credential,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_sites","arguments":{}}}`)
		if status != http.StatusOK || !strings.Contains(marshal(t, body), "example.com") {
			t.Fatalf("%s lost permitted dashboard reads: status %d (%+v)", name, status, body)
		}
	}

	var created int
	if err := h.Control.QueryRow(`SELECT COUNT(*) FROM sites WHERE domain = 'denied.example.com'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatal("a demoted MCP credential created a site")
	}
}

// TestHTTPRefusesTheServerStream checks the honest answer for a transport
// feature this server does not offer. It never speaks first — no sampling, no
// progress, no subscriptions — so holding a stream open would be a connection
// per client that carries nothing.
func TestHTTPRefusesTheServerStream(t *testing.T) {
	h := newHTTPFixture(t)

	request, err := http.NewRequest(http.MethodGet, h.Server.URL+Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+h.Raw)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)

	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.StatusCode)
	}

	if response.Header.Get("Allow") == "" {
		t.Error("a 405 must say what is allowed instead")
	}
}

// TestStdioRoundTrip checks the local transport a desktop assistant launches.
func TestStdioRoundTrip(t *testing.T) {
	f := newFixture(t)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + ProtocolVersion + `","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"query_stats","arguments":{"site_id":"example.com","metrics":["visitors"],"date_range":"7d"}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer

	if err := ServeStdio(context.Background(), f.Server, StdioOptions{
		In: strings.NewReader(input), Out: &out, Key: f.Key,
	}); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")

	// Two responses, not three: the notification in the middle gets no answer,
	// and a client's dispatcher cannot match an extra response to anything it
	// sent.
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want one per request that carried an id:\n%s", len(lines), out.String())
	}

	var answer struct {
		ID     int `json:"id"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}

	if err := json.Unmarshal([]byte(lines[1]), &answer); err != nil {
		t.Fatal(err)
	}

	if answer.ID != 2 {
		t.Errorf("the second response has id %d", answer.ID)
	}

	if !strings.Contains(answer.Result.Content[0].Text, "visitors") {
		t.Errorf("query_stats over stdio returned %q", answer.Result.Content[0].Text)
	}
}

// TestStdioNeedsAKey checks that a session cannot start unauthenticated. The
// pipe has exactly one peer, so a session with no credential is a session with
// no tenant.
func TestStdioNeedsAKey(t *testing.T) {
	f := newFixture(t)

	var out bytes.Buffer

	if err := ServeStdio(context.Background(), f.Server, StdioOptions{
		In: strings.NewReader(""), Out: &out,
	}); err == nil {
		t.Fatal("a stdio session started with no key")
	}
}

// TestStdioRevalidatesMembershipBeforeMessages checks the long-lived local
// transport cannot keep using a key that was resolved before its owner left.
func TestStdioRevalidatesMembershipBeforeMessages(t *testing.T) {
	f := newFixture(t)

	if _, err := f.Control.Exec(`DELETE FROM team_memberships WHERE team_id = ? AND user_id = 1`, teamID); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := ServeStdio(context.Background(), f.Server, StdioOptions{
		In:       strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n"),
		Out:      &out,
		Key:      f.Key,
		Validate: f.API.Keys.Validate,
	})
	if err == nil || !strings.Contains(err.Error(), "no longer valid") {
		t.Fatalf("stdio session continued after leaving: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdio wrote a response after revocation: %s", out.String())
	}
}

// TestOAuthFlowEndToEnd walks what a remote client does on its own: discover the
// metadata, register itself, complete the authorisation with a key, exchange the
// code, and use the token.
//
// It is one test rather than five because the steps only mean anything in
// sequence — a token endpoint that works against a hand-written code proves
// nothing about whether a real client could ever get one.
func TestOAuthFlowEndToEnd(t *testing.T) {
	h := newHTTPFixture(t)

	// 1. Discovery. The protected-resource document names the authorisation
	// server, and the authorisation server's document names its endpoints.
	metadata := getJSON(t, h.Server.URL+PathProtectedResourceMetadata)

	servers, _ := metadata["authorization_servers"].([]any)
	if len(servers) != 1 || servers[0] != h.Server.URL {
		t.Fatalf("authorization_servers = %v", metadata["authorization_servers"])
	}

	// The path-suffixed spelling is what a client derives from a resource URL of
	// /mcp, and clients differ on which they try.
	if suffixed := getJSON(t, h.Server.URL+PathProtectedResourceMetadata+Path); suffixed["resource"] == nil {
		t.Error("the path-suffixed metadata document is not served")
	}

	server := getJSON(t, h.Server.URL+PathAuthorizationServerMetadata)

	if server["registration_endpoint"] != h.Server.URL+PathRegister {
		t.Fatalf("registration_endpoint = %v", server["registration_endpoint"])
	}

	// OAuth 2.1 drops the "plain" challenge method, and offering it would let a
	// client opt out of the one protection that makes an intercepted code
	// useless.
	methods, _ := server["code_challenge_methods_supported"].([]any)
	if len(methods) != 1 || methods[0] != "S256" {
		t.Fatalf("code_challenge_methods_supported = %v", server["code_challenge_methods_supported"])
	}

	// 2. Dynamic registration. There is nobody to fill in a developer portal, so
	// the client registers itself.
	registration := postJSON(t, h.Server.URL+PathRegister, map[string]any{
		"client_name":   "Test Assistant",
		"redirect_uris": []string{"http://127.0.0.1:33418/callback"},
	})

	clientID, _ := registration["client_id"].(string)
	if clientID == "" {
		t.Fatalf("registration returned no client_id: %+v", registration)
	}

	// A public client is not handed a secret it cannot keep.
	if _, has := registration["client_secret"]; has {
		t.Error("a public client was issued a secret")
	}

	// 3. Authorisation, with PKCE.
	verifier := "a-verifier-long-enough-to-be-a-real-one-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	form := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {"http://127.0.0.1:33418/callback"},
		"state":                 {"xyz"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"api_key":               {h.Raw},
	}

	// The redirect is not followed: the callback is a loopback port nothing is
	// listening on, and the code is in the Location header anyway.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	response, err := client.PostForm(h.Server.URL+PathAuthorize, form)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)

	if response.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("authorize status = %d (%s)", response.StatusCode, body)
	}

	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in %s", location)
	}

	// The state is echoed back, which is how a client knows the callback belongs
	// to the request it started.
	if location.Query().Get("state") != "xyz" {
		t.Errorf("state = %q", location.Query().Get("state"))
	}

	// 4. Exchange.
	token := postForm(t, h.Server.URL+PathToken, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {"http://127.0.0.1:33418/callback"},
		"code_verifier": {verifier},
	})

	access, _ := token["access_token"].(string)
	refresh, _ := token["refresh_token"].(string)

	if access == "" || refresh == "" {
		t.Fatalf("token response = %+v", token)
	}

	// 5. Use it. This is the only step that proves the whole chain.
	status, body := h.rpc(t, access,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sites","arguments":{}}}`)

	if status != http.StatusOK {
		t.Fatalf("status = %d with an OAuth token (%+v)", status, body)
	}

	if !strings.Contains(marshal(t, body), "example.com") {
		t.Errorf("the token did not reach the right tenant: %+v", body)
	}

	// 6. Refreshing rotates. A stolen refresh token stops working the moment the
	// real client uses its own, which is the only way the theft becomes visible.
	refreshed := postForm(t, h.Server.URL+PathToken, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
	})

	if refreshed["access_token"] == access {
		t.Error("refreshing returned the same access token")
	}

	reused := postForm(t, h.Server.URL+PathToken, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
	})

	if reused["error"] != "invalid_grant" {
		t.Errorf("a used refresh token was accepted again: %+v", reused)
	}
	if status, _ := h.rpc(t, refreshed["access_token"].(string), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); status != http.StatusUnauthorized {
		t.Fatalf("descendant access survived refresh-token replay with status %d", status)
	}
}

// TestAuthorizationCodeIsSingleUse checks the replay defence. A second use means
// either a bug or an intercepted code, and in the second case the attacker and
// the real client both hold it — so the whole grant is revoked rather than the
// one request refused.
func TestAuthorizationCodeIsSingleUse(t *testing.T) {
	h := newHTTPFixture(t)

	clientID, code, verifier := h.authorize(t)

	first := postForm(t, h.Server.URL+PathToken, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})

	access, _ := first["access_token"].(string)
	if access == "" {
		t.Fatalf("the first exchange failed: %+v", first)
	}

	second := postForm(t, h.Server.URL+PathToken, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})

	if second["error"] != "invalid_grant" {
		t.Fatalf("the code was accepted twice: %+v", second)
	}

	// The replay revoked everything that grant had issued, so the token from the
	// first exchange no longer works either.
	if status, _ := h.rpc(t, access, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); status != http.StatusUnauthorized {
		t.Fatalf("status = %d after a replayed code, want the grant revoked", status)
	}
}

// TestPKCEIsEnforced checks that a wrong verifier cannot redeem a code. Without
// it, an authorisation code intercepted on a desktop client's loopback redirect
// is enough to take over the connection.
func TestPKCEIsEnforced(t *testing.T) {
	h := newHTTPFixture(t)

	clientID, code, _ := h.authorize(t)

	answer := postForm(t, h.Server.URL+PathToken, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"code_verifier": {"a-different-verifier-entirely-0123456789012345"},
	})

	if answer["error"] != "invalid_grant" {
		t.Fatalf("a wrong verifier was accepted: %+v", answer)
	}
}

// TestAuthorizationRefusesAnUnregisteredRedirect checks the open-redirect guard.
// An authorisation server that redirects to whatever it is handed can be used to
// deliver somebody else's code anywhere an attacker likes.
func TestAuthorizationRefusesAnUnregisteredRedirect(t *testing.T) {
	h := newHTTPFixture(t)

	registration := postJSON(t, h.Server.URL+PathRegister, map[string]any{
		"client_name":   "Test",
		"redirect_uris": []string{"http://127.0.0.1:33418/callback"},
	})

	clientID := registration["client_id"].(string)

	response, err := http.Get(h.Server.URL + PathAuthorize + "?" + url.Values{
		"response_type":  {"code"},
		"client_id":      {clientID},
		"redirect_uri":   {"https://attacker.example/steal"},
		"code_challenge": {"abc"},
	}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)

	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
}

// TestRegistrationRefusesUnsafeRedirects checks the scheme rule. A code
// delivered over plain http to a remote host is a code on the wire in the clear.
func TestRegistrationRefusesUnsafeRedirects(t *testing.T) {
	h := newHTTPFixture(t)

	cases := map[string]bool{
		"https://app.example/callback": true,
		"http://127.0.0.1:9000/cb":     true,
		"http://localhost:9000/cb":     true,
		"com.example.app:/oauth":       true,
		"http://attacker.example/cb":   false,
		"javascript:alert(1)":          false,
		"https://app.example/cb#frag":  false,
	}

	for redirect, allowed := range cases {
		answer := postJSON(t, h.Server.URL+PathRegister, map[string]any{
			"client_name":   "Test",
			"redirect_uris": []string{redirect},
		})

		_, refused := answer["error"]

		if allowed && refused {
			t.Errorf("%q was refused: %+v", redirect, answer)
		}

		if !allowed && !refused {
			t.Errorf("%q was registered", redirect)
		}
	}
}

// TestRevokingTheKeyEndsTheConnection checks that an OAuth token stands for the
// key it was authorised with. Without this, revoking a key would leave every
// assistant that had ever used it still connected.
func TestRevokingTheKeyEndsTheConnection(t *testing.T) {
	h := newHTTPFixture(t)

	clientID, code, verifier := h.authorize(t)

	token := postForm(t, h.Server.URL+PathToken, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})

	access := token["access_token"].(string)

	if status, _ := h.rpc(t, access, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); status != http.StatusOK {
		t.Fatalf("the token did not work before revocation: %d", status)
	}

	if err := h.API.Keys.Revoke(context.Background(), teamID, h.Key.ID); err != nil {
		t.Fatal(err)
	}

	if status, _ := h.rpc(t, access, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); status != http.StatusUnauthorized {
		t.Fatalf("status = %d after the key was revoked, want 401", status)
	}
}

// TestOAuthGrantScopesSurviveAuthentication proves the token keeps the narrow
// consent grant instead of silently becoming the full API key underneath it.
func TestOAuthGrantScopesSurviveAuthentication(t *testing.T) {
	h := newHTTPFixture(t)
	registration := postJSON(t, h.Server.URL+PathRegister, map[string]any{
		"client_name": "Read only", "redirect_uris": []string{"http://127.0.0.1:33418/callback"},
		"scope": apikeys.ScopeStatsRead,
	})
	clientID := registration["client_id"].(string)
	code, verifier := h.authorizeRegistered(t, clientID, apikeys.ScopeStatsRead)
	token := postForm(t, h.Server.URL+PathToken, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {clientID},
		"code": {code}, "code_verifier": {verifier},
	})
	access := token["access_token"].(string)
	key, err := h.OAuth.Authenticate(context.Background(), access)
	if err != nil {
		t.Fatal(err)
	}
	if !key.Allows(apikeys.ScopeStatsRead) || key.Allows(apikeys.ScopeSitesProvision) || key.Allows(apikeys.ScopeSitesRead) {
		t.Fatalf("authenticated OAuth scopes = %v over key scopes %v", key.GrantedScopes, key.Scopes)
	}

	_, body := h.rpc(t, access,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_site","arguments":{}}}`)
	if !strings.Contains(marshal(t, body), apikeys.ScopeSitesProvision) {
		t.Fatalf("read-only OAuth token was not refused before mutation: %+v", body)
	}
}

// TestOAuthScopeCannotExceedClientOrAPIKey proves both upper bounds are
// enforced before a code can carry a broader grant.
func TestOAuthScopeCannotExceedClientOrAPIKey(t *testing.T) {
	h := newHTTPFixture(t)
	registration := postJSON(t, h.Server.URL+PathRegister, map[string]any{
		"client_name": "Read only", "redirect_uris": []string{"http://127.0.0.1:33418/callback"},
		"scope": apikeys.ScopeStatsRead,
	})
	clientID := registration["client_id"].(string)
	_, err := h.OAuth.parseAuthorize(context.Background(), url.Values{
		"response_type": {"code"}, "client_id": {clientID},
		"redirect_uri":   {"http://127.0.0.1:33418/callback"},
		"code_challenge": {"challenge"}, "scope": {apikeys.ScopeSitesProvision},
	})
	if err == nil || !strings.Contains(err.Error(), "registered scope") {
		t.Fatalf("client scope widening error = %v", err)
	}

	key := &apikeys.Key{Scopes: []string{apikeys.ScopeStatsRead}}
	if _, err := grantedScopes(key, apikeys.ScopeSitesProvision); err == nil {
		t.Fatal("OAuth grant widened its underlying API key")
	}
}

// TestConfidentialClientsMustAuthenticate checks missing and wrong secrets,
// successful client_secret_post, and refusal of an auth method we do not serve.
func TestConfidentialClientsMustAuthenticate(t *testing.T) {
	h := newHTTPFixture(t)
	unsupported := postJSON(t, h.Server.URL+PathRegister, map[string]any{
		"client_name": "Unsupported", "redirect_uris": []string{"https://app.example/cb"},
		"token_endpoint_auth_method": "client_secret_basic",
	})
	if unsupported["error"] != "invalid_client_metadata" {
		t.Fatalf("unsupported auth method registered: %+v", unsupported)
	}

	registration := postJSON(t, h.Server.URL+PathRegister, map[string]any{
		"client_name": "Confidential", "redirect_uris": []string{"http://127.0.0.1:33418/callback"},
		"token_endpoint_auth_method": "client_secret_post",
	})
	clientID := registration["client_id"].(string)
	secret := registration["client_secret"].(string)
	code, verifier := h.authorizeRegistered(t, clientID, apikeys.ScopeStatsRead)

	base := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {verifier}}
	for _, presented := range []string{"", "wrong"} {
		attempt := cloneValues(base)
		if presented != "" {
			attempt.Set("client_secret", presented)
		}
		if answer := postForm(t, h.Server.URL+PathToken, attempt); answer["error"] != "invalid_client" {
			t.Fatalf("secret %q answer = %+v", presented, answer)
		}
	}
	base.Set("client_secret", secret)
	if answer := postForm(t, h.Server.URL+PathToken, base); answer["access_token"] == nil {
		t.Fatalf("correct confidential secret failed: %+v", answer)
	}
}

// TestOAuthUsesDistinctExpiryBoundaries checks equality at each deadline and
// proves refresh validity is not inherited from the one-hour access deadline.
func TestOAuthUsesDistinctExpiryBoundaries(t *testing.T) {
	t.Run("authorization code", func(t *testing.T) {
		h := newHTTPFixture(t)
		clientID, code, verifier := h.authorize(t)
		if _, err := h.Control.Exec(`UPDATE mcp_oauth_codes SET expires_at = ? WHERE code_hash = ?`,
			testNow.Unix(), hashToken(code)); err != nil {
			t.Fatal(err)
		}
		answer := postForm(t, h.Server.URL+PathToken, url.Values{
			"grant_type": {"authorization_code"}, "client_id": {clientID},
			"code": {code}, "code_verifier": {verifier},
		})
		if answer["error"] != "invalid_grant" {
			t.Fatalf("code at expiry was accepted: %+v", answer)
		}
	})

	t.Run("access and refresh", func(t *testing.T) {
		h := newHTTPFixture(t)
		clientID, code, verifier := h.authorize(t)
		pair := postForm(t, h.Server.URL+PathToken, url.Values{
			"grant_type": {"authorization_code"}, "client_id": {clientID},
			"code": {code}, "code_verifier": {verifier},
		})
		access := pair["access_token"].(string)
		refresh := pair["refresh_token"].(string)
		if _, err := h.Control.Exec(`UPDATE mcp_oauth_tokens SET expires_at = ? WHERE token_hash = ?`,
			testNow.Unix(), hashToken(access)); err != nil {
			t.Fatal(err)
		}
		if _, err := h.OAuth.Authenticate(context.Background(), access); err == nil {
			t.Fatal("access token was valid at its expiry boundary")
		}
		refreshed := postForm(t, h.Server.URL+PathToken, url.Values{
			"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refresh},
		})
		if refreshed["access_token"] == nil {
			t.Fatalf("refresh token inherited access expiry: %+v", refreshed)
		}

		newRefresh := refreshed["refresh_token"].(string)
		if _, err := h.Control.Exec(`UPDATE mcp_oauth_tokens SET refresh_expires_at = ? WHERE refresh_token_hash = ?`,
			testNow.Unix(), hashToken(newRefresh)); err != nil {
			t.Fatal(err)
		}
		expired := postForm(t, h.Server.URL+PathToken, url.Values{
			"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {newRefresh},
		})
		if expired["error"] != "invalid_grant" {
			t.Fatalf("refresh token was valid at expiry: %+v", expired)
		}
	})
}

// TestConcurrentOAuthOneTimeTokensHaveOneWinner races separate SQLite handles
// for both code redemption and refresh rotation.
func TestConcurrentOAuthOneTimeTokensHaveOneWinner(t *testing.T) {
	t.Run("authorization code", func(t *testing.T) {
		h := newHTTPFixture(t)
		clientID, code, verifier := h.authorize(t)
		other := secondOAuthServer(t, h)
		values := url.Values{
			"grant_type": {"authorization_code"}, "client_id": {clientID},
			"code": {code}, "code_verifier": {verifier},
		}
		winner := assertOneOAuthWinner(t, []string{h.Server.URL + PathToken, other.URL + PathToken}, values)
		if _, err := h.OAuth.Authenticate(context.Background(), winner); err == nil {
			t.Fatal("the code-race winner survived the concurrent replay")
		}
	})

	t.Run("refresh token", func(t *testing.T) {
		h := newHTTPFixture(t)
		clientID, code, verifier := h.authorize(t)
		pair := postForm(t, h.Server.URL+PathToken, url.Values{
			"grant_type": {"authorization_code"}, "client_id": {clientID},
			"code": {code}, "code_verifier": {verifier},
		})
		other := secondOAuthServer(t, h)
		values := url.Values{
			"grant_type": {"refresh_token"}, "client_id": {clientID},
			"refresh_token": {pair["refresh_token"].(string)},
		}
		winner := assertOneOAuthWinner(t, []string{h.Server.URL + PathToken, other.URL + PathToken}, values)
		if _, err := h.OAuth.Authenticate(context.Background(), winner); err == nil {
			t.Fatal("the refresh-race winner survived family replay")
		}
	})
}

// TestRefreshReplayRevokesDescendantsButNotAnotherFamily checks both possible
// request orders over independent servers. Whichever holder rotates first gets
// a child briefly; the second holder's replay revokes that child and every later
// descendant without touching an unrelated client grant for the same team.
func TestRefreshReplayRevokesDescendantsButNotAnotherFamily(t *testing.T) {
	for _, order := range []string{"attacker first", "legitimate client first"} {
		t.Run(order, func(t *testing.T) {
			h := newHTTPFixture(t)
			other := secondOAuthServer(t, h)
			clientID, code, verifier := h.authorize(t)
			original := postForm(t, h.Server.URL+PathToken, url.Values{
				"grant_type": {"authorization_code"}, "client_id": {clientID},
				"code": {code}, "code_verifier": {verifier},
			})

			isolatedClient, isolatedCode, isolatedVerifier := h.authorize(t)
			isolated := postForm(t, h.Server.URL+PathToken, url.Values{
				"grant_type": {"authorization_code"}, "client_id": {isolatedClient},
				"code": {isolatedCode}, "code_verifier": {isolatedVerifier},
			})

			firstURL, replayURL := h.Server.URL+PathToken, other.URL+PathToken
			if order == "legitimate client first" {
				firstURL, replayURL = replayURL, firstURL
			}
			rotated := postForm(t, firstURL, url.Values{
				"grant_type": {"refresh_token"}, "client_id": {clientID},
				"refresh_token": {original["refresh_token"].(string)},
			})
			if rotated["access_token"] == nil {
				t.Fatalf("first rotation failed: %+v", rotated)
			}
			replay := postForm(t, replayURL, url.Values{
				"grant_type": {"refresh_token"}, "client_id": {clientID},
				"refresh_token": {original["refresh_token"].(string)},
			})
			if replay["error"] != "invalid_grant" {
				t.Fatalf("replay response = %+v", replay)
			}
			if _, err := h.OAuth.Authenticate(context.Background(), rotated["access_token"].(string)); err == nil {
				t.Fatal("rotated descendant access token survived replay")
			}
			childReplay := postForm(t, h.Server.URL+PathToken, url.Values{
				"grant_type": {"refresh_token"}, "client_id": {clientID},
				"refresh_token": {rotated["refresh_token"].(string)},
			})
			if childReplay["error"] != "invalid_grant" {
				t.Fatalf("descendant refresh survived family revocation: %+v", childReplay)
			}
			if _, err := h.OAuth.Authenticate(context.Background(), isolated["access_token"].(string)); err != nil {
				t.Fatalf("unrelated token family was revoked: %v", err)
			}
		})
	}
}

// cloneValues makes independent form maps for mutation in table tests.
func cloneValues(values url.Values) url.Values {
	copy := url.Values{}
	for key, entries := range values {
		copy[key] = append([]string{}, entries...)
	}

	return copy
}

// secondOAuthServer mounts a token endpoint over a separate connection to the
// fixture's database file.
func secondOAuthServer(t *testing.T, h *httpFixture) *httptest.Server {
	t.Helper()
	var sequence int
	var name, path string
	if err := h.Control.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	oauth := &OAuth{DB: db, Keys: apikeys.NewStore(db), Now: func() time.Time { return testNow }}
	mux := http.NewServeMux()
	oauth.Routes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	oauth.BaseURL = server.URL

	return server
}

// oauthResult is one raw concurrent token response.
type oauthResult struct {
	status int
	body   map[string]any
	err    error
}

// assertOneOAuthWinner starts both requests together, checks exactly one
// committed a replacement token pair, and returns that pair's access token so
// the caller can verify replay revocation after both requests finish.
func assertOneOAuthWinner(t *testing.T, targets []string, values url.Values) string {
	t.Helper()
	start := make(chan struct{})
	results := make(chan oauthResult, len(targets))
	var wait sync.WaitGroup
	for _, target := range targets {
		wait.Add(1)
		go func(target string) {
			defer wait.Done()
			<-start
			response, err := http.PostForm(target, cloneValues(values))
			if err != nil {
				results <- oauthResult{err: err}
				return
			}
			defer func() { _ = response.Body.Close() }()
			body := map[string]any{}
			err = json.NewDecoder(response.Body).Decode(&body)
			results <- oauthResult{status: response.StatusCode, body: body, err: err}
		}(target)
	}
	close(start)
	wait.Wait()
	close(results)

	winners, losers := 0, 0
	winnerAccess := ""
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.body["access_token"] != nil {
			winners++
			winnerAccess = result.body["access_token"].(string)
		} else if result.body["error"] == "invalid_grant" {
			losers++
		} else {
			t.Fatalf("concurrent token response status/body = %d/%+v", result.status, result.body)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("concurrent OAuth winners/losers = %d/%d, want 1/1", winners, losers)
	}

	return winnerAccess
}

// authorize runs registration and the consent step, returning what the token
// endpoint needs.
func (h *httpFixture) authorize(t *testing.T) (clientID, code, verifier string) {
	t.Helper()

	registration := postJSON(t, h.Server.URL+PathRegister, map[string]any{
		"client_name":   "Test Assistant",
		"redirect_uris": []string{"http://127.0.0.1:33418/callback"},
	})

	clientID = registration["client_id"].(string)
	code, verifier = h.authorizeRegistered(t, clientID, "")

	return clientID, code, verifier
}

// authorizeRegistered completes consent for an existing client with an
// optional narrow grant.
func (h *httpFixture) authorizeRegistered(t *testing.T, clientID, scope string) (code, verifier string) {
	t.Helper()
	verifier = "a-verifier-long-enough-to-be-a-real-one-0123456789"

	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	form := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {"http://127.0.0.1:33418/callback"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"api_key":               {h.Raw},
	}
	if scope != "" {
		form.Set("scope", scope)
	}

	response, err := client.PostForm(h.Server.URL+PathAuthorize, form)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)

	if response.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("authorize status = %d (%s)", response.StatusCode, body)
	}

	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	return location.Query().Get("code"), verifier
}

// TestConsentRefusesABadKey checks that a mistyped key comes back to the form
// rather than redirecting to the client with an error, which would make somebody
// restart the whole flow over a typo.
func TestConsentRefusesABadKey(t *testing.T) {
	h := newHTTPFixture(t)

	registration := postJSON(t, h.Server.URL+PathRegister, map[string]any{
		"client_name":   "Test",
		"redirect_uris": []string{"http://127.0.0.1:33418/callback"},
	})

	verifier := "a-verifier-long-enough-to-be-a-real-one-0123456789"
	sum := sha256.Sum256([]byte(verifier))

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	response, err := client.PostForm(h.Server.URL+PathAuthorize, url.Values{
		"response_type":         {"code"},
		"client_id":             {registration["client_id"].(string)},
		"redirect_uri":          {"http://127.0.0.1:33418/callback"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"api_key":               {"feas_not-a-real-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the form back", response.StatusCode)
	}

	body, _ := io.ReadAll(response.Body)

	if !strings.Contains(string(body), "not valid") {
		t.Errorf("the form does not say what went wrong: %s", body)
	}
}

// getJSON reads a JSON document.
func getJSON(t *testing.T, target string) map[string]any {
	t.Helper()

	response, err := http.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)

	decoded := map[string]any{}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("%s: %v", target, err)
	}

	return decoded
}

// postJSON posts a JSON body and reads the JSON answer.
func postJSON(t *testing.T, target string, body map[string]any) map[string]any {
	t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	response, err := http.Post(target, "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)

	decoded := map[string]any{}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("%s: %v", target, err)
	}

	return decoded
}

// postForm posts a form and reads the JSON answer.
func postForm(t *testing.T, target string, form url.Values) map[string]any {
	t.Helper()

	response, err := http.PostForm(target, form)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResponse(t, response)

	decoded := map[string]any{}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("%s: %v", target, err)
	}

	return decoded
}

// marshal renders a value for a substring assertion.
func marshal(t *testing.T, value any) string {
	t.Helper()

	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}
