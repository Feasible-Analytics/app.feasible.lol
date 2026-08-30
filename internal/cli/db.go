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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

const dbHelp = `feasible db — database maintenance.

Commands:
  migrate   Migrate control.db and every account database.
  backup    Write a consistent snapshot of every database with VACUUM INTO.
`

const migrateHelp = `feasible db migrate — migrate every database.

Migrations never run on boot. Two processes racing migrations is a classic
self-hosting failure, and with one database per account the operation has to be
deliberate and resumable, so it is always an explicit command.

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

// runDBMigrate reports what each database is at and what it would be moved to.
// No migrations are registered yet, so today it is a survey; the walk over every
// database and the per-database schema version it prints are the parts that have
// to be right, because a partial migration across N account databases is only
// recoverable if you can see where it stopped.
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

	databases, err := discoverDatabases(e.cfg.App.DataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	if len(databases) == 0 {
		e.log.Info("no databases found yet",
			"data_dir", e.cfg.App.DataDir,
			"fresh", *fresh,
		)
		return ExitOK
	}

	for _, path := range databases {
		db, err := store.Open(path)
		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		version, err := store.SchemaVersion(ctx, db)
		db.Close()

		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		e.log.Info("migrate is not implemented yet",
			"database", path,
			"schema_version", version,
			"fresh", *fresh,
		)
	}

	return ExitOK
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
		name := strings.TrimSuffix(filepath.Base(path), ".db")
		dest := filepath.Join(*out, fmt.Sprintf("%s-%s.db", name, stamp))

		started := time.Now()
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

// discoverDatabases lists control.db and every account database under the data
// directory. Account databases live in their own subdirectory so that adding a
// backup file or a downloaded GeoIP database beside them can never be mistaken
// for an account.
func discoverDatabases(dataDir string) ([]string, error) {
	var found []string

	control := filepath.Join(dataDir, config.ControlDatabaseName)
	if _, err := os.Stat(control); err == nil {
		found = append(found, control)
	}

	accountDir := filepath.Join(dataDir, config.AccountDatabaseDir)

	entries, err := os.ReadDir(accountDir)
	if os.IsNotExist(err) {
		return found, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", accountDir, err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}

		found = append(found, filepath.Join(accountDir, entry.Name()))
	}

	// A stable order matters: a partially failed migration should stop at the
	// same place on a retry, so the run is resumable by re-running it.
	sort.Strings(found)

	return found, nil
}
