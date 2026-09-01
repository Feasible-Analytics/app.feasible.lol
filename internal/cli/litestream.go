//
// litestream.go
// Litestream configuration, coverage, and replica-lifecycle commands.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/litestream"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/replica"
)

const litestreamHelp = `feasible litestream — the replication configuration.

Litestream replicates each SQLite database continuously to object storage. The
set of databases is not fixed — an account database is created by that account's
first event — and Litestream reads its configuration once, so the file has to be
generated and the daemon restarted when a new account appears.

Commands:
  config   Write the configuration for every database on this box.
  check    Report databases on disk that the configuration does not cover.
  policy   Render the versioned S3-compatible replica lifecycle policy.
  lifecycle-check  Validate actual lifecycle, versioning and Object Lock exports.
`

const litestreamConfigHelp = `feasible litestream config — write the replication configuration.

Lists control.db and every account database, and writes a Litestream config for
them. The file is only replaced when it differs, so the restart hook fires when
an account is created rather than on every tick.

Bucket credentials are never written into it: Litestream reads
LITESTREAM_ACCESS_KEY_ID and LITESTREAM_SECRET_ACCESS_KEY from its own
environment.

Flags:
`

const litestreamCheckHelp = `feasible litestream check — is everything on this box replicated.

Exits non-zero and names the files when a database exists on disk that the
configuration does not cover. An unreplicated account is otherwise completely
silent, which is why this is a command monitoring can run rather than a note in
a document.

Flags:
`

const litestreamPolicyHelp = `feasible litestream policy — render the replica lifecycle policy.

Writes the versioned S3-compatible policy for the configured bucket and shard
prefix to stdout. Applying it is an explicit bucket-owner operation; this
command performs no cloud mutation.

Flags:
`

const litestreamLifecycleCheckHelp = `feasible litestream lifecycle-check — validate provider retention controls.

Reads one atomically published provider attestation. It fails unless the fresh
bundle is bound to the exact bucket and shard prefix, an enabled policy covers
that prefix, and no provider feature can retain historical versions beyond it.

Flags:
`

// runLitestream dispatches the two-word replication commands, so that
// `feasible litestream` on its own lists what it offers rather than printing
// the whole program's help.
func runLitestream(e *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, litestreamHelp)
		return ExitUsage
	}

	switch args[0] {
	case "config":
		return runLitestreamConfig(e, args[1:])
	case "check":
		return runLitestreamCheck(e, args[1:])
	case "policy":
		return runLitestreamPolicy(e, args[1:])
	case "lifecycle-check":
		return runLitestreamLifecycleCheck(e, args[1:])
	default:
		fmt.Fprintf(e.stderr, "unknown litestream command %q\n\n", args[0])
		fmt.Fprint(e.stderr, litestreamHelp)
		return ExitUsage
	}
}

// runLitestreamConfig writes the configuration once, or keeps it current.
//
// The watch mode exists because of a specific hole: an account database is
// created the moment that account sends its first event, and until the
// configuration names it, it is a live customer database with no replication
// and nothing anywhere reporting that. Watching closes the hole to one interval.
func runLitestreamConfig(e *env, args []string) int {
	fs := newFlagSet("litestream config", e, litestreamConfigHelp)

	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding control.db and the account databases")
	out := fs.String("out", e.cfg.Litestream.ConfigPath, "file to write the configuration to")
	replica := fs.String("replica-url", e.cfg.Litestream.ReplicaURL, "replica prefix, such as s3://bucket/shard-01")
	watch := fs.Bool("watch", false, "keep the file current as accounts are created")
	interval := fs.Duration("interval", e.cfg.Litestream.WatchInterval, "how often -watch re-reads the account directory")
	onChange := fs.String("on-change", e.cfg.Litestream.OnChange, "shell command to run after the file changes, such as 'systemctl restart litestream'")
	print := fs.Bool("print", false, "write to stdout instead of the file, and change nothing")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	opts := litestream.Options{
		DataDir:          *dataDir,
		ReplicaURL:       *replica,
		SyncInterval:     e.cfg.Litestream.SyncInterval,
		SnapshotInterval: e.cfg.Litestream.SnapshotInterval,
	}

	if err := opts.Validate(); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	if e.cfg.IsProduction() {
		if err := validateReplicaLifecycle(e, *replica); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}
	}

	// Printing resolves the same plan and writes nothing, so that "what would
	// this replicate" can be answered on a box whose real configuration file
	// belongs to root.
	if *print {
		plan, err := litestream.Plan(opts)
		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		if _, err := e.stdout.Write(litestream.Render(plan, opts)); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		return ExitOK
	}

	if !*watch {
		if _, err := syncLitestream(e, *out, opts, *onChange); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		return ExitOK
	}

	return watchLitestream(e, *out, opts, *onChange, *interval)
}

