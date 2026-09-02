//
// db.go
// The `db migrate` and `db backup` subcommands.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

const dbHelp = `feasible db — database maintenance.

Commands:
  migrate   Migrate system.db and every account database.
  backup    Write a consistent snapshot of every database with VACUUM INTO.
`

const migrateHelp = `feasible db migrate — migrate every database.

Migrations never run on boot. Two processes racing migrations is a classic
self-hosting failure, and with one database per account the operation has to be
deliberate and resumable, so it is always an explicit command.

system.db is created if it does not exist yet, which is what makes this the
first command a fresh install runs. Account databases are migrated where they
already exist; a new account's database is created when the account is.

Flags:
`

const backupHelp = `feasible db backup — snapshot every database.

Uses VACUUM INTO, which takes a read transaction and compacts as it copies.
Copying the files with cp while the process is writing is not safe.

Flags:
`

// runDB dispatches the two-word database commands. It is separate from the root
// switch so `feasible db` on its own can list what it offers rather than
// printing the whole program's help.
func runDB(e *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, dbHelp)
		return ExitUsage
	}

	switch args[0] {
	case "migrate":
		return runDBMigrate(e, args[1:])
	case "backup":
		return runDBBackup(e, args[1:])
	default:
		fmt.Fprintf(e.stderr, "unknown db command %q\n\n", args[0])
		fmt.Fprint(e.stderr, dbHelp)
		return ExitUsage
	}
}

// target is one database to migrate, paired with the migrations that belong to
// it. System and account databases have unrelated schemas and independent
// version numbers, so the set travels with the path rather than being decided
// again inside the loop.
type target struct {
	path string
	set  migrate.Set
}

// runDBMigrate brings system.db and every account database up to date. It
// stops at the first failure and says which database it stopped on: with one
// database per account a partial run is normal and recoverable, but only if you
// can see where it got to.
func runDBMigrate(e *env, args []string) int {
	fs := newFlagSet("db migrate", e, migrateHelp)
	fresh := fs.Bool("fresh", false, "drop everything and rebuild from an empty schema")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	// --fresh destroys customer data. It is a development convenience and there
	// is no reason it should ever be reachable on a production box.
	if *fresh && e.cfg.IsProduction() {
		fmt.Fprintln(e.stderr, "refusing to run db migrate --fresh with FEASIBLE_ENV=production")
		return ExitError
	}

	ctx := context.Background()
	if err := renameLegacySystemDatabase(e.cfg.App.DataDir); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	targets, err := migrateTargets(e.cfg.App.DataDir, e.systemMigrations)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	for _, item := range targets {
		if err := migrateOne(ctx, e, item, *fresh); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}
	}

	e.log.Info("migrations complete",
		"data_dir", e.cfg.App.DataDir,
		"databases", len(targets),
	)

	return ExitOK
}

// renameLegacySystemDatabase performs the one-time filename upgrade before
// migrations open anything. Renaming the WAL and shared-memory sidecars with
// the database keeps a cleanly stopped WAL database intact; refusing two main
// files prevents an ambiguous merge from silently choosing one history.
func renameLegacySystemDatabase(dataDir string) error {
	legacy := filepath.Join(dataDir, config.LegacyDatabaseName)
	current := filepath.Join(dataDir, config.SystemDatabaseName)

	_, legacyErr := os.Stat(legacy)
	_, currentErr := os.Stat(current)
	if legacyErr != nil {
		if os.IsNotExist(legacyErr) {
			return nil
		}
		return fmt.Errorf("inspect legacy system database %s: %w", legacy, legacyErr)
	}
	if currentErr == nil {
		return fmt.Errorf("both %s and %s exist — refusing to guess which system database is authoritative", legacy, current)
	}
	if !os.IsNotExist(currentErr) {
		return fmt.Errorf("inspect system database %s: %w", current, currentErr)
	}

	// Move sidecars first and the authoritative main file last. If any rename
	// fails, the legacy main filename remains as the retry marker and another
	// `db migrate` can finish instead of mistaking a partial move for success.
	for _, suffix := range []string{"-wal", "-shm", ""} {
		source := legacy + suffix
		destination := current + suffix
		if _, err := os.Stat(source); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspect legacy database file %s: %w", source, err)
		}
		if err := os.Rename(source, destination); err != nil {
			return fmt.Errorf("rename %s to %s: %w", source, destination, err)
		}
	}

	return nil
}

