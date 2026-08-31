//
// seed.go
// The generator: real derivation, simulated traffic, no network.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/salts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/useragent"
)

// Defaults for a plain `make seed`: a couple of accounts, five sites carrying
// traffic and six weeks of history. Six weeks is the shortest span that gives
// the dashboard a full previous period to compare a month against.
const (
	DefaultPageviews = 120_000
	DefaultDays      = 42
	DefaultSites     = 5

	// DefaultSeed is fixed so that two runs produce the same database. Without
	// it "this query got slower" might only mean "this is different data".
	DefaultSeed = int64(20260830)
)

// The share of pageviews that carry each extra kind of event. They are constants
// because they decide what fraction of the rows land in the cold table, which is
// the thing that makes a query against real-shaped data slower than one against
// tidy data.
const (
	engagementShare = 0.35
	customShare     = 0.045
	campaignShare   = 0.13
	botShare        = 0.015
	unvalidated     = 0.003

	// directShare is how much traffic arrives with no referrer at all. It is
	// the largest single bucket on every acquisition report there has ever
	// been, and a seed without it makes the sources page look impossible.
	directShare = 0.42
)

// Options configure a run. Every one of them changes the shape of the data
// rather than only its size, which is why they are all explicit rather than
// derived from a single "how big" number.
type Options struct {
	// DataDir holds control.db and the account databases.
	DataDir string

	// Pageviews is the total across every site. Other event kinds are generated
	// on top of it, so a run writes more rows than this.
	Pageviews int64

	// Days of history, ending today.
	Days int

	// Sites is how many of the fixture's traffic-carrying sites to use. The
	// site with no data at all is always created regardless.
	Sites int

	// Seed makes a run reproducible.
	Seed int64

	// Fresh deletes the seeded databases first. Without it a run adds to
	// whatever is already there, which is occasionally what somebody wants.
	Fresh bool

	// Now replaces the clock, so a test can pin the history to a fixed instant
	// instead of to whenever the suite happens to run.
	Now func() time.Time

	// Out receives the progress and the summary. Nil means no output.
	Out io.Writer

	Log *logger.Logger
}

