//
// catalog.go
// The values a fake visit is built from, and how concentrated each one is.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"fmt"
	"strings"
)

// The shape targets, stated as constants because they are the specification of
// this package rather than tuning. Each exponent is chosen so the head of its
// distribution carries the share named beside it, and a test asserts the
// achieved share rather than trusting the arithmetic.
const (
	// distinctPages is roughly what a mid-sized site accumulates, and it is the
	// number that decides whether the pathname dimension table is realistic.
	// With the entry-page bias on top of it, this exponent puts the ten busiest
	// pages at about half of all pageviews.
	distinctPages = 2000
	pageExponent  = 1.05

	// Sources: the top five take about seventy per cent, which is what every
	// real acquisition report looks like once Direct is included.
	sourceExponent = 1.5

	// Countries: the top ten take about eighty per cent. The exponent is
	// gentler than the others because the tail is a hundred and fifty countries
	// long and a steeper one would leave most of them with no traffic at all.
	countryExponent = 1.5

	// Browser and OS pairs: the top five take about ninety per cent. There are
	// only ever a couple of dozen combinations that matter.
	agentExponent = 2.0

	// Campaigns are far more concentrated than sources — a handful of live
	// campaigns and a tail of one-off links.
	campaignExponent = 1.4
)

// Session length. Roughly sixty per cent of visits are a single pageview, and
// the rest fall away as a power law that still reaches thirty — without the
// tail the session-fold rules are never exercised, and without the head the
// bounce rate is nothing like a real one.
const (
	maxSessionPageviews  = 30
	singlePageviewShare  = 0.60
	sessionLengthExponen = 2.2
)

// siteKind changes both the pages a site has and the shape of its week. A
// documentation site is dead at the weekend and a shop is not, and a seed that
// gave every site the same curve would test the hourly roll-ups against buckets
// no real site produces.
type siteKind string

const (
	kindMarketing siteKind = "marketing"
	kindBlog      siteKind = "blog"
	kindShop      siteKind = "shop"
	kindDocs      siteKind = "docs"
	kindEmpty     siteKind = "empty"
)

// accountState is the billing situation a seeded account is in. All three exist
// in the default dataset because the dashboard has a different empty state, a
// different banner and a different set of allowed actions for each, and none of
// them can be built without data in that state.
type accountState string

const (
	// stateActive is a paying account in good standing.
	stateActive accountState = "active"

	// stateLocked is an account whose subscription has been cancelled. It still
	// has data and still ingests during its grace period; what it loses is
	// dashboard access.
	stateLocked accountState = "locked"

	// stateDormant is an account past the end of its ingestion grace period.
	// Its events are dropped by the pipeline with a reason, which is what makes
	// its site look like a site that stopped reporting rather than a site that
	// never existed.
	stateDormant accountState = "dormant"
)

// siteFixture is one seeded site. Weight is its share of the run's pageviews,
// relative to the other traffic-carrying sites that were selected.
type siteFixture struct {
	Domain      string
	DisplayName string
	Timezone    string
	Kind        siteKind
	Weight      float64

	// Traffic is false for the site that deliberately has no data at all. Every
	// dashboard needs an empty state and nobody ever has one to look at.
	Traffic bool
}

// accountFixture is one seeded account, its owner and its sites.
type accountFixture struct {
	Name       string
	OwnerName  string
	OwnerEmail string
	State      accountState
	Sites      []siteFixture
}

