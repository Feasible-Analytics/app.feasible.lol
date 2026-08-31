//
// gate_test.go
// Who gets to see the reports, and what a locked account is told instead.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

// gateNow is the clock the gate tests run at.
var gateNow = time.Date(2026, time.March, 3, 12, 0, 0, 0, time.UTC)

// fixture is a control database with one team, one site, and a gate over both.
type fixture struct {
	t       *testing.T
	control *sql.DB
	gate    *Gate
	clock   time.Time
}

// newFixture builds the database and the gate.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	control, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(context.Background(), control, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	stamp := gateNow.Unix()

	if _, err := control.Exec(`INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Example Co', ?, ?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(`INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (1, 1, 'example.com', ?, ?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	siteCache := sites.New(control)
	if err := siteCache.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	f := &fixture{t: t, control: control, clock: gateNow}
	f.gate = New(lifecycle.NewStore(control), usage.NewStore(control), siteCache, nil)
	f.gate.Now = func() time.Time { return f.clock }

	return f
}

// refresh rebuilds the locked set from the database.
func (f *fixture) refresh() {
	f.t.Helper()

	if err := f.gate.Refresh(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// lapse puts the team onto a lifecycle clock starting at day 0.
func (f *fixture) lapse() {
	f.t.Helper()

	lifecycleStore := lifecycle.NewStore(f.control)

	state := lifecycle.State{Trigger: lifecycle.TriggerLapse, StartedAt: gateNow}
	if err := lifecycleStore.Save(context.Background(), 1, state); err != nil {
		f.t.Fatal(err)
	}
}

// protected wraps a handler that records whether it was reached.
func (f *fixture) protected(reached *bool) http.Handler {
	return f.gate.Protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	}))
}

// get issues one request through the gate.
func (f *fixture) get(path string, accept string) (*httptest.ResponseRecorder, bool) {
	f.t.Helper()

	reached := false

	request := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		request.Header.Set("Accept", accept)
	}

	recorder := httptest.NewRecorder()
	f.protected(&reached).ServeHTTP(recorder, request)

	return recorder, reached
}

// TestAPayingAccountPassesThrough is the common case, and the one that must
// never be broken by anything in this package.
func TestAPayingAccountPassesThrough(t *testing.T) {
	f := newFixture(t)
	f.refresh()

	response, reached := f.get("/dashboard/example.com", "")

	if !reached {
		t.Fatal("a paying account was blocked")
	}
	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
}

// TestGraceStillSeesTheDashboard covers the first thirty days. A lapsed account
// keeps full access with a banner; the lock comes later.
func TestGraceStillSeesTheDashboard(t *testing.T) {
	f := newFixture(t)
	f.lapse()
	f.refresh()

	if _, reached := f.get("/dashboard/example.com", ""); !reached {
		t.Fatal("an account in the grace window was blocked")
	}
}

// TestLockedIsRefused is the phase boundary, observed through the gate rather
// than through the machine.
func TestLockedIsRefused(t *testing.T) {
	f := newFixture(t)
	f.lapse()

	f.clock = gateNow.Add(lifecycle.GraceDays * lifecycle.Day)
	f.refresh()

	response, reached := f.get("/dashboard/example.com", "")

	if reached {
		t.Fatal("a locked account reached the dashboard")
	}

	// 402 is the one status that says exactly this. A 403 would be
	// indistinguishable from a permissions bug in a support ticket.
	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("status %d, want 402", response.Code)
	}

	body := response.Body.String()
	if !strings.Contains(body, "/billing") {
		t.Error("the locked page does not link to billing")
	}
	if !strings.Contains(body, "/billing/export") {
		t.Error("the locked page does not offer the export")
	}
}

// TestTheStatsAPIIsLockedToo is the reason this is middleware rather than a
// check inside the dashboard. The numbers come from the API, so a lock that only
// covered the HTML would be no lock at all.
func TestTheStatsAPIIsLockedToo(t *testing.T) {
	f := newFixture(t)
	f.lapse()

	f.clock = gateNow.Add(lifecycle.GraceDays * lifecycle.Day)
	f.refresh()

	reached := false

	request := httptest.NewRequest(http.MethodPost, "/api/stats/example.com/query", strings.NewReader("{}"))
	request.SetPathValue("domain", "example.com")

	recorder := httptest.NewRecorder()
	f.protected(&reached).ServeHTTP(recorder, request)

	if reached {
		t.Fatal("a locked account reached the stats API")
	}
	if recorder.Code != http.StatusPaymentRequired {
		t.Fatalf("status %d, want 402", recorder.Code)
	}

	// The dashboard is JavaScript, so an HTML error page here produces a parse
	// error in a console instead of a message on screen.
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("the API refusal is %q, want JSON", got)
	}
	if !strings.Contains(recorder.Body.String(), `"reason"`) {
		t.Errorf("the JSON refusal has no reason: %s", recorder.Body.String())
	}
}

