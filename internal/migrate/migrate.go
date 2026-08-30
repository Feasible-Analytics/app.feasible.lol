//
// migrate.go
// The migration runner: embedded SQL, applied in order, once, per database.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package migrate owns the schema. Every database in the system carries its own
// version, because there is one control database and one more per account, and
// a run that stops halfway through a thousand accounts has to be resumable by
// re-running it. Migrations are embedded in the binary so that `feasible db
// migrate` on a box with nothing but the binary on it does the right thing.
//
// Migrations never run on boot. Two processes racing them is a classic
// self-hosting failure, and with one database per account the operation is slow
// enough that it has to be deliberate and observable.
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// files holds the SQL. Embedding rather than reading from disk is what lets a
// release be one binary with nothing to copy alongside it — a directory of
// migrations that has to ship next to the binary is a directory that will be
// missing on someone's server.
//
//go:embed sql
var files embed.FS

// Directories inside the embedded tree. The two sets are versioned
// independently: an account database and the control database have unrelated
// schemas and there is no reason a change to one should renumber the other.
const (
	controlDir = "sql/control"
	accountDir = "sql/account"
)

// Migration is one numbered SQL file. The whole file runs as a single
// statement batch inside one transaction, so a migration is either entirely
// applied or entirely absent — there is no half-migrated database to reason
// about at three in the morning.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Set is every migration for one kind of database, in ascending order.
type Set struct {
	Name       string
	Migrations []Migration
}

// Version reports the schema version a database is brought to by this set. It
// is the highest file number rather than a count, so a migration can be
// reserved or skipped without silently shifting every version after it.
func (s Set) Version() int {
	if len(s.Migrations) == 0 {
		return 0
	}

	return s.Migrations[len(s.Migrations)-1].Version
}

// The two sets are parsed once at start-up. A malformed filename here is a
// programmer error caught by the first test run rather than something an
// operator can cause, so panicking is honest: the binary is broken and should
// not pretend it can migrate anything.
var (
	controlSet = mustLoad("control", controlDir)
	accountSet = mustLoad("account", accountDir)
)

// Control returns the migrations for control.db.
func Control() Set {
	return controlSet
}

// Account returns the migrations for one account's analytics.db.
func Account() Set {
	return accountSet
}

// mustLoad parses one embedded directory or dies trying. It exists so the sets
// are package variables rather than something every caller has to build and
// error-check, which would put a "cannot happen" branch at every call site.
func mustLoad(name, dir string) Set {
	set, err := load(name, dir)
	if err != nil {
		panic(fmt.Sprintf("migrate: %v", err))
	}

	return set
}

// load reads and sorts one directory of migrations. Filenames carry the version
// (`0001_initial.sql`) because a version that lives in the filename is visible
// in a directory listing, in a diff and in a code review, where a version
// hidden inside the file is not.
func load(name, dir string) (Set, error) {
	entries, err := files.ReadDir(dir)
	if err != nil {
		return Set{}, fmt.Errorf("read %s: %w", dir, err)
	}

	set := Set{Name: name}
	seen := map[int]string{}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		version, label, err := parseName(entry.Name())
		if err != nil {
			return Set{}, err
		}

		// Two files claiming one version would apply in an order that depended
		// on the filesystem, which is the kind of bug that only shows up on
		// somebody else's machine.
		if other, ok := seen[version]; ok {
			return Set{}, fmt.Errorf("%s: migrations %s and %s share version %d", dir, other, entry.Name(), version)
		}
		seen[version] = entry.Name()

		body, err := files.ReadFile(path.Join(dir, entry.Name()))
		if err != nil {
			return Set{}, fmt.Errorf("read %s: %w", entry.Name(), err)
		}

		set.Migrations = append(set.Migrations, Migration{Version: version, Name: label, SQL: string(body)})
	}

	sort.Slice(set.Migrations, func(i, j int) bool {
		return set.Migrations[i].Version < set.Migrations[j].Version
	})

	return set, nil
}

// parseName splits `0001_initial.sql` into its version and its label. The
// version must be positive because zero is the version of a database that has
// had nothing applied to it, and a migration numbered zero could never run.
func parseName(filename string) (int, string, error) {
	trimmed := strings.TrimSuffix(filename, ".sql")

	digits, label, found := strings.Cut(trimmed, "_")
	if !found {
		return 0, "", fmt.Errorf("migration %q: expected <version>_<name>.sql", filename)
	}

	version, err := strconv.Atoi(digits)
	if err != nil {
		return 0, "", fmt.Errorf("migration %q: %q is not a version number", filename, digits)
	}

	if version < 1 {
		return 0, "", fmt.Errorf("migration %q: versions start at 1", filename)
	}

	return version, label, nil
}

// Result describes what one database run did. It is returned rather than
// logged from in here so the caller decides how a migration across a thousand
// account databases reads, and so tests can assert on it.
type Result struct {
	From    int
	To      int
	Applied []int
}

