//
// bots.go
// Bot, datacentre and referrer-spam classification, from lists rather than rules.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"bufio"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
)

// The classification reasons. They are part of the closed set of values the
// `x-feasible-dropped` header can carry, so they are constants shared with the
// response path rather than strings written twice.
const (
	ReasonBot          = "bot"
	ReasonDatacenterIP = "datacenter_ip"
	ReasonReferrerSpam = "referrer_spam"
)

// File names the lists are refreshed into. Bot and spam lists go stale, so the
// embedded copies below are only a baseline; a background job replaces these
// files without a rebuild, and a self-hoster is never frozen at whatever list
// their binary shipped with.
const (
	BotListFileName        = "bots.txt"
	DatacenterListFileName = "datacenters.txt"
	SpamListFileName       = "referrer-spam.txt"
)

// ListsDirName is the subdirectory of the data directory the refreshed lists
// live in.
const ListsDirName = "lists"

// baselineBotTokens are lower-cased substrings that identify an automated
// client. It is a baseline, not the list: real coverage comes from the
// refreshed file, and the point of shipping any at all is that a fresh install
// is not counting search crawlers as visitors on its first day.
//
// Build bots are mostly absent on purpose. Netlify declined to identify its
// crawler and Vercel's is not reliably distinguishable, so hostname validation
// at the account writer catches that traffic instead; chasing it here would be
// effort spent on a problem that is solved somewhere else.
var baselineBotTokens = []string{
	"bot", "crawler", "spider", "crawl", "slurp",
	"headlesschrome", "phantomjs", "puppeteer", "playwright", "selenium",
	"curl/", "wget/", "python-requests", "python-urllib", "go-http-client",
	"java/", "okhttp", "apache-httpclient", "libwww-perl", "guzzlehttp",
	"axios/", "node-fetch", "got (", "httpie", "postmanruntime", "insomnia",
	"lighthouse", "pagespeed", "gtmetrix", "pingdom", "uptimerobot",
	"statuscake", "site24x7", "newrelicpinger", "datadog", "checkly",
	"ahrefs", "semrush", "mj12", "dotbot", "petalbot", "bytespider",
	"facebookexternalhit", "whatsapp", "telegrambot", "slackbot", "discordbot",
	"embedly", "quora link preview", "vkshare", "redditbot", "linkedinbot",
	"applebot", "duckduckbot", "yandexbot", "baiduspider", "bingpreview",
	"feedfetcher", "feedly", "rss", "archive.org_bot", "ia_archiver",
	"gptbot", "ccbot", "claudebot", "perplexitybot", "google-extended",
	"chatgpt-user", "oai-searchbot", "anthropic-ai", "amazonbot", "meta-externalagent",
	"scrapy", "zgrab", "masscan", "nmap", "nuclei", "sqlmap",
}

// baselineSpamDomains are referrers that exist only to appear in somebody's
// analytics. They are a small nuisance rather than a large one, but a referral
// row nobody can explain costs a support conversation every time.
var baselineSpamDomains = []string{
	"semalt.com", "buttons-for-website.com", "darodar.com", "ilovevitaly.com",
	"econom.co", "savetubevideo.com", "kambasoft.com", "priceg.com",
	"cenoval.ru", "hulfingtonpost.com", "bestwebsitesawards.com",
	"traffic2money.com", "trafficmonetize.org", "success-seo.com",
	"free-social-buttons.com", "4webmasters.org", "get-free-traffic-now.com",
	"video--production.com", "sitevaluation.org", "rankings-analytics.com",
}

// BotFilter classifies a request against the three lists. The lists are held
// behind an atomic pointer so the refresh job can swap a whole list in without
// taking a lock on the ingest path — a refresh is rare and a lookup is not.
type BotFilter struct {
	bots        atomic.Pointer[[]string]
	spam        atomic.Pointer[map[string]struct{}]
	datacenters atomic.Pointer[rangeSet]
}

