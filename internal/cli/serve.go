//
// serve.go
// The `serve` subcommand: the whole product in one process.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

const serveHelp = `feasible serve — run the whole product in one process.

This is the default mode and the only thing a self-hoster ever runs: the
dashboard, the API, the tracker and — with the direct transport — ingestion too.

Flags:
`

// runServe wires up and starts the app process. The HTTP server itself lands in
// a later milestone; what is real today is the configuration this resolves,
// because every other piece of the system is built against these values and
// getting them wrong is what makes cookies fail to set and OAuth redirects
// bounce.
func runServe(e *env, args []string) int {
	fs := newFlagSet("serve", e, serveHelp)
	listen := fs.String("listen", e.cfg.App.Listen, "public listen address (host:port)")
	internalListen := fs.String("internal-listen", e.cfg.App.InternalListen, "private listen address for /internal/*")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	e.cfg.App.Listen = *listen
	e.cfg.App.InternalListen = *internalListen

	e.log.Info("serve is not implemented yet",
		"listen", e.cfg.App.Listen,
		"internal_listen", e.cfg.App.InternalListen,
		"base_url", e.cfg.App.BaseURL,
		"data_dir", e.cfg.App.DataDir,
		"transport", e.cfg.App.Transport,
		"mail_transport", e.cfg.App.MailTransport,
		"env", e.cfg.Shared.Env,
		"trace_events", e.cfg.Shared.TraceEvents,
	)

	return ExitOK
}
