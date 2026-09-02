//
// shields_test.go
// Every rule kind blocking what it should, and the cap holding.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package shields

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// newAccount opens a migrated account database in a temporary directory.
func newAccount(t *testing.T) *accounts.Account {
	t.Helper()

	manager := accounts.NewManager(t.TempDir())
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	account, err := manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	return account
}

// TestEachKindBlocks covers the four rule kinds against the two evaluators. IP
// rules are answered in the ingest tier and the rest at the shard, and both
// halves have to work or a customer's rule is a setting that does nothing.
func TestEachKindBlocks(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	now := time.Unix(1_800_000_000, 0)

	for _, rule := range []struct{ kind, value string }{
		{KindIP, "203.0.113.14"},
		{KindIP, "198.51.100.0/24"},
		{KindCountry, "us"},
		{KindPage, "/admin*"},
		{KindPage, "/health"},
		{KindHostname, "example.com"},
	} {
		if _, err := Add(ctx, account.Writer(), 1, rule.kind, rule.value, "", now); err != nil {
			t.Fatalf("adding %s %q: %v", rule.kind, rule.value, err)
		}
	}

	rules, err := List(ctx, account.Reader(), 1)
	if err != nil {
		t.Fatal(err)
	}

	set := Compile(rules)

	// Blocked addresses, as a bare address and as a block.
	for _, blocked := range []string{"203.0.113.14", "198.51.100.7"} {
		if !set.BlocksIP(netip.MustParseAddr(blocked)) {
			t.Errorf("%s should be blocked", blocked)
		}
	}

	if set.BlocksIP(netip.MustParseAddr("203.0.113.15")) {
		t.Error("an address next to a blocked one was blocked too")
	}

	// A v4 address arriving as a v4-mapped v6 is the shape a dual-stack
	// listener produces, and it has to match a v4 rule.
	if !set.BlocksIP(netip.MustParseAddr("::ffff:203.0.113.14")) {
		t.Error("a v4-mapped address did not match its v4 rule")
	}

	// Country, matched case-insensitively because a rule may be typed either way.
	if allowed, reason := set.Allowed("example.com", "/", "US"); allowed || reason != ingest.ReasonShieldCountry {
		t.Errorf("a blocked country was allowed (reason %q)", reason)
	}

	if allowed, _ := set.Allowed("example.com", "/", "CA"); !allowed {
		t.Error("an unblocked country was blocked")
	}

	// Pages, exact and by prefix.
	if allowed, reason := set.Allowed("example.com", "/health", ""); allowed || reason != ingest.ReasonShieldPage {
		t.Errorf("an exactly blocked page was allowed (reason %q)", reason)
	}

	if allowed, _ := set.Allowed("example.com", "/admin/users", ""); allowed {
		t.Error("a page under a blocked prefix was allowed")
	}

	if allowed, _ := set.Allowed("example.com", "/pricing", ""); !allowed {
		t.Error("an unblocked page was blocked")
	}

	// Hostnames are an allow-list: anything not named is dropped, which is the
	// answer to somebody else's site sending events with your tracker id.
	if allowed, reason := set.Allowed("copycat.test", "/", ""); allowed || reason != ingest.ReasonHostnameNotAllowed {
		t.Errorf("an unlisted hostname was allowed (reason %q)", reason)
	}

	if allowed, _ := set.Allowed("www.example.com", "/", ""); !allowed {
		t.Error("the allowed hostname was blocked when it arrived with a www prefix")
	}
}

// TestRegisteredDomainIsTheDefaultHostnameAllowList proves hostname protection
// is active without customer configuration while explicit rules remain
// additive for preview or checkout hosts.
func TestRegisteredDomainIsTheDefaultHostnameAllowList(t *testing.T) {
	set := CompileFor("example.com", []Rule{{Kind: KindHostname, Value: "checkout.example.net"}})

	for _, allowed := range []string{"example.com", "www.example.com", "docs.example.com", "checkout.example.net"} {
		if !set.HostnameAllowed(allowed) {
			t.Errorf("default hostname policy rejected %q", allowed)
		}
	}
	for _, rejected := range []string{"example.net", "badexample.com", "com", "", ingest.NoneHostname} {
		if set.HostnameAllowed(rejected) {
			t.Errorf("default hostname policy allowed %q", rejected)
		}
	}
}

