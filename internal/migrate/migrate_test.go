//
// migrate_test.go
// Tests for the migration runner and the embedded schema it applies.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// accountThrough returns the real account migration prefix ending at version.
// Benchmarks use the M9 version-10 database as their before-sampling fixture;
// production always runs the complete Account set through version 11.
func accountThrough(version int) Set {
	return UpTo(Account(), version)
}

// BenchmarkSamplingIndexCost measures populated migration time, the
// conservative write-lock window, WAL bytes, database growth, fact insert
// throughput and production-shaped session UPSERT throughput with WAL enabled.
// Run with -benchtime=1x; each size creates and migrates one production-shaped
// fixture rather than repeating DDL in place.
func BenchmarkSamplingIndexCost(b *testing.B) {
	for _, eventRows := range []int64{10_000, 100_000, 500_000} {
		b.Run(fmt.Sprintf("events_%d", eventRows), func(b *testing.B) {
			basePath := filepath.Join(b.TempDir(), "sampling-cost-base.db")
			baseDB, err := store.Open(basePath)
			if err != nil {
				b.Fatal(err)
			}
			defer baseDB.Close()

			ctx := context.Background()
			if _, err := Run(ctx, baseDB, accountThrough(10)); err != nil {
				b.Fatal(err)
			}
			if _, err := baseDB.ExecContext(ctx, `
				PRAGMA journal_mode = WAL;
				PRAGMA wal_autocheckpoint = 0;`); err != nil {
				b.Fatal(err)
			}

			sessionRows := eventRows / 4
			if err := populateSamplingFacts(ctx, baseDB, eventRows, sessionRows); err != nil {
				b.Fatal(err)
			}
			if _, err := baseDB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				b.Fatal(err)
			}

			indexedPath := filepath.Join(b.TempDir(), "sampling-cost-indexed.db")
			if err := copyDatabase(basePath, indexedPath); err != nil {
				b.Fatal(err)
			}
			indexedDB, err := store.Open(indexedPath)
			if err != nil {
				b.Fatal(err)
			}
			defer indexedDB.Close()
			if _, err := indexedDB.ExecContext(ctx, "PRAGMA wal_autocheckpoint = 0"); err != nil {
				b.Fatal(err)
			}
			beforeSize := fileSize(b, indexedPath)

			migration := migrationByVersion(b, 11)
			started := time.Now()
			tx, err := indexedDB.BeginTx(ctx, nil)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
				_ = tx.Rollback()
				b.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			migrationTime := time.Since(started)
			walBytes := fileSizeIfPresent(indexedPath + "-wal")
			freeBytes := filesystemFreeBytes(b, indexedPath)
			integrity := ""
			if err := indexedDB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
				b.Fatal(err)
			}
			if integrity != "ok" {
				b.Fatalf("integrity_check = %q", integrity)
			}

			var checkpointBusy, checkpointFrames, checkpointedFrames int64
			if err := indexedDB.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").
				Scan(&checkpointBusy, &checkpointFrames, &checkpointedFrames); err != nil {
				b.Fatal(err)
			}
			afterSize := fileSize(b, indexedPath)

			const eventWrites = int64(10_000)
			const sessionWrites = eventWrites / 4
			beforeWrite := timedFactInsert(ctx, b, baseDB, eventRows+1, sessionRows+1, eventWrites, sessionWrites)
			afterWrite := timedFactInsert(ctx, b, indexedDB, eventRows+1, sessionRows+1, eventWrites, sessionWrites)
			factWrites := eventWrites + sessionWrites
			sessionUpdates := min(sessionRows, 10_000)
			beforeSessionUpdate := timedSessionUpsert(ctx, b, baseDB, sessionUpdates)
			afterSessionUpdate := timedSessionUpsert(ctx, b, indexedDB, sessionUpdates)

			b.ReportMetric(float64(migrationTime.Microseconds())/1000, "migration_ms")
			b.ReportMetric(float64(migrationTime.Microseconds())/1000, "lock_window_ms")
			b.ReportMetric(float64(walBytes), "wal_bytes")
			b.ReportMetric(float64(afterSize-beforeSize), "db_growth_bytes")
			b.ReportMetric(float64(freeBytes), "free_disk_bytes")
			b.ReportMetric(float64(checkpointBusy), "checkpoint_busy")
			b.ReportMetric(float64(checkpointFrames), "checkpoint_frames")
			b.ReportMetric(float64(checkpointedFrames), "checkpointed_frames")
			b.ReportMetric(float64(factWrites)/beforeWrite.Seconds(), "fact_writes_before/s")
			b.ReportMetric(float64(factWrites)/afterWrite.Seconds(), "fact_writes_after/s")
			b.ReportMetric(float64(sessionUpdates)/beforeSessionUpdate.Seconds(), "session_upserts_before/s")
			b.ReportMetric(float64(sessionUpdates)/afterSessionUpdate.Seconds(), "session_upserts_after/s")
		})
	}
}

// populateSamplingFacts inserts a production-shaped event/session ratio before
// the sampling indexes exist, using one transaction-sized recursive statement
// per fact table so fixture creation is not part of the measured migration.
func populateSamplingFacts(ctx context.Context, db *sql.DB, events, sessions int64) error {
	if _, err := db.ExecContext(ctx, `
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
		INSERT INTO sessions (id, site_id, started_at, last_seen_at, user_id)
		SELECT n, 1, 1788134400 + (n % 86400), 1788134400 + (n % 86400), n % 10000 FROM seq`, sessions); err != nil {
		return err
	}

	_, err := db.ExecContext(ctx, `
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id)
		SELECT n, 1, 1788134400 + (n % 86400), 0, n % 10000, 1 + (n % ?) FROM seq`, events, sessions)
	return err
}

// timedFactInsert measures identical event/session batches against checkpointed
// copies with and without migration 0011. Absolute throughput is fixture-specific;
// the ratio captures maintenance amplification for both new indexes.
func timedFactInsert(ctx context.Context, b *testing.B, db *sql.DB, eventStart, sessionStart, events, sessions int64) time.Duration {
	b.Helper()

	started := time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
		WITH RECURSIVE seq(n) AS (SELECT 0 UNION ALL SELECT n + 1 FROM seq WHERE n + 1 < ?)
		INSERT INTO sessions (id, site_id, started_at, last_seen_at, user_id)
		SELECT ? + n, 1, 1788134400 + (n % 86400), 1788134400 + (n % 86400), n % 10000 FROM seq`,
		sessions, sessionStart); err != nil {
		_ = tx.Rollback()
		b.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
		WITH RECURSIVE seq(n) AS (SELECT 0 UNION ALL SELECT n + 1 FROM seq WHERE n + 1 < ?)
		INSERT INTO events (id, site_id, timestamp, name_id, user_id, session_id)
		SELECT ? + n, 1, 1788134400 + (n % 86400), 0, n % 10000, ? + (n % ?) FROM seq`,
		events, eventStart, sessionStart, sessions); err != nil {
		_ = tx.Rollback()
		b.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}

	return time.Since(started)
}

