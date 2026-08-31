//
// session_test.go
// One test per accumulation rule, plus the shuffle that proves order does not matter.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

// testUser and testSite are the one visitor and one site these tests use. They
// are constants so a test that meant to change the visitor cannot do it by
// accident.
const (
	testUser int64 = 0x5eed
	testSite int64 = 7
)

// event builds a derived event for the fold. The uuid is derived from the
// timestamp and name rather than random, so that a shuffled replay of the same
// stream carries the same ids — which is what makes the tie-break deterministic
// and the dedupe meaningful.
func event(name string, timestamp int64, path string) Event {
	return Event{
		UUID:        uuid.NewSHA1(uuid.NameSpaceURL, []byte(name+path+itoa(timestamp))),
		AccountID:   1,
		SiteID:      testSite,
		UserID:      testUser,
		Timestamp:   timestamp,
		Name:        name,
		Pathname:    path,
		Hostname:    "example.com",
		Interactive: true,
	}
}

// applyAll folds a stream into a fresh cache and returns the session. Every test
// below is "these events, in this order, produce this row".
func applyAll(t *testing.T, events []Event) *Session {
	t.Helper()

	cache := NewSessionCache()

	var last *Session
	for i := range events {
		copied := events[i]
		session, ok, _ := cache.Apply(&copied)
		if ok {
			last = session
		}
	}

	return last
}

// TestPageviewsCountOnlyPageviews covers the first row of the accumulation
// table: pageviews is +1 only for a pageview.
func TestPageviewsCountOnlyPageviews(t *testing.T) {
	session := applyAll(t, []Event{
		event(EventPageview, 1000, "/"),
		event("signup", 1010, "/"),
		event(EventPageview, 1020, "/pricing"),
		event(EventEngagement, 1030, "/pricing"),
	})

	if session.Pageviews != 2 {
		t.Fatalf("pageviews = %d, want 2", session.Pageviews)
	}
}

// TestEventsCountEverythingExceptEngagement covers the second row. An engagement
// ping only refreshes the end of the visit; counting it would make every session
// look several times busier than it was.
func TestEventsCountEverythingExceptEngagement(t *testing.T) {
	session := applyAll(t, []Event{
		event(EventPageview, 1000, "/"),
		event("signup", 1010, "/"),
		event(EventEngagement, 1020, "/"),
		event(EventEngagement, 1030, "/"),
	})

	if session.Events != 2 {
		t.Fatalf("events = %d, want 2 (the engagement pings must not count)", session.Events)
	}
	if session.LastSeenAt != 1030 {
		t.Fatalf("last_seen_at = %d, want 1030 — engagement still refreshes it", session.LastSeenAt)
	}
}

// TestEntryPageIsTheEarliestPageview covers the third row, and the part of it
// that matters: "keyed on the event's own timestamp, not arrival order". The
// earliest pageview arrives last here.
func TestEntryPageIsTheEarliestPageview(t *testing.T) {
	session := applyAll(t, []Event{
		event(EventPageview, 1100, "/pricing"),
		event(EventPageview, 1200, "/signup"),
		event(EventPageview, 1000, "/"),
	})

	if session.EntryPage != "/" {
		t.Fatalf("entry_page = %q, want %q — the earliest pageview by timestamp", session.EntryPage, "/")
	}
	if session.EntryHostname != "example.com" {
		t.Fatalf("entry_hostname = %q, want example.com", session.EntryHostname)
	}
}

// TestEntryPageIgnoresNonPageviews checks that a custom event arriving first
// does not become the entry page. Only a pageview has a page.
func TestEntryPageIgnoresNonPageviews(t *testing.T) {
	session := applyAll(t, []Event{
		event("signup", 1000, "/hidden"),
		event(EventPageview, 1010, "/"),
	})

	if session.EntryPage != "/" {
		t.Fatalf("entry_page = %q, want /", session.EntryPage)
	}
}