// migrateOne migrates a single database and reports what it did. Each database
// is opened and closed in turn rather than all at once, because a box with a
// thousand accounts would otherwise hold a thousand sets of handles open for
// the length of the run.
func migrateOne(ctx context.Context, e *env, item target, fresh bool) (err error) {
	db, err := store.OpenDatabase(item.path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("%s: close database: %w", item.path, closeErr))
		}
	}()

	if fresh {
		if err := migrate.Fresh(ctx, db.Writer()); err != nil {
			return fmt.Errorf("%s: %w", item.path, err)
		}

		e.log.Warn("database emptied", "database", item.path)
	}

	started := time.Now()

	result, err := migrate.Run(ctx, db.Writer(), item.set)
	if err != nil {
		return fmt.Errorf("%s: %w", item.path, err)
	}

	// The no-op case is logged as loudly as the change. Re-running migrate is
	// how an interrupted run is resumed, and someone doing that needs to see
	// every database confirmed rather than silence.
	if !result.Changed() {
		e.log.Info("database already current",
			"database", item.path,
			"schema", item.set.Name,
			"schema_version", result.To,
		)

		return nil
	}

	e.log.Info("database migrated",
		"database", item.path,
		"schema", item.set.Name,
		"from", result.From,
		"schema_version", result.To,
		"applied", len(result.Applied),
		"duration", time.Since(started),
	)

	return nil
}

// migrateTargets lists what a migration run has to visit. system.db is always
// included, existing or not, because creating it is how a fresh install gets a
// schema at all. Account databases are only visited where they already exist —
// an account's database is created with the account, and inventing one here
// would mean guessing an id.
func migrateTargets(dataDir string, systemMigrations migrate.Set) ([]target, error) {
	targets := []target{{
		path: filepath.Join(dataDir, config.SystemDatabaseName),
		set:  systemMigrations,
	}}

	ids, err := accounts.Discover(dataDir)
	if err != nil {
		return nil, err
	}

	for _, id := range ids {
		targets = append(targets, target{
			path: accounts.Path(dataDir, id),
			set:  migrate.Account(),
		})
	}

	return targets, nil
}

// runDBBackup snapshots every database into a dated file. It reports a failure
// loudly rather than exiting zero on an empty run: a backup command that says
// nothing when it backed nothing up is how people discover they have no backups.
func runDBBackup(e *env, args []string) int {
	fs := newFlagSet("db backup", e, backupHelp)
	out := fs.String("out", filepath.Join(e.cfg.App.DataDir, "backups"), "directory to write snapshots into")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	ctx := context.Background()

	databases, err := discoverDatabases(e.cfg.App.DataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	if len(databases) == 0 {
		fmt.Fprintf(e.stderr, "no databases found in %s — nothing was backed up\n", e.cfg.App.DataDir)
		return ExitError
	}

	stamp := time.Now().UTC().Format("20060102T150405Z")

	for _, path := range databases {
		started := time.Now()

		dest := filepath.Join(*out, fmt.Sprintf("%s-%s.db", snapshotName(path), stamp))

		if err := store.Backup(ctx, path, dest); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		e.log.Info("database backed up",
			"source", path,
			"destination", dest,
			"duration", time.Since(started),
		)
	}

	return ExitOK
}

// snapshotName turns a database path into the stem of its snapshot file. Every
// account database is called analytics.db, so the account id has to be in the
// name or a directory of snapshots would be a directory of collisions.
func snapshotName(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".db")

	if filepath.Base(path) != accounts.DatabaseName {
		return base
	}

	return "account-" + filepath.Base(filepath.Dir(path))
}

// discoverDatabases lists the database files that already exist under the data
// directory. It is used by the commands that must never create anything —
// unlike migrate, which creates system.db on purpose.
func discoverDatabases(dataDir string) ([]string, error) {
	var found []string

	system := filepath.Join(dataDir, config.SystemDatabaseName)
	if _, err := os.Stat(system); err == nil {
		found = append(found, system)
	} else if os.IsNotExist(err) {
		legacy := filepath.Join(dataDir, config.LegacyDatabaseName)
		if _, legacyErr := os.Stat(legacy); legacyErr == nil {
			return nil, fmt.Errorf("legacy database %s must be renamed before backup — stop every feasible process and run `feasible db migrate`", legacy)
		}
	}

	ids, err := accounts.Discover(dataDir)
	if err != nil {
		return nil, err
	}

	for _, id := range ids {
		found = append(found, accounts.Path(dataDir, id))
	}

	// A stable order matters: a partially failed run should stop at the same
	// place on a retry, so the run is resumable by re-running it.
	sort.Strings(found)

	return found, nil
}