// timedSessionUpsert exercises the production conflict path, including its
// assignment of started_at. Migration 0011's move trigger is registered for
// that column on every session update, and its WHEN guard must make unchanged
// starts cheap instead of rewriting sampling facts on every arriving event.
func timedSessionUpsert(ctx context.Context, b *testing.B, db *sql.DB, sessions int64) time.Duration {
	b.Helper()

	started := time.Now()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at, duration,
		                       is_bounce, pageviews, events, entry_page_id, exit_page_id)
		SELECT id, site_id, user_id, started_at, last_seen_at + 1, duration + 1,
		       is_bounce, pageviews, events + 1, entry_page_id, exit_page_id
		FROM sessions
		WHERE id BETWEEN 1 AND ?
		ON CONFLICT(id) DO UPDATE SET
			last_seen_at = excluded.last_seen_at,
			started_at = excluded.started_at,
			duration = excluded.duration,
			is_bounce = excluded.is_bounce,
			pageviews = excluded.pageviews,
			events = excluded.events,
			entry_page_id = excluded.entry_page_id,
			exit_page_id = excluded.exit_page_id`, sessions); err != nil {
		b.Fatal(err)
	}

	return time.Since(started)
}

// filesystemFreeBytes returns the available bytes on the volume holding a
// benchmark database. Migration reports it beside peak WAL growth so an
// operator can compare required temporary space with actual headroom.
func filesystemFreeBytes(b *testing.B, path string) uint64 {
	b.Helper()

	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		b.Fatal(err)
	}

	return stats.Bavail * uint64(stats.Bsize)
}

// copyDatabase clones a checkpointed fixture so write throughput can compare
// identical row populations instead of two different points in one growing WAL.
func copyDatabase(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// migrationByVersion returns one embedded migration to a benchmark.
func migrationByVersion(b *testing.B, version int) Migration {
	b.Helper()

	for _, migration := range Account().Migrations {
		if migration.Version == version {
			return migration
		}
	}

	b.Fatalf("migration %d is missing", version)
	return Migration{}
}

// fileSize returns a required fixture file's current length.
func fileSize(b *testing.B, path string) int64 {
	b.Helper()

	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}

	return info.Size()
}

// fileSizeIfPresent returns zero for a checkpointed-away WAL and fails for
// errors other than absence.
func fileSizeIfPresent(path string) int64 {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		return 0
	}

	return info.Size()
}

// newDatabase opens an empty database in a temporary directory. Every test here
// starts from a file that has never been migrated, which is the state a fresh
// install and a brand-new account both begin in.
func newDatabase(t *testing.T) *sql.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })

	return db
}

// tableExists reports whether the schema holds a table. Asserting on the schema
// rather than on the runner's return value is what proves the SQL actually ran,
// as opposed to the version merely being stamped.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var count int

	query := "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?"
	if err := db.QueryRowContext(context.Background(), query, name).Scan(&count); err != nil {
		t.Fatal(err)
	}

	return count > 0
}

// controlBase returns the real M9 prefix immediately before M8's 0010. Keeping
// the boundary explicit lets migration tests exercise the final transition.
func controlBase() Set {
	return UpTo(Control(), 9)
}

// assertTrialEnrollment verifies the lifecycle source and both hot-path team
// mirrors use the same signup instant and the state machine's exact boundaries.
func assertTrialEnrollment(t *testing.T, db *sql.DB, teamID, startedAt int64) {
	t.Helper()

	var (
		trigger            string
		clockStarted       int64
		clockCreated       int64
		clockUpdated       int64
		trialEnds          int64
		acceptTrafficUntil int64
	)

	if err := db.QueryRowContext(context.Background(), `
		SELECT account_lifecycle.trigger,
		       account_lifecycle.started_at,
		       account_lifecycle.created_at,
		       account_lifecycle.updated_at,
		       teams.trial_ends_at,
		       teams.accept_traffic_until
		FROM account_lifecycle
		JOIN teams ON teams.id = account_lifecycle.team_id
		WHERE account_lifecycle.team_id = ?
	`, teamID).Scan(&trigger, &clockStarted, &clockCreated, &clockUpdated,
		&trialEnds, &acceptTrafficUntil); err != nil {
		t.Fatalf("read team %d trial enrollment: %v", teamID, err)
	}

	if trigger != "trial" || clockStarted != startedAt || clockCreated != startedAt || clockUpdated != startedAt {
		t.Errorf("team %d lifecycle is trigger=%q started=%d created=%d updated=%d, want trial at %d",
			teamID, trigger, clockStarted, clockCreated, clockUpdated, startedAt)
	}

	wantTrialEnds := startedAt + int64(30*24*time.Hour/time.Second)
	wantAcceptUntil := startedAt + int64(60*24*time.Hour/time.Second)
	if trialEnds != wantTrialEnds || acceptTrafficUntil != wantAcceptUntil {
		t.Errorf("team %d mirrors are trial=%d accept=%d, want %d and %d",
			teamID, trialEnds, acceptTrafficUntil, wantTrialEnds, wantAcceptUntil)
	}
}

// TestControlBaseMigratesAFreshDatabase proves the contiguous control prefix
// still produces the complete pre-branch schema for fixtures and upgrades.
func TestControlBaseMigratesAFreshDatabase(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)
	base := controlBase()

	result, err := Run(ctx, db, base)
	if err != nil {
		t.Fatal(err)
	}

	if result.From != 0 || result.To != base.Version() {
		t.Fatalf("migrated from %d to %d, want 0 to %d", result.From, result.To, base.Version())
	}
	if !result.Changed() {
		t.Fatal("a fresh database reported no changes")
	}

	for _, table := range []string{
		"users", "user_sessions", "teams", "team_memberships", "team_invitations",
		"site_folders", "sites", "guest_memberships", "subscriptions",
		"usage_counters", "api_keys", "shared_links", "salts", "jobs",
		"email_verification_codes", "password_reset_tokens",
		"billing_account_leases", "billing_account_customers", "billing_quiescence_objects", "billing_checkouts",
		"billing_checkout_cleanup", "lifecycle_account_leases", "lifecycle_outbox",
		"account_deletion_customers", "team_id_sequence", "destructive_operations",
		"report_subscriptions", "alert_rules", "notification_claims", "cron_slots",
	} {
		if !tableExists(t, db, table) {
			t.Errorf("control schema is missing %s", table)
		}
	}

	version, err := store.SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if version != base.Version() {
		t.Fatalf("schema version is %d, want %d", version, base.Version())
	}
}

// TestRandomSaltAuthorityMigrationInvalidatesDeterministicRows exercises the
// deployment boundary at control migration 0007. Existing material may have
// been derived from the permanent encryption key and must not survive the
// transition to random authority-owned values.
func TestRandomSaltAuthorityMigrationInvalidatesDeterministicRows(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	throughSix := Set{Name: "control"}
	for _, migration := range Control().Migrations {
		if migration.Version <= 6 {
			throughSix.Migrations = append(throughSix.Migrations, migration)
		}
	}
	if _, err := Run(ctx, db, throughSix); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO salts (salt, created_at) VALUES (x'0011223344556677', 123)"); err != nil {
		t.Fatal(err)
	}

	throughSeven := Set{Name: "control"}
	for _, migration := range Control().Migrations {
		if migration.Version <= 7 {
			throughSeven.Migrations = append(throughSeven.Migrations, migration)
		}
	}
	result, err := Run(ctx, db, throughSeven)
	if err != nil {
		t.Fatal(err)
	}
	if result.From != 6 || fmt.Sprint(result.Applied) != "[7]" {
		t.Fatalf("control authority upgrade moved from %d via %v, want 6 via [7]", result.From, result.Applied)
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM salts").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("authority migration retained %d deterministic-era salt rows", count)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO salts (salt, created_at, source_shard) VALUES (x'8899aabbccddeeff', 86400, 0)"); err != nil {
		t.Fatalf("authority source column is unavailable after migration 0007: %v", err)
	}
}

// TestControlDeletionCustomerAuditSurvivesTeamCascade proves every discovered
// Stripe customer remains independently retryable after local account rows are
// gone, while malformed and duplicate customer identities are rejected.
func TestControlDeletionCustomerAuditSurvivesTeamCascade(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)
	if _, err := Run(ctx, db, controlBase()); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO teams (id, name, created_at, updated_at)
		VALUES (42, 'Deletion audit', 100, 100);
		INSERT INTO account_deletion_customers (team_id, customer_id, created_at)
		VALUES (42, 'cus_first', 101), (42, 'cus_second', 102);
	`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO account_deletion_customers (team_id, customer_id, created_at)
		VALUES (42, '', 103)
	`); err == nil {
		t.Fatal("deletion customer audit accepted an empty customer id")
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO account_deletion_customers (team_id, customer_id, created_at)
		VALUES (42, 'cus_first', 104)
	`); err == nil {
		t.Fatal("deletion customer audit accepted a duplicate team/customer key")
	}

	if _, err := db.ExecContext(ctx, "DELETE FROM teams WHERE id = 42"); err != nil {
		t.Fatal(err)
	}

	var (
		customers int
		pending   int
		errors    int
		indexes   int
	)
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM account_deletion_customers WHERE team_id = 42").Scan(&customers); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM account_deletion_customers WHERE team_id = 42 AND removed_at IS NULL").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM account_deletion_customers WHERE team_id = 42 AND last_error = ''").Scan(&errors); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'account_deletion_customers_pending'
		  AND sql LIKE '%WHERE removed_at IS NULL%'
	`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}

	if customers != 2 || pending != 2 || errors != 2 {
		t.Fatalf("surviving audit has customers=%d pending=%d default-errors=%d, want 2 each",
			customers, pending, errors)
	}
	if indexes != 1 {
		t.Fatal("account deletion customers are missing their partial pending index")
	}
}

// TestControlEnrollsEveryNewTeamAtomically proves password registration,
// Google registration, and a direct future team insert all receive the same
// trial clock without a caller making a separate lifecycle request.
func TestControlEnrollsEveryNewTeamAtomically(t *testing.T) {
	ctx := context.Background()
	fixed := time.Date(2026, time.August, 31, 15, 4, 5, 0, time.UTC)

	for _, path := range []string{"password registration", "Google registration", "general team insert"} {
		t.Run(path, func(t *testing.T) {
			db := newDatabase(t)
			if _, err := Run(ctx, db, controlBase()); err != nil {
				t.Fatal(err)
			}

			var teamID int64
			switch path {
			case "password registration":
				result, err := db.ExecContext(ctx, `
						INSERT INTO users
							(email, name, password_hash, created_at, updated_at)
						VALUES ('password@example.test', 'Password', 'password-hash', ?, ?)
					`, fixed.Unix(), fixed.Unix())
				if err != nil {
					t.Fatal(err)
				}
				userID, err := result.LastInsertId()
				if err != nil {
					t.Fatal(err)
				}
				result, err = db.ExecContext(ctx, `
						INSERT INTO teams
							(name, trial_ends_at, accept_traffic_until, created_at, updated_at)
						VALUES ('Password', ?, ?, ?, ?)
					`, fixed.Add(30*24*time.Hour).Unix(), fixed.Add(60*24*time.Hour).Unix(),
					fixed.Unix(), fixed.Unix())
				if err != nil {
					t.Fatal(err)
				}
				teamID, err = result.LastInsertId()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `
						INSERT INTO team_memberships (team_id, user_id, role, created_at)
						VALUES (?, ?, 'owner', ?)
					`, teamID, userID, fixed.Unix()); err != nil {
					t.Fatal(err)
				}

			case "Google registration":
				result, err := db.ExecContext(ctx, `
						INSERT INTO users
							(email, name, google_sub, email_verified_at, created_at, updated_at)
						VALUES ('google@example.test', 'Google', 'google-subject', ?, ?, ?)
					`, fixed.Unix(), fixed.Unix(), fixed.Unix())
				if err != nil {
					t.Fatal(err)
				}
				userID, err := result.LastInsertId()
				if err != nil {
					t.Fatal(err)
				}
				result, err = db.ExecContext(ctx, `
						INSERT INTO teams
							(name, trial_ends_at, accept_traffic_until, created_at, updated_at)
						VALUES ('Google', ?, ?, ?, ?)
					`, fixed.Add(30*24*time.Hour).Unix(), fixed.Add(60*24*time.Hour).Unix(),
					fixed.Unix(), fixed.Unix())
				if err != nil {
					t.Fatal(err)
				}
				teamID, err = result.LastInsertId()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `
						INSERT INTO team_memberships (team_id, user_id, role, created_at)
						VALUES (?, ?, 'owner', ?)
					`, teamID, userID, fixed.Unix()); err != nil {
					t.Fatal(err)
				}

			case "general team insert":
				result, err := db.ExecContext(ctx, `
					INSERT INTO teams
						(name, trial_ends_at, accept_traffic_until, created_at, updated_at)
					VALUES ('General', 1, 2, ?, ?)
				`, fixed.Unix(), fixed.Unix())
				if err != nil {
					t.Fatal(err)
				}
				teamID, err = result.LastInsertId()
				if err != nil {
					t.Fatal(err)
				}
			}

			assertTrialEnrollment(t, db, teamID, fixed.Unix())
		})
	}
}

// TestControlRollsBackTeamCreationWhenEnrollmentFails proves the trigger's
// lifecycle write and the outer team insert are one SQLite atomic statement.
func TestControlRollsBackTeamCreationWhenEnrollmentFails(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)
	if _, err := Run(ctx, db, controlBase()); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER reject_trial_enrollment
		BEFORE INSERT ON account_lifecycle
		BEGIN
			SELECT RAISE(ABORT, 'injected lifecycle failure');
		END;
	`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO teams (id, name, created_at, updated_at)
		VALUES (77, 'Must roll back', 100, 100)
	`); err == nil {
		t.Fatal("team insert succeeded despite lifecycle enrollment failure")
	}

	var teams int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM teams WHERE id = 77").Scan(&teams); err != nil {
		t.Fatal(err)
	}
	if teams != 0 {
		t.Fatalf("failed enrollment left %d team rows", teams)
	}
}

// TestControlBackfillsMissingLifecycleAtUpgradeTime starts old beta accounts
// fresh while proving existing paid and lapse clocks and mirrors are unchanged.
func TestControlBackfillsMissingLifecycleAtUpgradeTime(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	throughFive := Set{Name: "control"}
	for _, migration := range Control().Migrations {
		if migration.Version <= 5 {
			throughFive.Migrations = append(throughFive.Migrations, migration)
		}
	}
	if _, err := Run(ctx, db, throughFive); err != nil {
		t.Fatal(err)
	}

	lapseStarted := int64(300)
	lapseTrialEnds := lapseStarted + int64(30*24*time.Hour/time.Second)
	lapseAcceptUntil := lapseStarted + int64(60*24*time.Hour/time.Second)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO teams (id, name, trial_ends_at, accept_traffic_until, created_at, updated_at)
		VALUES
			(1, 'Beta without clock', 11, 12, 1, 1),
			(2, 'Paid', NULL, NULL, 2, 2),
			(3, 'Lapsed', ?, ?, 3, 3)
	`, lapseTrialEnds, lapseAcceptUntil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO subscriptions
			(team_id, stripe_customer_id, stripe_subscription_id, status, plan,
			 stripe_price_id, billing_email, created_at, updated_at)
		VALUES (2, 'cus_paid', 'sub_paid', 'active', 'monthly', 'price_monthly',
		        'paid@example.test', 2, 2)
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO account_lifecycle
			(team_id, trigger, started_at, deleted_at, created_at, updated_at)
		VALUES
			(2, '', NULL, NULL, 20, 21),
			(3, 'lapse', ?, NULL, 30, 31)
	`, lapseStarted); err != nil {
		t.Fatal(err)
	}

	before := time.Now().UTC().Unix()
	result, err := Run(ctx, db, controlBase())
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC().Unix()
	if fmt.Sprint(result.Applied) != "[6 7 8 9]" {
		t.Fatalf("upgrade applied %v, want Stripe 6, topology 7, then M9 8 and 9", result.Applied)
	}

	var backfilledStarted int64
	if err := db.QueryRowContext(ctx,
		"SELECT started_at FROM account_lifecycle WHERE team_id = 1 AND trigger = 'trial'").
		Scan(&backfilledStarted); err != nil {
		t.Fatal(err)
	}
	if backfilledStarted < before || backfilledStarted > after || backfilledStarted == 1 {
		t.Fatalf("backfill started at %d, want migration time between %d and %d", backfilledStarted, before, after)
	}
	assertTrialEnrollment(t, db, 1, backfilledStarted)

	var (
		paidTrigger                   string
		paidStarted                   sql.NullInt64
		paidTrial, paidAccept         sql.NullInt64
		paidCreated, paidUpdated      int64
		lapseTrigger                  string
		gotLapseStarted               int64
		gotLapseTrial, gotLapseAccept int64
		lapseCreated, lapseUpdated    int64
	)
	if err := db.QueryRowContext(ctx, `
		SELECT account_lifecycle.trigger, account_lifecycle.started_at,
		       account_lifecycle.created_at, account_lifecycle.updated_at,
		       teams.trial_ends_at, teams.accept_traffic_until
		FROM account_lifecycle JOIN teams ON teams.id = account_lifecycle.team_id
		WHERE account_lifecycle.team_id = 2
	`).Scan(&paidTrigger, &paidStarted, &paidCreated, &paidUpdated, &paidTrial, &paidAccept); err != nil {
		t.Fatal(err)
	}
	if paidTrigger != "" || paidStarted.Valid || paidTrial.Valid || paidAccept.Valid || paidCreated != 20 || paidUpdated != 21 {
		t.Fatalf("paid lifecycle changed to trigger=%q started=%v mirrors=%v/%v created=%d updated=%d",
			paidTrigger, paidStarted, paidTrial, paidAccept, paidCreated, paidUpdated)
	}

	if err := db.QueryRowContext(ctx, `
		SELECT account_lifecycle.trigger, account_lifecycle.started_at,
		       account_lifecycle.created_at, account_lifecycle.updated_at,
		       teams.trial_ends_at, teams.accept_traffic_until
		FROM account_lifecycle JOIN teams ON teams.id = account_lifecycle.team_id
		WHERE account_lifecycle.team_id = 3
	`).Scan(&lapseTrigger, &gotLapseStarted, &lapseCreated, &lapseUpdated,
		&gotLapseTrial, &gotLapseAccept); err != nil {
		t.Fatal(err)
	}
	if lapseTrigger != "lapse" || gotLapseStarted != lapseStarted ||
		gotLapseTrial != lapseTrialEnds || gotLapseAccept != lapseAcceptUntil ||
		lapseCreated != 30 || lapseUpdated != 31 {
		t.Fatalf("lapse lifecycle changed to trigger=%q started=%d mirrors=%d/%d created=%d updated=%d",
			lapseTrigger, gotLapseStarted, gotLapseTrial, gotLapseAccept, lapseCreated, lapseUpdated)
	}

	second, err := Run(ctx, db, controlBase())
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed() {
		t.Fatalf("second migration run applied %v", second.Applied)
	}
	var secondStarted int64
	if err := db.QueryRowContext(ctx, "SELECT started_at FROM account_lifecycle WHERE team_id = 1").Scan(&secondStarted); err != nil {
		t.Fatal(err)
	}
	if secondStarted != backfilledStarted {
		t.Fatalf("second migration moved backfill from %d to %d", backfilledStarted, secondStarted)
	}
}

// TestControlUpgradesPopulatedBillingFromFiveToSix proves the production shape
// keeps existing Stripe, lifecycle, and event data while migration 6 adds the
// payment-ordering tables and migration 7 follows it with salt authority.
func TestControlUpgradesPopulatedBillingFromFiveToSix(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	throughFive := Set{Name: "control"}
	for _, migration := range Control().Migrations {
		if migration.Version <= 5 {
			throughFive.Migrations = append(throughFive.Migrations, migration)
		}
	}
	if _, err := Run(ctx, db, throughFive); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, created_at, updated_at)
		VALUES (1, 'owner@example.com', 1, 1);
		INSERT INTO teams (id, name, created_at, updated_at)
		VALUES (1, 'Existing account', 1, 1);
		INSERT INTO teams (id, name, created_at, updated_at)
		VALUES
			(2, 'Paid annual account', 2, 2),
			(3, 'Unpaid beta account', 3, 3),
			(4, 'Pending deletion account', 4, 4);
		INSERT INTO team_memberships (team_id, user_id, role, created_at)
		VALUES (1, 1, 'owner', 1);
		INSERT INTO subscriptions
			(team_id, stripe_customer_id, stripe_subscription_id, status, plan,
			 stripe_price_id, billing_email, created_at, updated_at)
		VALUES (1, 'cus_existing', 'sub_existing', 'active', 'monthly',
		        'price_monthly', 'billing@example.com', 1, 1);
		INSERT INTO subscriptions
			(team_id, stripe_customer_id, stripe_subscription_id, status, plan,
			 stripe_price_id, billing_email, created_at, updated_at)
		VALUES (2, 'cus_annual', 'sub_annual', 'active', 'yearly',
		        'price_yearly', 'annual@example.com', 2, 2);
		INSERT INTO subscriptions
			(team_id, stripe_customer_id, stripe_subscription_id, status, plan,
			 stripe_price_id, billing_email, created_at, updated_at)
		VALUES (4, 'cus_pending', 'sub_pending', 'active', 'yearly',
		        'price_yearly', 'pending@example.com', 4, 4);
		INSERT INTO account_lifecycle
			(team_id, trigger, started_at, created_at, updated_at)
		VALUES (1, 'lapse', 10, 10, 10);
		INSERT INTO lifecycle_emails
			(team_id, started_at, template, recipient, outcome, sent_at)
		VALUES
			(1, 10, 'ending_soon', 'billing@example.com', 'smtp/accepted: queued', 20),
			(1, 10, 'ending_tomorrow', 'billing@example.com', 'failed: relay down', 21);
			INSERT INTO stripe_events
			(event_id, type, team_id, payload, received_at, handled_at, outcome)
			VALUES ('evt_existing', 'invoice.payment_failed', 1, '{}', 10, 10, 'applied');
			INSERT INTO account_deletions
				(team_id, team_name, contact_email, stripe_customer_id,
				 clock_started_at, started_at, notes)
			VALUES (4, 'Pending deletion account', 'pending@example.com', 'cus_pending', 11, 80, 'claimed');
			INSERT INTO account_deletions
				(team_id, team_name, contact_email, stripe_customer_id,
				 clock_started_at, started_at, notes)
			VALUES (99, 'Historical deletion', 'old@example.com', 'cus_old', 1, 91, 'claimed');
			INSERT INTO account_deletions
				(team_id, team_name, contact_email, stripe_customer_id,
				 clock_started_at, started_at, completed_at, notified_at, notes)
			VALUES (100, 'Failed provider deletion', 'failed@example.com', 'cus_failed',
			        2, 92, 93, 94, 'payment customer NOT removed: provider unavailable; control rows removed');
	`); err != nil {
		t.Fatal(err)
	}

	result, err := Run(ctx, db, controlBase())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(result.Applied) != "[6 7 8 9]" {
		t.Fatalf("upgrade applied %v, want Stripe 6, topology 7, then M9 8 and 9", result.Applied)
	}

	var customer, subscription, paymentState string
	var failedAt sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT stripe_customer_id, stripe_subscription_id, payment_state, payment_failed_at
		FROM subscriptions WHERE team_id = 1
	`).Scan(&customer, &subscription, &paymentState, &failedAt); err != nil {
		t.Fatal(err)
	}
	if customer != "cus_existing" || subscription != "sub_existing" || paymentState != "" || failedAt.Valid {
		t.Fatalf("populated subscription changed to customer=%q subscription=%q payment=%q failed_at=%v",
			customer, subscription, paymentState, failedAt)
	}

	for _, table := range []string{
		"billing_account_leases", "billing_account_customers", "billing_quiescence_objects", "billing_checkouts",
		"billing_checkout_cleanup", "lifecycle_account_leases", "lifecycle_outbox",
		"account_deletion_customers", "team_id_sequence", "destructive_operations",
		"report_subscriptions", "alert_rules", "notification_claims", "cron_slots",
	} {
		if !tableExists(t, db, table) {
			t.Errorf("upgraded control schema is missing %s", table)
		}
	}

	var annualPaymentState string
	if err := db.QueryRowContext(ctx, `
		SELECT payment_state FROM subscriptions WHERE team_id = 2
	`).Scan(&annualPaymentState); err != nil {
		t.Fatal(err)
	}
	if annualPaymentState != "paid" {
		t.Fatalf("annual subscription payment_state=%q, want paid", annualPaymentState)
	}

	var pendingPaymentState string
	if err := db.QueryRowContext(ctx, `
		SELECT payment_state FROM subscriptions WHERE team_id = 4
	`).Scan(&pendingPaymentState); err != nil {
		t.Fatal(err)
	}
	if pendingPaymentState != "" {
		t.Fatalf("pending-deletion subscription payment_state=%q, want terminal evidence preserved", pendingPaymentState)
	}
	var pendingClock int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM account_lifecycle WHERE team_id = 4
	`).Scan(&pendingClock); err != nil {
		t.Fatal(err)
	}
	if pendingClock != 0 {
		t.Fatalf("pending deletion received %d manufactured lifecycle clocks", pendingClock)
	}

	var events, clocks, paidClock, betaClock int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM stripe_events WHERE event_id = 'evt_existing'").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_lifecycle WHERE team_id = 1").Scan(&clocks); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_lifecycle WHERE team_id = 2").Scan(&paidClock); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM account_lifecycle
		WHERE team_id = 3 AND trigger = 'trial' AND started_at > 0
	`).Scan(&betaClock); err != nil {
		t.Fatal(err)
	}
	if events != 1 || clocks != 1 || paidClock != 0 || betaClock != 1 {
		t.Fatalf("upgrade retained events=%d existing_clocks=%d paid_clocks=%d beta_clocks=%d",
			events, clocks, paidClock, betaClock)
	}

	var completed, retryable int
	if err := db.QueryRowContext(ctx, `
		SELECT
			SUM(CASE WHEN completed_at IS NOT NULL THEN 1 ELSE 0 END),
			SUM(CASE WHEN completed_at IS NULL AND lease_expires_at = 0 THEN 1 ELSE 0 END)
		FROM lifecycle_outbox WHERE team_id = 1
	`).Scan(&completed, &retryable); err != nil {
		t.Fatal(err)
	}
	if completed != 1 || retryable != 1 {
		t.Fatalf("upgraded outbox has completed=%d retryable=%d, want one each", completed, retryable)
	}

	var localRemoved, providerRemoved, controlRemoved sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT local_removed_at, provider_removed_at, control_removed_at
		FROM account_deletions WHERE team_id = 99
	`).Scan(&localRemoved, &providerRemoved, &controlRemoved); err != nil {
		t.Fatal(err)
	}
	if localRemoved.Valid || providerRemoved.Valid || controlRemoved.Valid {
		t.Fatalf("historical deletion checkpoints are local=%v provider=%v control=%v",
			localRemoved, providerRemoved, controlRemoved)
	}

	var pendingLegacyCustomers, reopenedFailures int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM account_deletion_customers
		WHERE (team_id = 99 AND customer_id = 'cus_old')
		   OR (team_id = 100 AND customer_id = 'cus_failed')
	`).Scan(&pendingLegacyCustomers); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM account_deletions
		WHERE team_id = 100 AND completed_at IS NULL AND notified_at = 94
	`).Scan(&reopenedFailures); err != nil {
		t.Fatal(err)
	}
	if pendingLegacyCustomers != 2 || reopenedFailures != 1 {
		t.Fatalf("legacy provider recovery has customers=%d reopened=%d, want 2 and 1",
			pendingLegacyCustomers, reopenedFailures)
	}

	var lastTeamID int64
	if err := db.QueryRowContext(ctx, `SELECT last_id FROM team_id_sequence WHERE singleton = 1`).Scan(&lastTeamID); err != nil {
		t.Fatal(err)
	}
	if lastTeamID != 100 {
		t.Fatalf("team id sequence started at %d, want historical maximum 100", lastTeamID)
	}
}

