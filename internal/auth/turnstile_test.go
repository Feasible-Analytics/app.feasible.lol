//
// turnstile_test.go
// The human check in front of account creation.
//
// Created: 2026-09-08
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// answering builds a Turnstile whose verification endpoint replies with the
// given status and body, and records the form it was sent. It goes through the
// real client so the tests exercise the real encoding.
func answering(t *testing.T, status int, body string, seen *url.Values) *Turnstile {
	t.Helper()

	checker := NewTurnstile("site-key", "secret-key")
	checker.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}

		parsed, err := url.ParseQuery(string(raw))
		if err != nil {
			return nil, err
		}

		if seen != nil {
			*seen = parsed
		}

		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{},
		}, nil
	})}

	return checker
}

func TestConfiguredNeedsBothHalves(t *testing.T) {
	cases := []struct {
		name   string
		value  *Turnstile
		wanted bool
	}{
		{"nil", nil, false},
		{"neither", NewTurnstile("", ""), false},
		{"site key only", NewTurnstile("site", ""), false},
		{"secret key only", NewTurnstile("", "secret"), false},
		{"both", NewTurnstile("site", "secret"), true},
		{"both, padded", NewTurnstile("  site  ", "  secret  "), true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.value.Configured(); got != c.wanted {
				t.Errorf("Configured() = %v, wanted %v", got, c.wanted)
			}
		})
	}
}

// The reason names the variable to set, because "which variable do I set" is
// the only question worth answering at start-up.
func TestDisabledReasonNamesTheMissingVariable(t *testing.T) {
	cases := []struct {
		name     string
		value    *Turnstile
		contains string
	}{
		{"nil", nil, "SITE_KEY and FEASIBLE_TURNSTILE_SECRET_KEY"},
		{"neither", NewTurnstile("", ""), "SITE_KEY and FEASIBLE_TURNSTILE_SECRET_KEY"},
		{"site key missing", NewTurnstile("", "secret"), "FEASIBLE_TURNSTILE_SITE_KEY"},
		{"secret key missing", NewTurnstile("site", ""), "FEASIBLE_TURNSTILE_SECRET_KEY"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if reason := c.value.DisabledReason(); !strings.Contains(reason, c.contains) {
				t.Errorf("DisabledReason() = %q, wanted it to mention %q", reason, c.contains)
			}
		})
	}

	if reason := NewTurnstile("site", "secret").DisabledReason(); reason != "" {
		t.Errorf("a configured checker should have no reason, got %q", reason)
	}
}

// An install with no Cloudflare account must keep working. The check passes
// everything rather than refusing every sign-up.
func TestAnUnconfiguredCheckPassesEverything(t *testing.T) {
	for _, checker := range []*Turnstile{nil, NewTurnstile("", "")} {
		if err := checker.Verify(context.Background(), "", ""); err != nil {
			t.Errorf("an unconfigured check should pass, got %v", err)
		}
	}
}

// A form posted without the widget's token never reaches Cloudflare: there is
// nothing to ask about.
func TestAnEmptyTokenIsRejectedWithoutAskingCloudflare(t *testing.T) {
	asked := false

	checker := NewTurnstile("site", "secret")
	checker.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		asked = true
		return nil, errors.New("should not have been called")
	})}

	for _, token := range []string{"", "   "} {
		if err := checker.Verify(context.Background(), token, ""); !errors.Is(err, ErrTurnstileRejected) {
			t.Errorf("token %q: wanted ErrTurnstileRejected, got %v", token, err)
		}
	}

	if asked {
		t.Error("an empty token should not cost a round trip")
	}
}

func TestAnAcceptedTokenSendsTheSecretAndTheSource(t *testing.T) {
	var sent url.Values

	checker := answering(t, http.StatusOK, `{"success":true}`, &sent)

	if err := checker.Verify(context.Background(), "a-token", "203.0.113.7"); err != nil {
		t.Fatalf("a successful verdict should pass, got %v", err)
	}

	if got := sent.Get("secret"); got != "secret-key" {
		t.Errorf("secret = %q, wanted the configured secret key", got)
	}

	if got := sent.Get("response"); got != "a-token" {
		t.Errorf("response = %q, wanted the submitted token", got)
	}

	if got := sent.Get("remoteip"); got != "203.0.113.7" {
		t.Errorf("remoteip = %q, wanted the resolved source", got)
	}
}

// The source is context for Cloudflare's scoring, not a requirement. A request
// we could not resolve an address for still gets checked.
func TestAnUnknownSourceIsOmittedRatherThanSentEmpty(t *testing.T) {
	var sent url.Values

	checker := answering(t, http.StatusOK, `{"success":true}`, &sent)

	if err := checker.Verify(context.Background(), "a-token", ""); err != nil {
		t.Fatalf("verify: %v", err)
	}

	if _, present := sent["remoteip"]; present {
		t.Error("an unresolved source should be left out of the form")
	}
}

func TestARefusedTokenIsRejected(t *testing.T) {
	checker := answering(t, http.StatusOK, `{"success":false,"error-codes":["invalid-input-response"]}`, nil)

	err := checker.Verify(context.Background(), "a-token", "")
	if !errors.Is(err, ErrTurnstileRejected) {
		t.Fatalf("wanted ErrTurnstileRejected, got %v", err)
	}

	// The code Cloudflare returned rides along, because "rejected" alone does
	// not tell an operator whether the secret key is wrong.
	if !strings.Contains(err.Error(), "invalid-input-response") {
		t.Errorf("the error should carry Cloudflare's code, got %q", err)
	}
}

