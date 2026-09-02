//
// litestream.go
// The replication configuration for system.db and every account database on a box.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package litestream generates the configuration that replicates every SQLite
// database in one storage group to object storage.
//
// It is generated rather than written by hand because the set of databases is
// not known when anything starts. An account database is created by that
// account's first event, and Litestream reads its configuration once, at
// start-up. A file somebody wrote by hand is correct until the next customer
// signs up and then silently replicates everything except the newest account —
// which is the one most likely to be lost and the one nobody would think to
// check.
//
// Nothing here runs Litestream. It writes a file and reports whether the file
// changed, so the process that owns the daemon — systemd, a supervisor, a
// person — decides when to restart it. Owning a child process would mean this
// binary had to stay alive for replication to continue, and replication that
// stops when the app is stopped is not replication.
//
// Credentials are deliberately absent from the generated file. Litestream reads
// LITESTREAM_ACCESS_KEY_ID and LITESTREAM_SECRET_ACCESS_KEY from its own
// environment, so the bucket credentials stay in the secrets mechanism rather
// than being rewritten into a world-readable file every time an account signs
// up.
package litestream

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
)

// Defaults for replication intervals.
//
// A one-second sync is the recovery point this project promises: at most a
// second of committed database state is unreplicated, and the events written in
// that second are still in the ingest outbox because a row there is deleted
// only after the shard acknowledges the commit.
const (
	DefaultSyncInterval     = time.Second
	DefaultSnapshotInterval = 6 * time.Hour
)

// SystemName is the system database's name inside the replica URL.
const SystemName = "system"

// Options are everything the generated configuration depends on.
type Options struct {
	// DataDir is the install being replicated. Every path in the generated file
	// is resolved from it and written absolute, because Litestream runs as its
	// own service with its own working directory and a relative path in the
	// config would silently point at the wrong file — or at nothing.
	DataDir string

	// ReplicaURL is the prefix every database is replicated under, such as
	// s3://feasible-backups/production-primary. Each database gets one path
	// segment below it.
	ReplicaURL string

	SyncInterval     time.Duration
	SnapshotInterval time.Duration
}

// withDefaults fills the intervals a caller left at zero, so that a partly
// configured install still produces a usable file rather than a config with
// "0s" in it that Litestream reads as "as fast as possible".
func (o Options) withDefaults() Options {
	if o.SyncInterval <= 0 {
		o.SyncInterval = DefaultSyncInterval
	}
	if o.SnapshotInterval <= 0 {
		o.SnapshotInterval = DefaultSnapshotInterval
	}

	o.ReplicaURL = strings.TrimRight(strings.TrimSpace(o.ReplicaURL), "/")

	return o
}

// Validate rejects the configurations that produce a replica nobody can restore
// from. Each of these fails at restore time rather than at write time, which is
// months later and in the middle of an incident.
func (o Options) Validate() error {
	o = o.withDefaults()

	if o.ReplicaURL == "" {
		return fmt.Errorf("no replica URL is configured — set FEASIBLE_LITESTREAM_REPLICA_URL")
	}

	if !strings.Contains(o.ReplicaURL, "://") {
		return fmt.Errorf("replica URL %q has no scheme — it should look like s3://bucket/prefix", o.ReplicaURL)
	}

	return nil
}

// Database is one file to replicate and where it goes.
type Database struct {
	// Path is absolute, for the reason Options.DataDir gives.
	Path string

	// Name is the segment under the replica prefix. It is derived from the
	// account's directory rather than formatted again here, so the name in the
	// bucket cannot drift from the name on disk.
	Name string

	// ReplicaURL is the full destination, prefix and name joined.
	ReplicaURL string
}

// Plan lists every database that must be replicated right now: the system
// database, then one per account directory that exists on disk.
//
// The account list comes from the disk rather than from system.db on purpose.
// This is the same choice the migrate and backup commands make, and for the
// same reason: a box whose system database is unreadable is exactly when
// somebody is trying to work out what is on it.
func Plan(opts Options) ([]Database, error) {
	opts = opts.withDefaults()

	if err := opts.Validate(); err != nil {
		return nil, err
	}

	dataDir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", opts.DataDir, err)
	}
	systemPath := filepath.Join(dataDir, config.SystemDatabaseName)
	legacyPath := filepath.Join(dataDir, config.LegacyDatabaseName)
	if _, systemErr := os.Stat(systemPath); os.IsNotExist(systemErr) {
		if _, legacyErr := os.Stat(legacyPath); legacyErr == nil {
			return nil, fmt.Errorf("legacy database %s must be renamed before replication — run `feasible db migrate`", legacyPath)
		}
	}

	// The system database is listed whether or not it exists yet. Covering a
	// storage group from the moment it is built protects its first hour, and
	// Litestream treats a missing file as not yet created rather than an error.
	plan := []Database{{
		Path:       systemPath,
		Name:       SystemName,
		ReplicaURL: opts.ReplicaURL + "/" + SystemName,
	}}

	ids, err := accounts.Discover(dataDir)
	if err != nil {
		return nil, err
	}

	for _, id := range ids {
		path := accounts.Path(dataDir, id)
		name := accountName(path)

		plan = append(plan, Database{
			Path:       path,
			Name:       name,
			ReplicaURL: opts.ReplicaURL + "/" + name,
		})
	}

	return plan, nil
}