// TestAccountMigratesAFreshDatabase covers the complete assembled schema every
// event is written into, including M9 annotations/health and sampling facts.
func TestAccountMigratesAFreshDatabase(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if _, err := Run(ctx, db, Account()); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"events", "event_details", "sessions", "dim_pathname", "dim_source", "dim_event_name",
		"ingest_session_state", "ingest_orphan_engagements", "hostname_rejections", "session_id_allocator",
		"annotations", "ingest_health", "ingest_last_request", "ingest_observations",
		"sampling_strata", "event_sampling", "session_sampling", "sampling_daily_counts",
	} {
		if !tableExists(t, db, table) {
			t.Errorf("account schema is missing %s", table)
		}
	}

	// The three filter indexes are the whole reason a filtered query does not
	// fall back to a full scan, and an index is easy to drop by accident in a
	// later migration.
	for _, index := range []string{
		"events_main", "events_session", "events_page", "events_source", "events_country",
	} {
		var count int

		query := "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?"
		if err := db.QueryRowContext(ctx, query, index).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Errorf("account schema is missing index %s", index)
		}
	}
}

// TestAccountV7ToCurrentKeepsPopulatedSessionOwnership validates the deployed
// topology and M9 upgrade before sampling 0011 materializes its session facts,
// without losing sessions or permanent UUID receipts.
func TestAccountV7ToCurrentKeepsPopulatedSessionOwnership(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	throughSeven := UpTo(Account(), 7)
	if _, err := Run(ctx, db, throughSeven); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sessions (id, site_id, user_id, started_at, last_seen_at)
		VALUES (41, 7, 9001, 100, 200);
		INSERT INTO recent_event_ids (event_uuid, received_at)
		VALUES (x'00112233445566778899aabbccddeeff', 123);
	`); err != nil {
		t.Fatal(err)
	}

	result, err := Run(ctx, db, Account())
	if err != nil {
		t.Fatal(err)
	}
	if result.From != 7 || result.To != 11 || fmt.Sprint(result.Applied) != "[8 9 10 11]" {
		t.Fatalf("account upgrade moved from %d to %d via %v, want 7 to 11 via [8 9 10 11]",
			result.From, result.To, result.Applied)
	}

	var nextID int64
	if err := db.QueryRowContext(ctx,
		"SELECT next_id FROM session_id_allocator WHERE singleton = 1").Scan(&nextID); err != nil {
		t.Fatal(err)
	}
	if nextID != 42 {
		t.Fatalf("allocator starts at %d, want 42", nextID)
	}

	for name, query := range map[string]string{
		"session": "SELECT COUNT(*) FROM sessions WHERE id = 41 AND site_id = 7 AND user_id = 9001",
		"receipt": "SELECT COUNT(*) FROM recent_event_ids WHERE event_uuid = x'00112233445566778899aabbccddeeff'",
	} {
		var count int
		if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("%s row count = %d, want 1", name, count)
		}
	}

	var pruningIndexes int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'recent_event_ids_received'`).Scan(&pruningIndexes); err != nil {
		t.Fatal(err)
	}
	if pruningIndexes != 0 {
		t.Fatal("account upgrade retained the obsolete timed-pruning receipt index")
	}

	var sampledSessions int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM session_sampling WHERE session_id = 41 AND site_id = 7").Scan(&sampledSessions); err != nil {
		t.Fatal(err)
	}
	if sampledSessions != 1 {
		t.Fatalf("sampling migration materialized %d session rows, want 1", sampledSessions)
	}
}