// TestExitPageIsTheLatestPageview covers the fourth row: overwritten by every
// pageview at or after the current end of the visit, and unmoved by one that
// happened earlier.
func TestExitPageIsTheLatestPageview(t *testing.T) {
	session := applyAll(t, []Event{
		event(EventPageview, 1000, "/"),
		event(EventPageview, 1200, "/checkout"),
		event(EventPageview, 1100, "/pricing"),
	})

	if session.ExitPage != "/checkout" {
		t.Fatalf("exit_page = %q, want /checkout — the latest pageview by timestamp", session.ExitPage)
	}
}

// TestDurationIsRecomputedNotAccumulated covers the fifth row. A duplicated or
// replayed event must not extend the visit, which is only true if the duration
// is derived from the two ends rather than added up.
func TestDurationIsRecomputedNotAccumulated(t *testing.T) {
	once := applyAll(t, []Event{
		event(EventPageview, 1000, "/"),
		event(EventPageview, 1090, "/pricing"),
	})

	twice := applyAll(t, []Event{
		event(EventPageview, 1000, "/"),
		event(EventPageview, 1090, "/pricing"),
		event(EventPageview, 1090, "/pricing"),
	})

	if once.Duration() != 90 {
		t.Fatalf("duration = %d, want 90", once.Duration())
	}
	if twice.Duration() != once.Duration() {
		t.Fatalf("a repeated event changed the duration: %d vs %d", twice.Duration(), once.Duration())
	}
}

// TestDurationSurvivesAnEventArrivingLate checks the case retries actually
// produce: an event from before the session's start turning up afterwards.
func TestDurationSurvivesAnEventArrivingLate(t *testing.T) {
	session := applyAll(t, []Event{
		event(EventPageview, 1100, "/pricing"),
		event(EventPageview, 1000, "/"),
	})

	if session.StartedAt != 1000 {
		t.Fatalf("started_at = %d, want 1000", session.StartedAt)
	}
	if session.Duration() != 100 {
		t.Fatalf("duration = %d, want 100", session.Duration())
	}
}

