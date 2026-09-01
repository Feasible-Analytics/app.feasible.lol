//
// session.go
// The session fold: one row per visit, updated in place, independent of arrival order.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"sync"
	"time"
)

// SessionTimeout is the inactivity gap that ends a visit. Thirty minutes is the
// industry convention and changing it would make every historical number
// incomparable, so it is a constant rather than a setting.
const SessionTimeout = 30 * time.Minute

// sessionTimeoutSeconds is the same value in the unit every timestamp uses.
const sessionTimeoutSeconds = int64(SessionTimeout / time.Second)

// SweepInterval is how often expired sessions leave the cache. Ten seconds is
// often enough that memory tracks reality and rare enough that the sweep never
// competes with ingestion for the bucket locks.
const SweepInterval = 10 * time.Second

// DirtyGrace is how long past its last event a session whose rows have never
// been written is kept. A writer wedged for an hour is not going to save it,
// and keeping every dirty session forever turns one stuck account into a process
// that grows until it is killed — which loses the whole cache rather than one
// visit.
const DirtyGrace = time.Hour

// sessionShards is how many independently locked buckets the cache has. It is a
// power of two so the bucket is a mask rather than a modulo, and 64 is enough
// that lock contention disappears well past the throughput one box can reach.
const sessionShards = 64

// Session is one visit, held in memory and flushed to SQLite as a single row
// that is updated in place. A column store that cannot UPDATE has to fake this
// with a sign column and a collapsing merge, which turns every average into a
// ratio of signed sums; we just update the row.
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

	// Dirty means the row differs from what is in SQLite. Sessions are flushed
	// as a dirty set rather than with an UPDATE per event, which is the
	// difference between a few hundred writes a second and tens of thousands.
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

// sessionKey is the cache key: a session belongs to one visitor on one site.
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

// SessionCache holds every live session. It is sharded because it is written on
// every single event, and one mutex over the whole map would be the first thing
// to saturate on a busy box.
type SessionCache struct {
	shards [sessionShards]sessionBucket

	// maxOrphans bounds only process-local folds. The production writer uses a
	// transaction-local cache backed by SQLite, where every accepted ping is
	// already durable and must remain adoptable; zero disables this memory cap.
	maxOrphans int

	// nextID allocates session ids. In direct mode this process is the only
	// writer of the sessions table, so a counter seeded from the file's high
	// water mark is both correct and free — no round trip per new visit.
	idMu   sync.Mutex
	nextID map[int64]int64

	// merges is drained by the writer. It is deliberately outside the buckets
	// so that draining it does not have to walk or lock every shard.
	mergeMu sync.Mutex
	merges  []Merge

	// OnOrphanExpired is called for every engagement ping whose visit never
	// arrived. The ping is a genuine drop at that point, and by then the
	// response went out half an hour ago — so this callback is the only way the
	// drop can be counted and told to anyone at all.
	OnOrphanExpired func(*Event)

	// OnSessionAbandoned is called for a dirty session evicted long after its
	// last event. It means a write never landed and its last few events are
	// gone, which is data loss and has to be said out loud.
	OnSessionAbandoned func(*Session)
}

// sessionBucket is one independently locked slice of the cache. Each key maps
// to a slice rather than a single session because an out-of-order event can
// briefly create a second live session for the same visitor, and the fold has
// to be able to see both to decide whether they are really one.
type sessionBucket struct {
	mu       sync.Mutex
	sessions map[sessionKey][]*Session

	// orphans holds engagement pings that arrived before the visit they belong
	// to. An engagement carries no page of its own, so it cannot open a session
	// — but dropping it outright would make the fold depend on arrival order,
	// and time-on-page is computed from exactly these events.
	orphans map[sessionKey][]*Event
}

// NewSessionCache builds an empty cache.
func NewSessionCache() *SessionCache {
	cache := &SessionCache{nextID: map[int64]int64{}, maxOrphans: MaxOrphanEngagements}

	for i := range cache.shards {
		cache.shards[i].sessions = map[sessionKey][]*Session{}
		cache.shards[i].orphans = map[sessionKey][]*Event{}
	}

	return cache
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
	c.idMu.Lock()
	defer c.idMu.Unlock()

	if current, ok := c.nextID[accountID]; !ok || highest+1 > current {
		c.nextID[accountID] = highest + 1
	}
}

// allocateID hands out the next session id for an account.
func (c *SessionCache) allocateID(accountID int64) int64 {
	c.idMu.Lock()
	defer c.idMu.Unlock()

	next := c.nextID[accountID]
	if next < 1 {
		next = 1
	}
	c.nextID[accountID] = next + 1

	return next
}

