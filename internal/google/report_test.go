//
// report_test.go
// Weighted positions, totals that do not shrink, and a search box that is not a scan.
//
// Created: 2026-09-09
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package google

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// day is one stored Search Console row.
type day struct {
	timestamp   int64
	query       string
	page        string
	country     string
	device      string
	clicks      int64
	impressions int64
	position    float64
}

// seedSearch writes rows the way the importer does, with the position stored
// multiplied by a thousand and weighted by impressions.
func seedSearch(t *testing.T, db *sql.DB, siteID int64, rows ...day) {
	t.Helper()

	for _, row := range rows {
		_, err := db.ExecContext(context.Background(), `
			INSERT INTO search_console_daily
				(site_id, timestamp, query, page, country, device, clicks, impressions, position_x1000_total)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			siteID, row.timestamp, row.query, row.page, row.country, row.device,
			row.clicks, row.impressions, int64(row.position*1000*float64(row.impressions)))
		if err != nil {
			t.Fatal(err)
		}
	}
}

// window is the range the rows below sit inside.
func window() (time.Time, time.Time) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	return start, start.AddDate(0, 0, 7)
}

// TestPositionIsWeightedByImpressions is the arithmetic that makes two days
// addable. A mean of daily averages treats a day with ten impressions and a day
// with ten thousand as worth the same, which is wrong in a way nobody notices
// until they compare against Search Console itself.
func TestPositionIsWeightedByImpressions(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	start, end := window()

	seedSearch(t, account.Writer(), 1,
		day{timestamp: start.Unix(), query: "widgets", clicks: 1, impressions: 10, position: 30},
		day{timestamp: start.AddDate(0, 0, 1).Unix(), query: "widgets", clicks: 9, impressions: 990, position: 3},
	)

	report, err := SearchReportRun(ctx, account.Reader(), SearchReportRequest{
		SiteID: 1, Dimension: DimensionQuery, Start: start, End: end,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Rows) != 1 {
		t.Fatalf("%d rows, want the two days folded into one keyword", len(report.Rows))
	}

	// (30*10 + 3*990) / 1000 = 3.27. An unweighted mean would answer 16.5.
	if got := report.Rows[0].Position; got < 3.26 || got > 3.28 {
		t.Errorf("position = %v, want ~3.27 rather than a mean of two averages", got)
	}

	if got := report.Rows[0].CTR; got < 0.0099 || got > 0.0101 {
		t.Errorf("CTR = %v, want clicks over impressions", got)
	}
}

// TestTotalsCoverTheWindowNotThePage keeps a hundred-row page of a thousand-
// keyword site from reporting a total that shrinks as the reader pages through.
func TestTotalsCoverTheWindowNotThePage(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	start, end := window()

	seedSearch(t, account.Writer(), 1,
		day{timestamp: start.Unix(), query: "one", clicks: 10, impressions: 100, position: 2},
		day{timestamp: start.Unix(), query: "two", clicks: 5, impressions: 50, position: 4},
		day{timestamp: start.Unix(), query: "three", clicks: 1, impressions: 10, position: 9},
	)

	report, err := SearchReportRun(ctx, account.Reader(), SearchReportRequest{
		SiteID: 1, Dimension: DimensionQuery, Start: start, End: end, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Rows) != 1 {
		t.Fatalf("%d rows, want the limit honoured", len(report.Rows))
	}

	if report.Totals.Clicks != 16 {
		t.Errorf("total clicks = %d, want every row in the window rather than the page", report.Totals.Clicks)
	}

	if report.Totals.Impressions != 160 {
		t.Errorf("total impressions = %d", report.Totals.Impressions)
	}
}

// TestRowsAreOrderedByClicks is what makes the first screenful the useful one.
func TestRowsAreOrderedByClicks(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	start, end := window()

	seedSearch(t, account.Writer(), 1,
		day{timestamp: start.Unix(), query: "quiet", clicks: 1, impressions: 900, position: 40},
		day{timestamp: start.Unix(), query: "loud", clicks: 50, impressions: 100, position: 1},
	)

	report, err := SearchReportRun(ctx, account.Reader(), SearchReportRequest{
		SiteID: 1, Dimension: DimensionQuery, Start: start, End: end,
	})
	if err != nil {
		t.Fatal(err)
	}

	if report.Rows[0].Value != "loud" {
		t.Errorf("first row is %q, want the most-clicked keyword", report.Rows[0].Value)
	}
}

// TestValuesAreRewrittenIntoOurVocabulary keeps a device row on this card and a
// device row on the Devices card reading as the same word, and a country row
// carrying the same flag.
func TestValuesAreRewrittenIntoOurVocabulary(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	start, end := window()

	seedSearch(t, account.Writer(), 1,
		day{timestamp: start.Unix(), query: "a", country: "usa", device: "MOBILE", clicks: 1, impressions: 2, position: 1},
	)

	for dimension, wanted := range map[string]string{
		DimensionDevice:  "Mobile",
		DimensionCountry: "US",
	} {
		report, err := SearchReportRun(ctx, account.Reader(), SearchReportRequest{
			SiteID: 1, Dimension: dimension, Start: start, End: end,
		})
		if err != nil {
			t.Fatal(err)
		}

		if report.Rows[0].Value != wanted {
			t.Errorf("%s row = %q, want %q", dimension, report.Rows[0].Value, wanted)
		}
	}
}

// TestSearchTextIsTakenLiterally covers the reader's filter box. A stored
// underscore is a character somebody is looking for, not a wildcard, and a
// percent sign in a search must not return the whole table.
func TestSearchTextIsTakenLiterally(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	start, end := window()

	seedSearch(t, account.Writer(), 1,
		day{timestamp: start.Unix(), query: "buy_widgets", clicks: 3, impressions: 30, position: 2},
		day{timestamp: start.Unix(), query: "buyXwidgets", clicks: 4, impressions: 40, position: 2},
	)

	report, err := SearchReportRun(ctx, account.Reader(), SearchReportRequest{
		SiteID: 1, Dimension: DimensionQuery, Start: start, End: end, Search: "buy_",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Rows) != 1 || report.Rows[0].Value != "buy_widgets" {
		t.Fatalf("rows = %v, want the underscore matched literally", report.Rows)
	}

	// The totals narrow with the filter, so the summary describes the rows the
	// reader is actually looking at.
	if report.Totals.Clicks != 3 {
		t.Errorf("filtered total = %d, want only the matching row", report.Totals.Clicks)
	}
}

// TestRangeAndSiteAreHonoured covers the two ways a report could quietly answer
// about somebody else's data.
func TestRangeAndSiteAreHonoured(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	start, end := window()

	seedSearch(t, account.Writer(), 1,
		day{timestamp: start.Unix(), query: "inside", clicks: 5, impressions: 50, position: 2},
		day{timestamp: start.AddDate(0, 0, -1).Unix(), query: "before", clicks: 7, impressions: 70, position: 2},
		day{timestamp: end.Unix(), query: "after", clicks: 9, impressions: 90, position: 2},
	)
	seedSearch(t, account.Writer(), 2,
		day{timestamp: start.Unix(), query: "another site", clicks: 11, impressions: 110, position: 2},
	)

	report, err := SearchReportRun(ctx, account.Reader(), SearchReportRequest{
		SiteID: 1, Dimension: DimensionQuery, Start: start, End: end,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Rows) != 1 || report.Rows[0].Value != "inside" {
		t.Fatalf("rows = %v, want only the day inside the half-open range and only this site", report.Rows)
	}
}

// TestUnknownDimensionIsRefused keeps a caller-supplied string away from the
// column name. Defaulting instead would answer a question nobody asked with
// numbers that look right.
func TestUnknownDimensionIsRefused(t *testing.T) {
	account := newAccount(t)
	start, end := window()

	_, err := SearchReportRun(context.Background(), account.Reader(), SearchReportRequest{
		SiteID: 1, Dimension: "clicks; DROP TABLE search_console_daily", Start: start, End: end,
	})
	if err == nil {
		t.Fatal("an unknown dimension was accepted")
	}
}

// TestDayBoundsAreReported is what lets the card explain an empty tail rather
// than leaving the reader to conclude their traffic collapsed.
func TestDayBoundsAreReported(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)
	start, _ := window()

	if latest, err := LatestSearchDay(ctx, account.Reader(), 1, time.UTC); err != nil || latest != "" {
		t.Fatalf("latest day = %q %v, want nothing before anything is imported", latest, err)
	}

	seedSearch(t, account.Writer(), 1,
		day{timestamp: start.Unix(), query: "a", clicks: 1, impressions: 2, position: 1},
		day{timestamp: start.AddDate(0, 0, 3).Unix(), query: "b", clicks: 1, impressions: 2, position: 1},
	)

	latest, err := LatestSearchDay(ctx, account.Reader(), 1, time.UTC)
	if err != nil {
		t.Fatal(err)
	}

	if latest != "2026-09-04" {
		t.Errorf("latest day = %q, want the most recent stored day", latest)
	}

	earliest, err := EarliestSearchDay(ctx, account.Reader(), 1)
	if err != nil {
		t.Fatal(err)
	}

	if !earliest.Equal(start) {
		t.Errorf("earliest day = %v, want %v", earliest, start)
	}
}

// TestTheLastDayIsReadInTheSiteTimezone covers a whole-day error nobody would
// trace back here. Rows are stamped with the site's own local midnight, so
// reading one back in UTC names the day before for every site east of
// Greenwich — and the card would tell a customer in Tokyo that Google is a day
// further behind than it is.
func TestTheLastDayIsReadInTheSiteTimezone(t *testing.T) {
	ctx := context.Background()
	account := newAccount(t)

	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}

	local := time.Date(2026, 9, 4, 0, 0, 0, 0, tokyo)

	seedSearch(t, account.Writer(), 1,
		day{timestamp: local.Unix(), query: "a", clicks: 1, impressions: 2, position: 1},
	)

	latest, err := LatestSearchDay(ctx, account.Reader(), 1, tokyo)
	if err != nil {
		t.Fatal(err)
	}

	if latest != "2026-09-04" {
		t.Errorf("latest day = %q, want the site's own local date", latest)
	}
}