// TestCoordinatedMigrationNumbers pins the upgrade order requested by the
// consolidated runtime: account ingest state and hostname authority follow the
// deployed 0007 settings migration, while control salt authority follows Stripe
// 0006.
func TestCoordinatedMigrationNumbers(t *testing.T) {
	for name, test := range map[string]struct {
		set  Set
		want []int
	}{
		"account": {set: Account(), want: []int{1, 2, 3, 4, 5, 7, 8, 9, 10, 11}},
		"control": {set: Control(), want: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
	} {
		t.Run(name, func(t *testing.T) {
			got := make([]int, 0, len(test.set.Migrations))
			for _, migration := range test.set.Migrations {
				got = append(got, migration.Version)
			}
			if fmt.Sprint(got) != fmt.Sprint(test.want) {
				t.Fatalf("migration order = %v, want %v", got, test.want)
			}
		})
	}
}

// TestEveryDimensionTableSeedsTheEmptyString pins the rule the whole ingest
// path depends on: id 0 is the empty string everywhere, so "not set" is an
// ordinary id and no query needs a NULL branch.
func TestEveryDimensionTableSeedsTheEmptyString(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if _, err := Run(ctx, db, Account()); err != nil {
		t.Fatal(err)
	}

	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'dim_%'")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(tables) == 0 {
		t.Fatal("no dimension tables were created")
	}

	for _, table := range tables {
		var value string

		if err := db.QueryRowContext(ctx, "SELECT value FROM "+table+" WHERE id = 0").Scan(&value); err != nil {
			t.Errorf("%s has no id 0: %v", table, err)
			continue
		}
		if value != "" {
			t.Errorf("%s id 0 is %q, want the empty string", table, value)
		}
	}
}