// TestRuleCap holds the thirty-per-kind limit. The cap is counted in code
// because a CHECK constraint cannot count sibling rows, so the only thing
// keeping it true is this path — and the error has to be a sentence the
// customer can act on rather than a constraint violation.
func TestRuleCap(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	now := time.Unix(1_800_000_000, 0)

	for i := 0; i < MaxRulesPerKind; i++ {
		value := fmt.Sprintf("203.0.113.%d", i)

		if _, err := Add(ctx, account.Writer(), 1, KindIP, value, "", now); err != nil {
			t.Fatalf("rule %d of %d was refused: %v", i+1, MaxRulesPerKind, err)
		}
	}

	if _, err := Add(ctx, account.Writer(), 1, KindIP, "203.0.113.200", "", now); err == nil {
		t.Fatalf("a %dst IP rule was accepted", MaxRulesPerKind+1)
	}

	// The cap is per kind, so a different kind is unaffected.
	if _, err := Add(ctx, account.Writer(), 1, KindCountry, "DE", "", now); err != nil {
		t.Fatalf("the IP cap blocked a country rule: %v", err)
	}

	// And per site.
	if _, err := Add(ctx, account.Writer(), 2, KindIP, "203.0.113.200", "", now); err != nil {
		t.Fatalf("one site's cap blocked another site's rule: %v", err)
	}
}

// TestNormaliseRejectsRulesThatCannotMatch guards the promise that a saved rule
// does something. A rule that silently blocks nothing is worse than no rule:
// the customer believes the problem is solved and stops looking.
func TestNormaliseRejectsRulesThatCannotMatch(t *testing.T) {
	for _, bad := range []struct{ kind, value string }{
		{KindIP, "not-an-address"},
		{KindIP, "203.0.113.0/99"},
		{KindCountry, "United States"},
		{KindHostname, "example.com/path"},
		{KindIP, ""},
	} {
		if _, err := Normalise(bad.kind, bad.value); err == nil {
			t.Errorf("%s rule %q was accepted", bad.kind, bad.value)
		}
	}

	// A CIDR block is stored masked, and a hostname without its scheme or www,
	// so matching never depends on how somebody typed it.
	for _, tc := range []struct{ kind, in, want string }{
		{KindIP, "198.51.100.7/24", "198.51.100.0/24"},
		{KindCountry, "us", "US"},
		{KindPage, "admin", "/admin"},
		{KindHostname, "https://WWW.Example.com/", "example.com"},
	} {
		got, err := Normalise(tc.kind, tc.in)
		if err != nil {
			t.Fatalf("%s %q: %v", tc.kind, tc.in, err)
		}

		if got != tc.want {
			t.Errorf("%s %q normalised to %q, want %q", tc.kind, tc.in, got, tc.want)
		}
	}
}

// TestViewerWarnsAboutLANAddress covers the self-hosting trap. Behind a reverse
// proxy that does not forward X-Forwarded-For, every visitor resolves to the
// proxy, so manually blocking the displayed address would block every visitor.
func TestViewerWarnsAboutLANAddress(t *testing.T) {
	trusted, err := clientip.ParseTrustedProxies(nil)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/settings/sites/example.com/shields", nil)
	request.RemoteAddr = "192.168.178.1:52344"

	viewer := ResolveViewer(request, trusted)

	if !viewer.Private {
		t.Fatal("a router's LAN address was reported as a usable public address")
	}

	if viewer.Warning == "" {
		t.Fatal("the LAN case produced no warning about blocking every visitor")
	}

	// A real forwarded address is reported as usable, and named by the header
	// it came from so a customer debugging a proxy can see what we believed.
	trusted, err = clientip.ParseTrustedProxies([]string{"192.168.178.1", "10.0.0.7"})
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(clientip.HeaderForwardedFor, "203.0.113.14, 10.0.0.7")

	viewer = ResolveViewer(request, trusted)

	if viewer.Private {
		t.Fatal("a forwarded public address was reported as private")
	}

	if viewer.Address != "203.0.113.14" {
		t.Fatalf("resolved address = %q, want the nearest untrusted entry in the forwarded chain", viewer.Address)
	}

	if viewer.Source != clientip.SourceForwardedFor {
		t.Fatalf("address source = %q, want %q", viewer.Source, clientip.SourceForwardedFor)
	}
}