// watchLitestream regenerates the configuration on a ticker until a signal.
//
// A failed pass logs and carries on rather than exiting. The most likely cause
// is a data directory that is briefly unreadable, and a watcher that died on it
// would leave every account created afterwards silently unreplicated — the
// exact failure it is here to prevent.
func watchLitestream(e *env, out string, opts litestream.Options, onChange string, interval time.Duration) int {
	if interval <= 0 {
		interval = time.Minute
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	e.log.Info("watching for new account databases",
		"config", out,
		"data_dir", opts.DataDir,
		"interval", interval,
		"on_change", onChange != "",
	)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if _, err := syncLitestream(e, out, opts, onChange); err != nil {
			e.log.Error("replication configuration not updated", "error", err)
		}

		select {
		case <-ctx.Done():
			return ExitOK
		case <-ticker.C:
		}
	}
}

// syncLitestream writes the configuration and runs the change hook if it moved.
//
// The hook runs only on a change because restarting Litestream interrupts
// replication for every database on the box, and doing that once a minute would
// cost more than the new account it is there to pick up.
func syncLitestream(e *env, out string, opts litestream.Options, onChange string) (litestream.Result, error) {
	result, err := litestream.Sync(out, opts)
	if err != nil {
		return result, err
	}

	if !result.Changed {
		e.log.Debug("replication configuration unchanged",
			"config", result.Path,
			"databases", result.Databases,
		)

		return result, nil
	}

	e.log.Info("replication configuration written",
		"config", result.Path,
		"databases", result.Databases,
	)

	if onChange == "" {
		// Said out loud rather than assumed: a configuration that names a new
		// database and a daemon that has not re-read it look identical from
		// the outside, and the second one is not replicating that account.
		e.log.Warn("no on-change command is set — Litestream is still running the previous configuration and the new databases are not replicated yet",
			"config", result.Path,
		)

		return result, nil
	}

	return result, runChangeHook(e, onChange)
}

// runChangeHook runs the operator's restart command and reports what it said.
//
// Its output is logged whether it succeeded or not. A restart command that
// fails quietly is a box that believes it is replicating and is not, which is
// the worst state of the three.
func runChangeHook(e *env, command string) error {
	ctx, cancel := context.WithTimeout(context.Background(), changeHookTimeout)
	defer cancel()

	output, err := exec.CommandContext(ctx, "/bin/sh", "-c", command).CombinedOutput()

	if trimmed := string(output); trimmed != "" {
		e.log.Info("on-change command output", "command", command, "output", trimmed)
	}

	if err != nil {
		return fmt.Errorf("on-change command %q failed: %w", command, err)
	}

	e.log.Info("on-change command ran", "command", command)

	return nil
}

// changeHookTimeout bounds the restart command. A service manager that hangs
// would otherwise stop the watcher for good, and the next account after that
// would never be replicated.
const changeHookTimeout = 60 * time.Second

