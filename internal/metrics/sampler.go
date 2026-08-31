//
// sampler.go
// The gauges that have to be read when they are asked for, not counted as they happen.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package metrics

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
)

// Sources is what a process can tell the metrics endpoint about itself. Every
// field is optional, because the two process shapes know different things: the
// ingest tier has a buffer and a routing map and no reports, and a shard has
// databases and a roll-up worker.
type Sources struct {
	// BufferDepth is how many accepted events are still waiting to be written.
	BufferDepth func() int

	// Sessions is how many visits the fold is holding open.
	Sessions func() int

	// Sites is the size of the routing map. A map that went empty looks exactly
	// like every customer stopping at once, which is worth being able to tell
	// apart at a glance.
	Sites func() int

	// OpenAccounts is how many account database handles are cached.
	OpenAccounts func() int

	// Jobs counts the background queue. It is a function rather than a number
	// because only the process that owns the queue can answer it, and only the
	// scrape knows when the answer is wanted.
	Jobs func(ctx context.Context) (JobCounts, error)

	// DataDir is the install. Its file sizes are the honest answer to "is the
	// disk about to be the problem".
	DataDir string
}

// JobCounts is the background queue, split into the two states worth watching.
// A queue that is filling up and one that is stuck on a single job look
// identical in a total and obvious side by side.
type JobCounts struct {
	Available int
	Executing int
}

// jobQueryTimeout bounds the one query a scrape makes. A metrics endpoint that
// hangs on a locked database is a monitoring outage on top of whatever was
// already wrong.
const jobQueryTimeout = 2 * time.Second

// sampler reads the Sources on every scrape. It is a Collector rather than a
// set of gauges updated on a timer because all of these are cheap to read and
// none of them is worth a goroutine: a gauge on a timer is a number that is
// always slightly out of date and occasionally very out of date.
type sampler struct {
	sources Sources

	bufferDepth  *prometheus.Desc
	sessions     *prometheus.Desc
	sites        *prometheus.Desc
	openAccounts *prometheus.Desc

	jobs *prometheus.Desc

	databaseBytes    *prometheus.Desc
	walBytes         *prometheus.Desc
	walBytesMax      *prometheus.Desc
	databaseCount    *prometheus.Desc
	databaseReadable *prometheus.Desc

	diskTotal     *prometheus.Desc
	diskAvailable *prometheus.Desc
}

// newSampler builds the collector for one process's sources.
func newSampler(sources Sources) *sampler {
	return &sampler{
		sources: sources,

		bufferDepth: prometheus.NewDesc(
			"feasible_ingest_buffer_events",
			"Accepted events waiting to be written. A number that only grows is a transport that has stopped accepting.",
			nil, nil),

		sessions: prometheus.NewDesc(
			"feasible_ingest_sessions_live",
			"Visits the session fold is holding open in memory.",
			nil, nil),

		sites: prometheus.NewDesc(
			"feasible_sites_routed",
			"Domains in the routing map. Events for a domain that is not in it are dropped.",
			nil, nil),

		openAccounts: prometheus.NewDesc(
			"feasible_database_open_handles",
			"Account databases this process currently holds open.",
			nil, nil),

		jobs: prometheus.NewDesc(
			"feasible_jobs",
			"Background jobs by state. Available that keeps growing is a worker that has stopped; executing that never falls is a job that is stuck.",
			[]string{"state"}, nil),

		databaseBytes: prometheus.NewDesc(
			"feasible_database_bytes",
			"Size of the databases on disk, in bytes.",
			[]string{"database"}, nil),

		walBytes: prometheus.NewDesc(
			"feasible_database_wal_bytes",
			"Size of the write-ahead logs, in bytes. SQLite exposes no last-checkpoint time, so a WAL that keeps growing is how a checkpoint that is not completing shows itself.",
			[]string{"database"}, nil),

		walBytesMax: prometheus.NewDesc(
			"feasible_database_wal_bytes_max",
			"The largest single account write-ahead log. One stuck account is invisible in a sum and obvious here.",
			nil, nil),

		databaseCount: prometheus.NewDesc(
			"feasible_database_files",
			"Account databases in the data directory, open or not.",
			nil, nil),

		databaseReadable: prometheus.NewDesc(
			"feasible_database_directory_readable",
			"1 when the data directory could be listed, 0 when it could not.",
			nil, nil),

		diskTotal: prometheus.NewDesc(
			"feasible_disk_total_bytes",
			"Size of the filesystem holding the data directory, in bytes.",
			nil, nil),

		diskAvailable: prometheus.NewDesc(
			"feasible_disk_available_bytes",
			"Space this process can still write on the filesystem holding the data directory, in bytes. Available rather than free: the reserved blocks are not ours, and an alert on free space fires after writes have already started failing.",
			nil, nil),
	}
}

