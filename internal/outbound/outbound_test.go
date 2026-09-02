//
// outbound_test.go
// The destinations a customer-supplied URL may and may not reach.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package outbound

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
)

// newReceiver is a loopback server that records whether it was reached, which
// is the only fact most of these tests need.
func newReceiver(t *testing.T) (*httptest.Server, *bool) {
	t.Helper()

	reached := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true

		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/target", http.StatusFound)
		case "/target":
			_, _ = io.WriteString(w, "followed")
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))
	t.Cleanup(server.Close)

	return server, &reached
}

// TestLoopbackIsAllowedOnlyWhenThePolicySaysSo covers both halves of the one
// setting that differs between a laptop and hosted production.
func TestLoopbackIsAllowedOnlyWhenThePolicySaysSo(t *testing.T) {
	server, reached := newReceiver(t)

	open := Policy{AllowLoopback: true}
	if _, err := open.ValidateURL(context.Background(), server.URL+"/hook"); err != nil {
		t.Fatalf("a loopback URL was refused with loopback allowed: %v", err)
	}

	response, err := open.NewClient(5 * time.Second).Get(server.URL + "/hook")
	if err != nil {
		t.Fatalf("a loopback request failed with loopback allowed: %v", err)
	}
	_ = response.Body.Close()

	if !*reached {
		t.Fatal("the loopback receiver was never reached")
	}

	*reached = false
	closed := Policy{}

	if _, err := closed.ValidateURL(context.Background(), server.URL+"/hook"); err == nil {
		t.Fatal("a loopback URL was accepted with loopback refused")
	}

	if _, err := closed.NewClient(5 * time.Second).Get(server.URL + "/hook"); err == nil {
		t.Fatal("a loopback request connected with loopback refused")
	}

	if *reached {
		t.Fatal("the receiver was reached even though the policy refuses loopback")
	}
}

// TestLocalhostIsRefusedAtConnectTime is the rebinding case: the name is
// checked by the dialer, not just by the form, so a name that validated as
// something else cannot be used to reach loopback later.
func TestLocalhostIsRefusedAtConnectTime(t *testing.T) {
	server, reached := newReceiver(t)

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	client := Policy{}.NewClient(5 * time.Second)

	if _, err := client.Get("http://localhost:" + parsed.Port() + "/hook"); err == nil {
		t.Fatal("localhost connected with loopback refused")
	}

	if *reached {
		t.Fatal("the receiver was reached through localhost")
	}
}

// TestPrivateAndLocalLiteralsAreRefused enumerates the addresses a customer
// URL must never send us to, whatever else the policy allows.
func TestPrivateAndLocalLiteralsAreRefused(t *testing.T) {
	policy := Policy{AllowLoopback: true}

	for _, raw := range []string{
		"https://10.0.0.5/",
		"http://169.254.169.254/latest/meta-data/",
		"https://192.168.1.1/",
		"https://172.16.0.9/",
		"https://[fd00::1]/",
		"https://[fe80::1]/",
		"https://0.0.0.0/",
		"https://[::ffff:10.0.0.5]/",
		"https://224.0.0.1/",
	} {
		if _, err := policy.ValidateURL(context.Background(), raw); err == nil {
			t.Errorf("%s was accepted", raw)
		}

		if _, err := policy.NewClient(time.Second).Get(raw); err == nil {
			t.Errorf("%s connected", raw)
		} else if !strings.Contains(err.Error(), "private or local") {
			t.Errorf("%s failed for another reason: %v", raw, err)
		}
	}
}

// TestValidationErrorsAreSafeToShow checks the shapes a form can produce, and
// that none of the messages leak anything about the network.
func TestValidationErrorsAreSafeToShow(t *testing.T) {
	policy := Policy{AllowLoopback: true}

	for _, raw := range []string{
		"",
		"   ",
		"not a url",
		"example.com/hook",
		"ftp://127.0.0.1/",
		"http://user:pass@127.0.0.1/",
		"http:///path",
	} {
		_, err := policy.ValidateURL(context.Background(), raw)
		if err == nil {
			t.Errorf("%q was accepted", raw)
			continue
		}

		var refusal Error
		if !errors.As(err, &refusal) {
			t.Errorf("%q produced an error that is not a customer-facing refusal: %v", raw, err)
		}

		if strings.Contains(err.Error(), "outbound") || strings.Contains(err.Error(), "lookup") {
			t.Errorf("%q produced an internal error message: %v", raw, err)
		}
	}
}