// TestRunningTwiceChangesNothing is the property that makes an interrupted run
// resumable: migrate is re-run until it succeeds, so a second pass over an
// already-current database has to be a no-op rather than an error.
func TestRunningTwiceChangesNothing(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if _, err := Run(ctx, db, Account()); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, "INSERT INTO dim_pathname (value) VALUES ('/pricing')"); err != nil {
		t.Fatal(err)
	}

	result, err := Run(ctx, db, Account())
	if err != nil {
		t.Fatal(err)
	}

	if result.Changed() {
		t.Fatalf("a second run applied %v", result.Applied)
	}
	if result.From != result.To {
		t.Fatalf("a second run moved the version from %d to %d", result.From, result.To)
	}

	// Data written between the two runs has to survive, or "idempotent" would
	// mean "rebuilt from scratch every time".
	var value string
	if err := db.QueryRowContext(ctx, "SELECT value FROM dim_pathname WHERE value = '/pricing'").Scan(&value); err != nil {
		t.Fatalf("the second run lost data: %v", err)
	}
}

// TestRunRequiresTheContiguousPendingSequence covers fresh, ordinary upgrade,
// missing-gap, and already-applied cases. Most importantly, validation happens
// before migration SQL so a later branch cannot stamp past absent versions.
func TestRunRequiresTheContiguousPendingSequence(t *testing.T) {
	ctx := context.Background()
	contiguous := Set{Name: "test", Migrations: []Migration{
		{Version: 1, Name: "one", SQL: `CREATE TABLE one (id INTEGER);`},
		{Version: 2, Name: "two", SQL: `CREATE TABLE two (id INTEGER);`},
	}}

	t.Run("fresh", func(t *testing.T) {
		db := newDatabase(t)
		result, err := Run(ctx, db, contiguous)
		if err != nil {
			t.Fatal(err)
		}
		if result.From != 0 || result.To != 2 || !tableExists(t, db, "one") || !tableExists(t, db, "two") {
			t.Fatalf("fresh result = %+v, one=%v two=%v", result, tableExists(t, db, "one"), tableExists(t, db, "two"))
		}
	})

	t.Run("upgrade", func(t *testing.T) {
		db := newDatabase(t)
		if _, err := Run(ctx, db, UpTo(contiguous, 1)); err != nil {
			t.Fatal(err)
		}
		result, err := Run(ctx, db, contiguous)
		if err != nil {
			t.Fatal(err)
		}
		if result.From != 1 || result.To != 2 || len(result.Applied) != 1 || result.Applied[0] != 2 {
			t.Fatalf("upgrade result = %+v", result)
		}
	})

	t.Run("missing gap", func(t *testing.T) {
		db := newDatabase(t)
		gapped := Set{Name: "test", Migrations: []Migration{
			{Version: 1, Name: "one", SQL: `CREATE TABLE should_not_exist (id INTEGER);`},
			{Version: 3, Name: "three", SQL: `SELECT 1;`},
		}}
		result, err := Run(ctx, db, gapped)
		if err == nil {
			t.Fatal("gapped migration sequence was accepted")
		}
		if result.From != 0 || result.To != 0 || tableExists(t, db, "should_not_exist") {
			t.Fatalf("gap wrote before validation: result=%+v table=%v", result, tableExists(t, db, "should_not_exist"))
		}
		if got := err.Error(); got != "test schema is at version 0: migration 0002 is missing before 0003_three; merge the reserved lower migration before upgrading" {
			t.Fatalf("gap error = %q", got)
		}
	})

	t.Run("already applied", func(t *testing.T) {
		db := newDatabase(t)
		if err := store.SetSchemaVersion(ctx, db, 3); err != nil {
			t.Fatal(err)
		}
		gapped := Set{Name: "test", Migrations: []Migration{
			{Version: 1, Name: "one", SQL: `SELECT 1;`},
			{Version: 3, Name: "three", SQL: `SELECT 1;`},
		}}
		result, err := Run(ctx, db, gapped)
		if err != nil {
			t.Fatal(err)
		}
		if result.Changed() || result.From != 3 || result.To != 3 {
			t.Fatalf("already-applied result = %+v", result)
		}
	})
}