// withDefaults fills in the values a caller did not set. It is a method rather
// than a constructor so the zero Options is usable from a test with one field
// set.
func (o *Options) withDefaults() {
	if o.DataDir == "" {
		o.DataDir = config.DefaultAppDataDir
	}
	if o.Pageviews <= 0 {
		o.Pageviews = DefaultPageviews
	}
	if o.Days <= 0 {
		o.Days = DefaultDays
	}
	if o.Sites <= 0 {
		o.Sites = DefaultSites
	}
	if o.Seed == 0 {
		o.Seed = DefaultSeed
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
}

// Result is what a run produced. It is returned rather than only printed so a
// test can assert on it and the command can decide how it reads.
type Result struct {
	Duration time.Duration

	// The three phases, timed separately because they answer different
	// questions: whether the generator is fast enough, what a bulk index build
	// costs, and how long a first real query takes against the result.
	Generating time.Duration
	Indexing   time.Duration
	Verifying  time.Duration

	Events    int64
	Pageviews int64
	Sessions  int64
	Dropped   int64

	// Report is the shape of the data that was generated, measured by querying
	// it back rather than by counting what was intended.
	Report Report
}

// accountRun is one open account database and the sites that write into it.
type accountRun struct {
	account *accounts.Account
	seeded  *seededAccount
	writer  *batchWriter
	sites   []*siteRun

	// suspended holds the indexes dropped for the load, with the statements
	// that rebuild them.
	suspended []suspendedIndex
}

// siteRun is one site's generator state for the whole run.
type siteRun struct {
	seeded  *seededSite
	domain  string
	name    string
	account *accountRun

	pages   []string
	sources []string

	pageChooser     *chooser
	entryChooser    *chooser
	sourceChooser   *chooser
	campaignChooser *chooser
	hourChooser     *chooser

	// pool is how many distinct people this site has over the whole run. It is
	// sized from the traffic so a busy site has more visitors rather than the
	// same few visiting more often.
	pool int

	// carry holds events that fell past midnight. They are emitted at the start
	// of the next day so that the run's clock never goes backwards — the salt
	// store rotates on the calendar and would refuse a day it had already
	// passed — and so a visit that spans midnight is folded by the same
	// previous-salt lookup that does it in production.
	carry []pending

	// index is the site's position in the traffic list, which is what decides
	// whether it is the one the spike lands on.
	index int
}

// pending is an event whose time has not come yet.
type pending struct {
	payload *ingest.Payload
	visitor visitor
	at      int64
}

// generator holds everything one run needs. It is a struct rather than a chain
// of parameters because the derive pipeline, the session cache and the random
// stream all have to be the same objects from the first event to the last.
type generator struct {
	opts Options

	rng   *rand.Rand
	start time.Time
	now   time.Time
	days  int

	// clock is the instant the pipeline believes it is. Every event sets it
	// before deriving, because the pipeline stamps its own timestamp from it —
	// which is exactly what makes six weeks of history possible without a
	// single fabricated column.
	clock time.Time

	control  *sql.DB
	manager  *accounts.Manager
	pipeline *ingest.Pipeline
	sessions *ingest.SessionCache

	accounts []*accountRun
	sites    []*siteRun
	budget   [][]int64

	lengths *chooser
	agents  *chooser
	langs   *chooser
	events  *chooser

	// request is reused for every event. Only the address and two headers
	// change, and building a fresh one a million times would be most of the
	// cost of not using the network.
	request *http.Request

	// The deliberate oddities, each generated once. Tidy data hides exactly the
	// bugs a seed exists to catch.
	singletonDone bool
	maxPropsDone  bool
	longSessions  int

	stats Result
}

// Run generates a dataset. It calls the same functions the ingest path calls —
// the fingerprint, the user-agent parser, the referrer and channel rules, the
// session fold — and skips only the network, which is what makes it minutes
// rather than hours while still producing rows that are indistinguishable from
// real ones.
func Run(ctx context.Context, opts Options) (*Result, error) {
	opts.withDefaults()

	started := time.Now()

	if opts.Fresh {
		if err := removeSeeded(opts.DataDir); err != nil {
			return nil, err
		}
	}

	g := &generator{opts: opts, rng: rand.New(rand.NewPCG(uint64(opts.Seed), 0x9e3779b97f4a7c15))}

	// Event ids come from the same seed. They are never stored, but they break
	// ties in the session fold — two pageviews in the same second — so a random
	// source here would make a run reproducible everywhere except the entry
	// page of a handful of visits.
	uuid.SetRand(&byteSource{rng: rand.New(rand.NewPCG(uint64(opts.Seed), 0x2545f4914f6cdd1d))})
	defer uuid.SetRand(nil)

	if err := g.open(ctx); err != nil {
		return nil, err
	}
	defer g.close()

	// The fact-table indexes are dropped for the load and rebuilt at the end. A
	// failed run has to put them back too: a database that quietly lost its
	// indexes would answer every query correctly and slowly, which is the one
	// outcome nobody would notice.
	defer g.restoreIndexes(ctx)

	if err := g.plan(ctx); err != nil {
		return nil, err
	}

	generating := time.Now()

	if err := g.generate(ctx); err != nil {
		return nil, err
	}

	g.stats.Generating = time.Since(generating)

	if err := g.finish(ctx); err != nil {
		return nil, err
	}

	g.stats.Duration = time.Since(started)

	return &g.stats, nil
}

// open brings up control.db, the account manager and the derive pipeline. The
// pipeline is assembled here rather than through ingest.NewService because two
// of its parts are deliberately not the production ones: geolocation comes from
// the distribution instead of the mmdb file, and there is no buffer, transport
// or HTTP handler in front of it.
func (g *generator) open(ctx context.Context) error {
	control, err := store.Open(filepath.Join(g.opts.DataDir, config.ControlDatabaseName))
	if err != nil {
		return err
	}
	g.control = control

	// A seed run migrates on its own. It is a development command against a
	// development database, and making somebody run two commands to get one
	// dataset is how they end up with neither.
	if _, err := migrate.Run(ctx, control, migrate.Control()); err != nil {
		return fmt.Errorf("seed: %w", err)
	}

	key, err := salts.LoadKey(g.opts.DataDir, "")
	if err != nil {
		return err
	}

	saltStore, err := salts.NewStore(control, key)
	if err != nil {
		return err
	}

	saltStore.SetClock(func() time.Time { return g.clock })
	saltStore.SetRandom(&byteSource{rng: rand.New(rand.NewPCG(uint64(g.opts.Seed), 0x1d872e4a))})

	trusted, err := ingest.ParseTrustedProxies(nil)
	if err != nil {
		return err
	}

	bots := ingest.NewBotFilter()

	// The VPN slice of the synthetic address range is registered as a
	// datacentre range, so those visitors are classified and bucketed as
	// "Anonymous VPN Service" by the production code rather than by the
	// generator writing the string itself.
	bots.SetDatacenterRanges([]string{vpnRange})

	g.manager = accounts.NewManager(g.opts.DataDir)
	g.sessions = ingest.NewSessionCache()

	g.pipeline = &ingest.Pipeline{
		Sites:   sites.New(control),
		Salts:   saltStore,
		Geo:     newLocator(),
		Agents:  useragent.NewCache(useragent.DefaultCapacity, useragent.DefaultTTL),
		Bots:    bots,
		Trusted: trusted,
		Shards:  ingest.DirectShard{},
		Shield:  ingest.NoShield{},
		Now:     func() time.Time { return g.clock },
	}

	g.request = &http.Request{
		Method:     http.MethodPost,
		Header:     http.Header{},
		URL:        &url.URL{Path: "/api/event"},
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}

	return nil
}

// close releases the databases. A seed run holds every account open at once —
// there are three of them — because the day loop visits all of them before
// moving on, and opening and closing a file per day would be the slowest thing
// in the run.
func (g *generator) close() {
	if g.manager != nil {
		_ = g.manager.CloseAll()
	}
	if g.control != nil {
		_ = g.control.Close()
	}
}

// plan works out the calendar, creates the fixture and allocates the pageview
// budget. Everything decided here is decided once: the loop that follows only
// spends what this hands it.
func (g *generator) plan(ctx context.Context) error {
	g.now = g.opts.Now().UTC()
	g.days = g.opts.Days
	g.start = g.now.Truncate(24*time.Hour).AddDate(0, 0, -(g.days - 1))

	// The clock starts at the beginning of history rather than at the real now,
	// because the salt store creates the day's salt the first time it is asked
	// and a run that started in the future would then refuse to go back.
	g.clock = g.start

	selected := selectFixture(g.opts.Sites)

	seeded, err := ensureFixture(ctx, g.control, selected, g.start, g.now)
	if err != nil {
		return err
	}

	// The routing map is read on every event and is normally rebuilt on a
	// timer. Nothing here waits fifteen seconds for a site it just created.
	if err := g.pipeline.Sites.Refresh(ctx); err != nil {
		return err
	}

	var traffic []siteFixture

	for _, account := range seeded {
		opened, err := g.manager.Open(ctx, account.ID)
		if err != nil {
			return err
		}

		run := &accountRun{account: opened, seeded: account, writer: newBatchWriter(opened, g.sessions)}

		// Seeding on top of a database that already holds data has to carry on
		// from its high water marks rather than collide with them.
		if err := run.writer.seedIDs(ctx); err != nil {
			return err
		}

		suspended, err := suspendIndexes(ctx, opened.Writer())
		if err != nil {
			return err
		}
		run.suspended = suspended

		for _, site := range account.trafficSites() {
			run.sites = append(run.sites, g.newSiteRun(run, site, len(g.sites)))
			g.sites = append(g.sites, run.sites[len(run.sites)-1])
			traffic = append(traffic, site.Fixture)
		}

		g.accounts = append(g.accounts, run)
	}

	if len(traffic) == 0 {
		return fmt.Errorf("seed: no sites to generate traffic for")
	}

	g.budget = allocate(g.opts.Pageviews, g.days, g.start, g.now, traffic)

	// The samplers that are shared by every site are built once. A chooser is a
	// cumulative table, and rebuilding one per site-day would cost more than
	// the sampling does.
	g.lengths = sessionLengths()
	g.agents = newChooser(zipf(len(agentCatalog), agentExponent))
	g.langs = newChooser(zipf(len(languages), 1.4))
	g.events = newChooser(zipf(len(customEvents), 1.1))

	// The visitor pool is sized from the traffic each site actually got, which
	// is only known now that the budget is allocated.
	for i, site := range g.sites {
		var total int64
		for day := 0; day < g.days; day++ {
			total += g.budget[day][i]
		}

		site.pool = int(total/3) + 64
	}

	return nil
}

// newSiteRun builds one site's samplers. Each site gets its own page catalogue
// because a documentation site and a shop do not share a single path, and its
// own campaign and source order so the acquisition reports are not identical
// across sites.
func (g *generator) newSiteRun(account *accountRun, site *seededSite, index int) *siteRun {
	pages := pageCatalog(site.Fixture.Kind)
	sources := sourceCatalog()

	hours := hourly(site.Fixture.Kind)
	hourWeights := make([]float64, len(hours))
	copy(hourWeights, hours[:])

	return &siteRun{
		seeded:  site,
		domain:  site.Fixture.Domain,
		name:    site.Fixture.DisplayName,
		account: account,
		pages:   pages,
		sources: sources,
		index:   index,

		pageChooser:  newChooser(zipf(len(pages), pageExponent)),
		entryChooser: newChooser(zipf(len(pages), pageExponent+entryBias)),

		sourceChooser:   newChooser(zipf(len(sources), sourceExponent)),
		campaignChooser: newChooser(zipf(len(campaigns), campaignExponent)),
		hourChooser:     newChooser(hourWeights),
	}
}

// selectFixture takes the first n traffic-carrying sites and drops the accounts
// that are left with none. The order of the fixture is what makes the default
// of five reach every account state; asking for fewer sites narrows the dataset
// from the bottom rather than at random.
func selectFixture(n int) []accountFixture {
	var (
		selected []accountFixture
		used     int
	)

	for _, account := range fixture {
		item := account
		item.Sites = nil

		for _, site := range account.Sites {
			if !site.Traffic {
				// The site with no data costs nothing and is the only way to
				// see an empty state, so it is never dropped.
				item.Sites = append(item.Sites, site)
				continue
			}

			if used >= n {
				continue
			}

			item.Sites = append(item.Sites, site)
			used++
		}

		// An account left with nothing at all is not created. Asking for one
		// site should not produce two empty accounts on the sites list.
		if len(item.Sites) == 0 {
			continue
		}

		selected = append(selected, item)
	}

	return selected
}

// generate walks the calendar. Days are the outer loop and accounts the inner
// one, which is not an implementation detail: the salt rotates on the calendar
// and refuses a day it has already passed, so every site has to finish a day
// before any site starts the next one.
func (g *generator) generate(ctx context.Context) error {
	for day := 0; day < g.days; day++ {
		dayStart := g.start.AddDate(0, 0, day)

		for _, account := range g.accounts {
			for _, site := range account.sites {
				if err := g.emitDay(ctx, site, day, dayStart); err != nil {
					return err
				}
			}

			if err := account.writer.flush(ctx); err != nil {
				return err
			}
		}

		// The sweep runs at midnight rather than half an hour past it, so the
		// visits that are still going at the end of the day survive into the
		// next one and are folded there by the previous-salt lookup.
		g.sessions.Sweep(dayStart.Add(24 * time.Hour).Unix())

		g.progress(day, dayStart)
	}

	return nil
}

// progress prints one line per day. A run that prints nothing for two minutes
// is a run people kill.
func (g *generator) progress(day int, dayStart time.Time) {
	fmt.Fprintf(g.opts.Out, "  %s  %9d events  %9d pageviews\n",
		dayStart.Format(time.DateOnly), g.stats.Events, g.stats.Pageviews)
}

// finish flushes what is left, records where each site's history starts and
// builds the roll-ups.
func (g *generator) finish(ctx context.Context) error {
	for _, account := range g.accounts {
		if err := account.writer.flush(ctx); err != nil {
			return err
		}
	}

	// Everything still live is swept well past the end of the run, so the last
	// day's visits are written rather than left in memory.
	g.sessions.Sweep(g.now.Add(48 * time.Hour).Unix())

	for _, account := range g.accounts {
		if err := account.writer.flush(ctx); err != nil {
			return err
		}
	}

	// The indexes go back before anything reads: the shape report is also the
	// first real query against the data, and measuring it without them would
	// measure the wrong thing.
	indexing := time.Now()
	g.restoreIndexes(ctx)
	g.stats.Indexing = time.Since(indexing)

	for _, account := range g.accounts {

		for _, site := range account.sites {
			if err := setStatsStart(ctx, g.control, site.seeded.ID, g.start); err != nil {
				return err
			}
		}

		if err := buildRollups(ctx, account.account); err != nil {
			return err
		}
	}

	verifying := time.Now()

	report, err := measure(ctx, g.opts.DataDir, g.accounts, g.start, g.days)
	if err != nil {
		return err
	}

	g.stats.Verifying = time.Since(verifying)

	g.stats.Report = report
	g.stats.Sessions = report.Sessions

	return nil
}

// restoreIndexes rebuilds every index the run dropped. It is safe to call twice
// — the second call has nothing left to do — because it runs both on the normal
// path and from a deferred cleanup after a failure.
func (g *generator) restoreIndexes(ctx context.Context) {
	for _, account := range g.accounts {
		if len(account.suspended) == 0 {
			continue
		}

		if err := restoreIndexes(ctx, account.account.Writer(), account.suspended); err != nil {
			fmt.Fprintf(g.opts.Out, "  %v\n", err)
			continue
		}

		account.suspended = nil
	}
}

// removeSeeded deletes the databases a previous run wrote. It removes the
// control database and the account directory and nothing else: the salt key,
// the geolocation databases and the refreshed bot lists are expensive to
// replace and have nothing to do with the seeded data.
func removeSeeded(dataDir string) error {
	control := filepath.Join(dataDir, config.ControlDatabaseName)

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(control + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("seed: remove %s: %w", control+suffix, err)
		}
	}

	if err := os.RemoveAll(filepath.Join(dataDir, config.AccountDatabaseDir)); err != nil {
		return fmt.Errorf("seed: remove account databases: %w", err)
	}

	// The session snapshot belongs to whatever was running before. Restoring it
	// on top of a freshly seeded dataset would resurrect visits that no longer
	// have a database to live in.
	if err := os.Remove(ingest.SessionFilePath(dataDir)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("seed: remove session snapshot: %w", err)
	}

	return nil
}

// byteSource turns a seeded generator into an io.Reader. Both the event ids and
// the daily salts are drawn from one, because a reproducible dataset cannot have
// a random number anywhere in it — and the salt in particular decides every
// visitor id in the database.
type byteSource struct {
	rng *rand.Rand
}

// Read fills a buffer eight bytes at a time.
func (s *byteSource) Read(p []byte) (int, error) {
	for i := 0; i < len(p); i += 8 {
		var word [8]byte
		binary.LittleEndian.PutUint64(word[:], s.rng.Uint64())
		copy(p[i:], word[:])
	}

	return len(p), nil
}

// numberPtr renders an integer as the JSON number the payload carries. The
// payload fields are pointers so that "absent" and "zero" stay distinguishable,
// which several of the pipeline's rules turn on.
func numberPtr(value int64) *json.Number {
	number := json.Number(strconv.FormatInt(value, 10))

	return &number
}

// min64 is the smaller of two values.
func min64(a, b int64) int64 {
	if a < b {
		return a
	}

	return b
}
