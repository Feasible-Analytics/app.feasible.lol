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
	"time"

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

// TestAttributionSurvivesASharedSecond is the other half of the freeze. A
// stored timestamp is only accurate to the second, so the pageviews of a quick
// visit routinely share one, and settling that on the event uuid alone hands
// the visit to whichever page drew the lower id: a coin toss that leaves the
// same visit Direct on one site and attributed on the next, with nothing anyone
// can point at.
func TestAttributionSurvivesASharedSecond(t *testing.T) {
	landing := event(EventPageview, 1000, "/")
	landing.DerivedAt = 1
	landing.Source = "Hacker News"
	landing.Channel = "Organic Social"

	pricing := event(EventPageview, 1000, "/pricing")
	pricing.DerivedAt = 2

	signup := event(EventPageview, 1000, "/signup")
	signup.DerivedAt = 3

	// Every arrival order has to give the same answer, because the order the
	// ingest tier saw them in travels on the events themselves.
	for _, arrivals := range [][]Event{
		{landing, pricing, signup},
		{signup, pricing, landing},
		{pricing, landing, signup},
	} {
		session := applyAll(t, arrivals)

		if session.Source != "Hacker News" || session.Channel != "Organic Social" {
			t.Fatalf("attribution = %q/%q, want Hacker News/Organic Social", session.Source, session.Channel)
		}
		if session.EntryPage != "/" {
			t.Fatalf("entry_page = %q, want / — the first of the three", session.EntryPage)
		}
	}
}

// TestAttributionPrefersAPageviewOverACustomEvent covers the precedence between
// the two kinds of first event. Only a page arrives from somewhere: a
// conversion reported a moment before the page it happened on carries no
// referrer of its own, and letting it win makes the visit Direct.
func TestAttributionPrefersAPageviewOverACustomEvent(t *testing.T) {
	conversion := event("signup", 999, "/pricing")

	landing := event(EventPageview, 1000, "/")
	landing.Source = "Google"
	landing.Channel = "Organic Search"

	session := applyAll(t, []Event{conversion, landing})

	if session.Source != "Google" || session.Channel != "Organic Search" {
		t.Fatalf("attribution = %q/%q, want Google/Organic Search", session.Source, session.Channel)
	}
}