// TestDormantIsAlsoRefused checks the phase after collection stops.
func TestDormantIsAlsoRefused(t *testing.T) {
	f := newFixture(t)
	f.lapse()

	f.clock = gateNow.Add(lifecycle.LockedDays * lifecycle.Day)
	f.refresh()

	if _, reached := f.get("/dashboard/example.com", ""); reached {
		t.Fatal("a dormant account reached the dashboard")
	}
}

// TestPayingUnlocksOnTheNextRefresh is the recovery path as a customer
// experiences it: pay, wait a few seconds, and the dashboard is back.
func TestPayingUnlocksOnTheNextRefresh(t *testing.T) {
	f := newFixture(t)
	f.lapse()

	f.clock = gateNow.Add(45 * lifecycle.Day)
	f.refresh()

	if _, reached := f.get("/dashboard/example.com", ""); reached {
		t.Fatal("a locked account reached the dashboard")
	}

	if err := lifecycle.NewStore(f.control).Save(context.Background(), 1, lifecycle.State{}); err != nil {
		t.Fatal(err)
	}

	f.refresh()

	if _, reached := f.get("/dashboard/example.com", ""); !reached {
		t.Fatal("paying did not unlock the dashboard")
	}
}

// TestVolumeLockUsesItsOwnWording is why the two reasons are told apart. One
// says "pay us" and the other says "talk to us", and telling a growing customer
// to upgrade would be answering a question they did not ask.
func TestVolumeLockUsesItsOwnWording(t *testing.T) {
	f := newFixture(t)

	if _, err := f.control.Exec(`
		INSERT INTO usage_overages (team_id, period, asked_at, reply_deadline, locked_at, updated_at)
		VALUES (1, '2026-03', ?, ?, ?, ?)
	`, gateNow.Unix(), gateNow.Unix(), gateNow.Unix(), gateNow.Unix()); err != nil {
		t.Fatal(err)
	}

	f.refresh()

	response, reached := f.get("/dashboard/example.com", "")

	if reached {
		t.Fatal("a volume-locked account reached the dashboard")
	}

	body := response.Body.String()
	if !strings.Contains(body, "included volume") {
		t.Errorf("the volume lock page does not explain itself: %s", body)
	}
	if strings.Contains(body, "not currently paying") {
		t.Error("the volume lock page tells a paying customer they have not paid")
	}
}

// TestALifecycleLockWinsOverAVolumeLock covers an account with both. The
// lifecycle one is what they have to resolve first, and telling somebody to
// email sales when their account is thirty days from deletion would be actively
// unhelpful.
func TestALifecycleLockWinsOverAVolumeLock(t *testing.T) {
	f := newFixture(t)
	f.lapse()

	if _, err := f.control.Exec(`
		INSERT INTO usage_overages (team_id, period, locked_at, updated_at) VALUES (1, '2026-03', ?, ?)
	`, gateNow.Unix(), gateNow.Unix()); err != nil {
		t.Fatal(err)
	}

	f.clock = gateNow.Add(40 * lifecycle.Day)
	f.refresh()

	reason, locked := f.gate.Locked(1)
	if !locked {
		t.Fatal("the account is not locked")
	}
	if reason != ReasonLifecycle {
		t.Fatalf("the reason is %q, want lifecycle", reason)
	}
}

// TestAnUnknownDomainPassesThrough keeps this package out of the way of the
// handler underneath. A request for a site we do not serve gets that handler's
// own 404, rather than a different error for the same condition.
func TestAnUnknownDomainPassesThrough(t *testing.T) {
	f := newFixture(t)
	f.lapse()

	f.clock = gateNow.Add(40 * lifecycle.Day)
	f.refresh()

	if _, reached := f.get("/dashboard/somebody-elses-site.com", ""); !reached {
		t.Fatal("a request for an unknown domain was refused by the gate")
	}
	if _, reached := f.get("/dashboard/", ""); !reached {
		t.Fatal("a request with no domain was refused by the gate")
	}
}

// TestTheGateStartsPermissive is the failure direction that matters. Until the
// first refresh completes, nothing is locked: briefly showing a lapsed account
// its own reports is far better than locking every paying customer out because
// a query has not run yet.
func TestTheGateStartsPermissive(t *testing.T) {
	f := newFixture(t)
	f.lapse()
	f.clock = gateNow.Add(40 * lifecycle.Day)

	// Deliberately no refresh.
	if _, reached := f.get("/dashboard/example.com", ""); !reached {
		t.Fatal("the gate blocked traffic before it had loaded anything")
	}

	if f.gate.Count() != 0 {
		t.Errorf("a fresh gate reports %d locked accounts", f.gate.Count())
	}
}

