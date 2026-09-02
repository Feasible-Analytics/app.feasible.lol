//
// session.go
// The session fold: one row per visit, updated in place, independent of arrival order.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"time"
)

// SessionTimeout is the inactivity gap that ends a visit. Thirty minutes is the
// industry convention and changing it would make every historical number
// incomparable, so it is a constant rather than a setting.
const SessionTimeout = 30 * time.Minute

// sessionTimeoutSeconds is the same value in the unit every timestamp uses.
const sessionTimeoutSeconds = int64(SessionTimeout / time.Second)

// Session is one visit. The writer loads it from the account database, folds a
// batch into it, and writes it back as a single row that is updated in place.
// A column store that cannot UPDATE has to fake this with a sign column and a
// collapsing merge, which turns every average into a ratio of signed sums; we
// just update the row.
//
// Several fields exist purely to make the fold independent of arrival order.
// They record *when* a fact was established rather than the order it was
// learned in, because retries reorder events relative to fresh traffic and the
// stored row has to be the same either way.
type Session struct {
	ID        int64
	AccountID int64
	SiteID    int64
	UserID    int64

	StartedAt  int64
	LastSeenAt int64

	Pageviews int64
	Events    int64

	EntryPage     string
	EntryHostname string
	ExitPage      string
	ExitHostname  string
	EntryProps    map[string]string

	Referrer    string
	Source      string
	Channel     string
	UTMSource   string
	UTMMedium   string
	UTMCampaign string

	Country string
	Region  string
	City    string

	DeviceType     string
	ScreenSize     string
	Browser        string
	BrowserVersion string
	OS             string
	OSVersion      string
	Language       string

	// InteractiveNonPageview records that a non-pageview interactive event has
	// arrived. Bounce is derived from this and the pageview count rather than
	// being a flag that gets flipped, so "once false, never true again" holds
	// by construction instead of by every caller remembering it.
	InteractiveNonPageview bool

	// FirstAt and FirstTie mark the event the session's attribution and device
	// block were taken from — the visit's first pageview. This is where
	// "attribution is frozen at session start" actually lives, and it is why a
	// UTM tag on the second pageview of a visit is discarded.
	FirstAt  int64
	FirstTie string

	// FirstIsPageview says the block above came from a pageview rather than
	// from a custom event that opened the visit. A pageview replaces such a
	// block whatever the timestamps say: only a page arrives from somewhere,
	// so a conversion fired a second before the page loaded must not be what
	// the visit is attributed to.
	FirstIsPageview bool

	// EntryAt/ExitAt mark the earliest and latest pageview. The tie strings
	// order two events that share a second, so that they resolve the same way
	// however they arrive.
	EntryAt  int64
	EntryTie string
	ExitAt   int64
	ExitTie  string

	// Dirty means the row differs from what is in SQLite. A batch's sessions
	// are written as one dirty set at the end of the fold rather than with an
	// UPDATE per event, which is the difference between a few hundred writes a
	// second and tens of thousands.
	Dirty bool

	// Restamp means the attribution and device block changed after events had
	// already been written carrying the old one — which is what a late event
	// that turns out to be the real start of the visit does. Every event row
	// holds a copy of its session's block, so those rows have to be rewritten
	// or one visit is reported under two sources.
	Restamp bool
}

// Duration is the visit length in seconds. It is derived from the two ends
// rather than accumulated, so an event arriving late — or twice — cannot
// inflate it.
func (s *Session) Duration() int64 {
	return s.LastSeenAt - s.StartedAt
}

// IsBounce reports whether this visit bounced. A bounce is a visit that never
// got past its first page and never interacted, and stating it as a derivation
// of two facts rather than a mutable flag is what makes it order-independent:
// there is no sequence of arrivals that can produce a different answer.
func (s *Session) IsBounce() bool {
	return s.Pageviews < 2 && !s.InteractiveNonPageview
}

// covers reports whether an event at this timestamp belongs to this session.
// The window extends the timeout in *both* directions, which is the whole trick
// to order-independence: an event that arrives after a later one still lands in
// the visit it happened during.
func (s *Session) covers(timestamp int64) bool {
	return timestamp >= s.StartedAt-sessionTimeoutSeconds &&
		timestamp <= s.LastSeenAt+sessionTimeoutSeconds
}

// distance is how far outside a session an event falls, and zero when it falls
// inside. It picks a winner when an out-of-order event sits between two
// sessions and both could claim it.
func (s *Session) distance(timestamp int64) int64 {
	if timestamp < s.StartedAt {
		return s.StartedAt - timestamp
	}
	if timestamp > s.LastSeenAt {
		return timestamp - s.LastSeenAt
	}

	return 0
}

