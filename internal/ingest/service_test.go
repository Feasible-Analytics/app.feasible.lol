//
// service_test.go
// The correctness harness: replay in order, then shuffled and duplicated, and compare.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/geo"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/salts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// fixtureDomain is the spelling the site is registered under. The requests
// deliberately do not all use it — see domainSpellings.
const fixtureDomain = "example.com"

// domainSpellings are the ways one site's own pages spell its own domain: a
// snippet somebody typed by hand, a trailing dot from a copied FQDN, www on one
// page and the apex on another. Sending only one of them is how a harness hides
// a fingerprint bug, because the site domain is a fingerprint input: hashing
// the spelling rather than the resolved site gives each spelling its own set of
// visitors, and no later job can put them back together.
//
// The spelling a request uses is a function of the request itself, so shuffling
// the stream cannot change which spelling an event was sent with.
var domainSpellings = []string{
	"example.com",
	"www.example.com",
	"Example.com",
	"EXAMPLE.com",
	"example.com.",
}

// urlHosts are the hosts the pages are actually served from. A browser
// lower-cases the host in location.href, so the disagreement a real site has is
// between its apex and its www name rather than one of case.
var urlHosts = []string{"example.com", "www.example.com"}

// fixtureIngestSalt pins daily derivation so every replay computes the same
// fingerprints without shared database state.
const fixtureIngestSalt = "fixture-shared-salt"

// fixtureStart is noon UTC, far enough from midnight that the whole stream sits
// inside one salt day.
var fixtureStart = time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)

// visitor is one distinct person: an address and a user agent, which together
// with the site and root domains are the four fingerprint inputs.
type visitor struct {
	ip        string
	userAgent string
}