// TestSetLocksWithoutARefresh covers the path that has just locked an account
// and should not have to wait out a refresh interval.
func TestSetLocksWithoutARefresh(t *testing.T) {
	f := newFixture(t)

	f.gate.Set(1, ReasonVolume)

	if _, reached := f.get("/dashboard/example.com", ""); reached {
		t.Fatal("an account locked directly still reached the dashboard")
	}
	if f.gate.Count() != 1 {
		t.Errorf("the gate reports %d locked accounts, want 1", f.gate.Count())
	}
}

// TestDormantSaysCollectionHasStopped is the difference between the two blocked
// lifecycle phases, in the only place a customer can see it. Both refuse, but
// telling a dormant account we are still recording its traffic is a promise it
// discovers is false on the day it pays.
func TestDormantSaysCollectionHasStopped(t *testing.T) {
	f := newFixture(t)
	f.lapse()

	f.clock = gateNow.Add(lifecycle.GraceDays * lifecycle.Day)
	f.refresh()

	locked, _ := f.get("/dashboard/example.com", "")
	if !strings.Contains(locked.Body.String(), "still collecting your data") {
		t.Errorf("the locked page does not say collection continues: %s", locked.Body.String())
	}

	f.clock = gateNow.Add(lifecycle.LockedDays * lifecycle.Day)
	f.refresh()

	if reason, _ := f.gate.Locked(1); reason != ReasonDormant {
		t.Fatalf("the reason is %q, want dormant", reason)
	}

	dormant, _ := f.get("/dashboard/example.com", "")

	body := dormant.Body.String()
	if strings.Contains(body, "still collecting your data") {
		t.Error("the dormant page claims we are still collecting, which we are not")
	}
	if !strings.Contains(body, "stopped collecting") {
		t.Errorf("the dormant page does not say collection stopped: %s", body)
	}
	if !strings.Contains(body, "/billing") {
		t.Error("the dormant page does not link to billing")
	}
}

// TestRefuseJSONNeverNegotiates is the entry point the public API and the MCP
// endpoint call. Their callers are programs in every case, and an HTML page
// produces a parse error in somebody's logs rather than the sentence that would
// have explained it.
func TestRefuseJSONNeverNegotiates(t *testing.T) {
	f := newFixture(t)
	f.gate.Set(1, ReasonLifecycle)

	recorder := httptest.NewRecorder()

	if !f.gate.RefuseJSON(recorder, 1) {
		t.Fatal("a locked account was not refused")
	}

	if recorder.Code != http.StatusPaymentRequired {
		t.Fatalf("status %d, want 402", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("the refusal is %q, want JSON", got)
	}

	var refusal Refusal
	if err := json.NewDecoder(recorder.Body).Decode(&refusal); err != nil {
		t.Fatal(err)
	}

	// All three fields are load-bearing. A caller has to be able to tell this
	// from a permissions bug, read a sentence a person can act on, and find the
	// page that fixes it without opening a support ticket.
	if refusal.Reason != ReasonLifecycle {
		t.Errorf("reason is %q", refusal.Reason)
	}
	if refusal.Action != "/billing" {
		t.Errorf("action is %q, want /billing", refusal.Action)
	}
	if !strings.Contains(refusal.Error, "not currently paying") {
		t.Errorf("the message does not say why: %q", refusal.Error)
	}
}

// TestRefuseJSONPassesAnActiveAccount is the common case again, through the
// entry point the API uses rather than the one the dashboard does.
func TestRefuseJSONPassesAnActiveAccount(t *testing.T) {
	f := newFixture(t)
	f.refresh()

	recorder := httptest.NewRecorder()

	if f.gate.RefuseJSON(recorder, 1) {
		t.Fatal("a paying account was refused")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("the gate wrote a body for an account it let through: %q", recorder.Body.String())
	}
}

// TestANilGateLocksNothing is what a self-hosted install is: no payment
// provider, no clock, and nothing anywhere that should refuse a request. Every
// component that can hold a gate holds nothing there, so a nil one has to
// answer rather than panic.
func TestANilGateLocksNothing(t *testing.T) {
	var gate *Gate

	if gate.Blocked(1) {
		t.Error("a nil gate blocked an account")
	}
	if _, locked := gate.Check(1); locked {
		t.Error("a nil gate reported an account locked")
	}

	recorder := httptest.NewRecorder()

	if gate.RefuseJSON(recorder, 1) {
		t.Error("a nil gate refused a request")
	}
}
