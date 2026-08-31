//
// mmdb.go
// A Locator backed by memory-mapped .mmdb files, country first and city on top.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package geo

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/oschwald/maxminddb-golang"
)

// File names inside the data directory. The country database ships with the
// binary so geolocation works with no setup; the city database is a separate
// ~60 MB download a background job fetches, which is why the two are found
// independently rather than as one configured path.
const (
	CountryFileName = "dbip-country-lite.mmdb"
	CityFileName    = "dbip-city-lite.mmdb"
)

// DataDirName is the subdirectory of the data directory these files live in.
// Keeping downloaded data files out of the databases' directory means a backup
// script that copies *.db never picks up 60 MB of geolocation data.
const DataDirName = "geoip"

// mmdbRecord is the subset of a DB-IP or GeoIP2 record we decode. Decoding a
// narrow struct rather than a map is what keeps a lookup allocation-light on a
// path that runs once per event: the reader skips every key the struct does not
// name, so the nine other languages in a `names` map are never materialised.
//
// The shape has to satisfy both schemas because the file underneath us is a
// licensing decision that has already changed once. DB-IP Lite names its
// subdivisions and cities and gives them neither an ISO code nor a geoname id,
// so a struct that asks only for `iso_code` and `geoname_id` decodes cleanly
// and comes back empty on every event.
type mmdbRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`

	Subdivisions []mmdbSubdivision `maxminddb:"subdivisions"`

	City struct {
		Names mmdbNames `maxminddb:"names"`
	} `maxminddb:"city"`
}

// mmdbSubdivision is one level of region. Both fields are read because the two
// schemas disagree about which one exists, and a reader that works with either
// file is what keeps swapping the database a file swap rather than a rewrite.
type mmdbSubdivision struct {
	ISOCode string    `maxminddb:"iso_code"`
	Names   mmdbNames `maxminddb:"names"`
}

// mmdbNames is the localised-name map, narrowed to the one language we store.
// English is not a preference so much as the only key every record in these
// databases is guaranteed to have.
type mmdbNames struct {
	English string `maxminddb:"en"`
}

// MMDB reads locations from one or two memory-mapped databases. Both are
// optional and either can be absent: a box with only the embedded country file
// answers countries, a box that has fetched the city file answers cities, and a
// box with neither answers nothing without ever failing.
type MMDB struct {
	country *maxminddb.Reader
	city    *maxminddb.Reader
}

// Open finds whatever geolocation databases exist under a data directory and
// returns a Locator for them. A missing file is not an error and never will be:
// the whole point of degrading to unknown is that an operator who has not
// downloaded the city database still has a working install.
func Open(dataDir string) (Locator, error) {
	dir := filepath.Join(dataDir, DataDirName)

	country, err := openIfPresent(filepath.Join(dir, CountryFileName))
	if err != nil {
		return Unknown{}, err
	}

	city, err := openIfPresent(filepath.Join(dir, CityFileName))
	if err != nil {
		if country != nil {
			country.Close()
		}
		return Unknown{}, err
	}

	if country == nil && city == nil {
		return Unknown{}, nil
	}

	return &MMDB{country: country, city: city}, nil
}

// openIfPresent opens one database file, treating absence as success. The
// distinction it draws is the one that matters operationally: a file nobody
// downloaded is normal, and a file that exists but cannot be parsed is a real
// problem an operator needs told about rather than silently ignored.
func openIfPresent(path string) (*maxminddb.Reader, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("geoip %s: %w", path, err)
	}

	reader, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("geoip %s: %w", path, err)
	}

	return reader, nil
}

// NewMMDB builds a Locator from already-open readers. It exists for tests and
// for a future downloader that wants to swap a database in without restarting;
// either reader may be nil.
func NewMMDB(country, city *maxminddb.Reader) *MMDB {
	return &MMDB{country: country, city: city}
}

// Lookup resolves an address, preferring the city database when it is present.
// The city file is a superset — it carries the country too — so there is no
// reason to consult both and merge, and one lookup is half the work of two on a
// path budgeted at well under a millisecond.
func (m *MMDB) Lookup(addr netip.Addr) Location {
	if !addr.IsValid() {
		return Location{}
	}

	// A private or loopback address is not in any geolocation database, and
	// asking is a wasted binary search on every request from a developer's
	// laptop or a misconfigured proxy.
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLinkLocalUnicast() {
		return Location{}
	}

	reader := m.city
	if reader == nil {
		reader = m.country
	}
	if reader == nil {
		return Location{}
	}

	var record mmdbRecord

	// A lookup error means the database disagrees with the struct above, which
	// is a data problem rather than an event problem. Unknown is the right
	// answer to give the event either way.
	if err := reader.Lookup(addr.AsSlice(), &record); err != nil {
		return Location{}
	}

	location := Location{
		Country: record.Country.ISOCode,
		City:    record.City.Names.English,
	}

	if len(record.Subdivisions) > 0 {
		location.Subdivision1 = region(location.Country, record.Subdivisions[0])
	}
	if len(record.Subdivisions) > 1 {
		location.Subdivision2 = region(location.Country, record.Subdivisions[1])
	}

	return location
}

// region picks the string a report groups by for one subdivision. The code is
// preferred where a database supplies one because it is stable across releases
// and languages, and the name is the fallback because DB-IP Lite supplies
// nothing else — and an empty region column on every event is worse than a
// region spelled the way a person would say it.
func region(country string, subdivision mmdbSubdivision) string {
	if subdivision.ISOCode != "" {
		return qualify(country, subdivision.ISOCode)
	}

	// A name is not prefixed with the country. "England" already reads as a
	// place, where "GB-England" reads as a code that no standard defines.
	return subdivision.Names.English
}

// qualify turns a bare subdivision code into a full ISO-3166-2 code. The
// databases store "CA" for California and "CA" for Cataluña alike, so without
// the country prefix two unrelated regions collapse into one row on every
// report that groups by region.
func qualify(country, subdivision string) string {
	if country == "" || subdivision == "" {
		return ""
	}

	return country + "-" + subdivision
}

// Close releases both mappings. Both are closed even if the first fails,
// because leaving a 60 MB mapping behind in a process that believes it has shut
// the database is worse than the error being reported late.
func (m *MMDB) Close() error {
	var firstErr error

	if m.country != nil {
		if err := m.country.Close(); err != nil {
			firstErr = err
		}
	}

	if m.city != nil {
		if err := m.city.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}
