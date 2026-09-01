//
// panel_test.go
// The named warnings, and the one-click remedy behind one of them.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package health

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// warningsByCode indexes a panel's warnings for assertions.
func warningsByCode(panel Panel) map[string]Warning {
	out := map[string]Warning{}

	for _, warning := range panel.Warnings {
		out[warning.Code] = warning
	}

	return out
}

// TestTheProxyWarningFiresWhenMostTrafficHasNoForwardingHeader is one of the
// four named warnings.
//
// A majority of requests resolved straight from the socket means every visitor
// is being counted as the proxy's own address: one visitor, located in a
// datacentre. Nothing anywhere reports it, and it is the root cause behind four
// separate documented incumbent bugs.
func TestTheProxyWarningFiresWhenMostTrafficHasNoForwardingHeader(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 80; i++ {
		f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{ClientIPSource: ingest.SourceSocket}})
	}

	for i := 0; i < 20; i++ {
		f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{ClientIPSource: ingest.SourceForwardedFor}})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	warning, found := warningsByCode(panel)[WarnProxyNotForwarding]
	if !found {
		t.Fatalf("the proxy warning did not fire: %+v", panel.Warnings)
	}

	if warning.Count != 80 {
		t.Errorf("the warning says %d requests, want 80", warning.Count)
	}

	if !strings.Contains(warning.Title, "X-Forwarded-For") {
		t.Errorf("the warning does not name the header: %q", warning.Title)
	}
}

// TestTheProxyWarningDoesNotFireOnAQuietSite checks the minimum. Two requests
// in a day, both from the socket, is a quiet site rather than a broken proxy.
func TestTheProxyWarningDoesNotFireOnAQuietSite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < ProxyWarningMinimum-1; i++ {
		f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{ClientIPSource: ingest.SourceSocket}})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if _, found := warningsByCode(panel)[WarnProxyNotForwarding]; found {
		t.Fatal("the proxy warning fired on a handful of requests")
	}
}

// TestTheProxyWarningDoesNotFireWhenTheProxyIsWorking checks that a small share
// of socket-resolved traffic — an API caller, a health check — is normal.
func TestTheProxyWarningDoesNotFireWhenTheProxyIsWorking(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 90; i++ {
		f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{ClientIPSource: ingest.SourceForwardedFor}})
	}

	for i := 0; i < 10; i++ {
		f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{ClientIPSource: ingest.SourceSocket}})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if _, found := warningsByCode(panel)[WarnProxyNotForwarding]; found {
		t.Fatal("the proxy warning fired on a working proxy")
	}
}

// TestTheUnknownHostnameWarningNamesTheHostnameAndOffersAllow is the second
// named warning, and the only one with a one-click remedy.
func TestTheUnknownHostnameWarningNamesTheHostnameAndOffersAllow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 412; i++ {
		f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{Hostname: "staging.other.example"}})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	warning, found := warningsByCode(panel)[WarnUnknownHostname]
	if !found {
		t.Fatalf("the hostname warning did not fire: %+v", panel.Warnings)
	}

	if warning.Hostname != "staging.other.example" {
		t.Errorf("the warning names %q", warning.Hostname)
	}

	if !strings.Contains(warning.Title, "staging.other.example") {
		t.Errorf("the title does not name the hostname: %q", warning.Title)
	}

	if warning.Action != ActionAllowHostname {
		t.Errorf("the warning offers no Allow action: %q", warning.Action)
	}

	if warning.Count != 412 {
		t.Errorf("the warning says %d events, want 412", warning.Count)
	}
}