// accountName turns an account database path into its name in the bucket. The
// account id is taken from the directory rather than formatted from the number,
// so the zero padding in the bucket always matches the zero padding on disk.
func accountName(path string) string {
	return "account-" + filepath.Base(filepath.Dir(path))
}

// Render produces the Litestream configuration for a plan.
//
// It is assembled as text rather than marshalled through a YAML library because
// the output is a fixed six-line shape per database, and a dependency that can
// reorder keys would make "did this file change" — the question the watcher
// asks every minute — depend on map iteration order.
func Render(plan []Database, opts Options) []byte {
	opts = opts.withDefaults()

	var b strings.Builder

	b.WriteString("# Generated by `feasible litestream config`. Hand edits are overwritten.\n")
	b.WriteString("#\n")
	b.WriteString("# One entry per database: system.db, then one per account. A new account\n")
	b.WriteString("# database is not replicated until this file is regenerated and Litestream is\n")
	b.WriteString("# restarted, which is what `feasible litestream config -watch` does.\n")
	b.WriteString("#\n")
	b.WriteString("# Bucket credentials are not in this file. Litestream reads\n")
	b.WriteString("# LITESTREAM_ACCESS_KEY_ID and LITESTREAM_SECRET_ACCESS_KEY from its own\n")
	b.WriteString("# environment, so a file that is rewritten whenever somebody signs up never\n")
	b.WriteString("# carries a secret.\n")
	b.WriteString("# Provider lifecycle is the authoritative remote-retention control. Litestream\n")
	b.WriteString("# v0.5.8+ must not issue DeleteObject with the least-privilege replicator role.\n")
	b.WriteString("retention:\n")
	b.WriteString("  enabled: false\n")
	b.WriteString("dbs:\n")

	for _, db := range plan {
		fmt.Fprintf(&b, "  - path: %s\n", db.Path)
		b.WriteString("    replicas:\n")
		fmt.Fprintf(&b, "      - url: %s\n", db.ReplicaURL)
		fmt.Fprintf(&b, "        sync-interval: %s\n", duration(opts.SyncInterval))
		fmt.Fprintf(&b, "        snapshot-interval: %s\n", duration(opts.SnapshotInterval))
	}

	return []byte(b.String())
}

// duration renders an interval the way Litestream's parser wants it. Go's own
// formatting produces "1h0m0s" and "6h0m0s", which parse correctly but make a
// generated file that nobody wants to read during an incident.
func duration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
}

// Result is what one synchronisation did.
type Result struct {
	// Path is the configuration file that was written or left alone.
	Path string

	// Databases is how many entries the file now has, system included.
	Databases int

	// Changed reports whether the file on disk is different from what it was.
	// It is the whole reason this returns a struct: restarting Litestream on
	// every tick would interrupt replication once a minute for no reason, and
	// never restarting it would leave new accounts unreplicated forever.
	Changed bool
}

// Sync regenerates the configuration and writes it if it differs.
//
// Writing only on a difference is what makes the caller's restart decision
// correct, and comparing the rendered bytes rather than the database list is
// what makes a changed interval count as a change too.
func Sync(configPath string, opts Options) (Result, error) {
	plan, err := Plan(opts)
	if err != nil {
		return Result{}, err
	}

	body := Render(plan, opts)

	changed, err := write(configPath, body)
	if err != nil {
		return Result{}, err
	}

	return Result{Path: configPath, Databases: len(plan), Changed: changed}, nil
}

// write replaces the file only when its contents differ, atomically.
//
// The temporary-file-and-rename is not ceremony: Litestream may be started by
// the same restart this write triggers, and a daemon that read a half-written
// file would come up replicating half the databases with no error anywhere.
func write(path string, body []byte) (bool, error) {
	existing, err := os.ReadFile(path)
	if err == nil && string(existing) == string(body) {
		return false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	temp := path + ".tmp"
	if err := os.WriteFile(temp, body, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", temp, err)
	}

	if err := os.Rename(temp, path); err != nil {
		return false, fmt.Errorf("replace %s: %w", path, err)
	}

	return true, nil
}

// Missing reports which databases on disk are absent from a configuration file.
//
// This is the alarm, not a convenience. An account whose database is not in the
// running configuration is an account with no replication and no error message
// anywhere — the failure this whole package exists to make impossible — so it
// is worth a command that can be run from monitoring and exits non-zero.
func Missing(configPath string, plan []Database) ([]string, error) {
	covered, err := paths(configPath)
	if err != nil {
		return nil, err
	}

	var missing []string

	for _, db := range plan {
		if !covered[db.Path] {
			missing = append(missing, db.Path)
		}
	}

	sort.Strings(missing)

	return missing, nil
}

// paths reads the database paths out of a configuration file.
//
// It scans for the one line shape this package emits rather than parsing YAML.
// The file being read is the file this package wrote, so a parser would buy
// nothing but a dependency — and a hand-edited file is meant to be reported as
// wrong, which a strict scan does and a tolerant parser does not.
func paths(configPath string) (map[string]bool, error) {
	file, err := os.Open(configPath)
	if os.IsNotExist(err) {
		// A missing file is not an error to report differently from an empty
		// one: in both cases nothing is being replicated, and the caller's
		// answer is the same list of everything.
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", configPath, err)
	}
	defer file.Close() //nolint:errcheck // a read-only handle

	found := map[string]bool{}
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		value, ok := strings.CutPrefix(line, "- path:")
		if !ok {
			continue
		}

		if value = strings.TrimSpace(value); value != "" {
			found[value] = true
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", configPath, err)
	}

	return found, nil
}
