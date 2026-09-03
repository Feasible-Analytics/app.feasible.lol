//
// fetch.go
// Rebuilding the datacentre list from the providers that publish their own ranges.
//
// Created: 2026-09-03
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package lists

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// UserAgent identifies us to the services below. Several of them rate-limit an
// unnamed client harder than a named one, and a maintainer who wants to know
// who is fetching hourly should be able to find out.
const UserAgent = "feasible-lol-lists/1.0 (+https://feasible.lol)"

// FetchTimeout bounds one source. Azure's file is four megabytes and RIPEstat
// is occasionally slow, so this is generous; the point is only that a hung
// connection cannot stall the whole rebuild forever.
const FetchTimeout = 90 * time.Second

// Source is one upstream that publishes address ranges.
//
// Every entry here is the provider describing its own network, which is the
// only kind of source worth trusting for this: a third-party aggregation is
// someone else's guess, and a wrong guess here deletes real visitors from
// somebody's dashboard.
type Source struct {
	Name string
	URL  string

	// Parse pulls CIDR strings out of whatever shape this provider publishes.
	Parse func(body []byte) ([]string, error)
}

// DatacenterSources is every upstream the baseline is built from.
//
// What is missing is as deliberate as what is here. Cloudflare, Fastly and
// Akamai are absent because their address space carries real people: Cloudflare
// WARP is a consumer VPN and iCloud Private Relay egresses through the other
// two, so listing them would classify a large slice of ordinary Safari traffic
// as automated.
func DatacenterSources() []Source {
	sources := []Source{
		{Name: "AWS", URL: "https://ip-ranges.amazonaws.com/ip-ranges.json", Parse: parseAWS},
		{Name: "Google Cloud", URL: "https://www.gstatic.com/ipranges/cloud.json", Parse: parseGoogleCloud},
		{Name: "Oracle Cloud", URL: "https://docs.oracle.com/en-us/iaas/tools/public_ip_ranges.json", Parse: parseOracle},
		{Name: "DigitalOcean", URL: "https://www.digitalocean.com/geo/google.csv", Parse: parseGeofeed},
		{Name: "Linode", URL: "https://geoip.linode.com/", Parse: parseGeofeed},
		{Name: "Vultr", URL: "https://geofeed.constant.com/", Parse: parseGeofeed},
	}

	// Azure's file name carries the date it was published, so its URL has to be
	// discovered rather than written down. It is added by the caller.

	// Providers that publish nothing machine-readable are covered through the
	// routing table instead, by the networks they announce.
	for _, host := range hostingASNs() {
		sources = append(sources, Source{
			Name:  host.name,
			URL:   "https://stat.ripe.net/data/announced-prefixes/data.json?resource=AS" + host.asn,
			Parse: parseRIPEstat,
		})
	}

	return sources
}

// hostingASN is one provider that has to be looked up by the networks it
// announces because it publishes no range file of its own.
type hostingASN struct {
	name string
	asn  string
}

// hostingASNs are the compute providers that publish no feed. They are large
// enough that leaving them out is the difference between catching a scraper
// farm and watching it inflate a customer's numbers.
//
// Networks that exist to carry consumer VPN exits are not here, for the same
// reason Cloudflare is not: a browser really does originate from a VPN exit,
// and the person driving it is real. They are also impossible to cover
// consistently — one provider's exits sit under several unrelated autonomous
// systems, so listing one of them classifies the same person differently
// depending on which city they picked.
func hostingASNs() []hostingASN {
	return []hostingASN{
		{"Alibaba Cloud", "45102"},
		{"Tencent Cloud", "132203"},
		{"Huawei Cloud", "136907"},
		{"Hetzner", "24940"},
		{"OVH", "16276"},
		{"Scaleway", "12876"},
		{"Contabo", "51167"},
		{"Leaseweb", "60781"},
		{"Choopa/Vultr", "20473"},
	}
}