// visitors are the people in the fixture. Their exact values do not matter, but
// they must be distinct, because the number of distinct visitors is one of the
// six metrics being asserted.
var visitors = []visitor{
	{"203.0.113.10", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"},
	{"203.0.113.11", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36"},
	{"203.0.113.12", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1"},
	{"198.51.100.20", "Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0"},
	{"198.51.100.21", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Safari/605.1.15"},
}

// fixtureBrowsers is what "current" means to the fixture. It matches the user
// agents above, so the people in the fixture are ordinary visitors rather than
// a browser version that happens to have aged since somebody wrote them down.
var fixtureBrowsers = map[string]int{"Chrome": 121, "Firefox": 121, "Edge": 121}

// visitorAt returns one person by index. The first five are the hand-written
// ones above; beyond that they are generated, which is what lets the same
// fixture be replayed as many distinct people at once without hand-writing a
// stream long enough to fill a production-sized write buffer.
//
// The generated addresses come out of 198.18.0.0/15, the benchmarking range: it
// is reserved, so no test can accidentally geolocate to somewhere real.
func visitorAt(i int) visitor {
	if i < len(visitors) {
		return visitors[i]
	}

	return visitor{
		ip:        fmt.Sprintf("198.18.%d.%d", (i/250)%256, i%250),
		userAgent: visitors[i%len(visitors)].userAgent,
	}
}

// pick chooses one of a set of interchangeable values from a request, so that
// the choice is a property of the event rather than of when it arrived.
// Anything that varied with arrival order would make the shuffled run differ
// from the ordered one for a reason that has nothing to do with the fold.
func pick(options []string, r request) string {
	sum := uint64(r.visitor) * 1099511628211
	sum ^= uint64(r.timestamp)

	for i := 0; i < len(r.url); i++ {
		sum = (sum ^ uint64(r.url[i])) * 1099511628211
	}
	for i := 0; i < len(r.name); i++ {
		sum = (sum ^ uint64(r.name[i])) * 1099511628211
	}

	return options[sum%uint64(len(options))]
}

// visitSpec describes one visit in the fixture. The expected metrics are
// computed from these specs by direct arithmetic rather than by running the
// fold, so the harness checks the implementation against the intent rather than
// against itself.
type visitSpec struct {
	visitor int

	// offset is when the visit starts, in seconds from fixtureStart. Visits by
	// the same visitor are hours apart so none of them can bridge.
	offset int64

	// pages are the pageviews, forty-five seconds apart.
	pages []string

	// custom is how many interactive non-pageview events the visit contains.
	// One is enough to end a bounce.
	custom int

	// pings is how many engagement events the visit contains. They refresh the
	// end of the visit and are counted in neither pageviews nor events.
	pings int

	// referrer and query are the acquisition inputs for the visit's first
	// event, so the harness exercises the referrer and channel derivation too.
	referrer string
	query    string
}

// fixture is the stream. It is written out rather than generated so that the
// expected numbers can be read off the page.
//
// Every offset stays inside one UTC day. Crossing midnight would rotate the salt
// mid-stream and give the later visits a new pseudonym for the same person,
// which is correct behaviour and would make the visitor count untellable by
// hand. The midnight case has its own test.
var fixture = []visitSpec{
	{visitor: 0, offset: 0, pages: []string{"/", "/pricing", "/signup"}, custom: 1, pings: 2, referrer: "https://www.google.com/", query: ""},
	{visitor: 0, offset: 7000, pages: []string{"/blog"}, pings: 1, referrer: "", query: ""},
	{visitor: 1, offset: 120, pages: []string{"/"}, referrer: "https://news.ycombinator.com/item?id=1", query: ""},
	{visitor: 1, offset: 11000, pages: []string{"/", "/docs"}, pings: 3, referrer: "", query: "?utm_source=newsletter&utm_medium=email&utm_campaign=launch"},
	{visitor: 2, offset: 300, pages: []string{"/"}, custom: 2, referrer: "", query: "?utm_source=facebook&utm_medium=cpc&gclid=SHOULD_NOT_BE_STORED"},
	{visitor: 2, offset: 14000, pages: []string{"/", "/pricing", "/pricing/enterprise", "/signup"}, pings: 4, referrer: "https://chatgpt.com/", query: ""},
	{visitor: 3, offset: 900, pages: []string{"/features"}, pings: 1, referrer: "https://twitter.com/someone/status/1", query: ""},
	{visitor: 3, offset: 18000, pages: []string{"/", "/features"}, custom: 1, referrer: "", query: "?ref=partner-site"},
	{visitor: 4, offset: 1500, pages: []string{"/", "/about"}, referrer: "", query: ""},
	{visitor: 4, offset: 22000, pages: []string{"/pricing"}, custom: 3, pings: 2, referrer: "https://duckduckgo.com/", query: ""},
	{visitor: 4, offset: 26000, pages: []string{"/"}, referrer: "", query: ""},
}

// request is one HTTP call the harness will make, carrying its own timestamp so
// that shuffling changes arrival order without changing when the event
// happened.
type request struct {
	visitor   int
	timestamp int64
	name      string
	url       string
	referrer  string
	props     map[string]string
}

// metrics are the six core numbers. They are the contract the dashboard is
// built on, and the whole point of the harness is that they do not move when
// the delivery order does.
type coreMetrics struct {
	Visitors      int64
	Visits        int64
	Pageviews     int64
	BounceRate    float64
	VisitDuration float64
	ViewsPerVisit float64
}

// expected computes the six metrics from the fixture specs by direct
// arithmetic. Nothing here folds an event: the counts come from the shape of
// each visit, so a bug in the fold cannot make the expectation agree with it.
func expected() coreMetrics {
	seen := map[int]struct{}{}

	var (
		visits         int64
		pageviews      int64
		bounces        int64
		totalDuration  int64
		totalPageviews int64
	)

	for _, spec := range fixture {
		seen[spec.visitor] = struct{}{}
		visits++

		pageviews += int64(len(spec.pages))
		totalPageviews += int64(len(spec.pages))

		// A visit bounces when it never got past its first page and nobody
		// interacted. Engagement pings are the tracker talking, not a person.
		if len(spec.pages) < 2 && spec.custom == 0 {
			bounces++
		}

		first, last := spec.span()
		totalDuration += last - first
	}

	return coreMetrics{
		Visitors:      int64(len(seen)),
		Visits:        visits,
		Pageviews:     pageviews,
		BounceRate:    100.0 * float64(bounces) / float64(visits),
		VisitDuration: float64(totalDuration) / float64(visits),
		ViewsPerVisit: float64(totalPageviews) / float64(visits),
	}
}

// span returns the first and last event timestamps of a visit, as offsets. The
// duration is the distance between them, and an engagement ping can be the last
// event — which is exactly why time-on-page has to survive a reordered stream.
func (s visitSpec) span() (int64, int64) {
	requests := s.requests()

	first, last := requests[0].timestamp, requests[0].timestamp
	for _, r := range requests {
		if r.timestamp < first {
			first = r.timestamp
		}
		if r.timestamp > last {
			last = r.timestamp
		}
	}

	return first, last
}

// requests expands one visit into the calls that make it up, always in
// chronological order. Shuffling happens to the assembled stream, not here.
func (s visitSpec) requests() []request {
	base := fixtureStart.Unix() + s.offset

	var out []request

	for i, page := range s.pages {
		at := base + int64(i)*45

		url := "https://" + fixtureDomain + page
		referrer := ""

		// Acquisition is frozen at session start, so the referrer and the
		// campaign tags only ever go on the first event of the visit.
		if i == 0 {
			url += s.query
			referrer = s.referrer
		}

		out = append(out, request{
			visitor:   s.visitor,
			timestamp: at,
			name:      EventPageview,
			url:       url,
			referrer:  referrer,
		})

		if i < s.pings {
			out = append(out, request{
				visitor:   s.visitor,
				timestamp: at + 20,
				name:      EventEngagement,
				url:       "https://" + fixtureDomain + page,
			})
		}
	}

	for i := 0; i < s.custom; i++ {
		out = append(out, request{
			visitor:   s.visitor,
			timestamp: base + 5 + int64(i),
			name:      "signup",
			url:       "https://" + fixtureDomain + s.pages[0],
			props:     map[string]string{"plan": "pro", "seats": "3"},
		})
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].timestamp < out[j].timestamp })

	return out
}

// stream assembles the whole fixture in chronological order.
func stream() []request {
	var out []request
	for _, spec := range fixture {
		out = append(out, spec.requests()...)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].timestamp < out[j].timestamp })

	return out
}

