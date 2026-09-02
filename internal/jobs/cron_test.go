//
// cron_test.go
// One tick per period, however many processes try to make it.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package jobs

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// available counts the jobs waiting to be claimed.
func available(t *testing.T, db *sql.DB) int {
	t.Helper()

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM jobs WHERE state = 'available'").Scan(&count); err != nil {
		t.Fatalf("count available jobs: %v", err)
	}

	return count
}

// metricValue reads one unlabelled scheduler metric from the default registry.
// Histograms return their sample count so duration observation is testable.
func metricValue(t *testing.T, name string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name || len(family.GetMetric()) == 0 {
			continue
		}
		metric := family.GetMetric()[0]
		switch {
		case metric.Counter != nil:
			return metric.GetCounter().GetValue()
		case metric.Gauge != nil:
			return metric.GetGauge().GetValue()
		case metric.Histogram != nil:
			return float64(metric.GetHistogram().GetSampleCount())
		}
	}

	t.Fatalf("metric %s was not registered", name)
	return 0
}

// TestOneTickPerPeriodAcrossEveryProcess is the property that lets every
// replica run its own Cron with no leader election. They all try, one insert
// wins, and the rest are no-ops.
func TestOneTickPerPeriodAcrossEveryProcess(t *testing.T) {
	db := newSystem(t)
	ctx := context.Background()

	now := time.Date(2026, 8, 30, 12, 5, 0, 0, time.UTC)

	// Three processes, each with its own Cron over the same queue.
	created := 0

	for process := 0; process < 3; process++ {
		cron := NewCron(NewClient(db), nil)
		cron.Add("notifications", "reports.schedule", time.Hour)

		made, err := cron.EnqueueDue(ctx, now)
		if err != nil {
			t.Fatalf("process %d: %v", process, err)
		}

		created += made
	}

	if created != 1 {
		t.Fatalf("three processes created %d jobs for one hour, want 1", created)
	}

	if count := available(t, db); count != 1 {
		t.Fatalf("%d jobs are queued for one hour", count)
	}
}