// TestAllowingAHostnameAlsoAllowsTheSitesOwnDomain is the trap inside the
// one-click remedy.
//
// An allow-list is all-or-nothing: adding one hostname to an empty list would
// start dropping the site's own traffic, which is the worst possible outcome of
// pressing a button labelled Allow.
func TestAllowingAHostnameAlsoAllowsTheSitesOwnDomain(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		f.observe(ingest.Observation{Accepted: true, Debug: ingest.Debug{Hostname: "staging.other.example"}})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if err := f.store.AllowHostname(ctx, f.domain, "staging.other.example"); err != nil {
		t.Fatalf("allow: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	allowed := map[string]bool{}
	for _, hostname := range panel.AllowedHostnames {
		allowed[hostname] = true
	}

	if !allowed["acme.example"] {
		t.Fatal("allowing a hostname did not also allow the site's own domain — its traffic would now be dropped")
	}

	if !allowed["staging.other.example"] {
		t.Fatal("the requested hostname was not allowed")
	}

	// And the routing snapshot the ingest path reads has to agree immediately,
	// not at the next fifteen-second refresh.
	site, ok := f.sites.Lookup(f.domain)
	if !ok {
		t.Fatal("the site vanished from the routing map")
	}

	if !site.HostnameAllowed("staging.other.example") || !site.HostnameAllowed("acme.example") {
		t.Fatalf("the routing snapshot has allow-list %v", site.AllowedHostnames)
	}

	if site.HostnameAllowed("somewhere.else.example") {
		t.Fatal("the allow-list is not actually restricting anything")
	}

	// The warning goes away in the same round trip, rather than staying on the
	// screen looking unfixed.
	if _, found := warningsByCode(panel)[WarnUnknownHostname]; found {
		t.Fatal("the warning survived the Allow button")
	}
}

// TestTheTruncationWarningNamesTheNumber is the third named warning.
//
// The incumbent drops a thirty-first property with no error, no warning and no
// rejection; this warning is that number.
func TestTheTruncationWarningNamesTheNumber(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 9; i++ {
		f.observe(ingest.Observation{
			Accepted:   true,
			Truncation: ingest.Truncation{PropsDropped: 2},
		})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	warning, found := warningsByCode(panel)[WarnPropsTruncated]
	if !found {
		t.Fatalf("the truncation warning did not fire: %+v", panel.Warnings)
	}

	if warning.Count != 9 {
		t.Errorf("the warning says %d events, want 9", warning.Count)
	}

	if !strings.Contains(warning.Detail, "30") {
		t.Errorf("the warning does not name the limit: %q", warning.Detail)
	}
	if !strings.Contains(warning.Title, "on 9 events") || !strings.Contains(warning.Detail, "on 9 events") ||
		strings.Contains(strings.ToLower(warning.Title), "occurrences") {
		t.Errorf("the affected-event wording is not explicit: title=%q detail=%q", warning.Title, warning.Detail)
	}
}

// TestTheOldTrackerWarningFires is the fourth named warning. A script survives
// in browser caches for months, so a customer running a build from before a fix
// has no other way to find out.
func TestTheOldTrackerWarningFires(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A version below the current one is old by definition. The constant is
	// used rather than a literal so this test keeps working when it is bumped.
	old := CurrentTrackerVersion - 1
	if old < 1 {
		t.Skip("there is no older tracker version than the current one yet")
	}

	for i := 0; i < 12; i++ {
		f.observe(ingest.Observation{Accepted: true, TrackerVersion: old})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if _, found := warningsByCode(panel)[WarnOldTracker]; !found {
		t.Fatalf("the old-tracker warning did not fire: %+v", panel.Warnings)
	}
}

// TestTheCurrentTrackerVersionRaisesNoWarning checks the other direction, which
// is what makes the warning worth acting on.
func TestTheCurrentTrackerVersionRaisesNoWarning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 40; i++ {
		f.observe(ingest.Observation{Accepted: true, TrackerVersion: CurrentTrackerVersion})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if _, found := warningsByCode(panel)[WarnOldTracker]; found {
		t.Fatal("the old-tracker warning fired for the current version")
	}

	if len(panel.TrackerVersions) != 1 {
		t.Fatalf("the tracker versions are %+v", panel.TrackerVersions)
	}
}

// TestNoTrafficAtAllIsItsOwnWarning checks the state that most often means a
// snippet is not installed.
func TestNoTrafficAtAllIsItsOwnWarning(t *testing.T) {
	f := newFixture(t)

	panel, err := f.store.Panel(context.Background(), f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if _, found := warningsByCode(panel)[WarnNoTraffic]; !found {
		t.Fatalf("a site with no events raised no warning: %+v", panel.Warnings)
	}
}

// TestAHostnameBlockedDropRaisesItsOwnWarning checks that an allow-list which
// is dropping events says so, rather than the events simply disappearing.
func TestAHostnameBlockedDropRaisesItsOwnWarning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		f.observe(ingest.Observation{DropReason: ingest.ReasonHostnameNotAllowed})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	warning, found := warningsByCode(panel)[WarnHostnameBlocked]
	if !found {
		t.Fatalf("a blocking allow-list raised no warning: %+v", panel.Warnings)
	}

	if warning.Count != 6 {
		t.Errorf("the warning says %d events, want 6", warning.Count)
	}
}

// TestEveryClosedReasonHasAnExplanation checks that a drop can never be
// described as "other" on the panel.
func TestEveryClosedReasonHasAnExplanation(t *testing.T) {
	// The sentences come from the message catalogue, and a catalogue miss
	// renders as the message id — which is neither empty nor equal to the
	// reason, so the two checks below would both pass while the panel showed
	// somebody `settings.health.reason.bot`. Rejecting anything that still
	// looks like an id is what closes that.
	explained := func(t *testing.T, reason string) {
		t.Helper()

		explanation := Explain(reason)

		switch {
		case explanation == "" || explanation == reason:
			t.Errorf("%s has no plain-English explanation", reason)
		case strings.HasPrefix(explanation, "settings."):
			t.Errorf("%s explains itself as the message id %q, so its string is missing", reason, explanation)
		}
	}

	for _, reason := range ingest.Reasons {
		explained(t, reason)
	}

	for _, truncation := range []string{
		ingest.TruncationProps, ingest.TruncationPropName, ingest.TruncationPropValue,
		ingest.TruncationPropUnsupported, ingest.TruncationURL, ingest.TruncationEngagement,
	} {
		explained(t, truncation)
	}

	// The property cap is interpolated rather than baked into the sentence, so
	// the number a reader is told is the number the pipeline enforces.
	if explanation := Explain(ingest.TruncationProps); !strings.Contains(explanation, strconv.Itoa(ingest.MaxProps)) {
		t.Errorf("the truncation sentence does not name the %d-property cap: %q", ingest.MaxProps, explanation)
	}
}

// TestDropsAreOrderedBiggestFirst checks that the reason worth fixing is at the
// top rather than wherever an id put it.
func TestDropsAreOrderedBiggestFirst(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		f.observe(ingest.Observation{DropReason: ingest.ReasonRateLimited})
	}

	for i := 0; i < 11; i++ {
		f.observe(ingest.Observation{DropReason: ingest.ReasonInvalidPayload})
	}

	if _, err := f.recorder.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatalf("panel: %v", err)
	}

	if len(panel.Drops) != 2 || panel.Drops[0].Reason != ingest.ReasonInvalidPayload {
		t.Fatalf("the drops are not ordered biggest first: %+v", panel.Drops)
	}
}

// TestAnUnknownSiteIsRefusedRatherThanEmpty checks that a domain nobody
// registered produces a sentinel rather than a page of zeroes.
func TestAnUnknownSiteIsRefusedRatherThanEmpty(t *testing.T) {
	f := newFixture(t)

	if _, err := f.store.Panel(context.Background(), "nobody.example"); err != ErrUnknownSite {
		t.Fatalf("Panel for an unknown site = %v, want ErrUnknownSite", err)
	}
}

// TestCorrectedWarningsAndDebugDetailsAgeOut keeps every panel claim inside
// the stated 24-hour window rather than retaining a historical warning forever.
func TestCorrectedWarningsAndDebugDetailsAgeOut(t *testing.T) {
	f := newFixture(t)
	f.observe(ingest.Observation{
		Accepted: true, TrackerVersion: CurrentTrackerVersion,
		Debug: ingest.Debug{Hostname: "staging.other.test", ClientIPSource: ingest.SourceSocket},
	})
	if _, err := f.recorder.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := f.store.Panel(context.Background(), f.domain)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.UnknownHostnames) == 0 || before.LastRequest == nil {
		t.Fatalf("fresh observations missing: %+v", before)
	}

	f.now = f.now.Add(Window + time.Hour)
	after, err := f.store.Panel(context.Background(), f.domain)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.UnknownHostnames) != 0 || len(after.TrackerVersions) != 0 || after.LastRequest != nil {
		t.Fatalf("aged observations remain: hostnames=%v versions=%v last=%+v",
			after.UnknownHostnames, after.TrackerVersions, after.LastRequest)
	}
	if _, found := warningsByCode(after)[WarnUnknownHostname]; found {
		t.Fatalf("corrected hostname warning remained: %+v", after.Warnings)
	}
}

