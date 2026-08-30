//
// ingest.go
// The `ingest` subcommand: accept events, buffer them, forward to a shard.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

const ingestHelp = `feasible ingest — run the ingest tier only.

Accepts events, derives them, writes them to a local outbox, answers 202, then
forwards to the shard that owns the account. Exists so the production front door
scales horizontally without forking the payload-parsing code.

Flags:
`

// runIngest starts the ingest-only process. The forwarding loop arrives with the
// store-and-forward milestone; this resolves and reports the shard list now
// because an ingestor with the wrong shards silently drops every event it
// receives, and that is the failure we least want to discover in production.
func runIngest(e *env, args []string) int {
	fs := newFlagSet("ingest", e, ingestHelp)
	listen := fs.String("listen", e.cfg.Ingest.Listen, "listen address (host:port)")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	e.cfg.Ingest.Listen = *listen

	e.log.Info("ingest is not implemented yet",
		"listen", e.cfg.Ingest.Listen,
		"shards", e.cfg.Ingest.Shards,
		"buffer_path", e.cfg.Ingest.BufferPath,
		"internal_keys", len(e.cfg.Shared.InternalKeys),
		"env", e.cfg.Shared.Env,
		"trace_events", e.cfg.Shared.TraceEvents,
	)

	return ExitOK
}
