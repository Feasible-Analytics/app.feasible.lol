//
// protocol.go
// The JSON-RPC envelope and the Model Context Protocol messages inside it.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package mcp is a working Model Context Protocol server over the stats and
// sites APIs.
//
// It is written against the protocol rather than against a library. The
// protocol is JSON-RPC 2.0 with about a dozen methods, the whole of it fits in
// this file, and the alternative is a dependency on somebody else's release
// cadence for a binary whose entire pitch is that it has no dependencies to
// install. This package's rule is the same as the rest of the product's: one
// self-contained binary, no runtime the customer has to bring.
//
// The differentiator is that it works. The incumbent's MCP routes are
// scaffolded in their router and answer 501; every tool here either does the
// thing or says precisely why it cannot.
package mcp

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the revision of the specification this server speaks. It
// is sent back on initialize so that a client which speaks a different one can
// decide what to do rather than discovering the mismatch three calls later.
const ProtocolVersion = "2025-06-18"

// ServerName and ServerVersion identify us in the initialize handshake, which
// is what an assistant shows the person connecting.
const (
	ServerName    = "feasible"
	ServerVersion = "1"
)

// JSON-RPC error codes. The first five are the specification's own; the last is
// ours, and it exists so that "you are not allowed to see that" is
// distinguishable from "something broke".
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeUnauthorized   = -32001

	// codePaymentRequired is the account lock. It is its own code rather than
	// an unauthorized so a client can tell "your token is wrong" from "your
	// account has not paid" — the first is fixed by reconnecting and the second
	// never is.
	codePaymentRequired = -32002

	// codeRateLimited is the hourly ceiling, distinct from both of the above
	// because the fix is to wait, not to reconnect or to pay.
	codeRateLimited = -32003
)

// rpcRequest is one incoming call. The id is a raw message because JSON-RPC
// allows a string or a number and echoing back the other type is a protocol
// violation some clients treat as a hang.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the caller expects no answer. A notification
// carries no id, and answering one is how a client's dispatcher ends up with a
// response it cannot match to anything.
func (r *rpcRequest) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// rpcResponse is one answer.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a protocol-level failure: a method that does not exist, params
// that could not be parsed, a credential that is not valid.
//
// A tool that runs and fails is *not* one of these. That comes back as a
// successful result carrying isError, because the model needs to read the
// failure and try something else — a protocol error is invisible to it.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// result builds a successful response.
func result(id json.RawMessage, payload any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: payload}
}

// failure builds an error response.
func failure(id json.RawMessage, code int, format string, args ...any) *rpcResponse {
	return &rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: fmt.Sprintf(format, args...)},
	}
}

// initializeResult is the handshake answer. Capabilities are advertised as
// empty objects rather than omitted: an absent capability means "not supported"
// and a client that reads it that way will never call tools/list.
type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	ServerInfo      serverInfo   `json:"serverInfo"`
	Instructions    string       `json:"instructions,omitempty"`
}

// capabilities says what this server offers.
type capabilities struct {
	Tools     *toolsCapability     `json:"tools,omitempty"`
	Resources *resourcesCapability `json:"resources,omitempty"`
	Prompts   *promptsCapability   `json:"prompts,omitempty"`
}

// toolsCapability advertises the tool surface. listChanged is false because our
// tool list is fixed at build time; claiming otherwise would have clients
// subscribing to notifications that never arrive.
type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// resourcesCapability advertises the resource surface. Subscribing to a
// resource is not offered: a site's schema changes when somebody adds a goal or
// a custom property, which is rare enough that re-reading beats a subscription
// nobody would notice was broken.
type resourcesCapability struct {
	Subscribe   bool `json:"subscribe"`
	ListChanged bool `json:"listChanged"`
}

// promptsCapability advertises the prompt surface.
type promptsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// serverInfo names the implementation.
type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

// instructions is the note an assistant reads before it uses anything here. It
// is short on purpose: it says what the server is for and points at the one
// thing a model would otherwise get wrong, which is guessing dimension names
// instead of reading the schema resource.
const instructions = `Web analytics for the sites this credential can see.

Start with list_sites, then read the feasible://site/<domain>/schema resource for
that site: it lists exactly which metrics, dimensions, goals and custom
properties exist, so you do not have to guess a name and get an error.

query_stats is the whole query surface. Use explain_traffic_change rather than a
pile of query_stats calls when the question is "why did this move" — it runs the
comparisons an analyst would and ranks what actually accounts for the change.`

// content is one piece of a tool result or a prompt message.
type content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// text builds a text content block.
func text(value string) content {
	return content{Type: "text", Text: value}
}

// toolResult is what tools/call answers with.
//
// Both a readable text block and a structured payload are returned. The text is
// what a model reads when it has no schema handling; the structured content is
// what one that does can compute over. Sending only the text would make every
// number a parsing problem, and sending only the structure would make a small
// model useless against it.
type toolResult struct {
	Content           []content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// toolFailure is a tool that ran and could not do the job. It is a successful
// JSON-RPC result carrying isError, which is what lets the model read the
// reason and correct itself instead of the call simply disappearing.
func toolFailure(format string, args ...any) *toolResult {
	return &toolResult{Content: []content{text(fmt.Sprintf(format, args...))}, IsError: true}
}