// fixture is the dataset's cast. It is a fixed list rather than something
// generated because the interesting part of a seed is the states it covers —
// locked, dormant, empty, multi-site — and those have to be present every time
// rather than when the dice fall a particular way.
//
// The order matters: `--sites N` takes the first N traffic-carrying sites, so
// the default of five reaches every account and therefore every account state.
var fixture = []accountFixture{
	{
		Name:       "Northwind Trading Co.",
		OwnerName:  "Ada Reyes",
		OwnerEmail: "ada@northwind.example",
		State:      stateActive,
		Sites: []siteFixture{
			{Domain: "northwind.example", DisplayName: "Northwind", Timezone: "America/Los_Angeles", Kind: kindMarketing, Weight: 0.55, Traffic: true},
			{Domain: "blog.northwind.example", DisplayName: "Northwind Blog", Timezone: "America/Los_Angeles", Kind: kindBlog, Weight: 0.20, Traffic: true},
			{Domain: "shop.northwind.example", DisplayName: "Northwind Shop", Timezone: "America/Los_Angeles", Kind: kindShop, Weight: 0.13, Traffic: true},

			// The empty state. A site with no rows at all is the one case every
			// report card has to handle and the one nobody has data for.
			{Domain: "status.northwind.example", DisplayName: "Northwind Status", Timezone: "America/Los_Angeles", Kind: kindEmpty, Weight: 0, Traffic: false},
		},
	},
	{
		Name:       "Harbourline Media",
		OwnerName:  "Tom Okafor",
		OwnerEmail: "tom@harbourline.example",
		State:      stateLocked,
		Sites: []siteFixture{
			{Domain: "harbourline.example", DisplayName: "Harbourline", Timezone: "Europe/London", Kind: kindDocs, Weight: 0.09, Traffic: true},
		},
	},
	{
		Name:       "Quietwater Labs",
		OwnerName:  "Mira Halvorsen",
		OwnerEmail: "mira@quietwater.example",
		State:      stateDormant,
		Sites: []siteFixture{
			{Domain: "quietwater.example", DisplayName: "Quietwater", Timezone: "Etc/UTC", Kind: kindMarketing, Weight: 0.03, Traffic: true},
		},
	},
}

// teamMate is a second person on the first account. A team of one exercises
// none of the membership, invitation or permission surfaces.
const (
	teamMateName  = "Jonah Six"
	teamMateEmail = "jonah@northwind.example"
)

// singletonPath is emitted exactly once in a whole run. A page with one
// pageview ever is what breaks a report that divides by a previous period, and
// it is invisible in data that was generated purely by sampling.
const singletonPath = "/blog/the-lighthouse-keepers-log"

// unvalidatedHostname stands in for traffic arriving from somewhere the site
// does not own — a preview deployment, a staging copy of the site, somebody's
// scraped mirror. Hostname validation lives at the shard and is a later
// milestone; what the seed can do now is make sure the rows it will have to
// classify exist.
const unvalidatedHostname = "preview-42.build-preview.example"

// headPaths are the pages every site of a kind has, in rank order. They carry
// the top of the distribution, which is what makes the concentration realistic:
// the ten busiest pages of a real site are its home page and its nine most
// important ones, not ten arbitrary posts.
var headPaths = map[siteKind][]string{
	kindMarketing: {
		"/", "/pricing", "/features", "/about", "/contact", "/signup", "/login",
		"/customers", "/integrations", "/changelog", "/security", "/careers",
		"/demo", "/compare", "/terms", "/privacy",
	},
	kindBlog: {
		"/", "/archive", "/authors", "/topics/engineering", "/topics/product",
		"/topics/design", "/newsletter", "/rss", "/about", "/topics/analytics",
		"/topics/privacy", "/topics/performance",
	},
	kindShop: {
		"/", "/collections/new", "/collections/best-sellers", "/cart",
		"/checkout", "/checkout/shipping", "/checkout/payment", "/order/complete",
		"/collections/sale", "/account", "/account/orders", "/support/returns",
	},
	kindDocs: {
		"/", "/docs", "/docs/quickstart", "/docs/install", "/docs/api",
		"/docs/api/events", "/docs/api/stats", "/docs/self-hosting",
		"/docs/faq", "/docs/troubleshooting", "/docs/changelog", "/docs/cli",
	},
}