// azureBase is the directory Microsoft publishes the service-tag file into.
// The directory is stable; only the file name moves.
const azureBase = "https://download.microsoft.com/download/7/1/D/71D86715-5596-4529-9B13-DA13A5DE5B63/ServiceTags_Public_"

// AzureSource finds the current Azure service-tag file.
//
// Microsoft publishes it weekly with the publication date in the file name and
// no "latest" alias, so the only way to name it is to walk back through recent
// Mondays until one answers.
func AzureSource(ctx context.Context, client *http.Client, now time.Time) (Source, error) {
	return azureSourceFrom(ctx, client, azureBase, now)
}

// azureSourceFrom is the walk, with the directory as an argument so a test can
// serve one. A function that can only ever talk to download.microsoft.com
// cannot be tested at all, and the walk is the part most likely to break.
func azureSourceFrom(ctx context.Context, client *http.Client, base string, now time.Time) (Source, error) {
	// Back to the most recent Monday, then eight weeks back from there. A gap
	// longer than that is Microsoft changing the scheme, which is a person's
	// problem rather than something to paper over.
	day := now.UTC()
	for day.Weekday() != time.Monday {
		day = day.AddDate(0, 0, -1)
	}

	for week := 0; week < 8; week++ {
		url := base + day.AddDate(0, 0, -7*week).Format("20060102") + ".json"

		if ok, err := exists(ctx, client, url); err != nil {
			return Source{}, err
		} else if ok {
			return Source{Name: "Azure", URL: url, Parse: parseAzure}, nil
		}
	}

	return Source{}, fmt.Errorf("no Azure service tag file found in the last eight weeks")
}

// exists reports whether a URL answers 200 to a HEAD.
func exists(ctx context.Context, client *http.Client, url string) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("User-Agent", UserAgent)

	response, err := client.Do(request)
	if err != nil {
		return false, err
	}
	defer func() { _ = response.Body.Close() }()

	_, _ = io.Copy(io.Discard, response.Body)

	return response.StatusCode == http.StatusOK, nil
}

// Fetch downloads and parses one source.
func Fetch(ctx context.Context, client *http.Client, source Source) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source.Name, err)
	}
	request.Header.Set("User-Agent", UserAgent)

	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source.Name, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", source.Name, response.Status)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source.Name, err)
	}

	cidrs, err := source.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source.Name, err)
	}

	// A provider that renames a field publishes a document that still parses
	// and yields nothing. Treating that as "this network announces no
	// addresses" would drop the source from the list with only a cheerful
	// zero in the output to show for it.
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("%s: parsed no prefixes, so its format has probably changed", source.Name)
	}

	return cidrs, nil
}

// parseAWS reads the ip-ranges.json Amazon publishes for exactly this purpose.
func parseAWS(body []byte) ([]string, error) {
	var doc struct {
		Prefixes []struct {
			IPPrefix string `json:"ip_prefix"`
		} `json:"prefixes"`
		IPv6Prefixes []struct {
			IPv6Prefix string `json:"ipv6_prefix"`
		} `json:"ipv6_prefixes"`
	}

	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}

	cidrs := make([]string, 0, len(doc.Prefixes)+len(doc.IPv6Prefixes))
	for _, prefix := range doc.Prefixes {
		cidrs = append(cidrs, prefix.IPPrefix)
	}
	for _, prefix := range doc.IPv6Prefixes {
		cidrs = append(cidrs, prefix.IPv6Prefix)
	}

	return cidrs, nil
}