// sessionKey is the fold key: a session belongs to one visitor on one site.
type sessionKey struct {
	siteID int64
	userID int64
}

// Merge records that one session was absorbed into another. Merges only happen
// when an out-of-order event bridges two sessions that would have been one all
// along; the writer uses this to repoint any events already on disk and to
// delete the row that lost.
type Merge struct {
	AccountID int64
	Survivor  int64
	Absorbed  int64
}

// SessionCache is the fold's working state for one write: the live sessions
// and parked engagement pings of the visitor keys that write touches. The
// writer builds one per account transaction from SQLite and discards it after
// commit, and the seed generator holds one for a single-threaded run, so it is
// never shared between goroutines and takes no locks.
type SessionCache struct {
	bucket sessionBucket

	// maxOrphans bounds how many early pings one visitor may park. The writer
	// disables it: SQLite already holds and bounds those rows, and every ping
	// it accepted must stay adoptable.
	maxOrphans int

	// nextID allocates session ids for callers with no database allocator.
	// The seed generator is the only writer of its file, so a counter seeded
	// from the high water mark is both correct and free.
	nextID map[int64]int64

	// merges is drained by the writer after the fold.
	merges []Merge
}

// sessionBucket is the fold state itself. Each key maps to a slice rather than
// a single session because an out-of-order event can briefly create a second
// live session for the same visitor, and the fold has to be able to see both
// to decide whether they are really one.
type sessionBucket struct {
	sessions map[sessionKey][]*Session

	// orphans holds engagement pings that arrived before the visit they belong
	// to. An engagement carries no page of its own, so it cannot open a session
	// — but dropping it outright would make the fold depend on arrival order,
	// and time-on-page is computed from exactly these events.
	orphans map[sessionKey][]*Event
}

// NewSessionCache builds an empty fold with the process-memory orphan cap.
func NewSessionCache() *SessionCache {
	return &SessionCache{
		bucket:     sessionBucket{sessions: map[sessionKey][]*Session{}, orphans: map[sessionKey][]*Event{}},
		nextID:     map[int64]int64{},
		maxOrphans: MaxOrphanEngagements,
	}
}

// newDurableSessionCache builds the transaction-local fold used by Writer.
// SQLite owns and bounds these orphan rows, so applying the process-memory cap
// here would acknowledge a ping while making it impossible to adopt later.
func newDurableSessionCache() *SessionCache {
	cache := NewSessionCache()
	cache.maxOrphans = 0

	return cache
}

// SeedIDs sets an account's session id high water mark, read from its database
// when the account is opened. Getting this wrong would make a new session
// collide with an existing row, so it is an explicit call rather than something
// inferred lazily.
func (c *SessionCache) SeedIDs(accountID, highest int64) {
	if current, ok := c.nextID[accountID]; !ok || highest+1 > current {
		c.nextID[accountID] = highest + 1
	}
}

// allocateID hands out the next session id for an account.
func (c *SessionCache) allocateID(accountID int64) int64 {
	next := c.nextID[accountID]
	if next < 1 {
		next = 1
	}
	c.nextID[accountID] = next + 1

	return next
}

// MaxOrphanEngagements caps how many early engagement pings one visitor may
// have parked at once. It is a bound on what a client can make us hold, not a
// tuning knob: a real visit produces a handful.
const MaxOrphanEngagements = 32

// Apply folds one event into its session, creating one if there is none. It
// returns the session, whether the event should be written, and any parked
// engagement pings this event has just given a home to.
//
// This is the function the two unrecoverable decisions live in. Every rule it
// applies is keyed off the event's own timestamp rather than the order it
// arrived in.
func (c *SessionCache) Apply(event *Event) (*Session, bool, []*Event) {
	session, ok, revived, _ := c.apply(event, func() (int64, error) {
		return c.allocateID(event.AccountID), nil
	})

	return session, ok, revived
}

// ApplyAllocated folds one event while taking a new session identity from the
// caller. The writer supplies identities from an atomic database range; direct
// fold users such as the deterministic seed generator keep using Apply.
func (c *SessionCache) ApplyAllocated(event *Event, allocate func() (int64, error)) (*Session, bool, []*Event, error) {
	return c.apply(event, allocate)
}