// expandedStream is the fixture replayed as several sets of distinct people at
// once. It exists so a replay can be long enough to fill a production-sized
// write buffer without hand-writing hundreds of visits: every extra set is the
// same visits by new visitors, so the expected numbers stay computable by hand.
func expandedStream(sets int) []request {
	var out []request

	for set := 0; set < sets; set++ {
		for _, r := range stream() {
			r.visitor += set * len(visitors)
			out = append(out, r)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].timestamp < out[j].timestamp })

	return out
}

// expectedFor scales the six metrics to an expanded stream. The three counts
// multiply by the number of sets and the three ratios do not, because every set
// is the same visits by different people.
func expectedFor(sets int) coreMetrics {
	m := expected()

	m.Visitors *= int64(sets)
	m.Visits *= int64(sets)
	m.Pageviews *= int64(sets)

	return m
}

// harness is one wired-up ingest service over its own account database.
type harness struct {
	service *Service
	manager *accounts.Manager

	// clock is unix seconds rather than a time.Time because the write buffer
	// flushes on its own goroutine at production sizes, and that goroutine
	// reads the clock while the test is setting it for the next request.
	clock atomic.Int64
}

// now is the clock every part of the pipeline reads.
func (h *harness) now() time.Time {
	return time.Unix(h.clock.Load(), 0).UTC()
}

// setClock moves the harness clock to the moment an event happened.
func (h *harness) setClock(at time.Time) {
	h.clock.Store(at.Unix())
}

// newSystem builds the app shard system database with one team and one site.
func newSystem(t testing.TB, dir string) *sql.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatal(err)
	}

	now := fixtureStart.Unix()

	if _, err := db.Exec("INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Fixture', ?, ?)", now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		"INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (1, 1, ?, ?, ?)",
		fixtureDomain, now, now,
	); err != nil {
		t.Fatal(err)
	}

	return db
}