// runLitestreamCheck compares the configuration against the disk.
//
// It is the counterpart to the watcher: the watcher keeps the file current, and
// this says whether it actually is. Both exist because replication that has
// quietly stopped covering an account produces no error until the day somebody
// needs the data.
func runLitestreamCheck(e *env, args []string) int {
	fs := newFlagSet("litestream check", e, litestreamCheckHelp)

	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding control.db and the account databases")
	configPath := fs.String("config", e.cfg.Litestream.ConfigPath, "the configuration file to check")
	replica := fs.String("replica-url", e.cfg.Litestream.ReplicaURL, "replica prefix, such as s3://bucket/shard-01")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	opts := litestream.Options{
		DataDir:          *dataDir,
		ReplicaURL:       *replica,
		SyncInterval:     e.cfg.Litestream.SyncInterval,
		SnapshotInterval: e.cfg.Litestream.SnapshotInterval,
	}
	if e.cfg.IsProduction() {
		if err := validateReplicaLifecycle(e, *replica); err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}
	}

	plan, err := litestream.Plan(opts)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	missing, err := litestream.Missing(*configPath, plan)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	if len(missing) == 0 {
		fmt.Fprintf(e.stdout, "ok — all %d databases are in %s\n", len(plan), *configPath)
		return ExitOK
	}

	// Every missing file is named rather than counted. The operator's next
	// action is to work out which customers are unprotected, and a number does
	// not answer that.
	fmt.Fprintf(e.stderr, "%d of %d databases are not replicated by %s:\n", len(missing), len(plan), *configPath)
	for _, path := range missing {
		fmt.Fprintf(e.stderr, "  %s\n", path)
	}

	return ExitError
}

// runLitestreamPolicy renders the canonical policy without applying it. Cloud
// mutation stays with the bucket owner so a local command cannot silently
// replace unrelated lifecycle rules in a shared provider configuration.
func runLitestreamPolicy(e *env, args []string) int {
	fs := newFlagSet("litestream policy", e, litestreamPolicyHelp)
	replicaURL := fs.String("replica-url", e.cfg.Litestream.ReplicaURL, "replica prefix, such as s3://bucket/shard-01")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	body, err := replica.Render(*replicaURL)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	if _, err := e.stdout.Write(body); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	return ExitOK
}

// runLitestreamLifecycleCheck validates fresh provider exports. It is suitable
// for deployment gates and monitoring and deliberately performs no provider
// mutation.
func runLitestreamLifecycleCheck(e *env, args []string) int {
	fs := newFlagSet("litestream lifecycle-check", e, litestreamLifecycleCheckHelp)
	replicaURL := fs.String("replica-url", e.cfg.Litestream.ReplicaURL, "replica prefix, such as s3://bucket/shard-01")
	attestationPath := fs.String("attestation", e.cfg.Litestream.AttestationPath, "atomic provider attestation JSON")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	if err := checkReplicaLifecycle(*replicaURL, *attestationPath); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	fmt.Fprintf(e.stdout, "ok — %s is covered by %s within the replica eligibility bound\n", *replicaURL, replica.PolicyID)
	return ExitOK
}

// validateReplicaLifecycle enforces the production configuration contract. A
// production watcher or coverage check without fresh atomic provider evidence
// fails closed rather than treating an unchecked bucket as compliant.
func validateReplicaLifecycle(e *env, replicaURL string) error {
	return checkReplicaLifecycle(replicaURL, e.cfg.Litestream.AttestationPath)
}

// checkReplicaLifecycle reads one file once and validates its complete evidence
// at one time, preventing stale provider responses from different fetches from
// being mixed together.
func checkReplicaLifecycle(replicaURL, attestationPath string) error {
	if attestationPath == "" {
		return fmt.Errorf("replica lifecycle: FEASIBLE_LITESTREAM_ATTESTATION is required")
	}
	body, err := os.ReadFile(attestationPath)
	if err != nil {
		return fmt.Errorf("replica lifecycle: read %s: %w", attestationPath, err)
	}
	return replica.ValidateAttestation(replicaURL, body, time.Now().UTC())
}