// apply contains the fold shared by the database-backed writer and direct
// callers. The allocator is invoked only when the event genuinely opens a new
// visit.
func (c *SessionCache) apply(event *Event, allocate func() (int64, error)) (*Session, bool, []*Event, error) {
	key := sessionKey{siteID: event.SiteID, userID: event.UserID}

	session, absorbed := c.bucket.claim(key, event.Timestamp)
	found := key

	// The previous salt is a session-lookup fallback and nothing else. Without
	// it a visitor who is mid-visit at 00:00 UTC gets a new fingerprint, a new
	// session, and is counted as two people.
	if session == nil && event.PreviousUserID != 0 && event.PreviousUserID != event.UserID {
		previousKey := sessionKey{siteID: event.SiteID, userID: event.PreviousUserID}
		session, absorbed = c.bucket.claim(previousKey, event.Timestamp)

		if session != nil {
			found = previousKey
		}
	}

	if session == nil {
		// An engagement ping has no page of its own, so opening a visit with it
		// would produce a session with no entry page and inflate the visit
		// count. Parking it instead of dropping it is what keeps the fold
		// independent of arrival order: a retry that delivers the ping before
		// its pageview must still produce the same row.
		if event.IsEngagement() {
			c.bucket.park(key, event, c.maxOrphans)
			return nil, false, nil, nil
		}

		id, err := allocate()
		if err != nil {
			return nil, false, nil, err
		}

		session = c.newSession(event, id)
		c.bucket.sessions[key] = append(c.bucket.sessions[key], session)
	}

	// The session's identity wins over the event's. Copying it onto the event
	// is what keeps one visitor as one visitor across a salt rotation.
	event.UserID = session.UserID

	session.fold(event)

	revived := c.bucket.adopt(key, session)

	// Pings parked under the pre-rotation fingerprint sit under that key, so a
	// session found through the previous salt adopts from both keys or every
	// engagement ping in flight across 00:00 UTC is dropped silently.
	if found != key {
		revived = append(revived, c.bucket.adopt(found, session)...)
	}

	c.recordMerges(session, absorbed)

	return session, true, revived, nil
}

// park holds an engagement ping until the visit it belongs to appears. A ping
// already parked is ignored, so a redelivery cannot be counted twice — the
// database's dedupe table cannot help here, because a parked event has never
// been written.
func (b *sessionBucket) park(key sessionKey, event *Event, limit int) {
	waiting := b.orphans[key]
	if limit > 0 && len(waiting) >= limit {
		return
	}

	for _, existing := range waiting {
		if existing.UUID == event.UUID {
			return
		}
	}

	copied := *event
	b.orphans[key] = append(waiting, &copied)
}

// adopt folds every parked ping the session now covers and returns them, so the
// writer stores the rows it skipped earlier.
func (b *sessionBucket) adopt(key sessionKey, session *Session) []*Event {
	waiting := b.orphans[key]
	if len(waiting) == 0 {
		return nil
	}

	var (
		kept    []*Event
		revived []*Event
	)

	for _, orphan := range waiting {
		if !session.covers(orphan.Timestamp) {
			kept = append(kept, orphan)
			continue
		}

		orphan.UserID = session.UserID
		session.fold(orphan)
		revived = append(revived, orphan)
	}

	if len(kept) == 0 {
		delete(b.orphans, key)
	} else {
		b.orphans[key] = kept
	}

	return revived
}

// claim returns the session an event at this timestamp belongs to, absorbing
// any others it bridges, and reports which ids were absorbed.
//
// Merging is purely an out-of-order repair. Arriving in order an event can
// never match two sessions at once, because the later session could not have
// been created if the gap were small enough to bridge — so in-order traffic
// never reaches the second loop below.
func (b *sessionBucket) claim(key sessionKey, timestamp int64) (*Session, []int64) {
	live := b.sessions[key]
	if len(live) == 0 {
		return nil, nil
	}

	var (
		winner *Session
		best   int64
	)

	for _, candidate := range live {
		if !candidate.covers(timestamp) {
			continue
		}

		distance := candidate.distance(timestamp)

		// Ties go to the session that started earlier, so the answer does not
		// depend on the order the slice happens to be in.
		if winner == nil || distance < best || (distance == best && candidate.StartedAt < winner.StartedAt) {
			winner, best = candidate, distance
		}
	}

	if winner == nil {
		return nil, nil
	}

	if len(live) == 1 {
		return winner, nil
	}

	// One pass is exhaustive. What a session is bridged by is the *event's*
	// window, and absorbing changes only the winner's — so the set of sessions
	// this event bridges is fixed before the first absorb and cannot grow
	// while they are being folded together.
	var absorbed []int64

	kept := live[:0]

	for _, candidate := range live {
		if candidate == winner || !candidate.covers(timestamp) {
			kept = append(kept, candidate)
			continue
		}

		winner.absorb(candidate)
		absorbed = append(absorbed, candidate.ID)
	}

	b.sessions[key] = kept

	return winner, absorbed
}