// newHarness wires a service whose clock drives both event timestamps and daily
// salt derivation, so replay ordering cannot change the salt day.
func newHarness(t testing.TB, control *sql.DB, dataDir string, wrap func(Transport) Transport) *harness {
	t.Helper()

	manager := accounts.NewManager(dataDir)
	t.Cleanup(func() { checkClose(t, "account manager", manager.CloseAll) })

	h := &harness{manager: manager}
	h.setClock(fixtureStart)

	service, err := NewService(context.Background(), control, manager, Options{
		DataDir:        dataDir,
		IngestSalt:     fixtureIngestSalt,
		Now:            h.now,
		TrustedProxies: []string{"192.0.2.0/24"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.service = service
	if !service.Handler.Durable {
		t.Fatal("the public direct-mode handler does not wait for a durable account commit")
	}

	// The serving command wires this; NewService does not. Without it every
	// writer-side drop — a live shield, an engagement ping whose pageview never
	// came — is invisible, and a replay that quietly loses events reads as a
	// number that no longer matches with no reason attached.
	service.Writer.Counters = service.Counters

	// The fixture people below run the browsers they ran when they were
	// written. Judging them against the embedded list would classify the whole
	// fixture as automated the moment those versions aged a year, so the floor
	// is pinned to the fixture instead. The rule itself is tested where it
	// lives.
	service.Pipeline.Bots.SetCurrentBrowsers(fixtureBrowsers)

	// The buffer is built with the same bounds production runs with. A harness
	// that raises them writes every event through a path production never
	// takes: a buffer bigger than the stream can never flush on size, so the
	// size trigger and the flush that runs while the next request is being
	// accepted are both untested.
	var transport Transport = NewDirect(service.Writer)
	if wrap != nil {
		transport = wrap(transport)
	}

	service.Buffer = NewBuffer(transport, DefaultBufferSize, DefaultFlushInterval)

	// Replay tests deliberately flush at chosen boundaries. Production
	// durability is asserted above; disabling the request waiter here prevents
	// each sequential fixture request from forcing its own timer transaction.
	service.Handler.Durable = false

	// The replay harness uses the asynchronous path, so a failed flush needs an
	// explicit test failure rather than being visible as a request status.
	//
	// The error is recorded rather than reported from the callback, because a
	// flush runs on its own goroutine and may outlive the request that started
	// it — reporting from there would be a log call after the test finished.
	var flushErr atomic.Pointer[error]

	service.Buffer.OnError = func(err error) { flushErr.CompareAndSwap(nil, &err) }

	t.Cleanup(func() {
		if err := flushErr.Load(); err != nil {
			t.Errorf("write buffer flush failed: %v", *err)
		}
	})

	service.Handler.Buffer = service.Buffer

	return h
}

// send posts one event, with the clock set to the moment it happened. Arrival
// order is the order send is called; the event's own timestamp is what every
// accumulation rule keys off.
func (h *harness) send(t testing.TB, r request) *httptest.ResponseRecorder {
	t.Helper()

	h.clock.Store(r.timestamp)

	// The snippet's domain and the host the page was served from are both
	// varied, in a way that depends only on the event. Every spelling resolves
	// to the same site, so every one of them has to produce the same visitor.
	domain := pick(domainSpellings, r)
	url := strings.Replace(r.url, "https://"+fixtureDomain, "https://"+pick(urlHosts, r), 1)

	payload := map[string]any{
		"n": r.name,
		"u": url,
		"d": domain,
		"r": r.referrer,
	}
	if len(r.props) > 0 {
		payload["p"] = r.props
	}
	if r.name == EventEngagement {
		payload["e"] = 12000
		payload["sd"] = 65
	}

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	// text/plain, because that is what the real trackers send: it avoids a CORS
	// preflight, and an endpoint that rejected it would break every existing
	// integration.
	req := httptest.NewRequest(http.MethodPost, "/api/event", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "text/plain")
	person := visitorAt(r.visitor)
	req.Header.Set("User-Agent", person.userAgent)
	req.Header.Set("X-Forwarded-For", person.ip)

	recorder := httptest.NewRecorder()
	h.service.Handler.ServeHTTP(recorder, req)

	return recorder
}

// replay sends a whole stream, flushing periodically so the batching path is
// exercised rather than bypassed.
func (h *harness) replay(t testing.TB, requests []request) {
	t.Helper()

	ctx := context.Background()

	for i, r := range requests {
		if recorder := h.send(t, r); recorder.Code != http.StatusAccepted {
			t.Fatalf("request %d: status %d, want 202: %s", i, recorder.Code, recorder.Body.String())
		}

		// Flushing every few events means the stream crosses many batches, so
		// the fold has to carry session state from one transaction to the next
		// rather than seeing a whole visit at once.
		if (i+1)%7 == 0 {
			if err := h.service.Buffer.Flush(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := h.service.Buffer.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

// metrics reads the six core numbers back out of the account database, using
// the definitions the dashboard will use. They are stated here in SQL rather
// than derived in Go on purpose: these are the queries that have to be right.
func (h *harness) metrics(t testing.TB) coreMetrics {
	t.Helper()

	account, err := h.manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	db := account.Reader()

	var m coreMetrics

	scan := func(query string, into any) {
		if err := db.QueryRow(query).Scan(into); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	scan("SELECT COUNT(DISTINCT user_id) FROM sessions", &m.Visitors)
	scan("SELECT COUNT(*) FROM sessions", &m.Visits)
	scan("SELECT COUNT(*) FROM events WHERE name_id = (SELECT id FROM dim_event_name WHERE value = 'pageview')", &m.Pageviews)

	// Bounces are counted as a share of sessions, and visit_duration includes
	// bounced sessions as zero rather than excluding them — excluding them is
	// why the incumbent's time-on-page numbers run high.
	scan("SELECT 100.0 * SUM(is_bounce) / COUNT(*) FROM sessions", &m.BounceRate)
	scan("SELECT AVG(duration) FROM sessions", &m.VisitDuration)
	scan("SELECT 1.0 * SUM(pageviews) / COUNT(*) FROM sessions", &m.ViewsPerVisit)

	return m
}

// sessionRow is one stored visit, with the columns whose values must not depend
// on delivery order. The row id and the visitor hash are excluded because both
// are allocation artefacts: ids are handed out in arrival order, and comparing
// them would fail for a reason that says nothing about correctness.
type sessionRow struct {
	StartedAt  int64
	LastSeenAt int64
	Duration   int64
	IsBounce   int64
	Pageviews  int64
	Events     int64
	EntryPage  string
	ExitPage   string
	Source     string
	Channel    string
	UTMSource  string
	UTMMedium  string
	Country    string
	Browser    string
}

// sessionRows reads every session back in a stable order, so two runs can be
// compared directly.
func (h *harness) sessionRows(t testing.TB) []sessionRow {
	t.Helper()

	account, err := h.manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := account.Reader().Query(`
		SELECT s.started_at, s.last_seen_at, s.duration, s.is_bounce, s.pageviews, s.events,
		       entry.value, exit.value, source.value, channel.value,
		       utm_source.value, utm_medium.value, country.value, browser.value
		FROM sessions s
		JOIN dim_pathname   entry      ON entry.id      = s.entry_page_id
		JOIN dim_pathname   exit       ON exit.id       = s.exit_page_id
		JOIN dim_source     source     ON source.id     = s.source_id
		JOIN dim_channel    channel    ON channel.id    = s.channel_id
		JOIN dim_utm_source utm_source ON utm_source.id = s.utm_source_id
		JOIN dim_utm_medium utm_medium ON utm_medium.id = s.utm_medium_id
		JOIN dim_country    country    ON country.id    = s.country_id
		JOIN dim_browser    browser    ON browser.id    = s.browser_id
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var out []sessionRow

	for rows.Next() {
		var row sessionRow
		if err := rows.Scan(
			&row.StartedAt, &row.LastSeenAt, &row.Duration, &row.IsBounce, &row.Pageviews, &row.Events,
			&row.EntryPage, &row.ExitPage, &row.Source, &row.Channel,
			&row.UTMSource, &row.UTMMedium, &row.Country, &row.Browser,
		); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}

	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt < out[j].StartedAt
		}
		if out[i].EntryPage != out[j].EntryPage {
			return out[i].EntryPage < out[j].EntryPage
		}
		return out[i].Browser < out[j].Browser
	})

	return out
}

// eventCount reports how many rows the events table holds, which is the number
// a duplicated replay must not change.
func (h *harness) eventCount(t testing.TB) int64 {
	t.Helper()

	account, err := h.manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	var count int64
	if err := account.Reader().QueryRow("SELECT COUNT(*) FROM events").Scan(&count); err != nil {
		t.Fatal(err)
	}

	return count
}

// duplicating re-sends a share of every batch, which is what a lost
// acknowledgement looks like from the shard's side: the write committed, the
// sender never heard, and it tried again.
type duplicating struct {
	inner Transport
	rate  float64
	rand  *rand.Rand

	// sent counts the redeliveries, so the test can prove it actually exercised
	// the dedupe path rather than passing because nothing was duplicated.
	sent int
}

// Send delivers the batch, then delivers a sample of it a second time. The
// second delivery must change nothing, because every event carries a uuid the
// shard has already seen.
func (d *duplicating) Send(ctx context.Context, shard int, batch []Event) ([]uuid.UUID, error) {
	committed, err := d.inner.Send(ctx, shard, batch)
	if err != nil {
		return committed, err
	}

	var again []Event
	for _, event := range batch {
		if d.rand.Float64() < d.rate {
			again = append(again, event)
		}
	}

	if len(again) == 0 {
		return committed, nil
	}

	d.sent += len(again)

	if _, err := d.inner.Send(ctx, shard, again); err != nil {
		return committed, err
	}

	return committed, nil
}

// TestReplayInOrder is run one: the stream delivered chronologically, with the
// six core metrics asserted against values computed from the fixture rather
// than from the code under test.
func TestReplayInOrder(t *testing.T) {
	dir := t.TempDir()
	control := newSystem(t, dir)

	h := newHarness(t, control, filepath.Join(dir, "run-a"), nil)
	h.replay(t, stream())

	want := expected()
	got := h.metrics(t)

	assertMetrics(t, got, want)
	assertNothingDropped(t, h)
}

// TestReplayShuffledWithDuplicatesMatches is run two, and the single
// highest-value test in the project. The same stream arrives in a random order
// with five per cent of every batch redelivered, and every number and every
// stored session row has to come out identical.
//
// Run two is what proves order-independence and idempotency together: the first
// property makes the delivery buffer invisible to the metrics, and the second
// makes a retry harmless. Without both, exit_page is quietly wrong on any site
// with retries and a duplicated pageview is a wrong number with no cause.
func TestReplayShuffledWithDuplicatesMatches(t *testing.T) {
	dir := t.TempDir()
	control := newSystem(t, dir)

	ordered := newHarness(t, control, filepath.Join(dir, "run-a"), nil)
	ordered.replay(t, stream())

	wantMetrics := ordered.metrics(t)
	wantRows := ordered.sessionRows(t)
	wantEvents := ordered.eventCount(t)

	// Several shuffles rather than one. A single ordering can miss a bug that
	// only appears when two particular events swap, and this test is the last
	// line of defence for both unrecoverable decisions.
	for seed := int64(1); seed <= 12; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			random := rand.New(rand.NewSource(seed))

			shuffled := append([]request(nil), stream()...)
			random.Shuffle(len(shuffled), func(i, j int) {
				shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
			})

			var duplicator *duplicating

			replayed := newHarness(t, control, filepath.Join(dir, fmt.Sprintf("run-%d", seed)), func(inner Transport) Transport {
				duplicator = &duplicating{inner: inner, rate: 0.25, rand: rand.New(rand.NewSource(seed * 977))}
				return duplicator
			})
			replayed.replay(t, shuffled)

			if duplicator.sent == 0 {
				t.Fatal("no events were redelivered, so the dedupe path was never exercised")
			}

			assertMetrics(t, replayed.metrics(t), wantMetrics)
			assertNothingDropped(t, replayed)

			if got := replayed.eventCount(t); got != wantEvents {
				t.Fatalf("events written = %d, want %d after %d redeliveries — a duplicate was counted",
					got, wantEvents, duplicator.sent)
			}

			if gotRows := replayed.sessionRows(t); !reflect.DeepEqual(gotRows, wantRows) {
				t.Fatalf("session rows differ after shuffling\n got: %s\nwant: %s", render(gotRows), render(wantRows))
			}
		})
	}
}

// counting is a transport that records what the buffer handed it. It is how a
// test tells "the buffer flushed because it filled up" from "the buffer flushed
// because the test asked it to", which is the difference between exercising the
// production trigger and exercising the test's own.
type counting struct {
	inner Transport

	batches atomic.Int64
	events  atomic.Int64
}

// Send passes the batch through, counting it on the way.
func (c *counting) Send(ctx context.Context, shard int, batch []Event) ([]uuid.UUID, error) {
	c.batches.Add(1)
	c.events.Add(int64(len(batch)))

	return c.inner.Send(ctx, shard, batch)
}

// replayUnflushed sends a stream and never calls Flush. Everything is written
// by the buffer's own size trigger, on the buffer's own goroutine, while later
// requests are still being accepted — which is how every event in production is
// written and is a path a harness with a buffer bigger than its fixture can
// never reach.
func (h *harness) replayUnflushed(t testing.TB, requests []request) {
	t.Helper()

	for i, r := range requests {
		if recorder := h.send(t, r); recorder.Code != http.StatusAccepted {
			t.Fatalf("request %d: status %d, want 202: %s", i, recorder.Code, recorder.Body.String())
		}
	}
}

// drain writes the tail the size trigger left behind. A stream is never an
// exact multiple of the buffer, so the last partial batch is still in memory
// when the final request returns — the ticker takes it in production and
// shutdown takes it on the way out, which is the same call this makes.
//
// One Flush is enough even with a flush already running: Flush waits on the
// same lock the running one holds, and nothing can be added behind it.
func (h *harness) drain(t testing.TB) {
	t.Helper()

	if err := h.service.Buffer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// assertNothingDropped fails if the replay lost an event. Without it a fixture
// that started being rejected — a domain spelling nothing recognises, a payload
// the parser tightened up on — would show as numbers that no longer match,
// which says nothing about why.
func assertNothingDropped(t testing.TB, h *harness) {
	t.Helper()

	for _, count := range h.service.Counters.Snapshot().Dropped {
		t.Errorf("%d events were dropped for site %d as %q", count.Count, count.SiteID, count.Reason)
	}
}

// TestReplayAtProductionBufferBounds replays a stream long enough to fill the
// real write buffer, with the real bounds and no flush the test controls.
//
// It exists because a harness that raises the buffer past the length of its own
// fixture tests a path production never takes: the size trigger never fires, no
// flush ever runs beside a request being accepted, and two of the three ways an
// event reaches disk go untested. The numbers are the same six, scaled by the
// number of people replaying the fixture at once.
func TestReplayAtProductionBufferBounds(t *testing.T) {
	dir := t.TempDir()
	control := newSystem(t, dir)

	// Enough sets that the stream is comfortably longer than the buffer, so the
	// size trigger fires several times rather than once at the very end.
	const sets = 20

	requests := expandedStream(sets)
	if len(requests) <= DefaultBufferSize {
		t.Fatalf("the stream is %d events and the buffer holds %d — it would never flush on size",
			len(requests), DefaultBufferSize)
	}

	var transport *counting

	h := newHarness(t, control, filepath.Join(dir, "production"), func(inner Transport) Transport {
		transport = &counting{inner: inner}
		return transport
	})

	h.replayUnflushed(t, requests)

	sizeTriggered := transport.batches.Load()
	if sizeTriggered == 0 {
		t.Fatal("nothing was written before the final flush, so the size trigger never fired")
	}

	h.drain(t)

	assertMetrics(t, h.metrics(t), expectedFor(sets))
	assertNothingDropped(t, h)

	if got, want := transport.events.Load(), int64(len(requests)); got != want {
		t.Fatalf("the transport was handed %d events, want %d", got, want)
	}
}

// TestSessionSurvivesSaltRotation is the midnight case, end to end. A visitor
// mid-visit at 00:00 UTC gets a new fingerprint, and without the previous-salt
// fallback they would get a new session too and be counted as two people.
func TestSessionSurvivesSaltRotation(t *testing.T) {
	dir := t.TempDir()
	control := newSystem(t, dir)

	h := newHarness(t, control, filepath.Join(dir, "midnight"), nil)

	before := time.Date(2026, time.August, 30, 23, 55, 0, 0, time.UTC)
	after := time.Date(2026, time.August, 31, 0, 5, 0, 0, time.UTC)

	h.replay(t, []request{
		{visitor: 0, timestamp: before.Unix(), name: EventPageview, url: "https://" + fixtureDomain + "/"},
		{visitor: 0, timestamp: after.Unix(), name: EventPageview, url: "https://" + fixtureDomain + "/pricing"},
	})

	// Both salts must be live, or the test would pass for the wrong reason.
	pair, err := h.service.Salts.Pair(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pair.Previous) != salts.Size {
		t.Fatal("there is no previous salt, so the fallback was never exercised")
	}

	m := h.metrics(t)

	if m.Visits != 1 {
		t.Fatalf("visits = %d, want 1 — the salt rotation split the session", m.Visits)
	}
	if m.Visitors != 1 {
		t.Fatalf("visitors = %d, want 1 — the salt rotation split the visitor", m.Visitors)
	}
	if m.Pageviews != 2 {
		t.Fatalf("pageviews = %d, want 2", m.Pageviews)
	}

	rows := h.sessionRows(t)
	if len(rows) != 1 {
		t.Fatalf("stored %d sessions, want 1", len(rows))
	}
	if rows[0].EntryPage != "/" || rows[0].ExitPage != "/pricing" {
		t.Fatalf("entry/exit = %q/%q, want / and /pricing", rows[0].EntryPage, rows[0].ExitPage)
	}
	if rows[0].Duration != int64(after.Sub(before)/time.Second) {
		t.Fatalf("duration = %d, want %d", rows[0].Duration, int64(after.Sub(before)/time.Second))
	}
}

// TestClickIDValueIsNeverStored checks the acquisition parser keeps the
// parameter's name and throws its value away. A click id is a unique
// per-click identifier and is not ours to keep without consent.
func TestClickIDValueIsNeverStored(t *testing.T) {
	dir := t.TempDir()
	control := newSystem(t, dir)

	h := newHarness(t, control, filepath.Join(dir, "clickid"), nil)
	h.replay(t, stream())

	account, err := h.manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	// The fixture sends gclid=SHOULD_NOT_BE_STORED. It must not survive into
	// any dimension table or any detail column.
	for _, table := range []string{"dim_pathname", "dim_source", "dim_utm_source", "dim_utm_campaign", "dim_referrer"} {
		var count int64
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE value LIKE '%%SHOULD_NOT_BE_STORED%%'", table)
		if err := account.Reader().QueryRow(query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s holds the click id value", table)
		}
	}

	var details int64
	if err := account.Reader().QueryRow(
		"SELECT COUNT(*) FROM event_details WHERE COALESCE(full_url,'') LIKE '%SHOULD_NOT_BE_STORED%'",
	).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if details != 0 {
		t.Fatal("the click id value reached event_details")
	}
}

// fixedGeo places an address from a table. The harness ships no geolocation
// database, so without it every visitor has an empty country and an assertion
// about geo would pass by saying nothing.
type fixedGeo map[string]geo.Location

// Lookup answers from the table, and nothing for an address that is not in it.
func (f fixedGeo) Lookup(addr netip.Addr) geo.Location { return f[addr.String()] }

// Close satisfies the locator interface; there is nothing open to release.
func (f fixedGeo) Close() error { return nil }

// TestEveryEventCarriesItsSessionsAcquisition posts three visitors through the
// real handler and asserts the two halves of one guarantee: a visit is
// attributed to the first page it landed on, and every event of that visit
// carries a copy of that attribution.
//
// Without both, a source breakdown counts the second page of a visit as another
// Direct visitor, and three visitors come back as five across four rows with no
// error anywhere — which is the failure mode this product exists to not have.
//
// Every request lands in the same second, which is what a burst of quick clicks
// looks like: the stored timestamp is only accurate to the second, so the fold
// cannot tell these pageviews apart on it alone.
func TestEveryEventCarriesItsSessionsAcquisition(t *testing.T) {
	dir := t.TempDir()
	control := newSystem(t, dir)

	h := newHarness(t, control, filepath.Join(dir, "acquisition"), nil)

	// One country per visitor, so "the event carries its session's country" is
	// a claim that can fail rather than three empty strings agreeing.
	h.service.Pipeline.Geo = fixedGeo{
		visitors[0].ip: {Country: "US", Subdivision1: "US-NY", City: "Syracuse"},
		visitors[1].ip: {Country: "GB", Subdivision1: "GB-ENG", City: "London"},
		visitors[2].ip: {Country: "DE", Subdivision1: "DE-BE", City: "Berlin"},
	}

	at := fixtureStart.Unix()
	site := "https://" + fixtureDomain

	requests := []request{
		{visitor: 0, timestamp: at, name: EventPageview, url: site + "/?utm_source=twitter&utm_medium=social"},
		{visitor: 0, timestamp: at, name: EventPageview, url: site + "/pricing"},
		{visitor: 0, timestamp: at, name: EventPageview, url: site + "/signup"},
		{visitor: 1, timestamp: at, name: EventPageview, url: site + "/", referrer: "https://news.ycombinator.com/item?id=1"},
		{visitor: 1, timestamp: at, name: EventPageview, url: site + "/pricing"},
		{visitor: 2, timestamp: at, name: EventPageview, url: site + "/blog/hello", referrer: "https://www.google.com/"},
	}

	// One flush per request, so every event but the first of a visit is written
	// into a session an earlier transaction already stored — which is where a
	// copy taken at the wrong moment goes stale.
	for _, r := range requests {
		h.replay(t, []request{r})
	}

	// The breakdown a Sources card runs: three visitors, three sources, one
	// visitor each.
	sources := h.sourceVisitors(t)

	want := map[string]int64{"X": 1, "Hacker News": 1, "Google": 1}
	if !reflect.DeepEqual(sources, want) {
		t.Errorf("visit:source gave %v, want %v", sources, want)
	}

	var total int64
	for _, count := range sources {
		total += count
	}
	if total != 3 {
		t.Errorf("visit:source totals %d visitors across %d rows, want 3 across 3", total, len(sources))
	}

	// Every event carries its own session's block. This is the invariant the
	// denormalisation exists for: the numbers on a breakdown add up only if an
	// event and its visit can never disagree.
	rows := h.eventAttribution(t)
	if len(rows) != len(requests) {
		t.Fatalf("stored %d events, want %d", len(rows), len(requests))
	}

	for _, row := range rows {
		if row.Source != row.SessionSource || row.Channel != row.SessionChannel {
			t.Errorf("%s (session %d) is %q/%q, but its visit is %q/%q",
				row.Path, row.Session, row.Source, row.Channel, row.SessionSource, row.SessionChannel)
		}

		if row.Country != row.SessionCountry || row.Browser != row.SessionBrowser {
			t.Errorf("%s (session %d) is %q/%q, but its visit is %q/%q",
				row.Path, row.Session, row.Country, row.Browser, row.SessionCountry, row.SessionBrowser)
		}
	}

	// And the visits themselves are attributed to the page they landed on,
	// which is the half of this the fold owns. They are looked up by country
	// because two of the three entered on the same page.
	byCountry := map[string]sessionRow{}
	for _, row := range h.sessionRows(t) {
		byCountry[row.Country] = row
	}

	for country, source := range map[string]string{"US": "X", "GB": "Hacker News", "DE": "Google"} {
		if byCountry[country].Source != source {
			t.Errorf("the visit from %s is attributed to %q, want %q", country, byCountry[country].Source, source)
		}
	}
}

// sourceVisitors is the visitors-by-visit:source breakdown the dashboard's
// Sources card asks for, written as the statement the query engine compiles for
// that pair: a breakdown with no session-scoped metric reads the events table,
// and visitors is counted on whichever table the query is already reading.
// Answering it from that one table with no join is the whole point of copying
// the block onto every event, so that is where it has to be right.
func (h *harness) sourceVisitors(t testing.TB) map[string]int64 {
	t.Helper()

	account, err := h.manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := account.Reader().Query(`
		SELECT source.value, COUNT(DISTINCT e.user_id)
		FROM events e
		JOIN dim_source source ON source.id = e.source_id
		GROUP BY source.value`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int64{}

	for rows.Next() {
		var (
			source   string
			visitors int64
		)
		if err := rows.Scan(&source, &visitors); err != nil {
			t.Fatal(err)
		}
		out[source] = visitors
	}

	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	return out
}

// eventAttribution is one event beside the visit it belongs to. Reading both
// sides in one row is what makes the assertion the guarantee itself — these two
// must agree — rather than a restatement of the values the test happened to
// send.
type eventAttribution struct {
	Path    string
	Session int64

	Source  string
	Channel string
	Country string
	Browser string

	SessionSource  string
	SessionChannel string
	SessionCountry string
	SessionBrowser string
}

// eventAttribution reads every event joined to its session, in insertion order.
func (h *harness) eventAttribution(t testing.TB) []eventAttribution {
	t.Helper()

	account, err := h.manager.Open(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := account.Reader().Query(`
		SELECT path.value, e.session_id,
		       event_source.value, event_channel.value, event_country.value, event_browser.value,
		       visit_source.value, visit_channel.value, visit_country.value, visit_browser.value
		FROM events e
		JOIN sessions s ON s.id = e.session_id
		JOIN dim_pathname path          ON path.id          = e.pathname_id
		JOIN dim_source   event_source  ON event_source.id  = e.source_id
		JOIN dim_channel  event_channel ON event_channel.id = e.channel_id
		JOIN dim_country  event_country ON event_country.id = e.country_id
		JOIN dim_browser  event_browser ON event_browser.id = e.browser_id
		JOIN dim_source   visit_source  ON visit_source.id  = s.source_id
		JOIN dim_channel  visit_channel ON visit_channel.id = s.channel_id
		JOIN dim_country  visit_country ON visit_country.id = s.country_id
		JOIN dim_browser  visit_browser ON visit_browser.id = s.browser_id
		ORDER BY e.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var out []eventAttribution

	for rows.Next() {
		var row eventAttribution
		if err := rows.Scan(
			&row.Path, &row.Session,
			&row.Source, &row.Channel, &row.Country, &row.Browser,
			&row.SessionSource, &row.SessionChannel, &row.SessionCountry, &row.SessionBrowser,
		); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}

	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	return out
}

// assertMetrics compares the six numbers, allowing for float noise on the three
// that are ratios.
func assertMetrics(t testing.TB, got, want coreMetrics) {
	t.Helper()

	if got.Visitors != want.Visitors {
		t.Errorf("visitors = %d, want %d", got.Visitors, want.Visitors)
	}
	if got.Visits != want.Visits {
		t.Errorf("visits = %d, want %d", got.Visits, want.Visits)
	}
	if got.Pageviews != want.Pageviews {
		t.Errorf("pageviews = %d, want %d", got.Pageviews, want.Pageviews)
	}
	if math.Abs(got.BounceRate-want.BounceRate) > 1e-9 {
		t.Errorf("bounce_rate = %v, want %v", got.BounceRate, want.BounceRate)
	}
	if math.Abs(got.VisitDuration-want.VisitDuration) > 1e-9 {
		t.Errorf("visit_duration = %v, want %v", got.VisitDuration, want.VisitDuration)
	}
	if math.Abs(got.ViewsPerVisit-want.ViewsPerVisit) > 1e-9 {
		t.Errorf("views_per_visit = %v, want %v", got.ViewsPerVisit, want.ViewsPerVisit)
	}
}

// render formats session rows one per line, so a failure shows which visit
// differs rather than one unreadable line.
func render(rows []sessionRow) string {
	var out strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&out, "\n  %+v", row)
	}

	return out.String()
}