// NewBotFilter builds a filter carrying only the embedded baselines. Loading
// the refreshed files is a separate call, because a missing list file must
// leave a working filter rather than a nil one.
func NewBotFilter() *BotFilter {
	filter := &BotFilter{}

	tokens := append([]string(nil), baselineBotTokens...)
	filter.bots.Store(&tokens)

	spam := make(map[string]struct{}, len(baselineSpamDomains))
	for _, domain := range baselineSpamDomains {
		spam[domain] = struct{}{}
	}
	filter.spam.Store(&spam)

	empty := &rangeSet{}
	filter.datacenters.Store(empty)

	return filter
}

// LoadLists replaces whatever is in memory with the files under a data
// directory. Every file is optional and a missing one leaves that list at its
// baseline, because an install that has never run the refresh job still has to
// work.
func (f *BotFilter) LoadLists(dataDir string) error {
	dir := filepath.Join(dataDir, ListsDirName)

	if lines, ok, err := readList(filepath.Join(dir, BotListFileName)); err != nil {
		return err
	} else if ok {
		tokens := make([]string, 0, len(lines))
		for _, line := range lines {
			tokens = append(tokens, strings.ToLower(line))
		}
		f.bots.Store(&tokens)
	}

	if lines, ok, err := readList(filepath.Join(dir, SpamListFileName)); err != nil {
		return err
	} else if ok {
		spam := make(map[string]struct{}, len(lines))
		for _, line := range lines {
			spam[strings.ToLower(line)] = struct{}{}
		}
		f.spam.Store(&spam)
	}

	if lines, ok, err := readList(filepath.Join(dir, DatacenterListFileName)); err != nil {
		return err
	} else if ok {
		f.datacenters.Store(newRangeSet(lines))
	}

	return nil
}

// SetDatacenterRanges replaces the datacentre ranges directly. Tests and the
// refresh job both want this without going through a file.
func (f *BotFilter) SetDatacenterRanges(cidrs []string) {
	f.datacenters.Store(newRangeSet(cidrs))
}

// IsBotUserAgent reports whether a user agent identifies an automated client.
// An empty user agent is not treated as a bot on its own: a stripped header is
// far more often a privacy-conscious browser or a proxy that dropped it than a
// crawler, and CloudFront strips it by default.
func (f *BotFilter) IsBotUserAgent(ua string) bool {
	if ua == "" {
		return false
	}

	lower := strings.ToLower(ua)

	for _, token := range *f.bots.Load() {
		if strings.Contains(lower, token) {
			return true
		}
	}

	return false
}

// IsDatacenterIP reports whether an address belongs to a hosting provider. The
// answer is deliberately not "this is a bot": commercial VPN exits are
// datacentre addresses too, and treating the two the same is how the incumbent
// dropped real Mullvad and Proton users for months.
func (f *BotFilter) IsDatacenterIP(addr netip.Addr) bool {
	return f.datacenters.Load().contains(addr)
}

// IsReferrerSpam reports whether a referrer host exists only to appear in
// somebody's analytics.
func (f *BotFilter) IsReferrerSpam(host string) bool {
	if host == "" {
		return false
	}

	host = strings.ToLower(strings.TrimPrefix(host, "www."))
	spam := *f.spam.Load()

	if _, ok := spam[host]; ok {
		return true
	}

	// Spam referrers use throwaway subdomains, so the registrable domain is
	// what the list is worth keeping entries for.
	if root := RootDomain(host); root != host {
		_, ok := spam[root]
		return ok
	}

	return false
}

// Sizes reports how many entries each list holds. The ingestion health panel
// shows them, because a refresh job that quietly started returning an empty
// file looks exactly like a sudden surge of legitimate traffic.
func (f *BotFilter) Sizes() (bots, datacenters, spam int) {
	return len(*f.bots.Load()), f.datacenters.Load().len(), len(*f.spam.Load())
}

