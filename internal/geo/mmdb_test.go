//
// mmdb_test.go
// Tests for the mmdb reader, driven by a real database built in the test itself.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package geo

import (
	"encoding/binary"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestCityDatabaseAnswersWithNames is the bug this file exists for. The shipped
// database names its subdivisions and cities and numbers neither, so a reader
// that asks for an ISO code and a geoname id decodes without error and returns
// an empty region and an empty city on every single event.
func TestCityDatabaseAnswersWithNames(t *testing.T) {
	locator := openFixture(t)

	got := locator.Lookup(netip.MustParseAddr("81.2.69.142"))

	if got.Country != "GB" {
		t.Errorf("country = %q, want GB", got.Country)
	}
	if got.Subdivision1 != "England" {
		t.Errorf("region = %q, want England", got.Subdivision1)
	}
	if got.City != "London" {
		t.Errorf("city = %q, want London", got.City)
	}
}

// TestSubdivisionCodeBeatsItsName covers the other schema. A paid GeoIP2 file
// carries both, and the code is the one worth keeping: it is stable across
// releases and languages where a name is neither, and this is the check that
// the fallback did not quietly become the only path.
func TestSubdivisionCodeBeatsItsName(t *testing.T) {
	locator := openFixture(t)

	got := locator.Lookup(netip.MustParseAddr("24.24.24.24"))

	if got.Subdivision1 != "US-NY" {
		t.Errorf("region = %q, want US-NY — the ISO code lost to the name", got.Subdivision1)
	}
	if got.City != "Syracuse" {
		t.Errorf("city = %q, want Syracuse", got.City)
	}
}

// TestAddressOutsideTheDatabaseIsUnknown checks the empty record. Most of the
// address space is in no geolocation database at all, and a miss has to be an
// unknown location rather than an error that fails the event.
func TestAddressOutsideTheDatabaseIsUnknown(t *testing.T) {
	locator := openFixture(t)

	if got := locator.Lookup(netip.MustParseAddr("203.0.113.7")); !got.IsZero() {
		t.Fatalf("an address the database does not cover returned %+v", got)
	}
}

// TestNoDatabaseDegradesToUnknown is the rule an optional data file lives by. A
// grey country map is a far smaller problem than a process that will not start,
// and a self-hoster who has not downloaded the city database still has a working
// install.
func TestNoDatabaseDegradesToUnknown(t *testing.T) {
	locator, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("a missing database was reported as an error: %v", err)
	}
	defer func() {
		if err := locator.Close(); err != nil {
			t.Errorf("close locator: %v", err)
		}
	}()

	got := locator.Lookup(netip.MustParseAddr("203.0.113.7"))
	if !got.IsZero() {
		t.Fatalf("Lookup returned %+v with no database, want nothing", got)
	}
}

// TestCorruptDatabaseIsReported checks the distinction that matters
// operationally: a file nobody downloaded is normal, and a file that exists but
// cannot be read is a real problem somebody has to be told about.
func TestCorruptDatabaseIsReported(t *testing.T) {
	dir := t.TempDir()
	geoDir := filepath.Join(dir, DataDirName)

	if err := os.MkdirAll(geoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(geoDir, CountryFileName), []byte("not an mmdb"), 0o644); err != nil {
		t.Fatal(err)
	}

	locator, err := Open(dir)
	if err == nil {
		t.Fatal("a corrupt database was accepted")
	}

	// Even on failure the caller gets something usable, so a bad file cannot
	// leave the pipeline holding a nil Locator.
	if locator == nil {
		t.Fatal("Open returned a nil Locator alongside its error")
	}
	if got := locator.Lookup(netip.MustParseAddr("203.0.113.7")); !got.IsZero() {
		t.Fatal("the fallback Locator returned a location")
	}
}

// TestPrivateAddressesAreNotLookedUp checks the shortcut. A loopback or private
// address is in no geolocation database, and asking is a wasted binary search on
// every request from a developer's laptop or a misconfigured proxy.
func TestPrivateAddressesAreNotLookedUp(t *testing.T) {
	locator := NewMMDB(nil, nil)

	for _, value := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.1.1", "::1"} {
		if got := locator.Lookup(netip.MustParseAddr(value)); !got.IsZero() {
			t.Errorf("%s returned %+v", value, got)
		}
	}
}