// TestAVisitWithNoPageviewKeepsItsFirstEvent checks the fallback. A visit made
// entirely of custom events is unusual but real — a server-side integration
// reporting conversions — and leaving it unattributed would be worse than
// attributing it to the event that opened it.
func TestAVisitWithNoPageviewKeepsItsFirstEvent(t *testing.T) {
	first := event("signup", 1000, "/pricing")
	first.Source = "Google"
	first.Channel = "Organic Search"

	second := event("upgrade", 1100, "/pricing")

	session := applyAll(t, []Event{second, first})

	if session.Source != "Google" || session.Channel != "Organic Search" {
		t.Fatalf("attribution = %q/%q, want Google/Organic Search", session.Source, session.Channel)
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
// allocated id, and the two flags that say what the writer still owes the
// database rather than what the visit was. A shuffled stream reaches the same
// row by a different route, and a late event that turns out to have started the
// visit leaves rows on disk to rewrite where an in-order one never wrote them.
func normalise(session *Session) Session {
	copied := *session
	copied.ID = 0
	copied.Dirty = false
	copied.Restamp = false

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

// TestPreviousSaltAdoptsFromItsOwnShard is the midnight case for parked pings.
// A ping parked under yesterday's fingerprint lives in that key's own bucket,
// which is a different shard sixty-three times in sixty-four — so adopting it
// from the current key's bucket silently drops every engagement ping in flight
// across 00:00 UTC.
func TestPreviousSaltAdoptsFromItsOwnShard(t *testing.T) {
	cache := NewSessionCache()

	// A rotated fingerprint that lands in a different bucket, which is what
	// makes the bug visible rather than a coincidence.
	rotated := testUser + 1
	for cache.bucket(sessionKey{testSite, rotated}) == cache.bucket(sessionKey{testSite, testUser}) {
		rotated++
	}

	// A ping too early for any session parks under yesterday's key.
	ping := event(EventEngagement, 2000, "/")
	if _, ok, _ := cache.Apply(&ping); ok {
		t.Fatal("an engagement ping with no session created one")
	}

	// A visit under the same key, far enough away that the ping stays parked.
	opening := event(EventPageview, 5000, "/")
	if _, ok, revived := cache.Apply(&opening); !ok || len(revived) != 0 {
		t.Fatalf("the ping was adopted too early: ok=%v revived=%d", ok, len(revived))
	}

	// After rotation the same visitor arrives under a new fingerprint, with an
	// earlier timestamp that widens the session back over the parked ping.
	after := event(EventPageview, 3300, "/pricing")
	after.UserID = rotated
	after.PreviousUserID = testUser

	session, ok, revived := cache.Apply(&after)
	if !ok {
		t.Fatal("the event was dropped")
	}
	if len(revived) != 1 {
		t.Fatalf("revived %d parked pings, want 1 — the ping is parked in the previous key's bucket", len(revived))
	}
	if session.StartedAt != 2000 {
		t.Fatalf("session started at %d, want 2000 — the adopted ping did not reach the fold", session.StartedAt)
	}
}

// TestExpiredOrphanIsReported checks the one drop nobody can be told about at
// request time is still told about. By the time a ping's visit is known never to
// have arrived the response was sent half an hour ago, so the counter is the
// only place the customer ever hears about it.
func TestExpiredOrphanIsReported(t *testing.T) {
	cache := NewSessionCache()

	var expired []*Event
	cache.OnOrphanExpired = func(event *Event) { expired = append(expired, event) }

	ping := event(EventEngagement, 1000, "/")
	cache.Apply(&ping)

	// Still inside the window: the visit could yet arrive.
	cache.Sweep(1000 + sessionTimeoutSeconds)
	if len(expired) != 0 {
		t.Fatalf("reported %d drops for a ping that can still be adopted", len(expired))
	}

	cache.Sweep(1000 + sessionTimeoutSeconds + 1)

	if len(expired) != 1 {
		t.Fatalf("reported %d expired pings, want 1", len(expired))
	}
	if expired[0].SiteID != testSite {
		t.Fatalf("the drop was reported against site %d, want %d — a count nobody can attribute is not visibility",
			expired[0].SiteID, testSite)
	}
}

// TestAbandonedDirtySessionIsEvicted checks a wedged writer costs a session
// rather than the process. A dirty session is held past the timeout because its
// last events have not been written, but holding it forever turns one stuck
// shard into a cache that grows until the box runs out of memory.
func TestAbandonedDirtySessionIsEvicted(t *testing.T) {
	cache := NewSessionCache()

	var abandoned []*Session
	cache.OnSessionAbandoned = func(session *Session) { abandoned = append(abandoned, session) }

	first := event(EventPageview, 1000, "/")
	cache.Apply(&first)

	// Inside the grace period the session stays: the write may still land.
	if removed := cache.Sweep(1000 + sessionTimeoutSeconds + 60); removed != 0 {
		t.Fatalf("swept %d dirty sessions inside the grace period, want 0", removed)
	}

	past := int64(1000) + sessionTimeoutSeconds + int64(DirtyGrace/time.Second) + 1
	if removed := cache.Sweep(past); removed != 1 {
		t.Fatalf("swept %d sessions past the grace period, want 1", removed)
	}
	if cache.Len() != 0 {
		t.Fatalf("cache holds %d sessions, want 0 — a wedged writer would grow it without bound", cache.Len())
	}
	if len(abandoned) != 1 {
		t.Fatalf("reported %d abandoned sessions, want 1 — losing rows silently is the thing we do not do", len(abandoned))
	}
}

// TestRedirtyStaysWithinItsAccount checks a failed write for one customer does
// not rewrite another's rows. Session ids are allocated per account, so id 7
// exists in every account database and matching on the id alone dirties
// unrelated accounts on every rollback.
func TestRedirtyStaysWithinItsAccount(t *testing.T) {
	cache := NewSessionCache()

	mine := event(EventPageview, 1000, "/")
	mine.AccountID = 1
	mineSession, _, _ := cache.Apply(&mine)

	theirs := event(EventPageview, 1000, "/")
	theirs.AccountID = 2
	theirs.SiteID = testSite + 1
	theirs.UserID = testUser + 1
	theirsSession, _, _ := cache.Apply(&theirs)

	// Both are written and clean, and both happen to hold the same id — which
	// is the normal state of two accounts, not a contrived one.
	cache.TakeDirty(1)
	cache.TakeDirty(2)
	theirsSession.ID = mineSession.ID

	cache.Redirty(1, []*Session{{ID: mineSession.ID, AccountID: 1}})

	if len(cache.TakeDirty(1)) != 1 {
		t.Fatal("the failed account's session was not put back in its dirty set")
	}
	if got := len(cache.TakeDirty(2)); got != 0 {
		t.Fatalf("dirtied %d sessions in an unrelated account, want 0", got)
	}
}

// TestFoldIsOrderIndependentUnderMerges permutes every arrival order of streams
// whose gaps force the out-of-order merge path. Retries reorder events against
// fresh traffic, so a stream folded in any order has to produce the same row —
// and the merge path is where that is hardest and least exercised.
func TestFoldIsOrderIndependentUnderMerges(t *testing.T) {
	type arrival struct {
		timestamp   int64
		name        string
		path        string
		interactive bool
	}

	streams := [][]arrival{
		{{0, EventPageview, "/a", true}, {1800, EventPageview, "/b", true}, {3600, EventPageview, "/c", true}},
		{{0, EventPageview, "/a", true}, {1800, EventEngagement, "", false}, {3600, EventPageview, "/c", true}},
		{{0, EventPageview, "/a", true}, {1799, "signup", "", true}, {3598, EventPageview, "/c", true}},
		{{0, EventPageview, "/a", true}, {1800, EventPageview, "/b", true}, {3600, EventEngagement, "", false}, {5400, EventPageview, "/d", true}},
		{{0, EventPageview, "/a", true}, {900, EventPageview, "/b", true}, {2600, EventPageview, "/c", true}, {4300, EventPageview, "/d", true}},
	}

	// folded is the part of the cache a stored row is built from, flattened so
	// two orderings can be compared as one value.
	type folded struct {
		started, lastSeen, pageviews, events int64
		bounce                               bool
		entry, exit, source                  string
		sessions                             int
	}

	for n, arrivals := range streams {
		var want folded
		first := true

		order := make([]int, len(arrivals))
		for i := range order {
			order[i] = i
		}

		var permute func(k int)
		permute = func(k int) {
			if k == len(order) {
				cache := NewSessionCache()

				for _, i := range order {
					a := arrivals[i]
					cache.Apply(&Event{
						UUID:      uuid.NewSHA1(uuid.NameSpaceURL, []byte(itoa(int64(i)))),
						AccountID: 1, SiteID: testSite, UserID: testUser,
						Timestamp: a.timestamp, Name: a.name, Pathname: a.path,
						Interactive: a.interactive, Source: a.path,
					})
				}

				got := folded{}
				for _, session := range cache.Snapshot() {
					got.sessions++
					if got.started == 0 || session.StartedAt < got.started {
						got.started = session.StartedAt
					}
					if session.LastSeenAt > got.lastSeen {
						got.lastSeen = session.LastSeenAt
					}
					got.pageviews += session.Pageviews
					got.events += session.Events
					got.bounce = got.bounce || session.IsBounce()
					got.entry, got.exit, got.source = session.EntryPage, session.ExitPage, session.Source
				}

				if first {
					want, first = got, false
					return
				}
				if got != want {
					t.Fatalf("stream %d: order %v folded to %+v, want %+v", n, order, got, want)
				}

				return
			}

			for i := k; i < len(order); i++ {
				order[k], order[i] = order[i], order[k]
				permute(k + 1)
				order[k], order[i] = order[i], order[k]
			}
		}

		permute(0)
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