// parseGoogleCloud reads cloud.json, which carries one of the two prefix
// fields per entry depending on family.
func parseGoogleCloud(body []byte) ([]string, error) {
	var doc struct {
		Prefixes []struct {
			IPv4Prefix string `json:"ipv4Prefix"`
			IPv6Prefix string `json:"ipv6Prefix"`
		} `json:"prefixes"`
	}

	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}

	cidrs := make([]string, 0, len(doc.Prefixes))
	for _, prefix := range doc.Prefixes {
		if prefix.IPv4Prefix != "" {
			cidrs = append(cidrs, prefix.IPv4Prefix)
		}
		if prefix.IPv6Prefix != "" {
			cidrs = append(cidrs, prefix.IPv6Prefix)
		}
	}

	return cidrs, nil
}

// parseOracle reads the per-region CIDR blocks Oracle publishes.
func parseOracle(body []byte) ([]string, error) {
	var doc struct {
		Regions []struct {
			CIDRs []struct {
				CIDR string `json:"cidr"`
			} `json:"cidrs"`
		} `json:"regions"`
	}

	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}

	var cidrs []string
	for _, region := range doc.Regions {
		for _, block := range region.CIDRs {
			cidrs = append(cidrs, block.CIDR)
		}
	}

	return cidrs, nil
}

// parseAzure reads the service-tag file. The same address appears under several
// tags, so this returns far more entries than there are distinct ranges; the
// merge afterwards is what makes that harmless.
func parseAzure(body []byte) ([]string, error) {
	var doc struct {
		Values []struct {
			Properties struct {
				AddressPrefixes []string `json:"addressPrefixes"`
			} `json:"properties"`
		} `json:"values"`
	}

	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}

	var cidrs []string
	for _, value := range doc.Values {
		cidrs = append(cidrs, value.Properties.AddressPrefixes...)
	}

	return cidrs, nil
}

