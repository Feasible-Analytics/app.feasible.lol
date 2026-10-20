//
// swarm_test.go
// Tests for catching one crawler that loads each page from a different address.
//
// Created: 2026-10-20
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// swarmEvent builds one event from a crawler that sends each page from a new
// address: a different visitor every time, with the same device and place.
func swarmEvent(visitor int, name string, timestamp int64, path string) Event {
	e := writerEvent(1, name, timestamp, path)
	e.UUID = uuid.NewSHA1(uuid.NameSpaceURL, []byte(strconv.Itoa(visitor)+name+path+strconv.FormatInt(timestamp, 10)))
	e.UserID = testUser + 1000 + int64(visitor)
	e.Source = ""
	e.Channel = "Direct"
	e.ScreenSize = ScreenLaptop
	e.DeviceType = "Desktop"
	e.OS = "GNU/Linux"
	e.BrowserVersion = "148"
	e.Language = "en-US"

	return e
}

// crawlerPages is the walk the crawler took: one page per address, seconds
// apart.
var crawlerPages = []struct {
	offset int64
	path   string
}{
	{0, "/blog/a-post/"},
	{13, "/blog/another-post/"},
	{17, "/blog/a-third-post/"},
	{21, "/blog/"},
	{23, "/blog/a-fourth-post/"},
	{70, "/"},
	{95, "/projects/"},
}

// writeOneByOne writes each event in its own batch, the way they arrive.
func writeOneByOne(t *testing.T, writer *Writer, events []Event) {
	t.Helper()

	for _, e := range events {
		if _, err := writer.Write(context.Background(), []Event{e}); err != nil {
			t.Fatal(err)
		}
	}
}

// crawlerWalk returns the crawler's pageviews from a start time.
func crawlerWalk(base int64) []Event {
	events := make([]Event, 0, len(crawlerPages))
	for i, page := range crawlerPages {
		events = append(events, swarmEvent(i, EventPageview, base+page.offset, page.path))
	}

	return events
}

// TestCrawlerSwarmIsMarked replays the crawler page by page. Two pages are not
// enough to call it, and the third marks every visit, the earlier ones too.
func TestCrawlerSwarmIsMarked(t *testing.T) {
	writer, manager := newWriter(t)
	walk := crawlerWalk(fixtureStart.Unix())

	writeOneByOne(t, writer, walk[:2])
	if got := reasonRows(t, manager, ReasonCrawlerSwarm); got != 0 {
		t.Fatalf("%d events carry %s after two pages, want 0", got, ReasonCrawlerSwarm)
	}

	writeOneByOne(t, writer, walk[2:])
	if got := reasonRows(t, manager, ReasonCrawlerSwarm); got != int64(len(walk)) {
		t.Fatalf("%d events carry %s, want %d", got, ReasonCrawlerSwarm, len(walk))
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM session_sampling WHERE is_bot = 1"); got != int64(len(walk)) {
		t.Fatalf("%d sampled sessions are marked automated, want %d", got, len(walk))
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events"); got != int64(len(walk)) {
		t.Fatalf("events table holds %d rows, want %d — classification must not delete", got, len(walk))
	}
}

// TestCrawlerSwarmIgnoresArrivalOrder writes the same walk shuffled into random
// batches. The verdict depends on the visits, not the order they came in.
func TestCrawlerSwarmIgnoresArrivalOrder(t *testing.T) {
	random := rand.New(rand.NewSource(7))

	for round := 0; round < 10; round++ {
		writer, manager := newWriter(t)

		walk := crawlerWalk(fixtureStart.Unix())
		random.Shuffle(len(walk), func(i, j int) { walk[i], walk[j] = walk[j], walk[i] })

		for len(walk) > 0 {
			n := 1 + random.Intn(len(walk))
			if _, err := writer.Write(context.Background(), walk[:n]); err != nil {
				t.Fatal(err)
			}
			walk = walk[n:]
		}

		if got := reasonRows(t, manager, ReasonCrawlerSwarm); got != int64(len(crawlerPages)) {
			t.Fatalf("round %d: %d events carry %s, want %d", round, got, ReasonCrawlerSwarm, len(crawlerPages))
		}
	}
}