// tailWords build the long tail of paths. Two small word lists crossed together
// produce thousands of plausible slugs with no data file and no repetition,
// which is what a real site's archive looks like from a query's point of view.
var (
	tailAdjectives = []string{
		"quiet", "fast", "honest", "small", "open", "simple", "steady", "clear",
		"bright", "plain", "sharp", "calm", "solid", "light", "brave", "warm",
		"deep", "wide", "true", "kind", "swift", "neat", "bold", "fresh",
		"lean", "keen", "spare", "sound", "even", "ready",
	}
	tailNouns = []string{
		"metrics", "sessions", "funnels", "cohorts", "retention", "latency",
		"budgets", "queries", "indexes", "backups", "migrations", "rollups",
		"dashboards", "alerts", "exports", "webhooks", "segments", "filters",
		"goals", "sources", "campaigns", "devices", "regions", "referrers",
		"privacy", "consent", "sampling", "storage", "pipelines", "shards",
		"caches", "keys", "limits", "onboarding", "pricing", "billing",
		"support", "roadmap", "release", "postmortem", "benchmark", "profile",
		"tracing", "logging", "uptime", "failover", "restore", "audit",
		"schema", "columns", "joins", "vacuum", "checkpoints", "compaction",
		"forecast", "anomaly", "attribution", "channels", "revenue", "currency",
	}
)

// pageCatalog builds one site's pages, head first. The tail is generated rather
// than listed because two thousand realistic paths is what makes the dimension
// table the size a real one is, and no hand-written list stays that long and
// stays readable.
func pageCatalog(kind siteKind) []string {
	head := headPaths[kind]
	if len(head) == 0 {
		head = headPaths[kindMarketing]
	}

	paths := make([]string, 0, distinctPages)
	paths = append(paths, head...)

	prefix := tailPrefix(kind)

	// The two word lists are coprime in length, so walking them at different
	// strides visits every pair before it repeats a single one.
	for i := 0; len(paths) < distinctPages; i++ {
		adjective := tailAdjectives[i%len(tailAdjectives)]
		noun := tailNouns[(i/len(tailAdjectives)+i)%len(tailNouns)]

		paths = append(paths, fmt.Sprintf("%s/%s-%s-%d", prefix, adjective, noun, i+1))
	}

	return paths
}

// tailPrefix is where a site's long tail lives. A shop's tail is products and a
// documentation site's is reference pages, and grouping them under one prefix
// is what makes a path-prefix filter — the most common filter there is — have
// something real to match.
func tailPrefix(kind siteKind) string {
	switch kind {
	case kindBlog:
		return "/blog"
	case kindShop:
		return "/products"
	case kindDocs:
		return "/docs/reference"
	default:
		return "/resources"
	}
}

// entryBias reweights the page distribution for the first pageview of a visit.
// Landing pages are far more concentrated than pages in general — almost
// everyone arrives on the home page or one of a handful of others — and using
// the same distribution for both would make the entry-page report a copy of the
// top-pages report.
const entryBias = 0.25

// knownSources are hosts the referrer package recognises. They are here so the
// seeded data actually exercises the source mapping and the channel rules
// rather than only the unrecognised-host fallback.
var knownSources = []string{
	"https://www.google.com/", "https://www.google.co.uk/", "https://duckduckgo.com/",
	"https://www.bing.com/search", "https://search.yahoo.com/", "https://yandex.ru/",
	"https://news.ycombinator.com/", "https://www.reddit.com/r/analytics",
	"https://x.com/", "https://t.co/", "https://www.linkedin.com/feed",
	"https://www.facebook.com/", "https://l.facebook.com/", "https://www.instagram.com/",
	"https://www.youtube.com/", "https://github.com/", "https://stackoverflow.com/questions",
	"https://medium.com/", "https://dev.to/", "https://lobste.rs/",
	"https://www.producthunt.com/", "https://mastodon.social/", "https://bsky.app/",
	"https://www.quora.com/", "https://www.pinterest.com/", "https://t.me/",
	"https://web.telegram.org/", "https://slack.com/", "https://discord.com/",
	"https://mail.google.com/", "https://outlook.live.com/", "https://substack.com/",
	"https://chatgpt.com/", "https://www.perplexity.ai/", "https://claude.ai/",
	"https://www.baidu.com/", "https://www.ecosia.org/", "https://search.brave.com/",
	"https://www.startpage.com/", "https://vk.com/",
}