// parseRIPEstat reads the prefixes an autonomous system announces.
func parseRIPEstat(body []byte) ([]string, error) {
	var doc struct {
		Data struct {
			Prefixes []struct {
				Prefix string `json:"prefix"`
			} `json:"prefixes"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}

	cidrs := make([]string, 0, len(doc.Data.Prefixes))
	for _, prefix := range doc.Data.Prefixes {
		cidrs = append(cidrs, prefix.Prefix)
	}

	return cidrs, nil
}

// parseGeofeed reads an RFC 8805 self-published geofeed, whose first column is
// the prefix and whose remaining columns are the location we do not want.
func parseGeofeed(body []byte) ([]string, error) {
	reader := csv.NewReader(bytes.NewReader(body))
	reader.FieldsPerRecord = -1
	reader.Comment = '#'

	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}

	var cidrs []string
	for _, record := range records {
		if len(record) == 0 {
			continue
		}

		if field := strings.TrimSpace(record[0]); field != "" {
			cidrs = append(cidrs, field)
		}
	}

	return cidrs, nil
}

// Merge normalises, deduplicates and collapses a pile of CIDR blocks into the
// smallest equivalent set.
//
// It matters more than it looks: the raw sources run to roughly 135,000
// prefixes, mostly because Azure lists every address under several service
// tags, and they collapse to about a tenth of that. That is the difference
// between a list worth embedding and one that is not.
func Merge(cidrs []string) []string {
	var v4, v6 []netip.Prefix

	for _, raw := range cidrs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			continue
		}

		prefix = unmap(prefix)

		prefix = prefix.Masked()
		if !routable(prefix) {
			continue
		}

		if prefix.Addr().Is4() {
			v4 = append(v4, prefix)
		} else {
			v6 = append(v6, prefix)
		}
	}

	merged := append(collapse(v4), collapse(v6)...)

	// Numeric order, not the lexicographic order the strings would take. A
	// regeneration changes a few hundred lines, and sorted by address they land
	// next to the network they belong to instead of scattered through the file,
	// which is the difference between a reviewable diff and an unread one.
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Addr().Is4() != merged[j].Addr().Is4() {
			return merged[i].Addr().Is4()
		}

		if merged[i].Addr() != merged[j].Addr() {
			return merged[i].Addr().Less(merged[j].Addr())
		}

		return merged[i].Bits() < merged[j].Bits()
	})

	out := make([]string, 0, len(merged))
	for _, prefix := range merged {
		out = append(out, prefix.String())
	}

	return out
}

// reserved are the ranges no client address can legitimately come from:
// private, loopback, link-local, carrier NAT, multicast, and the blocks set
// aside for documentation and benchmarking.
//
// They are filtered because the feeds really do contain them — Vultr's own
// geofeed places the three RFC 5737 documentation blocks in Lithia Springs,
// Georgia — and a reserved range in this list is worse than useless. It can
// never match real traffic, and it silently classifies every test fixture and
// every internal health check written against the documentation ranges.
var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("ff00::/8"),
}

// unmap rewrites a v4 address written in its v6-mapped form as a plain v4
// prefix. Left alone it sorts, merges and stores as IPv6, and the range set
// then measures its length against 128 bits rather than 32 — turning a whole
// network into a single address with nothing to show for it.
func unmap(prefix netip.Prefix) netip.Prefix {
	addr := prefix.Addr()
	if !addr.Is4In6() {
		return prefix
	}

	return netip.PrefixFrom(addr.Unmap(), prefix.Bits()-96)
}

// routable reports whether a prefix describes address space a real visitor
// could reach us from. A prefix that merely overlaps a reserved block is
// dropped whole rather than split: the only prefixes that do are junk entries,
// and carving a hole in one would invent a range no provider announced.
func routable(prefix netip.Prefix) bool {
	for _, block := range reserved {
		if block.Overlaps(prefix) {
			return false
		}
	}

	return true
}

// collapse merges one address family. Sorting puts a covering prefix before
// everything it covers, so absorbing is a single pass; joining siblings then
// runs until nothing more will join, because merging two halves can create a
// new half of something larger.
func collapse(prefixes []netip.Prefix) []netip.Prefix {
	if len(prefixes) == 0 {
		return nil
	}

	for {
		prefixes = absorb(prefixes)

		joined := join(prefixes)
		if len(joined) == len(prefixes) {
			return prefixes
		}

		prefixes = joined
	}
}

// absorb sorts and drops every prefix already covered by another.
func absorb(prefixes []netip.Prefix) []netip.Prefix {
	sort.Slice(prefixes, func(i, j int) bool {
		if prefixes[i].Addr() != prefixes[j].Addr() {
			return prefixes[i].Addr().Less(prefixes[j].Addr())
		}

		return prefixes[i].Bits() < prefixes[j].Bits()
	})

	kept := prefixes[:0]
	for _, prefix := range prefixes {
		if len(kept) > 0 {
			last := kept[len(kept)-1]
			if last.Bits() <= prefix.Bits() && last.Contains(prefix.Addr()) {
				continue
			}
		}

		kept = append(kept, prefix)
	}

	return kept
}

// join replaces adjacent halves of the same parent with the parent itself, so
// that 10.0.0.0/25 and 10.0.0.128/25 become 10.0.0.0/24.
func join(prefixes []netip.Prefix) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(prefixes))

	for i := 0; i < len(prefixes); i++ {
		if i+1 < len(prefixes) && siblings(prefixes[i], prefixes[i+1]) {
			out = append(out, parent(prefixes[i]))
			i++

			continue
		}

		out = append(out, prefixes[i])
	}

	return out
}

// siblings reports whether two prefixes are the two halves of one parent.
func siblings(a, b netip.Prefix) bool {
	if a.Bits() != b.Bits() || a.Bits() == 0 {
		return false
	}

	if a.Addr().Is4() != b.Addr().Is4() {
		return false
	}

	return parent(a) == parent(b) && a.Addr() != b.Addr()
}

// parent is the prefix one bit shorter, which covers this one and its sibling.
func parent(prefix netip.Prefix) netip.Prefix {
	return netip.PrefixFrom(prefix.Addr(), prefix.Bits()-1).Masked()
}