// TestQualifySubdivision checks the country prefix. The databases store "CA" for
// both California and Cataluña, so without it two unrelated regions collapse
// into one row on every report that groups by region.
func TestQualifySubdivision(t *testing.T) {
	if got := qualify("US", "CA"); got != "US-CA" {
		t.Errorf("qualify = %q, want US-CA", got)
	}
	if got := qualify("ES", "CA"); got != "ES-CA" {
		t.Errorf("qualify = %q, want ES-CA", got)
	}
	if got := qualify("", "CA"); got != "" {
		t.Errorf("qualify with no country = %q, want empty", got)
	}
	if got := qualify("US", ""); got != "" {
		t.Errorf("qualify with no subdivision = %q, want empty", got)
	}
}

// openFixture writes a two-network database and opens it the way the process
// does. The file is built here rather than checked in because the real one is
// 130 MB and CI will never have it, and because a fixture whose bytes are
// generated by readable code is a fixture the next person can change.
func openFixture(t *testing.T) Locator {
	t.Helper()

	fixture := newFixture()
	fixture.insert(t, "81.2.69.0/24", dbipRecord())
	fixture.insert(t, "24.24.24.0/24", geoIP2Record())

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, DataDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DataDirName, CityFileName), fixture.bytes(t), 0o644); err != nil {
		t.Fatal(err)
	}

	locator, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { locator.Close() }) //nolint:errcheck // a close failure in a test tells us nothing

	return locator
}

// dbipRecord is what the shipped city database really holds for 81.2.69.142,
// dumped key for key. The keys we do not read are here on purpose: they are
// what a decode has to skip past, and a struct that quietly matched none of the
// keys present is exactly the failure this file is testing for.
func dbipRecord() map[string]any {
	return map[string]any{
		"city": map[string]any{
			"names": map[string]any{"en": "London"},
		},
		"continent": map[string]any{
			"code":       "EU",
			"geoname_id": uint32(6255148),
			"names":      map[string]any{"en": "Europe", "fr": "Europe"},
		},
		"country": map[string]any{
			"geoname_id":           uint32(2635167),
			"is_in_european_union": false,
			"iso_code":             "GB",
			"names": map[string]any{
				"en":    "United Kingdom",
				"fr":    "Royaume-Uni",
				"pt-BR": "Reino Unido",
			},
		},
		"location": map[string]any{
			"latitude":  51.5143,
			"longitude": -0.0912244,
		},
		"subdivisions": []any{
			map[string]any{"names": map[string]any{"en": "England"}},
		},
	}
}

// geoIP2Record is the shape a paid city database uses: every place carries an
// id and a code as well as a name. It is here so the reader is pinned against
// both schemas, because the file underneath us is a licensing decision that has
// already changed once.
func geoIP2Record() map[string]any {
	return map[string]any{
		"city": map[string]any{
			"geoname_id": uint32(5140405),
			"names":      map[string]any{"en": "Syracuse"},
		},
		"country": map[string]any{
			"geoname_id": uint32(6252001),
			"iso_code":   "US",
			"names":      map[string]any{"en": "United States"},
		},
		"subdivisions": []any{
			map[string]any{
				"geoname_id": uint32(5128638),
				"iso_code":   "NY",
				"names":      map[string]any{"en": "New York"},
			},
		},
	}
}

// The three things a search-tree record can hold. An empty record is written as
// the node count, which is how the format spells "no data for this branch".
const (
	recordEmpty = iota
	recordNode
	recordData
)

// fixtureNode is one node of the binary search tree: a left record for a zero
// bit and a right record for a one bit.
type fixtureNode struct {
	kind  [2]int
	value [2]int
}

// fixture builds a MaxMind DB file. It is a writer rather than a checked-in
// blob so the test reads as the records it asserts on, and it is deliberately
// minimal: IPv4 only, 32-bit records, no data-section pointers, because none of
// that changes what the decoder does to a record.
type fixture struct {
	nodes []fixtureNode
	data  []byte
}

// newFixture starts a tree holding nothing but its root.
func newFixture() *fixture {
	return &fixture{nodes: []fixtureNode{{}}}
}

// insert files one record under one network, creating the nodes along the way.
// Walking bit by bit is the same walk the reader does, so a fixture that inserts
// wrongly fails the lookup rather than producing a file the reader rejects.
func (f *fixture) insert(t *testing.T, network string, record map[string]any) {
	t.Helper()

	prefix, err := netip.ParsePrefix(network)
	if err != nil {
		t.Fatal(err)
	}
	if !prefix.Addr().Is4() || prefix.Bits() == 0 {
		t.Fatalf("the fixture holds IPv4 networks with at least one bit, not %s", network)
	}

	offset := len(f.data)
	f.data = append(f.data, encode(t, record)...)

	address := prefix.Addr().As4()
	node := 0

	for bit := 0; bit < prefix.Bits(); bit++ {
		side := int(address[bit/8]>>(7-bit%8)) & 1

		if bit == prefix.Bits()-1 {
			f.nodes[node].kind[side] = recordData
			f.nodes[node].value[side] = offset

			return
		}

		if f.nodes[node].kind[side] != recordNode {
			f.nodes = append(f.nodes, fixtureNode{})
			f.nodes[node].kind[side] = recordNode
			f.nodes[node].value[side] = len(f.nodes) - 1
		}

		node = f.nodes[node].value[side]
	}
}