// Describe announces every series this collector can produce, whether or not
// this process has a source for it. Describing them is what makes the collector
// identifiable, and that is what lets Watch replace one rather than fail on a
// duplicate; a collector may still return fewer series than it described, which
// is exactly what a process with no buffer does.
func (s *sampler) Describe(out chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		s.bufferDepth, s.sessions, s.sites, s.openAccounts, s.jobs,
		s.databaseBytes, s.walBytes, s.walBytesMax, s.databaseCount, s.databaseReadable,
		s.diskTotal, s.diskAvailable,
	} {
		out <- desc
	}
}

// Collect reads everything the process can tell us, right now.
func (s *sampler) Collect(out chan<- prometheus.Metric) {
	if s.sources.BufferDepth != nil {
		out <- prometheus.MustNewConstMetric(s.bufferDepth, prometheus.GaugeValue, float64(s.sources.BufferDepth()))
	}
	if s.sources.Sessions != nil {
		out <- prometheus.MustNewConstMetric(s.sessions, prometheus.GaugeValue, float64(s.sources.Sessions()))
	}
	if s.sources.Sites != nil {
		out <- prometheus.MustNewConstMetric(s.sites, prometheus.GaugeValue, float64(s.sources.Sites()))
	}
	if s.sources.OpenAccounts != nil {
		out <- prometheus.MustNewConstMetric(s.openAccounts, prometheus.GaugeValue, float64(s.sources.OpenAccounts()))
	}

	if s.sources.Jobs != nil {
		s.collectJobs(out)
	}

	if s.sources.DataDir != "" {
		s.collectStorage(out)
	}
}

// collectJobs reads the queue. A failed read exports nothing rather than a zero:
// zero available jobs is what a healthy queue looks like, and reporting it for a
// database we could not read would be an all-clear we have not earned.
func (s *sampler) collectJobs(out chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), jobQueryTimeout)
	defer cancel()

	counts, err := s.sources.Jobs(ctx)
	if err != nil {
		return
	}

	out <- prometheus.MustNewConstMetric(s.jobs, prometheus.GaugeValue, float64(counts.Available), "available")
	out <- prometheus.MustNewConstMetric(s.jobs, prometheus.GaugeValue, float64(counts.Executing), "executing")
}

// collectStorage stats the databases. It is a handful of stat calls against a
// few files per account, which is cheap enough to do on a scrape and far more
// truthful than a size cached from start-up.
func (s *sampler) collectStorage(out chan<- prometheus.Metric) {
	// Free space is read before anything else, because it is the one number
	// here that predicts a failure rather than describing one. A database that
	// cannot grow stops accepting writes with no warning from any of the sizes
	// below, all of which look perfectly healthy right up to the moment.
	if total, available, ok := diskSpace(s.sources.DataDir); ok {
		out <- prometheus.MustNewConstMetric(s.diskTotal, prometheus.GaugeValue, float64(total))
		out <- prometheus.MustNewConstMetric(s.diskAvailable, prometheus.GaugeValue, float64(available))
	}

	control := filepath.Join(s.sources.DataDir, config.ControlDatabaseName)

	out <- prometheus.MustNewConstMetric(s.databaseBytes, prometheus.GaugeValue, float64(sizeOf(control)), "control")
	out <- prometheus.MustNewConstMetric(s.walBytes, prometheus.GaugeValue, float64(sizeOf(control+"-wal")), "control")

	ids, err := accounts.Discover(s.sources.DataDir)
	if err != nil {
		// A data directory we cannot list is worth one series rather than a
		// silent gap: a gap looks like the scrape failed, and this is a real
		// answer that something is wrong with the disk.
		out <- prometheus.MustNewConstMetric(s.databaseReadable, prometheus.GaugeValue, 0)
		return
	}

	out <- prometheus.MustNewConstMetric(s.databaseReadable, prometheus.GaugeValue, 1)

	var total, wal, worst int64

	for _, id := range ids {
		path := accounts.Path(s.sources.DataDir, id)

		total += sizeOf(path)

		size := sizeOf(path + "-wal")
		wal += size

		if size > worst {
			worst = size
		}
	}

	// Summed rather than reported per account: an account id is a customer, and
	// naming customers on a metrics endpoint is not something the endpoint is
	// for. The largest single WAL carries the outlier a sum would hide.
	out <- prometheus.MustNewConstMetric(s.databaseBytes, prometheus.GaugeValue, float64(total), "accounts")
	out <- prometheus.MustNewConstMetric(s.walBytes, prometheus.GaugeValue, float64(wal), "accounts")
	out <- prometheus.MustNewConstMetric(s.walBytesMax, prometheus.GaugeValue, float64(worst))
	out <- prometheus.MustNewConstMetric(s.databaseCount, prometheus.GaugeValue, float64(len(ids)))
}

// sizeOf returns a file's size, or zero if it is not there. A missing WAL is
// normal — it means the database was checkpointed and closed cleanly — so it is
// zero rather than an error.
func sizeOf(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}

	return info.Size()
}