// TestControlFinalChainAppliesM8AfterM9 proves the merged chain advances from
// the real M9 schema through M8's cleanup without a placeholder or version gap.
func TestControlFinalChainAppliesM8AfterM9(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if _, err := Run(ctx, db, controlBase()); err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, db, Control())
	if err != nil {
		t.Fatal(err)
	}
	if result.From != 9 || result.To != 10 || fmt.Sprint(result.Applied) != "[10]" {
		t.Fatalf("control M8 upgrade = %+v, want 9 through [10]", result)
	}
}

// TestRunRefusesADatabaseFromANewerBuild covers a rolled-back deploy. Running
// queries against a schema this binary does not know about would fail somewhere
// far away from the cause, so it is refused up front.
func TestRunRefusesADatabaseFromANewerBuild(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	if err := store.SetSchemaVersion(ctx, db, 999); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(ctx, db, Control()); err == nil {
		t.Fatal("expected an error for a database newer than the binary")
	}
}

// TestFreshEmptiesAndRebuilds covers the development flag. It has to leave a
// database that migrates cleanly again, otherwise the only way out is deleting
// files by hand.
func TestFreshEmptiesAndRebuilds(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	base := controlBase()
	if _, err := Run(ctx, db, base); err != nil {
		t.Fatal(err)
	}

	// A row with a foreign key pointing at it, so the drop order actually
	// matters rather than being trivially safe.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Acme', 0, 0);
		INSERT INTO sites (id, account_id, domain, created_at, updated_at) VALUES (1, 1, 'example.com', 0, 0);
	`); err != nil {
		t.Fatal(err)
	}

	if err := Fresh(ctx, db); err != nil {
		t.Fatal(err)
	}

	if tableExists(t, db, "users") {
		t.Fatal("fresh left the schema behind")
	}

	version, err := store.SchemaVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("fresh left the version at %d", version)
	}

	result, err := Run(ctx, db, base)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed() {
		t.Fatal("the rebuilt database applied nothing")
	}

	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sites").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("fresh kept %d rows", count)
	}
}

// TestForeignKeysSurviveFresh checks the pinned connection is handed back the
// way every other connection in the process is configured. A pool connection
// that quietly lost foreign key enforcement would let orphans through days
// later, with nothing pointing back at this command.
func TestForeignKeysSurviveFresh(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	base := controlBase()
	if _, err := Run(ctx, db, base); err != nil {
		t.Fatal(err)
	}
	if err := Fresh(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, db, base); err != nil {
		t.Fatal(err)
	}

	// team_memberships has no team 42, so this must be refused.
	_, err := db.ExecContext(ctx, "INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (42, 42, 'owner', 0)")
	if err == nil {
		t.Fatal("foreign keys are not being enforced after fresh")
	}
}

// TestSetsAreOrdered guards the loader. Migrations applying out of order, or a
// version appearing twice, is a class of bug that only shows up on someone
// else's filesystem.
func TestSetsAreOrdered(t *testing.T) {
	for _, set := range []Set{Control(), Account()} {
		if len(set.Migrations) == 0 {
			t.Fatalf("%s has no migrations", set.Name)
		}

		previous := 0
		for _, migration := range set.Migrations {
			if migration.Version <= previous {
				t.Fatalf("%s: version %d follows %d", set.Name, migration.Version, previous)
			}
			if migration.SQL == "" {
				t.Fatalf("%s: migration %d is empty", set.Name, migration.Version)
			}

			previous = migration.Version
		}

		if set.Version() != previous {
			t.Fatalf("%s reports version %d, want %d", set.Name, set.Version(), previous)
		}
	}
}

// TestParseName covers the filename rules. The version lives in the filename so
// it is visible in a diff, which only works if a malformed name is rejected
// rather than guessed at.
func TestParseName(t *testing.T) {
	version, name, err := parseName("0012_add_rollups.sql")
	if err != nil {
		t.Fatal(err)
	}
	if version != 12 || name != "add_rollups" {
		t.Fatalf("got %d %q", version, name)
	}

	for _, bad := range []string{"initial.sql", "abc_initial.sql", "0000_zero.sql"} {
		if _, _, err := parseName(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// TestPublicAPIMigrationKeepsExistingKeys runs the control migrations in two
// halves with data written in between.
//
// The 0004 migration rebuilds api_keys to change a column default, which SQLite
// can only express by writing the table again — and a table rebuild is the one
// migration shape that can silently drop rows. Migrating a fresh database would
// never notice, because there is nothing in the table to lose.
func TestPublicAPIMigrationKeepsExistingKeys(t *testing.T) {
	ctx := context.Background()

	db, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Stop short of 0004, so the database is at the shape somebody upgrading
	// from an earlier build actually has.
	earlier := Set{Name: "control"}
	for _, migration := range Control().Migrations {
		if migration.Version < 3 {
			earlier.Migrations = append(earlier.Migrations, migration)
		}
	}

	if _, err := Run(ctx, db, earlier); err != nil {
		t.Fatal(err)
	}

	seed := []string{
		`INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Test', 0, 0)`,
		`INSERT INTO users (id, email, created_at, updated_at) VALUES (1, 'a@example.test', 0, 0)`,
		`INSERT INTO api_keys (id, team_id, user_id, name, key_hash, scopes, hourly_limit, created_at)
		 VALUES (1, 1, 1, 'existing', 'deadbeef', '["stats:read"]', 600, 0)`,
	}

	for _, statement := range seed {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	if _, err := Run(ctx, db, UpTo(Control(), 5)); err != nil {
		t.Fatalf("the 0004 migration failed on a database with data in it: %v", err)
	}

	var (
		name   string
		hash   string
		scopes string
		limit  int
	)

	if err := db.QueryRow(
		`SELECT name, key_hash, scopes, hourly_limit FROM api_keys WHERE id = 1`).
		Scan(&name, &hash, &scopes, &limit); err != nil {
		t.Fatalf("the existing key did not survive the rebuild: %v", err)
	}

	if name != "existing" || hash != "deadbeef" || scopes != `["stats:read"]` {
		t.Errorf("the key came back as %q/%q/%q", name, hash, scopes)
	}

	// The old hard-coded 600 becomes "take the deployment's configured limit",
	// which is the whole point of the rebuild: the shipped default moves without
	// anybody editing a database by hand.
	if limit != 0 {
		t.Errorf("hourly_limit = %d, want 0 meaning the configured default", limit)
	}

	// The new columns exist and the new default applies to a key created after
	// the migration.
	if _, err := db.Exec(
		`INSERT INTO api_keys (team_id, user_id, key_hash, key_prefix, created_at) VALUES (1, 1, 'cafe', 'feas_ab', 0)`); err != nil {
		t.Fatal(err)
	}

	var fresh sql.NullInt64
	if err := db.QueryRow(`SELECT hourly_limit FROM api_keys WHERE key_hash = 'cafe'`).Scan(&fresh); err != nil {
		t.Fatal(err)
	}

	if fresh.Int64 != 0 {
		t.Errorf("a new key defaulted to %d", fresh.Int64)
	}

	// Everything else the migration adds has to be there too, because a
	// half-applied migration is exactly what the transaction is supposed to make
	// impossible.
	for _, table := range []string{
		"site_tracker_config", "site_custom_properties",
		"webhook_endpoints", "webhook_deliveries",
		"mcp_oauth_clients", "mcp_oauth_codes", "mcp_oauth_tokens",
	} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}

		if count != 1 {
			t.Errorf("table %s was not created", table)
		}
	}
}