// recordMerges queues the repairs the writer has to make on disk: repoint any
// events already written to the absorbed session, and delete its row.
func (c *SessionCache) recordMerges(survivor *Session, absorbed []int64) {
	for _, id := range absorbed {
		c.merges = append(c.merges, Merge{AccountID: survivor.AccountID, Survivor: survivor.ID, Absorbed: id})
	}
}

// TakeMerges hands one account's pending merges to the writer and clears them.
// It is scoped to an account because each account is a separate database file,
// and a merge queued for one of them means nothing in another.
func (c *SessionCache) TakeMerges(accountID int64) []Merge {
	if len(c.merges) == 0 {
		return nil
	}

	var (
		mine []Merge
		rest []Merge
	)

	for _, merge := range c.merges {
		if merge.AccountID == accountID {
			mine = append(mine, merge)
			continue
		}
		rest = append(rest, merge)
	}

	c.merges = rest

	return mine
}

// TakeDirty returns one account's sessions that differ from their stored rows
// and marks them clean. Flushing a dirty set is what keeps SQLite writing a few
// hundred rows in one transaction instead of one row per event.
func (c *SessionCache) TakeDirty(accountID int64) []*Session {
	var dirty []*Session

	for _, live := range c.bucket.sessions {
		for _, session := range live {
			if !session.Dirty || session.AccountID != accountID {
				continue
			}
			session.Dirty = false

			// The writer gets a copy so that folding the next batch cannot
			// change a row halfway through being written.
			snapshot := *session

			// The restamp travels on the snapshot and is cleared here, because
			// it describes a repair the writer is about to make. Leaving it set
			// would rewrite every event of the visit again on every later flush.
			session.Restamp = false

			dirty = append(dirty, &snapshot)
		}
	}

	return dirty
}

// Sweep drops sessions and parked pings that can no longer receive an event,
// and reports how many went. A long-running fold — the seed generator's —
// would otherwise hold every visit of the run in memory. A dirty session is
// never swept: its last events have not been written yet.
func (c *SessionCache) Sweep(now int64) int {
	removed := 0

	for key, waiting := range c.bucket.orphans {
		var kept []*Event
		for _, orphan := range waiting {
			if now-orphan.Timestamp <= sessionTimeoutSeconds {
				kept = append(kept, orphan)
				continue
			}
			removed++
		}

		if len(kept) == 0 {
			delete(c.bucket.orphans, key)
			continue
		}
		c.bucket.orphans[key] = kept
	}

	for key, live := range c.bucket.sessions {
		kept := live[:0]

		for _, session := range live {
			if session.Dirty || now-session.LastSeenAt <= sessionTimeoutSeconds {
				kept = append(kept, session)
				continue
			}
			removed++
		}

		if len(kept) == 0 {
			delete(c.bucket.sessions, key)
			continue
		}
		c.bucket.sessions[key] = kept
	}

	return removed
}

// Sentinels for the "no event yet" state of the entry, exit and first-event
// markers.
const (
	maxInt64 = int64(^uint64(0) >> 1)
	minInt64 = -maxInt64 - 1
)

// orderKey is how two events of one visit are ordered inside the second they
// share. It is the derive stamp first and the event uuid second, both written
// so that comparing the strings compares the values: the derive stamp says
// which event the ingest tier actually saw first, and the uuid settles what is
// left — two events with no stamp of their own, where the only requirement is
// that the answer never changes.
//
// It is a property of the event and of nothing else, which is what keeps the
// fold independent of arrival order: a batch replayed backwards carries the
// same keys and therefore produces the same row.
func orderKey(event *Event) string {
	var stamp [20]byte

	value := event.DerivedAt
	for i := len(stamp) - 1; i >= 0; i-- {
		stamp[i] = byte('0' + value%10)
		value /= 10
	}

	return string(stamp[:]) + "|" + event.UUID.String()
}

// earlier reports whether one timestamped fact precedes another, breaking an
// exact tie on the order key. The tie-break exists so two events in the same
// second always resolve identically, however they arrive — without it the
// shuffled replay of a stream would produce a different entry page.
func earlier(timestamp int64, tie string, thanTimestamp int64, thanTie string) bool {
	if timestamp != thanTimestamp {
		return timestamp < thanTimestamp
	}

	return tie < thanTie
}

// later is the mirror of earlier, for the exit page.
func later(timestamp int64, tie string, thanTimestamp int64, thanTie string) bool {
	if timestamp != thanTimestamp {
		return timestamp > thanTimestamp
	}

	return tie > thanTie
}