// A check that lets everything through whenever Cloudflare is unreachable is a
// check an attacker turns off by making it unreachable. Every one of these is
// an error, and none of them is a rejection the visitor caused.
func TestAnUnreachableCheckFailsClosedAndIsNotAVerdict(t *testing.T) {
	cases := map[string]*Turnstile{
		"a server error":        answering(t, http.StatusInternalServerError, ``, nil),
		"an unparseable answer": answering(t, http.StatusOK, `not json`, nil),
	}

	broken := NewTurnstile("site", "secret")
	broken.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial: no route to host")
	})}

	cases["an unreachable host"] = broken

	for name, checker := range cases {
		t.Run(name, func(t *testing.T) {
			err := checker.Verify(context.Background(), "a-token", "")
			if err == nil {
				t.Fatal("wanted an error so the caller refuses the sign-up")
			}

			if errors.Is(err, ErrTurnstileRejected) {
				t.Error("a fault is not a verdict about the visitor, and must not be reported as one")
			}
		})
	}
}

func TestTheSiteKeyReachesTheTemplateOnlyWhenBothHalvesAreSet(t *testing.T) {
	app := newTestApp(t)

	app.Turnstile = NewTurnstile("site-key-abc", "")
	if got := app.turnstileSiteKey(); got != "" {
		t.Errorf("a half-configured check should draw no widget, got %q", got)
	}

	app.Turnstile = NewTurnstile("site-key-abc", "secret-key")
	if got := app.turnstileSiteKey(); got != "site-key-abc" {
		t.Errorf("turnstileSiteKey() = %q, wanted the configured site key", got)
	}
}

// The widget only appears once the check can actually be run, so a
// half-configured install does not show a control whose answer nobody reads.
func TestTheSignUpFormDrawsTheWidgetWhenConfigured(t *testing.T) {
	app := newTestApp(t)
	c := newClient(t, app)

	if body := c.body("/register"); strings.Contains(body, "cf-turnstile") {
		t.Error("an unconfigured install should render no widget")
	}

	app.Turnstile = NewTurnstile("site-key-abc", "secret-key")

	body := c.body("/register")
	if !strings.Contains(body, "cf-turnstile") || !strings.Contains(body, "site-key-abc") {
		t.Error("a configured install should render the widget and its site key")
	}
}

// The whole point of the feature: a refused token must not create an account,
// and must not send an email to the address the script chose.
func TestARefusedSignUpCreatesNothingAndSendsNothing(t *testing.T) {
	app := newTestApp(t)
	app.Turnstile = answering(t, http.StatusOK, `{"success":false,"error-codes":["invalid-input-response"]}`, nil)

	c := newClient(t, app)

	resp := c.post("/register", url.Values{
		"email":    {"victim@example.com"},
		"password": {"a long enough password"},
		"name":     {"Person"},
	})
	closeResponseBody(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, wanted the form back with an error", resp.StatusCode)
	}

	if _, err := app.store.UserByEmail(context.Background(), "victim@example.com"); err == nil {
		t.Error("a refused sign-up must not create the account")
	}

	if len(app.sent.messages) != 0 {
		t.Errorf("a refused sign-up must send no email, got %d", len(app.sent.messages))
	}
}

// A fault at Cloudflare refuses the sign-up too, for the same reason.
func TestAnUnreachableCheckRefusesTheSignUp(t *testing.T) {
	app := newTestApp(t)

	broken := NewTurnstile("site", "secret")
	broken.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial: no route to host")
	})}

	app.Turnstile = broken

	c := newClient(t, app)

	resp := c.post("/register", url.Values{
		"email":    {"victim@example.com"},
		"password": {"a long enough password"},
	})
	closeResponseBody(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, wanted the sign-up refused", resp.StatusCode)
	}

	if len(app.sent.messages) != 0 {
		t.Error("an unverifiable sign-up must send no email")
	}
}

// An accepted token behaves exactly as it did before the check existed.
func TestAnAcceptedSignUpStillCreatesTheAccount(t *testing.T) {
	app := newTestApp(t)

	var sent url.Values

	app.Turnstile = answering(t, http.StatusOK, `{"success":true}`, &sent)

	c := newClient(t, app)

	resp := c.post("/register", url.Values{
		"email":    {"person@example.com"},
		"password": {"a long enough password"},
		"name":     {"Person"},
		TokenField: {"a-token"},
	})
	closeResponseBody(t, resp)

	if location := resp.Header.Get("Location"); location != "/verify-email" {
		t.Fatalf("registration should go to the verification screen, got %q (status %d)", location, resp.StatusCode)
	}

	if got := sent.Get("response"); got != "a-token" {
		t.Errorf("the widget's token should reach Cloudflare, got %q", got)
	}

	if len(app.sent.messages) != 1 {
		t.Errorf("wanted one verification email, got %d", len(app.sent.messages))
	}
}

// The ceiling is installation-wide, so it holds against somebody who has a
// thousand addresses to come from.
func TestTheInstallationWideCeilingRefusesFurtherSignUps(t *testing.T) {
	app := newTestApp(t)
	c := newClient(t, app)

	for i := 0; i < SignupEmails; i++ {
		if !app.Limiter.Allow(GlobalKey("signup-email"), SignupEmails, SignupEmailWindow) {
			t.Fatalf("the ceiling closed early, at %d of %d", i, SignupEmails)
		}
	}

	resp := c.post("/register", url.Values{
		"email":    {"person@example.com"},
		"password": {"a long enough password"},
	})
	closeResponseBody(t, resp)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, wanted the sign-up refused", resp.StatusCode)
	}

	if len(app.sent.messages) != 0 {
		t.Error("a sign-up over the ceiling must send no email")
	}

	if _, err := app.store.UserByEmail(context.Background(), "person@example.com"); err == nil {
		t.Error("a refusal must not leave somebody holding an account they cannot verify")
	}
}