// Changed reports whether anything was actually written. Re-running migrate is
// expected to be a no-op, and a command that said "migrated" a thousand times
// when it did nothing would train people to ignore the output.
func (r Result) Changed() bool {
	return len(r.Applied) > 0
}

// Run applies everything a database is missing. It reads the database's own
// version, applies each later migration in a transaction that also stamps the
// new version, and stops at the first failure with the version left at the last
// migration that fully succeeded — which is what makes an interrupted run
// resumable by simply running it again.
func Run(ctx context.Context, db *sql.DB, set Set) (Result, error) {
	current, err := store.SchemaVersion(ctx, db)
	if err != nil {
		return Result{}, err
	}

	result := Result{From: current, To: current}

	// A database newer than the binary means someone has rolled the binary
	// back. Carrying on would run queries against columns this build does not
	// know about, so refuse and say so.
	if current > set.Version() {
		return result, fmt.Errorf("database is at schema version %d but this build only knows %s migrations up to %d", current, set.Name, set.Version())
	}

	for _, migration := range set.Migrations {
		if migration.Version <= current {
			continue
		}

		if err := apply(ctx, db, migration); err != nil {
			return result, err
		}

		result.To = migration.Version
		result.Applied = append(result.Applied, migration.Version)
	}

	return result, nil
}

// apply runs one migration and its version stamp in a single transaction.
// SQLite makes DDL transactional and keeps user_version in the file header, so
// both land together: the version can never claim a migration that did not
// finish, and a migration can never be applied twice.
func apply(ctx context.Context, db *sql.DB, migration Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %04d_%s: begin: %w", migration.Version, migration.Name, err)
	}
	defer tx.Rollback() //nolint:errcheck // a rollback after a successful commit is a no-op

	if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
		return fmt.Errorf("migration %04d_%s: %w", migration.Version, migration.Name, err)
	}

	// PRAGMA takes no bind parameters. The value is an int from a filename we
	// parsed ourselves, which is why formatting it in is safe here.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", migration.Version)); err != nil {
		return fmt.Errorf("migration %04d_%s: stamp version: %w", migration.Version, migration.Name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %04d_%s: commit: %w", migration.Version, migration.Name, err)
	}

	return nil
}

// Fresh empties a database back to the state of a file that has never been
// migrated. It exists for development, where the alternative is deleting files
// by hand and getting the path wrong; it destroys data and the command that
// calls it refuses to do so in production.
func Fresh(ctx context.Context, db *sql.DB) error {
	// The drops have to run on one pinned connection: foreign_keys is a
	// per-connection setting, and turning it off on whichever connection the
	// pool happened to hand out would leave the drops enforcing constraints on
	// another.
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("fresh: %w", err)
	}
	defer conn.Close()

	// Dropping a parent table before its children fails while foreign keys are
	// enforced, and there is no ordering that is correct for every schema we
	// might ever write.
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("fresh: %w", err)
	}

	objects, err := listObjects(ctx, conn)
	if err != nil {
		return err
	}

	for _, object := range objects {
		// Dropping a table takes its indexes and triggers with it, which is why
		// only tables and views are listed.
		if _, err := conn.ExecContext(ctx, "DROP "+object.kind+" IF EXISTS "+quoteIdentifier(object.name)); err != nil {
			return fmt.Errorf("fresh: drop %s %s: %w", object.kind, object.name, err)
		}
	}

	if _, err := conn.ExecContext(ctx, "PRAGMA user_version = 0"); err != nil {
		return fmt.Errorf("fresh: reset version: %w", err)
	}

	// The connection goes back to the pool, so it has to go back the way every
	// other connection in this process is configured.
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("fresh: %w", err)
	}

	return nil
}

// quoteIdentifier wraps a table or view name for SQL. An identifier cannot be a
// bind parameter, and SQL escapes an embedded quote by doubling it, which is
// not what Go's %q would produce.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// object is one droppable schema entry.
type object struct {
	kind string
	name string
}

// listObjects reads the tables and views a database holds. It reads them into
// a slice first rather than dropping as it iterates, because dropping while a
// cursor is open on sqlite_master is exactly the kind of thing that works until
// the day it does not.
func listObjects(ctx context.Context, conn *sql.Conn) ([]object, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT type, name
		FROM sqlite_master
		WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite_%'
	`)
	if err != nil {
		return nil, fmt.Errorf("fresh: read schema: %w", err)
	}
	defer rows.Close()

	var objects []object

	for rows.Next() {
		var found object
		if err := rows.Scan(&found.kind, &found.name); err != nil {
			return nil, fmt.Errorf("fresh: read schema: %w", err)
		}

		objects = append(objects, found)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fresh: read schema: %w", err)
	}

	return objects, nil
}