// TestBounceRules covers the sixth row in full: it starts true, becomes false on
// a second pageview or on an interactive non-pageview event, and once false is
// never true again.
func TestBounceRules(t *testing.T) {
	cases := []struct {
		name   string
		events []Event
		want   bool
	}{
		{
			name:   "one pageview bounces",
			events: []Event{event(EventPageview, 1000, "/")},
			want:   true,
		},
		{
			name: "two pageviews do not",
			events: []Event{
				event(EventPageview, 1000, "/"),
				event(EventPageview, 1010, "/pricing"),
			},
			want: false,
		},
		{
			name: "an interactive non-pageview ends the bounce on its own",
			events: []Event{
				event(EventPageview, 1000, "/"),
				event("signup", 1010, "/"),
			},
			want: false,
		},
		{
			name: "engagement pings alone do not end a bounce",
			events: []Event{
				event(EventPageview, 1000, "/"),
				event(EventEngagement, 1010, "/"),
				event(EventEngagement, 1020, "/"),
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := applyAll(t, tc.events).IsBounce(); got != tc.want {
				t.Fatalf("is_bounce = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNonInteractiveFirstEventStillBounces checks the initial-state formula:
// is_bounce starts as (name == pageview || !interactive), so a non-interactive
// custom event opening a session is still a bounce.
func TestNonInteractiveFirstEventStillBounces(t *testing.T) {
	first := event("scroll", 1000, "/")
	first.Interactive = false

	if !applyAll(t, []Event{first}).IsBounce() {
		t.Fatal("a lone non-interactive event should still be a bounce")
	}
}

// TestInteractiveFirstEventDoesNotBounce is the other half of the same formula.
func TestInteractiveFirstEventDoesNotBounce(t *testing.T) {
	if applyAll(t, []Event{event("signup", 1000, "/")}).IsBounce() {
		t.Fatal("a lone interactive non-pageview event should not be a bounce")
	}
}

// TestBounceNeverReturns checks "once false, never true again" against the
// ordering that would break a naive implementation — the second pageview
// arriving before the first.
func TestBounceNeverReturns(t *testing.T) {
	session := applyAll(t, []Event{
		event(EventPageview, 1100, "/pricing"),
		event("signup", 1150, "/pricing"),
		event(EventPageview, 1000, "/"),
	})

	if session.IsBounce() {
		t.Fatal("is_bounce came back to true after an earlier event arrived late")
	}
}

// TestAttributionIsFrozenAtSessionStart is the rule that generates the most
// support questions: a UTM tag on the second pageview of a visit is discarded.
// The later event carries a different source and must not overwrite the first.
func TestAttributionIsFrozenAtSessionStart(t *testing.T) {
	first := event(EventPageview, 1000, "/")
	first.Source = "Google"
	first.Channel = "Organic Search"
	first.UTMCampaign = "spring"

	second := event(EventPageview, 1100, "/pricing")
	second.Source = "Facebook"
	second.Channel = "Paid Social"
	second.UTMCampaign = "summer"

	// The second event arrives first, so this also proves the freeze is by
	// timestamp rather than by arrival.
	session := applyAll(t, []Event{second, first})

	if session.Source != "Google" || session.Channel != "Organic Search" || session.UTMCampaign != "spring" {
		t.Fatalf("attribution = %q/%q/%q, want Google/Organic Search/spring",
			session.Source, session.Channel, session.UTMCampaign)
	}
}

// TestNewSessionAfterTheTimeout checks the lookup rule: a gap of more than
// thirty minutes starts a new visit.
func TestNewSessionAfterTheTimeout(t *testing.T) {
	cache := NewSessionCache()

	first := event(EventPageview, 1000, "/")
	firstSession, _, _ := cache.Apply(&first)

	// One second past the window.
	late := event(EventPageview, 1000+sessionTimeoutSeconds+1, "/pricing")
	lateSession, _, _ := cache.Apply(&late)

	if firstSession.ID == lateSession.ID {
		t.Fatal("an event past the 30-minute gap joined the previous session")
	}

	// Exactly at the boundary is inside the window, checked on its own cache so
	// the session created above cannot claim it first.
	boundary := NewSessionCache()

	opening := event(EventPageview, 1000, "/")
	openingSession, _, _ := boundary.Apply(&opening)

	edge := event(EventPageview, 1000+sessionTimeoutSeconds, "/pricing")
	edgeSession, _, _ := boundary.Apply(&edge)

	if edgeSession.ID != openingSession.ID {
		t.Fatal("an event exactly at the 30-minute boundary started a new session")
	}
}

// TestEngagementWithNoSessionIsDropped covers the one event kind that cannot
// create a session. It carries no page of its own, so inventing a visit for it
// would produce a session with no entry page and inflate the visit count.
func TestEngagementWithNoSessionIsDropped(t *testing.T) {
	cache := NewSessionCache()

	ping := event(EventEngagement, 1000, "/")
	if _, ok, _ := cache.Apply(&ping); ok {
		t.Fatal("an engagement ping with no live session created one")
	}

	if cache.Len() != 0 {
		t.Fatalf("cache holds %d sessions, want 0", cache.Len())
	}

	// It is parked rather than thrown away, so a retry that delivered it ahead
	// of its own pageview still produces the right time-on-page.
	view := event(EventPageview, 1010, "/")
	session, ok, revived := cache.Apply(&view)
	if !ok {
		t.Fatal("the pageview was dropped")
	}
	if len(revived) != 1 {
		t.Fatalf("revived %d parked pings, want 1", len(revived))
	}
	if session.LastSeenAt != 1010 || session.StartedAt != 1000 {
		t.Fatalf("session span = %d..%d, want 1000..1010", session.StartedAt, session.LastSeenAt)
	}
	if session.Events != 1 {
		t.Fatalf("events = %d, want 1 — a revived ping still does not count", session.Events)
	}
}

// TestOrphanedEngagementExpires checks a ping whose visit never turned up is a
// genuine drop rather than something the cache holds for the life of the
// process.
func TestOrphanedEngagementExpires(t *testing.T) {
	cache := NewSessionCache()

	ping := event(EventEngagement, 1000, "/")
	cache.Apply(&ping)

	if removed := cache.Sweep(1000 + sessionTimeoutSeconds + 1); removed != 1 {
		t.Fatalf("swept %d orphaned pings, want 1", removed)
	}

	view := event(EventPageview, 1000+sessionTimeoutSeconds+2, "/")
	if _, _, revived := cache.Apply(&view); len(revived) != 0 {
		t.Fatalf("revived %d expired pings, want 0", len(revived))
	}
}

// TestPreviousSaltFindsTheSession is the midnight case. A visitor mid-visit at
// 00:00 UTC gets a new fingerprint; without the previous-salt fallback they
// would get a new session too and be counted as two people.
func TestPreviousSaltFindsTheSession(t *testing.T) {
	cache := NewSessionCache()

	before := event(EventPageview, 1000, "/")
	original, _, _ := cache.Apply(&before)

	// After rotation the current fingerprint is new and the previous one is
	// what the session was created under.
	after := event(EventPageview, 1100, "/pricing")
	after.UserID = 0xd1ff
	after.PreviousUserID = testUser

	found, ok, _ := cache.Apply(&after)
	if !ok {
		t.Fatal("the event was dropped")
	}
	if found.ID != original.ID {
		t.Fatal("a salt rotation mid-session split the visit in two")
	}

	// The session's identity wins, and is copied onto the event, so every row
	// in the visit carries one visitor id rather than two.
	if after.UserID != testUser {
		t.Fatalf("event user_id = %d, want the session's %d", after.UserID, testUser)
	}
	if cache.Len() != 1 {
		t.Fatalf("cache holds %d sessions, want 1", cache.Len())
	}
}

// TestOutOfOrderEventMergesBridgedSessions is the repair. An event that arrives
// last but happened in the gap between two sessions proves they were one visit
// all along, and the fold has to say so.
func TestOutOfOrderEventMergesBridgedSessions(t *testing.T) {
	cache := NewSessionCache()

	// Two events far enough apart to be separate visits when seen in this order.
	late := event(EventPageview, 4000, "/checkout")
	early := event(EventPageview, 1000, "/")

	if _, ok, _ := cache.Apply(&late); !ok {
		t.Fatal("the late event was dropped")
	}
	if _, ok, _ := cache.Apply(&early); !ok {
		t.Fatal("the early event was dropped")
	}
	if cache.Len() != 2 {
		t.Fatalf("expected two sessions before the bridge, got %d", cache.Len())
	}

	// The bridging event sits within thirty minutes of both.
	bridge := event(EventPageview, 2500, "/pricing")
	session, ok, _ := cache.Apply(&bridge)
	if !ok {
		t.Fatal("the bridging event was dropped")
	}

	if cache.Len() != 1 {
		t.Fatalf("the bridge left %d sessions, want 1", cache.Len())
	}
	if session.Pageviews != 3 {
		t.Fatalf("merged pageviews = %d, want 3", session.Pageviews)
	}
	if session.StartedAt != 1000 || session.LastSeenAt != 4000 {
		t.Fatalf("merged span = %d..%d, want 1000..4000", session.StartedAt, session.LastSeenAt)
	}
	if session.EntryPage != "/" || session.ExitPage != "/checkout" {
		t.Fatalf("merged entry/exit = %q/%q, want / and /checkout", session.EntryPage, session.ExitPage)
	}

	merges := cache.TakeMerges(1)
	if len(merges) != 1 {
		t.Fatalf("queued %d merges for the writer, want 1", len(merges))
	}
	if merges[0].Survivor != session.ID {
		t.Fatalf("merge survivor = %d, want %d", merges[0].Survivor, session.ID)
	}
}

// TestShuffledStreamProducesAnIdenticalRow is the property the whole design is
// built around. Retries reorder events relative to fresh traffic, so a stream
// folded in any order has to produce byte-identical session state — otherwise
// the delivery buffer quietly changes the numbers.
func TestShuffledStreamProducesAnIdenticalRow(t *testing.T) {
	stream := []Event{
		event(EventPageview, 1000, "/"),
		event(EventEngagement, 1015, "/"),
		event(EventPageview, 1030, "/pricing"),
		event("signup", 1042, "/pricing"),
		event(EventEngagement, 1050, "/pricing"),
		event(EventPageview, 1075, "/checkout"),
		event(EventPageview, 1120, "/thanks"),
		event(EventEngagement, 1140, "/thanks"),
	}

	inOrder := normalise(applyAll(t, stream))

	random := rand.New(rand.NewSource(20260830))

	for attempt := 0; attempt < 200; attempt++ {
		shuffled := append([]Event(nil), stream...)
		random.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})

		got := normalise(applyAll(t, shuffled))

		if !reflect.DeepEqual(got, inOrder) {
			t.Fatalf("shuffle %d produced a different session\n got: %+v\nwant: %+v", attempt, got, inOrder)
		}
	}
}

// normalise strips the fields that legitimately differ between two runs — the
// allocated id and the dirty flag — so the comparison is about the accumulated
// facts and nothing else.
func normalise(session *Session) Session {
	copied := *session
	copied.ID = 0
	copied.Dirty = false

	return copied
}

// TestSweepDropsExpiredSessions checks the cache does not grow for the life of
// the process. An abandoned session is never touched again by definition, so
// nothing else would ever notice it.
func TestSweepDropsExpiredSessions(t *testing.T) {
	cache := NewSessionCache()

	first := event(EventPageview, 1000, "/")
	cache.Apply(&first)

	// A dirty session is kept whatever its age: dropping it would lose the last
	// events of a visit that has not been written yet.
	if removed := cache.Sweep(1000 + sessionTimeoutSeconds + 60); removed != 0 {
		t.Fatalf("swept %d dirty sessions, want 0", removed)
	}

	cache.TakeDirty(1)

	if removed := cache.Sweep(1000 + sessionTimeoutSeconds + 60); removed != 1 {
		t.Fatalf("swept %d sessions, want 1", removed)
	}
	if cache.Len() != 0 {
		t.Fatalf("cache holds %d sessions after the sweep, want 0", cache.Len())
	}
}

// TestSnapshotRoundTrip covers the shutdown path: the live cache has to come
// back, or a restart splits every in-flight session in two.
func TestSnapshotRoundTrip(t *testing.T) {
	cache := NewSessionCache()

	first := event(EventPageview, 1000, "/")
	second := event(EventPageview, 1030, "/pricing")
	cache.Apply(&first)
	cache.Apply(&second)

	snapshot := cache.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("snapshot holds %d sessions, want 1", len(snapshot))
	}

	restored := NewSessionCache()
	if count := restored.Restore(snapshot, 1040); count != 1 {
		t.Fatalf("restored %d sessions, want 1", count)
	}

	// A third event must join the restored session rather than start a new one.
	third := event(EventPageview, 1060, "/checkout")
	session, _, _ := restored.Apply(&third)

	if session.Pageviews != 3 {
		t.Fatalf("pageviews after restore = %d, want 3", session.Pageviews)
	}
	if session.EntryPage != "/" {
		t.Fatalf("entry_page after restore = %q, want /", session.EntryPage)
	}
}

// TestRestoreSkipsExpiredSessions checks a process that was down for an hour
// does not resurrect visits that ended while it was away.
func TestRestoreSkipsExpiredSessions(t *testing.T) {
	cache := NewSessionCache()

	first := event(EventPageview, 1000, "/")
	cache.Apply(&first)

	restored := NewSessionCache()
	if count := restored.Restore(cache.Snapshot(), 1000+sessionTimeoutSeconds+1); count != 0 {
		t.Fatalf("restored %d expired sessions, want 0", count)
	}
}
