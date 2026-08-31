//
// stdio.go
// The local transport: one JSON-RPC message per line, over a pipe.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
)

// The stdio transport is how somebody points a desktop assistant at their own
// instance: the assistant launches the binary as a subprocess and talks to it
// over a pipe. There is no network, no OAuth and no callback URL, so the
// credential is an API key handed to the process — which is also why this
// transport authenticates once, at start-up, rather than per message: the pipe
// has exactly one peer and it is the process that was given the key.

// maxLineBytes bounds one message. A JSON-RPC line is small; a megabyte is far
// past anything legitimate and stops a malformed stream from being read into
// memory without limit.
const maxLineBytes = 1 << 20

// StdioOptions are the inputs to a stdio session.
type StdioOptions struct {
	In  io.Reader
	Out io.Writer

	// Key is the credential every call on this pipe runs as.
	Key *apikeys.Key
}

// ServeStdio runs a session until the input closes or the context is cancelled.
//
// Messages are newline-delimited JSON, which is the framing the transport uses.
// Nothing but protocol messages may ever be written to the output — a stray
// log line on stdout is indistinguishable from a malformed message and takes
// the whole session down, which is why every log in this path goes to stderr.
func ServeStdio(ctx context.Context, server *Server, opts StdioOptions) error {
	if opts.Key == nil {
		return fmt.Errorf("mcp: stdio needs an API key to run as")
	}

	reader := bufio.NewReaderSize(opts.In, 64*1024)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	encoder := json.NewEncoder(opts.Out)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		response := server.Handle(ctx, opts.Key, line)
		if response == nil {
			continue
		}

		if err := encoder.Encode(response); err != nil {
			return fmt.Errorf("mcp: stdio write: %w", err)
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("mcp: stdio read: %w", err)
	}

	return nil
}