// bucket picks the shard for a key. The hash is a cheap integer mix rather than
// a real hash function, because the inputs are already a 64-bit fingerprint and
// a small site id — the only thing needed here is that neighbouring ids do not
// land in the same bucket.
func (c *SessionCache) bucket(key sessionKey) *sessionBucket {
	mixed := uint64(key.userID)*0x9e3779b97f4a7c15 + uint64(key.siteID)

	return &c.shards[(mixed>>32)&(sessionShards-1)]
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
// visit, while the visitor bucket is locked against a competing local fold.
func (c *SessionCache) apply(event *Event, allocate func() (int64, error)) (*Session, bool, []*Event, error) {
	key := sessionKey{siteID: event.SiteID, userID: event.UserID}
	bucket := c.bucket(key)

	bucket.mu.Lock()

	session, absorbed := bucket.claim(key, event.Timestamp)
	found := key

	// The previous salt is a session-lookup fallback and nothing else. Without
	// it a visitor who is mid-visit at 00:00 UTC gets a new fingerprint, a new
	// session, and is counted as two people.
	if session == nil && event.PreviousUserID != 0 && event.PreviousUserID != event.UserID {
		previousKey := sessionKey{siteID: event.SiteID, userID: event.PreviousUserID}
		previousBucket := c.bucket(previousKey)

		// Two different keys can land in the same bucket, and locking one twice
		// would deadlock.
		if previousBucket != bucket {
			previousBucket.mu.Lock()
			session, absorbed = previousBucket.claim(previousKey, event.Timestamp)
			previousBucket.mu.Unlock()
		} else {
			session, absorbed = previousBucket.claim(previousKey, event.Timestamp)
		}

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
			bucket.park(key, event, c.maxOrphans)
			bucket.mu.Unlock()
			return nil, false, nil, nil
		}

		id, err := allocate()
		if err != nil {
			bucket.mu.Unlock()
			return nil, false, nil, err
		}

		session = c.newSession(event, id)
		bucket.sessions[key] = append(bucket.sessions[key], session)
	}

	// The session's identity wins over the event's. Copying it onto the event
	// is what keeps one visitor as one visitor across a salt rotation.
	event.UserID = session.UserID

	session.fold(event)

	revived := bucket.adopt(key, session)

	// Pings parked under the pre-rotation fingerprint live in that key's own
	// bucket, which is a different shard 63 times in 64. Adopting them from
	// this one finds nothing and drops every engagement ping in flight across
	// 00:00 UTC, silently.
	if found != key {
		previousBucket := c.bucket(found)
		if previousBucket != bucket {
			previousBucket.mu.Lock()
			revived = append(revived, previousBucket.adopt(found, session)...)
			previousBucket.mu.Unlock()
		} else {
			revived = append(revived, previousBucket.adopt(found, session)...)
		}
	}

	bucket.mu.Unlock()

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
// writer stores the rows it skipped earlier. The caller must hold the bucket
// lock.
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
// any others it bridges, and reports which ids were absorbed. The caller must
// already hold the bucket lock.
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
	if len(absorbed) == 0 {
		return
	}

	c.mergeMu.Lock()
	defer c.mergeMu.Unlock()

	for _, id := range absorbed {
		c.merges = append(c.merges, Merge{AccountID: survivor.AccountID, Survivor: survivor.ID, Absorbed: id})
	}
}

// TakeMerges hands one account's pending merges to the writer and clears them.
// It is scoped to an account because each account is a separate database file,
// and a merge queued for one of them means nothing in another.
func (c *SessionCache) TakeMerges(accountID int64) []Merge {
	c.mergeMu.Lock()
	defer c.mergeMu.Unlock()

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

	for i := range c.shards {
		bucket := &c.shards[i]

		bucket.mu.Lock()
		for _, live := range bucket.sessions {
			for _, session := range live {
				if !session.Dirty || session.AccountID != accountID {
					continue
				}
				session.Dirty = false

				// The copy is what crosses the lock boundary. Handing the live
				// pointer to the writer would let a concurrent fold change a
				// row halfway through being written.
				snapshot := *session

				// The restamp travels on the snapshot and is cleared here,
				// because it describes a repair the writer is about to make.
				// Leaving it set would rewrite every event of the visit again
				// on every later flush.
				session.Restamp = false

				dirty = append(dirty, &snapshot)
			}
		}
		bucket.mu.Unlock()
	}

	return dirty
}