// tailSourceHosts are invented, and deliberately so: the long tail of a real
// referrer report is hundreds of small sites, and naming real ones in a fixture
// would put words in their mouths. The .example TLD is reserved for exactly
// this.
var tailSourceHosts = []string{
	"weekly", "digest", "roundup", "letter", "notes", "journal", "review",
	"daily", "monthly", "reader", "wire", "post", "times", "report",
}

// sourceCatalog builds the referrer list: the recognised hosts first, then a
// generated tail out to roughly two hundred distinct sources.
func sourceCatalog() []string {
	sources := make([]string, 0, 200)
	sources = append(sources, knownSources...)

	for i := 0; len(sources) < 200; i++ {
		word := tailSourceHosts[i%len(tailSourceHosts)]
		noun := tailNouns[(i*7)%len(tailNouns)]

		sources = append(sources, fmt.Sprintf("https://%s%s.example/%s", noun, word, tailAdjectives[i%len(tailAdjectives)]))
	}

	return sources
}

// place is one geolocation answer. It is the same shape the mmdb reader
// produces, because the seed hands these straight back through the Locator
// interface rather than looking anything up: the lookup is not what a seeded
// dataset is for, and skipping it is measurably faster.
type place struct {
	Country string
	Region  string
	City    string

	// Weight is this place's share within its country, so the country-level
	// concentration stays exactly what the Zipf exponent says whether a country
	// has one city or eight.
	Weight float64
}

