//
// dimension.go
// The registry of what a query may group by or filter on, and where it lives.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package query

import (
	"sort"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/intern"
)

// dimension describes one thing a query can group by or filter on. It is a
// table rather than a switch because the same three facts — which column holds
// it on each fact table, and which dim_* table turns its id back into a string
// — are needed by the grouper, the filter compiler and the label resolver, and
// three copies of that knowledge is three chances for a report to group by one
// column and label it from another.
type dimension struct {
	// Name is the wire name, such as visit:source.
	Name string

	// EventColumn is the column on `events`, empty when the dimension does not
	// exist there. SessionColumn is the same for `sessions`.
	EventColumn   string
	SessionColumn string

	// EntryColumn is the sessions column that answers an event-scoped
	// dimension at session grain. A page is an event-scoped thing, but "the
	// session that entered on this page" is the correctly-scoped session-grain
	// answer, and it is the only honest way to put a bounce rate next to a
	// page. A dimension with no entry analogue simply does not compose with
	// session-scoped metrics, and the planner says so rather than guessing.
	EntryColumn string

	// Interned is the dim_* table this dimension's ids point at. Empty for
	// dimensions whose value is not an id — time buckets and properties.
	Interned intern.Dimension

	// Time marks a time bucket, whose value is the bucket label itself.
	Time bool

	// Interval is the bucket width for a time dimension: minute, hour, day,
	// week or month. Empty on the bare `time` dimension, which takes the width
	// the date range implies.
	Interval string

	// PropKey is the custom property this dimension reads, for event:props:*.
	PropKey string
}

// Interval names for time dimensions.
const (
	IntervalMinute = "minute"
	IntervalHour   = "hour"
	IntervalDay    = "day"
	IntervalWeek   = "week"
	IntervalMonth  = "month"
)

// propPrefix is the wire prefix for a custom property dimension.
const propPrefix = "event:props:"

// maxPropKeyLength bounds a property name. A property key is written into a
// JSON path, and while the path travels as a bind parameter rather than as SQL,
// an unbounded key is still an unbounded string in every error message and log
// line that mentions it.
const maxPropKeyLength = 64

// dimensions is the registry. Names follow the established query-API
// vocabulary, because a shared vocabulary is what lets somebody point an
// existing dashboard or script at us without rewriting it.
var dimensions = map[string]dimension{
	// Event-scoped. These describe one hit, not one visit.
	"event:page": {
		Name: "event:page", EventColumn: "pathname_id",
		EntryColumn: "entry_page_id", Interned: intern.Pathname,
	},
	"event:hostname": {
		Name: "event:hostname", EventColumn: "hostname_id",
		EntryColumn: "entry_hostname_id", Interned: intern.Hostname,
	},
	"event:page_title": {
		Name: "event:page_title", EventColumn: "page_title_id", Interned: intern.PageTitle,
	},
	"event:name": {
		Name: "event:name", EventColumn: "name_id", Interned: intern.EventName,
	},

	// Session-scoped and nowhere else: a visit has one entry and one exit, and
	// an event has neither.
	"visit:entry_page": {
		Name: "visit:entry_page", SessionColumn: "entry_page_id", Interned: intern.Pathname,
	},
	"visit:exit_page": {
		Name: "visit:exit_page", SessionColumn: "exit_page_id", Interned: intern.Pathname,
	},

	// Carried on both tables. The events table holds a copy of its session's
	// acquisition, geo and device block, which is what makes a source or
	// country breakdown of an event metric a single scan with no join.
	"visit:referrer": {
		Name: "visit:referrer", EventColumn: "referrer_id", SessionColumn: "referrer_id", Interned: intern.Referrer,
	},
	"visit:source": {
		Name: "visit:source", EventColumn: "source_id", SessionColumn: "source_id", Interned: intern.Source,
	},
	"visit:channel": {
		Name: "visit:channel", EventColumn: "channel_id", SessionColumn: "channel_id", Interned: intern.Channel,
	},
	"visit:utm_source": {
		Name: "visit:utm_source", EventColumn: "utm_source_id", SessionColumn: "utm_source_id", Interned: intern.UTMSource,
	},
	"visit:utm_medium": {
		Name: "visit:utm_medium", EventColumn: "utm_medium_id", SessionColumn: "utm_medium_id", Interned: intern.UTMMedium,
	},
	"visit:utm_campaign": {
		Name: "visit:utm_campaign", EventColumn: "utm_campaign_id", SessionColumn: "utm_campaign_id", Interned: intern.UTMCampaign,
	},
	"visit:country": {
		Name: "visit:country", EventColumn: "country_id", SessionColumn: "country_id", Interned: intern.Country,
	},
	"visit:region": {
		Name: "visit:region", EventColumn: "region_id", SessionColumn: "region_id", Interned: intern.Region,
	},
	"visit:city": {
		Name: "visit:city", EventColumn: "city_id", SessionColumn: "city_id", Interned: intern.City,
	},
	"visit:device": {
		Name: "visit:device", EventColumn: "device_type_id", SessionColumn: "device_type_id", Interned: intern.DeviceType,
	},
	"visit:screen": {
		Name: "visit:screen", EventColumn: "screen_size_id", SessionColumn: "screen_size_id", Interned: intern.ScreenSize,
	},
	"visit:browser": {
		Name: "visit:browser", EventColumn: "browser_id", SessionColumn: "browser_id", Interned: intern.Browser,
	},
	"visit:browser_version": {
		Name: "visit:browser_version", EventColumn: "browser_version_id", SessionColumn: "browser_version_id", Interned: intern.BrowserVersion,
	},
	"visit:os": {
		Name: "visit:os", EventColumn: "os_id", SessionColumn: "os_id", Interned: intern.OS,
	},
	"visit:os_version": {
		Name: "visit:os_version", EventColumn: "os_version_id", SessionColumn: "os_version_id", Interned: intern.OSVersion,
	},
	"visit:language": {
		Name: "visit:language", EventColumn: "language_id", SessionColumn: "language_id", Interned: intern.Language,
	},

	// Time buckets. Their value is the local bucket label itself rather than an
	// id, because the bucket is computed in the query from a UTC integer and a
	// timezone and has nothing to intern.
	"time":        {Name: "time", Time: true},
	"time:minute": {Name: "time:minute", Time: true, Interval: IntervalMinute},
	"time:hour":   {Name: "time:hour", Time: true, Interval: IntervalHour},
	"time:day":    {Name: "time:day", Time: true, Interval: IntervalDay},
	"time:week":   {Name: "time:week", Time: true, Interval: IntervalWeek},
	"time:month":  {Name: "time:month", Time: true, Interval: IntervalMonth},
}