// TestTheHostAllowListIsExact is the Slack case: only the one host, and no
// prefix or suffix trick around it.
func TestTheHostAllowListIsExact(t *testing.T) {
	policy := Policy{AllowLoopback: true, AllowedHosts: []string{"127.0.0.1"}}

	if _, err := policy.ValidateURL(context.Background(), "https://127.0.0.1/services/T0/B0/x"); err != nil {
		t.Fatalf("the allowed host was refused: %v", err)
	}

	for _, raw := range []string{
		"https://hooks.slack.com.evil.example/",
		"https://evil.example/hooks.slack.com",
		"https://[::1]/",
	} {
		if _, err := policy.ValidateURL(context.Background(), raw); err == nil {
			t.Errorf("%s was accepted against an allow-list of 127.0.0.1", raw)
		}
	}

	slack := Policy{AllowedHosts: []string{"hooks.slack.com"}}

	_, err := slack.ValidateURL(context.Background(), "https://example.com/hook")
	if err == nil || !strings.Contains(err.Error(), "hooks.slack.com") {
		t.Fatalf("the refusal does not name the host that is allowed: %v", err)
	}
}

// TestRequireHTTPSRefusesPlainHTTPExceptLoopback covers hosted production,
// where a webhook over plain http puts an account's events on the wire in
// clear text.
func TestRequireHTTPSRefusesPlainHTTPExceptLoopback(t *testing.T) {
	strict := Policy{RequireHTTPS: true}

	if _, err := strict.ValidateURL(context.Background(), "http://203.0.113.5/hook"); err == nil {
		t.Fatal("plain http to a public address was accepted under RequireHTTPS")
	}

	lenient := Policy{RequireHTTPS: true, AllowLoopback: true}

	if _, err := lenient.ValidateURL(context.Background(), "http://127.0.0.1:9000/hook"); err != nil {
		t.Fatalf("plain http to loopback was refused with loopback allowed: %v", err)
	}
}

// TestRedirectsAreNotFollowed checks that the response handed back is the 3xx
// itself. A redirect is a second destination nobody checked.
func TestRedirectsAreNotFollowed(t *testing.T) {
	server, _ := newReceiver(t)

	response, err := Policy{AllowLoopback: true}.NewClient(5 * time.Second).Get(server.URL + "/redirect")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want the 302 itself", response.StatusCode)
	}

	body, _ := io.ReadAll(response.Body)
	if strings.Contains(string(body), "followed") {
		t.Fatal("the redirect was followed")
	}
}

// TestPolicyForFollowsTheEnvironment checks the two switches land where the
// deployment shapes need them.
func TestPolicyForFollowsTheEnvironment(t *testing.T) {
	development := &config.Config{}
	development.Shared.Env = config.EnvDevelopment

	if p := PolicyFor(development); !p.AllowLoopback || p.RequireHTTPS {
		t.Fatalf("development policy = %+v, want loopback allowed and http tolerated", p)
	}

	selfHosted := &config.Config{}
	selfHosted.Shared.Env = config.EnvProduction

	if p := PolicyFor(selfHosted); !p.AllowLoopback || !p.RequireHTTPS {
		t.Fatalf("self-hosted production policy = %+v, want loopback allowed and https required", p)
	}

	hosted := &config.Config{}
	hosted.Shared.Env = config.EnvProduction
	hosted.App.Hosted = true

	if p := PolicyFor(hosted); p.AllowLoopback || !p.RequireHTTPS {
		t.Fatalf("hosted production policy = %+v, want loopback refused and https required", p)
	}
}