// bytes renders the file: search tree, the sixteen zero bytes that separate it
// from the data section, the data, the metadata marker and the metadata. A data
// record is addressed by its offset plus the node count plus that separator,
// which is the arithmetic the reader undoes on every lookup.
func (f *fixture) bytes(t *testing.T) []byte {
	t.Helper()

	nodeCount := len(f.nodes)
	file := make([]byte, 0, nodeCount*8+len(f.data)+256)

	var record [4]byte
	for _, node := range f.nodes {
		for side := range node.kind {
			var value uint32

			switch node.kind[side] {
			case recordEmpty:
				value = uint32(nodeCount)
			case recordNode:
				value = uint32(node.value[side])
			case recordData:
				value = uint32(node.value[side] + nodeCount + 16)
			}

			binary.BigEndian.PutUint32(record[:], value)
			file = append(file, record[:]...)
		}
	}

	file = append(file, make([]byte, 16)...)
	file = append(file, f.data...)
	file = append(file, "\xAB\xCD\xEFMaxMind.com"...)
	file = append(file, encode(t, map[string]any{
		"binary_format_major_version": uint16(2),
		"binary_format_minor_version": uint16(0),
		"build_epoch":                 uint32(1785548524),
		"database_type":               "Feasible-Test-City",
		"description":                 map[string]any{"en": "geo test fixture"},
		"ip_version":                  uint16(4),
		"languages":                   []any{"en"},
		"node_count":                  uint32(nodeCount),
		"record_size":                 uint16(32),
	})...)

	return file
}

// encode writes one value in the MaxMind data format. The format puts the type
// in the top three bits of a control byte and the length in the low five, with
// a type above seven pushed into a second byte — which is why the header and
// the payload are built separately here.
func encode(t *testing.T, value any) []byte {
	t.Helper()

	switch typed := value.(type) {
	case string:
		return append(header(t, 2, len(typed)), typed...)

	case float64:
		var payload [8]byte
		binary.BigEndian.PutUint64(payload[:], math.Float64bits(typed))

		return append(header(t, 3, 8), payload[:]...)

	case uint16:
		digits := bigEndian(uint64(typed))

		return append(header(t, 5, len(digits)), digits...)

	case uint32:
		digits := bigEndian(uint64(typed))

		return append(header(t, 6, len(digits)), digits...)

	case bool:
		// A boolean has no payload at all: the length field is the value.
		size := 0
		if typed {
			size = 1
		}

		return header(t, 14, size)

	case map[string]any:
		encoded := header(t, 7, len(typed))

		// The format does not care about key order. Sorting it makes the same
		// fixture produce the same bytes on every run, which is what makes a
		// failure here reproducible.
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		for _, key := range keys {
			encoded = append(encoded, encode(t, key)...)
			encoded = append(encoded, encode(t, typed[key])...)
		}

		return encoded

	case []any:
		encoded := header(t, 11, len(typed))
		for _, item := range typed {
			encoded = append(encoded, encode(t, item)...)
		}

		return encoded
	}

	t.Fatalf("the fixture encoder has no case for %T", value)

	return nil
}

// header builds the control byte and whatever follows it. The order is fixed by
// the format: control byte, then the extended type byte, then the extra length
// bytes — get it the other way round and the reader misreads every value after
// this one.
func header(t *testing.T, kind, size int) []byte {
	t.Helper()

	control := byte(0)
	if kind <= 7 {
		control = byte(kind) << 5
	}

	var length []byte

	switch {
	case size < 29:
		control |= byte(size)
	case size < 285:
		control |= 29
		length = []byte{byte(size - 29)}
	default:
		t.Fatalf("the fixture encoder holds nothing longer than 284 bytes, got %d", size)
	}

	encoded := []byte{control}
	if kind > 7 {
		encoded = append(encoded, byte(kind-7))
	}

	return append(encoded, length...)
}

// bigEndian renders an unsigned integer as the format stores one: big-endian
// with the leading zero bytes dropped, so zero is no bytes at all.
func bigEndian(value uint64) []byte {
	var full [8]byte
	binary.BigEndian.PutUint64(full[:], value)

	for i, digit := range full {
		if digit != 0 {
			return full[i:]
		}
	}

	return nil
}
