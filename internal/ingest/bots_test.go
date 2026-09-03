//
// bots_test.go
// Tests for the three classification lists and the datacentre range lookup.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// TestBotUserAgents checks the baseline list catches what it should and, more
// importantly, leaves real browsers alone. A false positive here removes a real
// visitor from every number on the dashboard.
func TestBotUserAgents(t *testing.T) {
	filter := NewBotFilter()

	bots := []string{
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
		"curl/8.4.0",
		"python-requests/2.31.0",
		"Go-http-client/1.1",
		"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/120.0.0.0 Safari/537.36",
		"GPTBot/1.0",
		"Mozilla/5.0 (compatible; AhrefsBot/7.0; +http://ahrefs.com/robot/)",
	}

	for _, ua := range bots {
		if !filter.IsBotUserAgent(ua) {
			t.Errorf("missed a bot: %q", ua)
		}
	}

	for _, ua := range visitors {
		if filter.IsBotUserAgent(ua.userAgent) {
			t.Errorf("a real browser was classified as a bot: %q", ua.userAgent)
		}
	}

	// An absent user agent is far more often a privacy-conscious browser or a
	// proxy that stripped it — CloudFront strips it by default — than a crawler.
	if filter.IsBotUserAgent("") {
		t.Error("an empty user agent was classified as a bot")
	}
}

// TestDatacenterRanges checks the merged range set answers correctly at both
// edges of every block, in both address families.
func TestDatacenterRanges(t *testing.T) {
	filter := NewBotFilter()
	filter.SetDatacenterRanges([]string{
		"192.0.2.0/24",
		"198.51.100.128/25",
		"203.0.113.7",
		"2001:db8::/32",
	})

	inside := []string{
		"192.0.2.0", "192.0.2.128", "192.0.2.255",
		"198.51.100.128", "198.51.100.255",
		"203.0.113.7",
		"2001:db8::1", "2001:db8:ffff:ffff:ffff:ffff:ffff:ffff",
	}

	for _, value := range inside {
		if !filter.IsDatacenterIP(netip.MustParseAddr(value)) {
			t.Errorf("%s should be inside a datacentre range", value)
		}
	}

	outside := []string{
		"192.0.1.255", "192.0.3.0",
		"198.51.100.127", "198.51.101.0",
		"203.0.113.6", "203.0.113.8",
		"2001:db7:ffff::1", "2001:db9::1",
	}

	for _, value := range outside {
		if filter.IsDatacenterIP(netip.MustParseAddr(value)) {
			t.Errorf("%s should be outside every datacentre range", value)
		}
	}
}

// TestOverlappingRangesAreMerged checks the set collapses overlaps rather than
// storing them twice, because the binary search assumes the ranges are disjoint
// and sorted.
func TestOverlappingRangesAreMerged(t *testing.T) {
	filter := NewBotFilter()
	filter.SetDatacenterRanges([]string{
		"10.0.0.0/16",
		"10.0.128.0/17",
		"10.0.0.0/8",
		"10.1.0.0/16",
	})

	if _, count, _ := filter.Sizes(); count != 1 {
		t.Fatalf("stored %d ranges, want 1 after merging", count)
	}

	for _, value := range []string{"10.0.0.1", "10.128.0.1", "10.255.255.255"} {
		if !filter.IsDatacenterIP(netip.MustParseAddr(value)) {
			t.Errorf("%s should be inside the merged range", value)
		}
	}
	if filter.IsDatacenterIP(netip.MustParseAddr("11.0.0.1")) {
		t.Error("the merge widened the range past its block")
	}
}

// TestUnparseableRangesAreSkipped checks one bad line does not disable
// datacentre detection entirely. These files are refreshed from the open
// internet and a malformed entry is a matter of time.
func TestUnparseableRangesAreSkipped(t *testing.T) {
	filter := NewBotFilter()
	filter.SetDatacenterRanges([]string{"not-a-range", "192.0.2.0/24", "10.0.0.0/99", ""})

	if !filter.IsDatacenterIP(netip.MustParseAddr("192.0.2.9")) {
		t.Fatal("a valid range was lost because another line was malformed")
	}
}