// aliases are spellings that mean an existing dimension. They exist so that a
// caller coming from another product's API does not get an error over a word,
// which is the cheapest possible migration failure to avoid.
var aliases = map[string]string{
	"visit:screen_size": "visit:screen",
	"visit:device_type": "visit:device",
	"event:path":        "event:page",
}

// resolveDimension turns a wire name into its registry entry, including the
// event:props:<key> form which is not a fixed name but a family of them.
func resolveDimension(name string) (dimension, error) {
	if target, ok := aliases[name]; ok {
		name = target
	}

	if strings.HasPrefix(name, propPrefix) {
		key := strings.TrimPrefix(name, propPrefix)

		if key == "" {
			return dimension{}, invalid("%q needs a property name after the last colon", name)
		}

		if len(key) > maxPropKeyLength {
			return dimension{}, invalid("property name in %q is longer than %d characters", name, maxPropKeyLength)
		}

		// A quote or a backslash in the key would have to be escaped into the
		// JSON path, and a property nobody can name is not worth the escaping
		// code that would then have to be right.
		if strings.ContainsAny(key, "\"\\") {
			return dimension{}, invalid("property name in %q cannot contain a quote or a backslash", name)
		}

		return dimension{Name: name, PropKey: key}, nil
	}

	found, ok := dimensions[name]
	if !ok {
		return dimension{}, invalid("unknown dimension %q — known dimensions are %s, plus event:props:<key>", name, strings.Join(DimensionNames(), ", "))
	}

	return found, nil
}

// ValidDimension reports whether a name is something a query may group by or
// filter on, returning the engine's own caller-facing message when it is not.
//
// It exists so the public API can refuse a mistyped dimension while it is still
// a query-string parameter, with the same wording the engine would have used
// three layers down. Validating once, in the engine, would still be correct —
// but it would mean the API cannot answer "which of these parameters is wrong"
// until after it has built a query it already knows will fail.
func ValidDimension(name string) error {
	_, err := resolveDimension(name)

	return err
}

// ValidMetric is the same check for a metric name.
func ValidMetric(name string) error {
	if _, ok := metricByName(name); !ok {
		return invalid("unknown metric %q — known metrics are %s", name, strings.Join(MetricNames(), ", "))
	}

	return nil
}

// DimensionNames lists every fixed dimension, sorted. It is what the error
// message for an unknown one prints, because "unknown dimension" without the
// list means a round trip to the documentation for a typo.
func DimensionNames() []string {
	names := make([]string, 0, len(dimensions))
	for name := range dimensions {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// isTimeDimension reports whether a name is a time bucket, without going
// through the full resolver. Order defaulting asks this before validation has
// run, so it has to tolerate a name that turns out to be nonsense.
func isTimeDimension(name string) bool {
	return name == "time" || strings.HasPrefix(name, "time:")
}

// eventOnly reports that this dimension exists on events and not on sessions.
// It is the question the planner asks to decide whether a session-scoped metric
// can be answered at all.
func (d dimension) eventOnly() bool {
	return d.SessionColumn == "" && !d.Time
}

// sessionOnly reports that this dimension exists on sessions and nowhere else.
func (d dimension) sessionOnly() bool {
	return d.SessionColumn != "" && d.EventColumn == ""
}

// isProp reports whether this is a custom property dimension.
func (d dimension) isProp() bool {
	return d.PropKey != ""
}

// jsonPath is the bind parameter json_extract reads a property by. The key is
// a parameter rather than concatenated SQL, which is why the only escaping this
// needs is the check resolveDimension already made.
func (d dimension) jsonPath() string {
	return `$."` + d.PropKey + `"`
}