// readList reads one newline-delimited list file, skipping comments and blanks.
// A missing file reports absence rather than an error, which is what lets every
// list be optional.
func readList(path string) (lines []string, found bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close list %s: %w", path, closeErr))
		}
	}()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, false, err
	}

	return lines, true, nil
}

// ipRange is one contiguous span of addresses, held as the 16-byte form of both
// ends so v4 and v6 compare with the same code.
type ipRange struct {
	lo [16]byte
	hi [16]byte
}

// rangeSet answers "is this address in any of these ranges" over a list that is
// tens of thousands of entries long. Overlapping ranges are merged and the
// result is sorted at build time, so a lookup is a binary search rather than a
// scan — which is what keeps a per-event check inside the sub-millisecond
// budget for the whole request.
type rangeSet struct {
	ranges []ipRange
}

// newRangeSet parses, sorts and merges a list of CIDR blocks. An unparseable
// line is skipped rather than fatal: these files are refreshed from the open
// internet, and one bad line must not disable datacentre detection entirely.
func newRangeSet(cidrs []string) *rangeSet {
	parsed := make([]ipRange, 0, len(cidrs))

	for _, raw := range cidrs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}

		// A bare address is a range of one, which is how a single abusive host
		// ends up on the list alongside whole allocations.
		if !strings.Contains(raw, "/") {
			addr, err := netip.ParseAddr(raw)
			if err != nil {
				continue
			}
			bytes := to16(addr)
			parsed = append(parsed, ipRange{lo: bytes, hi: bytes})
			continue
		}

		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			continue
		}
		parsed = append(parsed, prefixRange(prefix.Masked()))
	}

	sort.Slice(parsed, func(i, j int) bool {
		return lessBytes(parsed[i].lo, parsed[j].lo)
	})

	merged := parsed[:0]
	for _, r := range parsed {
		if len(merged) > 0 && !lessBytes(merged[len(merged)-1].hi, r.lo) {
			if lessBytes(merged[len(merged)-1].hi, r.hi) {
				merged[len(merged)-1].hi = r.hi
			}
			continue
		}
		merged = append(merged, r)
	}

	return &rangeSet{ranges: merged}
}

// contains binary-searches for the range that could hold an address.
func (s *rangeSet) contains(addr netip.Addr) bool {
	if s == nil || len(s.ranges) == 0 || !addr.IsValid() {
		return false
	}

	key := to16(addr.Unmap())

	// The first range starting after the address is the boundary; the candidate
	// is the one before it.
	index := sort.Search(len(s.ranges), func(i int) bool {
		return lessBytes(key, s.ranges[i].lo)
	})
	if index == 0 {
		return false
	}

	candidate := s.ranges[index-1]

	return !lessBytes(key, candidate.lo) && !lessBytes(candidate.hi, key)
}

// len reports how many merged ranges the set holds.
func (s *rangeSet) len() int {
	if s == nil {
		return 0
	}

	return len(s.ranges)
}

// prefixRange turns a CIDR block into its first and last address.
func prefixRange(prefix netip.Prefix) ipRange {
	lo := to16(prefix.Addr().Unmap())
	hi := lo

	// A v4 prefix is stored in the v4-mapped part of the 16-byte form, so its
	// host bits start 96 bits in.
	bits := prefix.Bits()
	if prefix.Addr().Unmap().Is4() {
		bits += 96
	}

	for i := bits; i < 128; i++ {
		hi[i/8] |= 1 << (7 - uint(i%8))
	}

	return ipRange{lo: lo, hi: hi}
}

// to16 renders an address as 16 bytes so v4 and v6 sort and compare together.
func to16(addr netip.Addr) [16]byte {
	return addr.As16()
}

// lessBytes compares two 16-byte addresses. It is a plain loop rather than
// bytes.Compare on slices so nothing escapes to the heap on the hot path.
func lessBytes(a, b [16]byte) bool {
	for i := 0; i < 16; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}

	return false
}