// TestReferrerSpam checks both an exact host and a throwaway subdomain of one,
// which is how these referrers actually arrive.
func TestReferrerSpam(t *testing.T) {
	filter := NewBotFilter()

	for _, host := range []string{"semalt.com", "www.semalt.com", "buttons-for-website.com", "free.semalt.com"} {
		if !filter.IsReferrerSpam(host) {
			t.Errorf("%q should be referrer spam", host)
		}
	}

	for _, host := range []string{"google.com", "news.ycombinator.com", "", "example.com"} {
		if filter.IsReferrerSpam(host) {
			t.Errorf("%q should not be referrer spam", host)
		}
	}
}

// TestListsRefreshFromFiles checks the refreshed files replace the baseline. The
// embedded lists go stale, and a self-hoster frozen at whatever their build
// shipped with is one of the things we are fixing.
func TestListsRefreshFromFiles(t *testing.T) {
	dir := t.TempDir()
	listsDir := filepath.Join(dir, ListsDirName)

	if err := os.MkdirAll(listsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(listsDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(BotListFileName, "# a comment\n\nsome-new-crawler\n")
	write(SpamListFileName, "brand-new-spam.example\n")
	write(DatacenterListFileName, "# ranges\n192.0.2.0/24\n")

	filter := NewBotFilter()
	if err := filter.LoadLists(dir); err != nil {
		t.Fatal(err)
	}

	if !filter.IsBotUserAgent("Some-New-Crawler/1.0") {
		t.Error("the refreshed bot list was not applied")
	}
	if !filter.IsReferrerSpam("brand-new-spam.example") {
		t.Error("the refreshed spam list was not applied")
	}
	if !filter.IsDatacenterIP(netip.MustParseAddr("192.0.2.9")) {
		t.Error("the refreshed datacentre list was not applied")
	}

	// The refreshed list replaces the baseline rather than adding to it, so the
	// operator's file is the whole truth.
	if filter.IsBotUserAgent("curl/8.4.0") {
		t.Error("the baseline list survived a refresh")
	}
}

// TestMissingListFilesLeaveTheBaseline checks an install that has never run the
// refresh job still filters. A missing optional file must never break anything.
func TestMissingListFilesLeaveTheBaseline(t *testing.T) {
	filter := NewBotFilter()

	if err := filter.LoadLists(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	if !filter.IsBotUserAgent("curl/8.4.0") {
		t.Fatal("the baseline list was lost when no files were present")
	}
}

// TestNewFilterCarriesDatacenterRanges checks that a filter nobody configured
// still knows about hosting providers.
//
// A filter with no ranges answers "human" to every address, which does not look
// like a fault from the outside — it looks like clean traffic. Every other test
// in this file supplies its own ranges, so none of them can see an empty
// default.
func TestNewFilterCarriesDatacenterRanges(t *testing.T) {
	filter := NewBotFilter()

	if _, datacenters, _ := filter.Sizes(); datacenters < 10000 {
		t.Fatalf("a fresh filter holds %d datacentre ranges — run `make lists`", datacenters)
	}

	// One address per major provider, straight out of the box with no file
	// loaded and nothing configured.
	for _, addr := range []string{"52.94.76.1", "34.64.32.1", "20.36.0.1", "45.32.0.1"} {
		if !filter.IsDatacenterIP(netip.MustParseAddr(addr)) {
			t.Errorf("%s is not recognised as a datacentre address", addr)
		}
	}

	// And an ordinary visitor still is one.
	if filter.IsDatacenterIP(netip.MustParseAddr("203.0.113.7")) {
		t.Error("a documentation address was classified as a datacentre")
	}
}

// BenchmarkDatacenterLookup keeps the range check inside the per-request budget
// with a list the size of the real one.
func BenchmarkDatacenterLookup(b *testing.B) {
	filter := NewBotFilter()

	ranges := make([]string, 0, 32000)
	for i := 0; i < 32000; i++ {
		ranges = append(ranges, netip.PrefixFrom(
			netip.AddrFrom4([4]byte{byte(i >> 16), byte(i >> 8), byte(i), 0}), 24,
		).String())
	}
	filter.SetDatacenterRanges(ranges)

	addr := netip.MustParseAddr("203.0.113.7")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = filter.IsDatacenterIP(addr)
	}
}