// TestAccountDeletionCleanupMigration0010 starts from M9's complete schema and
// verifies both the ordered upgrade and M8's deletion-audit data rewrite.
func TestAccountDeletionCleanupMigration0010(t *testing.T) {
	ctx := context.Background()
	db := newDatabase(t)

	result, err := Run(ctx, db, controlBase())
	if err != nil {
		t.Fatal(err)
	}
	if result.To != 9 {
		t.Fatalf("initial migration stopped at %d, want 9", result.To)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO account_deletions
			(team_id, team_name, contact_email, stripe_customer_id, clock_started_at,
			 started_at, completed_at, notified_at, notes)
		VALUES
			(101, 'Finished Team', 'finished@example.test', 'cus_finished', 1, 2, 3, 4,
			 'account directory /private/101 removed; payment customer cus_finished removed'),
			(102, 'Retry Team', 'retry@example.test', 'cus_retry', 1, 2, 3, NULL,
			 'account directory /private/102 removed; payment customer NOT removed: private provider error'),
			(103, 'Confirm Team', 'confirm@example.test', 'cus_confirm', 1, 2, 3, NULL,
			 'account directory /private/103 removed; payment customer cus_confirm removed')
	`)
	if err != nil {
		t.Fatal(err)
	}

	result, err = Run(ctx, db, Control())
	if err != nil {
		t.Fatal(err)
	}
	if result.From != 9 || result.To != 10 || fmt.Sprint(result.Applied) != "[10]" {
		t.Fatalf("deletion cleanup upgrade = %+v, want 9 through [10]", result)
	}

	tests := []struct {
		teamID         int
		teamName       string
		email          string
		customerID     string
		notes          string
		completed      bool
		controlRemoved bool
		localRemoved   bool
		artifactsKnown bool
		globalRemoved  bool
	}{
		{101, "", "", "", "live account data removed; deletion confirmation sent", false, false, true, false, false},
		{102, "Retry Team", "retry@example.test", "cus_retry", "live account data removed; payment customer removal pending", false, false, true, false, false},
		{103, "Confirm Team", "confirm@example.test", "", "live account data removed", false, false, true, false, false},
	}

	for _, test := range tests {
		var teamName, email, customerID, notes string
		var completed, controlRemoved, localRemoved, artifactsKnown, globalRemoved sql.NullInt64
		err := db.QueryRowContext(ctx, `
			SELECT team_name, contact_email, stripe_customer_id, notes,
			       completed_at, control_removed_at, local_removed_at,
			       artifacts_indexed_at, global_removed_at
			FROM account_deletions
			WHERE team_id = ?
		`, test.teamID).Scan(&teamName, &email, &customerID, &notes,
			&completed, &controlRemoved, &localRemoved, &artifactsKnown, &globalRemoved)
		if err != nil {
			t.Fatal(err)
		}

		if teamName != test.teamName || email != test.email || customerID != test.customerID || notes != test.notes {
			t.Errorf("team %d cleanup = %q/%q/%q/%q, want %q/%q/%q/%q",
				test.teamID, teamName, email, customerID, notes,
				test.teamName, test.email, test.customerID, test.notes)
		}
		if completed.Valid != test.completed || controlRemoved.Valid != test.controlRemoved ||
			localRemoved.Valid != test.localRemoved || artifactsKnown.Valid != test.artifactsKnown ||
			globalRemoved.Valid != test.globalRemoved {
			t.Errorf("team %d checkpoints completed/control/local/artifacts/global = %v/%v/%v/%v/%v, want %v/%v/%v/%v/%v",
				test.teamID, completed.Valid, controlRemoved.Valid, localRemoved.Valid, artifactsKnown.Valid, globalRemoved.Valid,
				test.completed, test.controlRemoved, test.localRemoved, test.artifactsKnown, test.globalRemoved)
		}
	}
}