// TestTheNextPeriodGetsItsOwnTick checks that the bucket key advances, so an
// hourly job actually runs every hour rather than once ever.
func TestTheNextPeriodGetsItsOwnTick(t *testing.T) {
	db := newSystem(t)
	client := NewClient(db)
	ctx := context.Background()

	cron := NewCron(client, nil)
	cron.Add("notifications", "reports.schedule", time.Hour)

	base := time.Date(2026, 8, 30, 12, 5, 0, 0, time.UTC)

	// Within the same hour: no second tick, even after the first job completes.
	if made, _ := cron.EnqueueDue(ctx, base); made != 1 {
		t.Fatalf("the first tick created %d jobs, want 1", made)
	}

	if made, _ := cron.EnqueueDue(ctx, base.Add(50*time.Minute)); made != 0 {
		t.Fatalf("a second look inside the same hour created %d jobs", made)
	}

	// Complete the first job, then prove the durable slot still owns this hour
	// while the next hour receives an independent slot.
	runner := NewRunner(client)
	runner.Register("notifications", "reports.schedule",
		Reporting(nil, func(context.Context, Job) (Outcome, error) {
			return Nothing("nothing was due"), nil
		}))

	if _, err := runner.Once(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if made, _ := cron.EnqueueDue(ctx, base.Add(54*time.Minute)); made != 0 {
		t.Fatalf("a completed job allowed %d duplicate ticks inside its bucket", made)
	}

	if made, _ := cron.EnqueueDue(ctx, base.Add(time.Hour)); made != 1 {
		t.Fatalf("the next hour created %d jobs, want 1", made)
	}
}

// TestASecondEntryTicksOnItsOwnPeriod checks that two entries with different
// periods do not share a bucket key.
func TestASecondEntryTicksOnItsOwnPeriod(t *testing.T) {
	ctx := context.Background()

	cron := NewCron(NewClient(newSystem(t)), nil)
	cron.Add("notifications", "reports.schedule", time.Hour)
	cron.Add("notifications", "reports.alerts", 10*time.Minute)

	made, err := cron.EnqueueDue(ctx, time.Date(2026, 8, 30, 12, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if made != 2 {
		t.Fatalf("two entries created %d jobs, want 2", made)
	}
}

// TestAnEntryWithNoPeriodIsSkipped checks that a misconfigured entry is ignored
// rather than enqueueing a job every time Cron looks.
func TestAnEntryWithNoPeriodIsSkipped(t *testing.T) {
	cron := NewCron(NewClient(newSystem(t)), nil)
	cron.Add("notifications", "broken", 0)

	made, err := cron.EnqueueDue(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if made != 0 {
		t.Fatalf("an entry with no period created %d jobs", made)
	}
}

// TestCronTicksOntoTheQueueTheRunnerDrains is the reason there is one queue.
// Cron writes through the same client the runner claims from, so a tick it
// enqueues is a job that actually gets dispatched rather than a row on a second
// queue nothing is draining.
func TestCronTicksOntoTheQueueTheRunnerDrains(t *testing.T) {
	ctx := context.Background()
	client := NewClient(newSystem(t))

	cron := NewCron(client, nil)
	cron.Add("notifications", "reports.schedule", time.Hour)

	if _, err := cron.EnqueueDue(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	ran := false

	runner := NewRunner(client)
	runner.Register("notifications", "reports.schedule",
		Reporting(nil, func(context.Context, Job) (Outcome, error) {
			ran = true

			return Outcome{Handled: 1}, nil
		}))

	if _, err := runner.Once(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if !ran {
		t.Fatal("the runner did not dispatch the job cron enqueued")
	}
}

// TestCronQueuesAreDrainableByTheSameProcess pins the pairing the wiring
// depends on: every queue Cron writes to is one the process can register a
// worker for. A tick on a queue nothing claims is work that never happens and
// says nothing about it.
func TestCronQueuesAreDrainableByTheSameProcess(t *testing.T) {
	cron := NewCron(NewClient(newSystem(t)), nil)
	cron.Add("notifications", "reports.schedule", time.Hour)
	cron.Add("notifications", "reports.alerts", time.Hour)

	queues := cron.Queues()

	if len(queues) != 1 || queues[0] != "notifications" {
		t.Fatalf("cron reported queues %v, want one notifications queue", queues)
	}
}

// TestCronCatchesUpDurableBucketsWithinItsBound proves outage recovery, a
// duplicate pass, and the bounded ceiling used by report scheduling.
func TestCronCatchesUpDurableBucketsWithinItsBound(t *testing.T) {
	client := NewClient(newSystem(t))
	cron := NewCron(client, nil)
	cron.AddCatchUp("notifications", "reports.schedule", time.Hour, 32*24*time.Hour)
	ctx := context.Background()
	start := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	created, err := cron.EnqueueDue(ctx, start)
	if err != nil || created != 1 {
		t.Fatalf("initial enqueue = %d, %v", created, err)
	}
	created, err = cron.EnqueueDue(ctx, start.Add(50*time.Hour))
	if err != nil || created != 50 {
		t.Fatalf("50-hour recovery = %d, %v", created, err)
	}
	created, err = cron.EnqueueDue(ctx, start.Add(50*time.Hour))
	if err != nil || created != 0 {
		t.Fatalf("duplicate recovery = %d, %v", created, err)
	}

	created, err = cron.EnqueueDue(ctx, start.Add(100*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	maximum := 32*24 + 1
	if created != maximum {
		t.Fatalf("bounded recovery created %d jobs, want %d", created, maximum)
	}

	cron.recordRun(start, 0, nil)
	if err := cron.Health(start.Add(2 * time.Minute)); err != nil {
		t.Fatalf("fresh zero-created run was unhealthy: %v", err)
	}
	if err := cron.Health(start.Add(4 * time.Minute)); err == nil {
		t.Fatal("stale scheduler reported healthy")
	}
}

// TestSchedulerMetricsDescribeSuccessCatchUpAndFailure checks every exported
// scheduler signal against real enqueue passes and keeps the label set empty.
func TestSchedulerMetricsDescribeSuccessCatchUpAndFailure(t *testing.T) {
	db := newSystem(t)
	cron := NewCron(NewClient(db), nil)
	cron.AddCatchUp("notifications", "reports.schedule", time.Hour, 24*time.Hour)
	ctx := context.Background()
	start := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)

	names := map[string]string{
		"runs":       "feasible_scheduler_runs_total",
		"failures":   "feasible_scheduler_failures_total",
		"slots":      "feasible_scheduler_catch_up_slots_total",
		"catch_jobs": "feasible_scheduler_catch_up_jobs_total",
		"jobs":       "feasible_scheduler_created_jobs_total",
		"duration":   "feasible_scheduler_duration_seconds",
		"success":    "feasible_scheduler_last_success_timestamp_seconds",
	}
	before := map[string]float64{}
	for key, name := range names {
		before[key] = metricValue(t, name)
	}

	if created, err := cron.EnqueueDue(ctx, start); err != nil || created != 1 {
		t.Fatalf("initial scheduler pass = %d, %v", created, err)
	}
	if created, err := cron.EnqueueDue(ctx, start.Add(3*time.Hour)); err != nil || created != 3 {
		t.Fatalf("catch-up scheduler pass = %d, %v", created, err)
	}
	if got := metricValue(t, names["runs"]) - before["runs"]; got != 2 {
		t.Fatalf("scheduler runs delta = %v, want 2", got)
	}
	if got := metricValue(t, names["jobs"]) - before["jobs"]; got != 4 {
		t.Fatalf("scheduler created jobs delta = %v, want 4", got)
	}
	if got := metricValue(t, names["slots"]) - before["slots"]; got != 2 {
		t.Fatalf("scheduler catch-up slots delta = %v, want 2", got)
	}
	if got := metricValue(t, names["catch_jobs"]) - before["catch_jobs"]; got != 2 {
		t.Fatalf("scheduler catch-up jobs delta = %v, want 2", got)
	}
	if got := metricValue(t, names["duration"]) - before["duration"]; got != 2 {
		t.Fatalf("scheduler duration samples delta = %v, want 2", got)
	}
	lastSuccess := metricValue(t, names["success"])
	if lastSuccess != float64(start.Add(3*time.Hour).Unix()) {
		t.Fatalf("scheduler last success = %.0f", lastSuccess)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cron.EnqueueDue(ctx, start.Add(4*time.Hour)); err == nil {
		t.Fatal("scheduler pass over a closed database reported success")
	}
	if got := metricValue(t, names["failures"]) - before["failures"]; got != 1 {
		t.Fatalf("scheduler failures delta = %v, want 1", got)
	}
	if got := metricValue(t, names["runs"]) - before["runs"]; got != 3 {
		t.Fatalf("scheduler runs after failure delta = %v, want 3", got)
	}
	if got := metricValue(t, names["success"]); got != lastSuccess {
		t.Fatalf("failed run changed last success from %.0f to %.0f", lastSuccess, got)
	}

	// Referencing the collectors here also makes accidental removal from the
	// metrics package a compile-time failure for scheduler code.
	_ = metrics.SchedulerRuns
}