// countryPlaces lists the countries in traffic-rank order with the cities
// worth naming. The top of the list carries most of the traffic, so those get
// real regions and cities and the tail gets a country and nothing else —
// which is also what a country-level database returns for most of the world.
var countryPlaces = [][]place{
	{
		{Country: "US", Region: "US-CA", City: "San Francisco", Weight: 0.16},
		{Country: "US", Region: "US-NY", City: "New York", Weight: 0.15},
		{Country: "US", Region: "US-TX", City: "Austin", Weight: 0.11},
		{Country: "US", Region: "US-WA", City: "Seattle", Weight: 0.10},
		{Country: "US", Region: "US-IL", City: "Chicago", Weight: 0.09},
		{Country: "US", Region: "US-MA", City: "Boston", Weight: 0.08},
		{Country: "US", Region: "US-CO", City: "Denver", Weight: 0.07},
		{Country: "US", Region: "US-GA", City: "Atlanta", Weight: 0.07},
		{Country: "US", Region: "US-FL", City: "Miami", Weight: 0.06},
		{Country: "US", Region: "US-OR", City: "Portland", Weight: 0.06},
		{Country: "US", Region: "", City: "", Weight: 0.05},
	},
	{
		{Country: "GB", Region: "England", City: "London", Weight: 0.55},
		{Country: "GB", Region: "England", City: "Manchester", Weight: 0.14},
		{Country: "GB", Region: "Scotland", City: "Edinburgh", Weight: 0.12},
		{Country: "GB", Region: "England", City: "Bristol", Weight: 0.10},
		{Country: "GB", Region: "Wales", City: "Cardiff", Weight: 0.09},
	},
	{
		{Country: "DE", Region: "Berlin", City: "Berlin", Weight: 0.38},
		{Country: "DE", Region: "Bavaria", City: "Munich", Weight: 0.24},
		{Country: "DE", Region: "Hamburg", City: "Hamburg", Weight: 0.20},
		{Country: "DE", Region: "Hesse", City: "Frankfurt", Weight: 0.18},
	},
	{
		{Country: "CA", Region: "CA-ON", City: "Toronto", Weight: 0.44},
		{Country: "CA", Region: "CA-BC", City: "Vancouver", Weight: 0.31},
		{Country: "CA", Region: "CA-QC", City: "Montreal", Weight: 0.25},
	},
	{
		{Country: "FR", Region: "Île-de-France", City: "Paris", Weight: 0.62},
		{Country: "FR", Region: "Auvergne-Rhône-Alpes", City: "Lyon", Weight: 0.21},
		{Country: "FR", Region: "Occitanie", City: "Toulouse", Weight: 0.17},
	},
	{
		{Country: "IN", Region: "Karnataka", City: "Bengaluru", Weight: 0.40},
		{Country: "IN", Region: "Maharashtra", City: "Mumbai", Weight: 0.24},
		{Country: "IN", Region: "Delhi", City: "New Delhi", Weight: 0.20},
		{Country: "IN", Region: "Telangana", City: "Hyderabad", Weight: 0.16},
	},
	{
		{Country: "AU", Region: "AU-NSW", City: "Sydney", Weight: 0.46},
		{Country: "AU", Region: "AU-VIC", City: "Melbourne", Weight: 0.34},
		{Country: "AU", Region: "AU-QLD", City: "Brisbane", Weight: 0.20},
	},
	{
		{Country: "NL", Region: "North Holland", City: "Amsterdam", Weight: 0.68},
		{Country: "NL", Region: "South Holland", City: "Rotterdam", Weight: 0.32},
	},
	{
		{Country: "BR", Region: "São Paulo", City: "São Paulo", Weight: 0.58},
		{Country: "BR", Region: "Rio de Janeiro", City: "Rio de Janeiro", Weight: 0.42},
	},
	{
		{Country: "JP", Region: "Tokyo", City: "Tokyo", Weight: 0.72},
		{Country: "JP", Region: "Osaka", City: "Osaka", Weight: 0.28},
	},
	{{Country: "SE", Region: "Stockholm", City: "Stockholm", Weight: 1}},
	{{Country: "ES", Region: "Madrid", City: "Madrid", Weight: 1}},
	{{Country: "IT", Region: "Lazio", City: "Rome", Weight: 1}},
	{{Country: "PL", Region: "Masovia", City: "Warsaw", Weight: 1}},
	{{Country: "IE", Region: "Leinster", City: "Dublin", Weight: 1}},
	{{Country: "NO", Region: "Oslo", City: "Oslo", Weight: 1}},
	{{Country: "DK", Region: "Capital Region", City: "Copenhagen", Weight: 1}},
	{{Country: "FI", Region: "Uusimaa", City: "Helsinki", Weight: 1}},
	{{Country: "CH", Region: "Zurich", City: "Zurich", Weight: 1}},
	{{Country: "AT", Region: "Vienna", City: "Vienna", Weight: 1}},
	{{Country: "BE", Region: "Brussels", City: "Brussels", Weight: 1}},
	{{Country: "PT", Region: "Lisbon", City: "Lisbon", Weight: 1}},
	{{Country: "SG", Region: "", City: "Singapore", Weight: 1}},
	{{Country: "NZ", Region: "Auckland", City: "Auckland", Weight: 1}},
	{{Country: "MX", Region: "Mexico City", City: "Mexico City", Weight: 1}},
}

// tailCountries fill the list out to roughly a hundred and fifty. They have no
// region and no city, which is exactly what a country-level lookup returns and
// is worth having in the data: the dashboard has to render a country with no
// city breakdown without looking broken.
var tailCountries = []string{
	"AR", "CL", "CO", "PE", "UY", "CR", "PA", "DO", "GT", "EC",
	"ZA", "NG", "KE", "GH", "EG", "MA", "TN", "DZ", "SN", "UG",
	"TZ", "ET", "ZM", "ZW", "BW", "NA", "MU", "RW", "CI", "CM",
	"CN", "HK", "TW", "KR", "TH", "VN", "PH", "ID", "MY", "BD",
	"PK", "LK", "NP", "KH", "MM", "MN", "KZ", "UZ", "GE", "AM",
	"AE", "SA", "IL", "TR", "QA", "KW", "BH", "OM", "JO", "LB",
	"CZ", "SK", "HU", "RO", "BG", "GR", "HR", "SI", "RS", "BA",
	"LT", "LV", "EE", "UA", "BY", "MD", "AL", "MK", "ME", "IS",
	"LU", "MT", "CY", "MC", "LI", "AD", "SM", "FO", "GL", "GI",
	"JM", "TT", "BB", "BS", "BM", "KY", "PR", "FJ", "PG", "NC",
	"RU", "BO", "PY", "VE", "HN", "NI", "SV", "BZ", "SR", "GY",
	"MZ", "AO", "MW", "SD", "LY", "GA", "BJ", "TG", "NE", "ML",
}