// TestPanelUsesAnExactOpenClosedDayWindow places evidence on both boundaries
// and just outside them. The claimed interval is exactly (now-24h, now], so a
// coarse hour bucket or an unbounded future row cannot pass this test.
func TestPanelUsesAnExactOpenClosedDayWindow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	account, err := f.accounts.Open(ctx, f.teamID)
	if err != nil {
		t.Fatal(err)
	}
	from := f.now.Add(-Window).Unix()
	to := f.now.Unix()
	for _, observedAt := range []int64{from, from + 1, to, to + 1} {
		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO ingest_health (site_id, observed_at, kind, reason, count)
			VALUES (?, ?, 'accepted', '', 1)
		`, f.siteID, observedAt); err != nil {
			t.Fatal(err)
		}
		if _, err := account.Writer().ExecContext(ctx, `
			INSERT INTO ingest_observations
				(site_id, observed_at, kind, value, count, first_seen_at, last_seen_at)
			VALUES (?, ?, 'ip_source', 'socket', 1, ?, ?)
		`, f.siteID, observedAt, observedAt, observedAt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := account.Writer().ExecContext(ctx, `
		INSERT INTO ingest_last_request (site_id, received_at) VALUES (?, ?)
	`, f.siteID, to+1); err != nil {
		t.Fatal(err)
	}

	panel, err := f.store.Panel(ctx, f.domain)
	if err != nil {
		t.Fatal(err)
	}
	if panel.From != from || panel.To != to || panel.Accepted != 2 {
		t.Fatalf("panel interval/count = (%d,%d]/%d, want (%d,%d]/2",
			panel.From, panel.To, panel.Accepted, from, to)
	}
	if len(panel.IPSources) != 1 || panel.IPSources[0].Count != 2 ||
		panel.IPSources[0].FirstSeen != from+1 || panel.IPSources[0].LastSeen != to {
		t.Fatalf("bounded observations = %+v", panel.IPSources)
	}
	if panel.LastRequest != nil {
		t.Fatalf("future last request leaked into panel: %+v", panel.LastRequest)
	}
}