// Redirty puts one account's sessions back in the dirty set after a failed
// write. Without it a transaction that rolled back would leave the cache
// believing rows are on disk that never landed, and the next flush would skip
// them forever.
//
// The account is part of the match, not context: session ids are allocated per
// account, so id 7 exists in every account database and matching on the id
// alone would dirty an unrelated customer's row on every failed write.
func (c *SessionCache) Redirty(accountID int64, sessions []*Session) {
	if len(sessions) == 0 {
		return
	}

	wanted := make(map[int64]*Session, len(sessions))
	for _, session := range sessions {
		wanted[session.ID] = session
	}

	for i := range c.shards {
		bucket := &c.shards[i]

		bucket.mu.Lock()
		for _, live := range bucket.sessions {
			for _, session := range live {
				if session.AccountID != accountID {
					continue
				}

				snapshot, ok := wanted[session.ID]
				if !ok {
					continue
				}

				session.Dirty = true

				// A restamp the failed transaction was carrying goes back with
				// the session. It is the only record that events on disk hold
				// an attribution the session no longer has.
				session.Restamp = session.Restamp || snapshot.Restamp
			}
		}
		bucket.mu.Unlock()
	}
}

// Sweep drops sessions that can no longer receive an event. It runs on a timer
// rather than on access because an abandoned session is never touched again by
// definition, so nothing else would ever notice it.
func (c *SessionCache) Sweep(now int64) int {
	removed := 0

	// What expired is reported after every bucket is unlocked. The callbacks
	// take locks of their own — a counter, a log — and calling them under a
	// shard lock would put the sweep in the way of live ingestion.
	var (
		expired   []*Event
		abandoned []*Session
	)

	for i := range c.shards {
		bucket := &c.shards[i]

		bucket.mu.Lock()

		// A ping whose visit never turned up is a genuine drop. Expiring them
		// here is what stops a client that only ever sends engagement events
		// from growing the cache without bound.
		for key, waiting := range bucket.orphans {
			var kept []*Event
			for _, orphan := range waiting {
				if now-orphan.Timestamp <= sessionTimeoutSeconds {
					kept = append(kept, orphan)
					continue
				}
				expired = append(expired, orphan)
				removed++
			}

			if len(kept) == 0 {
				delete(bucket.orphans, key)
				continue
			}
			bucket.orphans[key] = kept
		}

		for key, live := range bucket.sessions {
			kept := live[:0]

			for _, session := range live {
				if now-session.LastSeenAt <= sessionTimeoutSeconds {
					kept = append(kept, session)
					continue
				}

				// A dirty session is held past the timeout, because dropping it
				// would lose the last few events of a visit that has not been
				// written yet. It is not held forever: past the grace period the
				// write is not coming, and an unbounded cache costs every other
				// visit in memory rather than this one.
				if session.Dirty && now-session.LastSeenAt <= sessionTimeoutSeconds+int64(DirtyGrace/time.Second) {
					kept = append(kept, session)
					continue
				}

				if session.Dirty {
					abandoned = append(abandoned, session)
				}
				removed++
			}

			if len(kept) == 0 {
				delete(bucket.sessions, key)
				continue
			}
			bucket.sessions[key] = kept
		}
		bucket.mu.Unlock()
	}

	if c.OnOrphanExpired != nil {
		for _, orphan := range expired {
			c.OnOrphanExpired(orphan)
		}
	}

	if c.OnSessionAbandoned != nil {
		for _, session := range abandoned {
			c.OnSessionAbandoned(session)
		}
	}

	return removed
}

// Len reports how many sessions are live. It is what a health check reads to
// answer "is the cache growing without bound", which is the shape of a sweep
// that has stopped running.
func (c *SessionCache) Len() int {
	count := 0

	for i := range c.shards {
		bucket := &c.shards[i]
		bucket.mu.Lock()
		for _, live := range bucket.sessions {
			count += len(live)
		}
		bucket.mu.Unlock()
	}

	return count
}

// Snapshot copies every live session out for persistence. On graceful shutdown
// the cache is written to disk and reloaded at boot, because otherwise a
// restart splits every in-flight session in two and there is no way to tell
// afterwards which visits were really one.
func (c *SessionCache) Snapshot() []Session {
	var out []Session

	for i := range c.shards {
		bucket := &c.shards[i]
		bucket.mu.Lock()
		for _, live := range bucket.sessions {
			for _, session := range live {
				out = append(out, *session)
			}
		}
		bucket.mu.Unlock()
	}

	return out
}

// Restore puts a snapshot back. Sessions past the timeout are skipped, so a
// process that was down for an hour does not resurrect visits that ended while
// it was away.
func (c *SessionCache) Restore(sessions []Session, now int64) int {
	restored := 0

	for i := range sessions {
		session := sessions[i]
		if now-session.LastSeenAt > sessionTimeoutSeconds {
			continue
		}

		key := sessionKey{siteID: session.SiteID, userID: session.UserID}
		bucket := c.bucket(key)

		bucket.mu.Lock()
		bucket.sessions[key] = append(bucket.sessions[key], &session)
		bucket.mu.Unlock()

		c.SeedIDs(session.AccountID, session.ID)
		restored++
	}

	return restored
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