// TestCrawlerSwarmIsTakenBackWhenVisitsAreRead covers people who only looked
// like a swarm until they read. A ping clears its own visit, and once too few
// unread pages are left the rest are cleared with it.
func TestCrawlerSwarmIsTakenBackWhenVisitsAreRead(t *testing.T) {
	writer, manager := newWriter(t)
	walk := crawlerWalk(fixtureStart.Unix())[:4]

	writeOneByOne(t, writer, walk)
	if got := reasonRows(t, manager, ReasonCrawlerSwarm); got != 4 {
		t.Fatalf("%d events carry %s, want 4", got, ReasonCrawlerSwarm)
	}

	// Three unread pages remain, so only the visit that was read leaves.
	writeOneByOne(t, writer, []Event{swarmEvent(0, EventEngagement, walk[0].Timestamp+30, walk[0].Pathname)})

	if got := reasonRows(t, manager, ReasonCrawlerSwarm); got != 3 {
		t.Fatalf("%d events carry %s after one visit was read, want 3", got, ReasonCrawlerSwarm)
	}
	if got := countRows(t, manager, 1,
		"SELECT COUNT(*) FROM events WHERE bot_reason_id <> 0 AND user_id = "+strconv.FormatInt(walk[0].UserID, 10),
	); got != 0 {
		t.Fatalf("%d events of the visit that was read still carry a bot reason, want 0", got)
	}

	// Two unread pages are not a swarm, so the rest are cleared too.
	writeOneByOne(t, writer, []Event{swarmEvent(1, EventEngagement, walk[1].Timestamp+30, walk[1].Pathname)})

	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events WHERE bot_reason_id <> 0"); got != 0 {
		t.Fatalf("%d events still carry a bot reason after a visit was read, want 0", got)
	}
	if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM session_sampling WHERE is_bot = 1"); got != 0 {
		t.Fatalf("%d sampled sessions are still marked automated, want 0", got)
	}
}

// TestCrawlerSwarmLeavesOtherVisitsAlone shows what keeps real people out: a
// crowd on one shared link, visits that came from somewhere, visits spread
// over minutes, and visits from different devices.
func TestCrawlerSwarmLeavesOtherVisitsAlone(t *testing.T) {
	base := fixtureStart.Unix()

	for name, events := range map[string][]Event{
		"one landing page": func() []Event {
			var events []Event
			for i := 0; i < 5; i++ {
				events = append(events, swarmEvent(i, EventPageview, base+int64(i)*5, "/blog/a-post/"))
			}
			return events
		}(),
		"referred": func() []Event {
			events := crawlerWalk(base)
			for i := range events {
				events[i].Source = "Hacker News"
				events[i].Channel = "Social"
			}
			return events
		}(),
		"minutes apart": {
			swarmEvent(0, EventPageview, base, "/a/"),
			swarmEvent(1, EventPageview, base+swarmWindowSeconds+1, "/b/"),
			swarmEvent(2, EventPageview, base+2*swarmWindowSeconds+2, "/c/"),
		},
		"different devices": func() []Event {
			events := crawlerWalk(base)
			for i := range events {
				events[i].BrowserVersion = strconv.Itoa(140 + i)
			}
			return events
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			writer, manager := newWriter(t)
			writeOneByOne(t, writer, events)

			if got := countRows(t, manager, 1, "SELECT COUNT(*) FROM events WHERE bot_reason_id <> 0"); got != 0 {
				t.Fatalf("%d events carry a bot reason, want 0", got)
			}
		})
	}
}

// TestCrawlerSwarmKeepsAnEventReason checks that a visit already flagged for
// its own reason keeps it, and still counts toward the swarm around it.
func TestCrawlerSwarmKeepsAnEventReason(t *testing.T) {
	writer, manager := newWriter(t)

	walk := crawlerWalk(fixtureStart.Unix())[:3]
	walk[0].BotReason = ReasonDatacenterIP

	writeOneByOne(t, writer, walk)

	if got := reasonRows(t, manager, ReasonDatacenterIP); got != 1 {
		t.Fatalf("%d events carry %s, want 1 — the swarm overwrote it", got, ReasonDatacenterIP)
	}
	if got := reasonRows(t, manager, ReasonCrawlerSwarm); got != 2 {
		t.Fatalf("%d events carry %s, want 2", got, ReasonCrawlerSwarm)
	}
}

// TestCrawlerSwarmReadSeeksItsWindow pins the read to the session time index,
// so a busy site pays for a few minutes of visits rather than all of them.
func TestCrawlerSwarmReadSeeksItsWindow(t *testing.T) {
	plan := queryPlan(t, context.Background(), planDatabase(t), selectSwarmMembers)

	if !strings.Contains(plan, "SEARCH s USING INDEX sessions_range (site_id=? AND started_at>? AND started_at<?)") {
		t.Fatalf("plan is\n%s\nwant it to search sessions_range", plan)
	}
}