// placeCatalog builds the geolocation distribution: every place with the
// country's Zipf weight split across its cities. The result is a single flat
// list a sampler can index, which is what lets the country concentration be a
// property of the catalogue rather than of the code that draws from it.
func placeCatalog() ([]place, []float64) {
	countryWeights := zipf(len(countryPlaces)+len(tailCountries), countryExponent)

	var (
		places  []place
		weights []float64
	)

	for i, cities := range countryPlaces {
		for _, city := range cities {
			places = append(places, city)
			weights = append(weights, countryWeights[i]*city.Weight)
		}
	}

	for i, code := range tailCountries {
		places = append(places, place{Country: code, Weight: 1})
		weights = append(weights, countryWeights[len(countryPlaces)+i])
	}

	return places, weights
}

// agent is one browser, its user agent string and the viewport it reports. The
// width matters as much as the header: it is what the screen-size dimension is
// bucketed from, and a desktop header with a phone's viewport would make every
// device report disagree with itself.
type agent struct {
	UA    string
	Width int
}

// agentCatalog is the browser and operating system pairs, in share order. Thirty
// is the whole realistic universe: the top five are ninety per cent of the web
// and the rest is a tail that still has to render on a dashboard.
//
// The build numbers vary within a browser on purpose. They do not change the
// stored browser version — that is trimmed to the major — but they do change
// the fingerprint, which is how one household or office produces several
// visitors from one address.
var agentCatalog = []agent{
	{UA: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36", Width: 1920},
	{UA: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36", Width: 1512},
	{UA: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Mobile/15E148 Safari/604.1", Width: 390},
	{UA: "Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36", Width: 412},
	{UA: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Safari/605.1.15", Width: 1440},
	{UA: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0", Width: 1920},
	{UA: "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:143.0) Gecko/20100101 Firefox/143.0", Width: 1680},
	{UA: "Mozilla/5.0 (Linux; Android 15; SM-S928B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/27.0 Chrome/139.0.0.0 Mobile Safari/537.36", Width: 384},
	{UA: "Mozilla/5.0 (iPad; CPU OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Safari/604.1", Width: 1024},
	{UA: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36", Width: 1920},
	{UA: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:143.0) Gecko/20100101 Firefox/143.0", Width: 1440},
	{UA: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_6_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1", Width: 375},
	{UA: "Mozilla/5.0 (Linux; Android 14; moto g play) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36", Width: 360},
	{UA: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 OPR/125.0.0.0", Width: 1600},
	{UA: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Edg/141.0.0.0", Width: 1512},
	{UA: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/141.0.0.0 Mobile/15E148 Safari/604.1", Width: 390},
	{UA: "Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:143.0) Gecko/20100101 Firefox/143.0", Width: 1920},
	{UA: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Vivaldi/7.6", Width: 1728},
	{UA: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 Brave/141", Width: 1512},
	{UA: "Mozilla/5.0 (Linux; Android 15; SM-A556B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Mobile Safari/537.36", Width: 412},
	{UA: "Mozilla/5.0 (iPad; CPU OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/140.0.0.0 Mobile/15E148 Safari/604.1", Width: 820},
	{UA: "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36", Width: 1366},
	{UA: "Mozilla/5.0 (Windows NT 6.1; Win64; x64; rv:128.0) Gecko/20100101 Firefox/128.0", Width: 1366},
	{UA: "Mozilla/5.0 (Linux; Android 13; SM-T870) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/139.0.0.0 Safari/537.36", Width: 800},
	{UA: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/139.0 Mobile/15E148 Safari/605.1.15", Width: 393},
	{UA: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 YaBrowser/25.8.0", Width: 1536},
	{UA: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36 DuckDuckGo/7", Width: 1440},
	{UA: "Mozilla/5.0 (Linux; Android 15; Pixel Tablet) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36", Width: 1600},
	{UA: "Mozilla/5.0 (Windows NT 10.0; Win64; x64; Trident/7.0; rv:11.0) like Gecko", Width: 1280},
	{UA: "Mozilla/5.0 (SMART-TV; Linux; Tizen 7.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/108.0.0.0 Safari/537.36", Width: 1920},
}

// botAgents are the automated clients every site gets. They are classified
// rather than dropped, so seeded data has to contain some: the bot toggle on
// the dashboard has nothing to toggle otherwise.
var botAgents = []string{
	"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
	"Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
	"Mozilla/5.0 (compatible; AhrefsBot/7.0; +http://ahrefs.com/robot/)",
	"Mozilla/5.0 (compatible; GPTBot/1.2; +https://openai.com/gptbot)",
	"curl/8.7.1",
	"python-requests/2.32.3",
	"Mozilla/5.0 (compatible; UptimeRobot/2.0; http://uptimerobot.com/)",
}

// languages are the Accept-Language headers, weighted the way the country list
// is. Only the first tag is stored, so the quality values are here purely
// because real headers carry them and the parser has to keep working.
var languages = []string{
	"en-US,en;q=0.9", "en-GB,en;q=0.9", "de-DE,de;q=0.9,en;q=0.8",
	"fr-FR,fr;q=0.9,en;q=0.8", "es-ES,es;q=0.9,en;q=0.8", "pt-BR,pt;q=0.9",
	"nl-NL,nl;q=0.9,en;q=0.8", "sv-SE,sv;q=0.9,en;q=0.8", "ja-JP,ja;q=0.9",
	"it-IT,it;q=0.9", "pl-PL,pl;q=0.9", "en-CA,en;q=0.9,fr;q=0.8",
	"en-AU,en;q=0.9", "en-IN,en;q=0.9,hi;q=0.8", "da-DK,da;q=0.9,en;q=0.8",
	"nb-NO,nb;q=0.9,en;q=0.8", "fi-FI,fi;q=0.9,en;q=0.8", "cs-CZ,cs;q=0.9",
	"tr-TR,tr;q=0.9", "ko-KR,ko;q=0.9",
}

// campaign is one set of acquisition tags. They are sampled as a set rather
// than field by field because a campaign is a real thing somebody set up: a
// medium of "cpc" with a source of "newsletter" is a combination nobody has,
// and generating it would make the channel report nonsense.
type campaign struct {
	Source   string
	Medium   string
	Name     string
	Content  string
	Term     string
	ClickID  string
	Referrer string
}

// campaigns covers every channel the rules can produce that a campaign can
// reach. Without a spread here the channel report is three rows deep and the
// rules that took the longest to get right are never exercised at all.
var campaigns = []campaign{
	{Source: "google", Medium: "cpc", Name: "brand-search", Content: "headline-a", Term: "web analytics", ClickID: "gclid", Referrer: "https://www.google.com/"},
	{Source: "google", Medium: "cpc", Name: "competitor-search", Content: "headline-b", Term: "analytics alternative", ClickID: "gclid", Referrer: "https://www.google.com/"},
	{Source: "bing", Medium: "cpc", Name: "brand-search", Content: "headline-a", ClickID: "msclkid", Referrer: "https://www.bing.com/"},
	{Source: "newsletter", Medium: "email", Name: "product-update-october", Content: "top-button"},
	{Source: "newsletter", Medium: "email", Name: "welcome-series-2", Content: "footer-link"},
	{Source: "facebook", Medium: "paid-social", Name: "retargeting-visitors", Content: "carousel"},
	{Source: "linkedin", Medium: "cpc", Name: "founders-q4", Content: "single-image"},
	{Source: "x", Medium: "social", Name: "launch-thread"},
	{Source: "reddit", Medium: "cpc", Name: "r-analytics-promo"},
	{Source: "youtube", Medium: "video", Name: "walkthrough-launch"},
	{Source: "partner-directory", Medium: "affiliate", Name: "listing-q4"},
	{Source: "podcast", Medium: "audio", Name: "sponsor-read-14"},
	{Source: "sms", Medium: "sms", Name: "cart-recovery"},
	{Source: "display-network", Medium: "display", Name: "awareness-eu"},
	{Source: "conference", Medium: "banner", Name: "booth-qr"},
	{Source: "shopping", Medium: "cpc", Name: "shopping-feed-uk"},
	{Source: "app-push", Medium: "push", Name: "reengagement-7d"},
}

// customEvent is a named event and the properties it carries. Properties are
// what the goals, funnels and filters milestones are built against, so seeded
// data needs the shapes rather than one example of one.
type customEvent struct {
	Name  string
	Props map[string]string

	// Revenue is set for the events that carry money. The currency varies
	// deliberately — three of them — because a revenue report that has only
	// ever seen one currency has never had to decide what to do about two.
	Revenue  bool
	Currency string
}

// customEvents are the events a site fires beyond pageviews. The mix is what a
// real product sends: mostly signups and clicks, occasionally a purchase.
var customEvents = []customEvent{
	{Name: "Signup", Props: map[string]string{"plan": "starter", "referred": "false"}},
	{Name: "Signup", Props: map[string]string{"plan": "growth", "referred": "true"}},
	{Name: "Purchase", Props: map[string]string{"plan": "growth", "seats": "5"}, Revenue: true, Currency: "USD"},
	{Name: "Purchase", Props: map[string]string{"plan": "scale", "seats": "25"}, Revenue: true, Currency: "EUR"},
	{Name: "Purchase", Props: map[string]string{"plan": "starter", "seats": "1"}, Revenue: true, Currency: "GBP"},
	{Name: "Newsletter Signup", Props: map[string]string{"placement": "footer"}},
	{Name: "Outbound Link Click", Props: map[string]string{"url": "https://docs.northwind.example/api"}},
	{Name: "File Download", Props: map[string]string{"file": "quickstart.pdf"}},
	{Name: "Add to Cart", Props: map[string]string{"sku": "NW-114", "quantity": "2"}},
	{Name: "Contact Form", Props: map[string]string{"topic": "pricing"}},
	{Name: "Search", Props: map[string]string{"query": "session timeout"}},
	{Name: "Video Play", Props: map[string]string{"title": "product tour"}},
}

// maxPropsEvent is one event carrying exactly the property cap. Thirty is where
// the pipeline starts dropping properties and reporting the truncation, and
// nothing below the cap ever exercises the boundary.
func maxPropsEvent() map[string]string {
	props := make(map[string]string, 30)

	for i := 0; i < 30; i++ {
		props[fmt.Sprintf("dimension_%02d", i+1)] = fmt.Sprintf("%s-%s", tailAdjectives[i%len(tailAdjectives)], tailNouns[i%len(tailNouns)])
	}

	return props
}

// pageTitle turns a path into the title a browser would send. Titles are their
// own dimension and their own report, and a dataset where every page shares one
// title cannot show that the two group differently.
func pageTitle(path, siteName string) string {
	if path == "/" {
		return siteName
	}

	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.Split(trimmed, "/")
	last := strings.ReplaceAll(parts[len(parts)-1], "-", " ")

	// The trailing number on a generated slug is noise in a title, where in a
	// path it is what makes the page distinct.
	if index := strings.LastIndex(last, " "); index > 0 {
		if _, err := fmt.Sscanf(last[index+1:], "%d", new(int)); err == nil {
			last = last[:index]
		}
	}

	return strings.ToUpper(last[:1]) + last[1:] + " — " + siteName
}
