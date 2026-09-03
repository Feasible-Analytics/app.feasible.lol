//
// fetch_test.go
// Parsing each provider's shape, and collapsing the pile they add up to.
//
// Created: 2026-09-03
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package lists

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// TestParsersReadTheirOwnShape checks each provider's format against a cut-down
// copy of the real document. A parser that silently returns nothing turns into
// a coverage hole with no error anywhere, which is the failure this list was
// built to end rather than to repeat.
func TestParsersReadTheirOwnShape(t *testing.T) {
	cases := []struct {
		name  string
		parse func([]byte) ([]string, error)
		body  string
		want  []string
	}{
		{
			name:  "aws",
			parse: parseAWS,
			body: `{"prefixes":[{"ip_prefix":"52.94.76.0/22"}],
			        "ipv6_prefixes":[{"ipv6_prefix":"2600:1f00::/24"}]}`,
			want: []string{"52.94.76.0/22", "2600:1f00::/24"},
		},
		{
			name:  "google cloud",
			parse: parseGoogleCloud,
			body:  `{"prefixes":[{"ipv4Prefix":"34.64.32.0/19"},{"ipv6Prefix":"2600:1900::/28"}]}`,
			want:  []string{"34.64.32.0/19", "2600:1900::/28"},
		},
		{
			name:  "oracle",
			parse: parseOracle,
			body:  `{"regions":[{"cidrs":[{"cidr":"129.213.16.0/20"},{"cidr":"140.91.0.0/17"}]}]}`,
			want:  []string{"129.213.16.0/20", "140.91.0.0/17"},
		},
		{
			name:  "azure",
			parse: parseAzure,
			body:  `{"values":[{"properties":{"addressPrefixes":["20.36.0.0/19","2603:1000::/24"]}}]}`,
			want:  []string{"20.36.0.0/19", "2603:1000::/24"},
		},
		{
			name:  "ripestat",
			parse: parseRIPEstat,
			body:  `{"data":{"prefixes":[{"prefix":"5.9.0.0/16"},{"prefix":"2a01:4f8::/29"}]}}`,
			want:  []string{"5.9.0.0/16", "2a01:4f8::/29"},
		},
		{
			name:  "geofeed",
			parse: parseGeofeed,
			body: "# a comment line\n" +
				"139.162.1.0/24,GB,GB-ENG,London,\n" +
				"45.32.0.0/16,US,US-NJ,Piscataway,08854\n",
			want: []string{"139.162.1.0/24", "45.32.0.0/16"},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.parse([]byte(test.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if !slices.Equal(got, test.want) {
				t.Errorf("got %v, want %v", got, test.want)
			}
		})
	}
}

// TestMergeCollapsesAdjacentAndNested checks the three things the merge has to
// get right: a range inside another disappears, two halves become their parent,
// and unrelated ranges are left alone.
//
// It matters because the raw sources are roughly 135,000 prefixes and the file
// is roughly 14,000. Everything between those two numbers is this function, and
// a merge that dropped coverage would look exactly like a merge that worked.
func TestMergeCollapsesAdjacentAndNested(t *testing.T) {
	got := Merge([]string{
		"10.0.0.0/24",   // reserved, dropped entirely
		"52.0.0.0/25",   // joins the next one
		"52.0.0.128/25", // into 52.0.0.0/24
		"93.184.216.0/24",
		"93.184.216.128/25", // already inside the line above
		"203.0.113.0/24",    // reserved documentation space
		"not a prefix",
	})

	want := []string{"52.0.0.0/24", "93.184.216.0/24"}

	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestMergeKeepsCoverage checks that no address covered before the merge is
// uncovered after it. Losing an address is the one merge bug that cannot be
// seen in the output, because the file still looks like a sensible list.
func TestMergeKeepsCoverage(t *testing.T) {
	input := []string{
		"52.0.0.0/25", "52.0.0.128/25", "52.0.1.0/24",
		"203.113.0.0/17", "203.113.128.0/17",
		"2600:1f00::/32", "2600:1f01::/32",
	}

	merged := Merge(input)

	parsed := make([]netip.Prefix, 0, len(merged))
	for _, entry := range merged {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			t.Fatalf("merge produced %q, which does not parse: %v", entry, err)
		}

		parsed = append(parsed, prefix)
	}

	for _, entry := range input {
		before := netip.MustParsePrefix(entry)

		var covered bool
		for _, after := range parsed {
			if after.Bits() <= before.Bits() && after.Contains(before.Addr()) {
				covered = true

				break
			}
		}

		if !covered {
			t.Errorf("%s was covered before the merge and is not after it", before)
		}
	}
}

// TestMergeIsIdempotent checks that merging an already merged list changes
// nothing. A merge that keeps finding more to do is a merge that is wrong in
// one direction or the other.
func TestMergeIsIdempotent(t *testing.T) {
	once := Merge(Datacenters())
	twice := Merge(once)

	if !slices.Equal(once, twice) {
		t.Errorf("merging twice gave %d ranges, merging once gave %d", len(twice), len(once))
	}
}

// TestFetchReportsABadStatus checks that a source answering 500 is an error
// rather than an empty list. Silently treating a failed download as "this
// provider announces nothing" is how coverage disappears without a word.
func TestFetchReportsABadStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := Fetch(context.Background(), server.Client(), Source{
		Name:  "test",
		URL:   server.URL,
		Parse: parseAWS,
	})

	if err == nil {
		t.Fatal("a 500 response produced no error")
	}
}

// TestAzureSourceWalksBackToAPublishedFile checks the date-walk. Microsoft
// publishes the file weekly with the date in its name and no stable alias, so
// today's guess is usually wrong and the walk is the whole mechanism.
func TestAzureSourceWalksBackToAPublishedFile(t *testing.T) {
	// Only one date answers, and it is three weeks before the Monday the walk
	// starts from, so a walk that gives up early fails here.
	published := "ServiceTags_Public_20260810.json"

	var tried int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tried++

		if r.URL.Path == "/"+published {
			w.WriteHeader(http.StatusOK)

			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	source, err := azureSourceFrom(context.Background(), server.Client(),
		server.URL+"/ServiceTags_Public_", time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("azure source: %v", err)
	}

	if source.URL != server.URL+"/"+published {
		t.Errorf("found %q, want the file at %q", source.URL, published)
	}

	if tried < 2 {
		t.Errorf("only %d URLs tried, so the walk is not walking", tried)
	}
}
