//
// server.go
// Dispatch: one method name in, one JSON-RPC response out.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/publicapi"
)

// Tool is one callable this server offers.
type Tool struct {
	Name string

	// Title is the human label a client shows in a consent prompt. It is
	// separate from the name because the name is an identifier a model types
	// and the title is a phrase a person reads.
	Title string

	Description string

	// InputSchema is JSON Schema. It is what makes the difference between a
	// model calling a tool correctly first time and a model guessing at
	// argument names, so every property here carries its own description.
	InputSchema map[string]any

	// ReadOnly marks a tool that changes nothing. Clients use it to decide
	// whether to ask a person before running it, and getting it wrong means
	// either an unwanted prompt on every read or a silent write.
	ReadOnly bool

	Handler func(ctx context.Context, key *apikeys.Key, args json.RawMessage) (*toolResult, error)
}

// Server answers MCP calls over whichever transport carried them.
//
// It holds no per-connection state beyond what the transport gives it, which is
// what lets the same instance serve the HTTP endpoint and a stdio session at
// once: a tool call is a pure function of its arguments and the credential the
// transport authenticated.
type Server struct {
	// API is the public API this server is a second front end onto. Every tool
	// goes through it rather than reaching for a database, so an assistant and
	// a dashboard cannot end up with two different answers.
	API *publicapi.API

	Log *logger.Logger

	tools map[string]*Tool
	order []string
}

// New builds a server over the public API, with every tool registered.
func New(api *publicapi.API, log *logger.Logger) *Server {
	server := &Server{API: api, Log: log, tools: map[string]*Tool{}}
	server.register(server.toolset()...)

	return server
}

// register adds tools, keeping the declaration order so tools/list is stable.
// A list that reorders itself between calls invalidates whatever the client
// cached and makes a diff of two sessions unreadable.
func (s *Server) register(tools ...*Tool) {
	for _, tool := range tools {
		s.tools[tool.Name] = tool
		s.order = append(s.order, tool.Name)
	}
}

// Handle answers one request. It returns nil for a notification, because a
// notification that gets a response is a response the client cannot match to
// anything it sent.
func (s *Server) Handle(ctx context.Context, key *apikeys.Key, raw []byte) *rpcResponse {
	var request rpcRequest

	if err := json.Unmarshal(raw, &request); err != nil {
		return failure(nil, codeParseError, "the request is not valid JSON-RPC")
	}

	if request.JSONRPC != "2.0" {
		return failure(request.ID, codeInvalidRequest, "jsonrpc must be \"2.0\"")
	}

	response := s.dispatch(ctx, key, &request)

	if request.isNotification() {
		return nil
	}

	return response
}

// dispatch routes one method.
func (s *Server) dispatch(ctx context.Context, key *apikeys.Key, request *rpcRequest) *rpcResponse {
	switch request.Method {
	case "initialize":
		return result(request.ID, initializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities: capabilities{
				Tools:     &toolsCapability{},
				Resources: &resourcesCapability{},
				Prompts:   &promptsCapability{},
			},
			ServerInfo:   serverInfo{Name: ServerName, Version: ServerVersion, Title: "Feasible Analytics"},
			Instructions: instructions,
		})

	case "notifications/initialized", "notifications/cancelled":
		// Acknowledged and ignored. They are notifications, so the answer is
		// discarded anyway; handling them explicitly keeps them out of the
		// "method not found" branch and out of the logs.
		return result(request.ID, map[string]any{})

	case "ping":
		return result(request.ID, map[string]any{})

	case "tools/list":
		return result(request.ID, map[string]any{"tools": s.describeTools()})

	case "tools/call":
		return s.callTool(ctx, key, request)

	case "resources/list":
		return s.listResources(ctx, key, request)

	case "resources/templates/list":
		return result(request.ID, map[string]any{"resourceTemplates": resourceTemplates()})

	case "resources/read":
		return s.readResource(ctx, key, request)

	case "prompts/list":
		return result(request.ID, map[string]any{"prompts": promptDefinitions()})

	case "prompts/get":
		return s.getPrompt(request)

	default:
		return failure(request.ID, codeMethodNotFound, "no method named %q", request.Method)
	}
}

// describeTools renders the tool list.
func (s *Server) describeTools() []map[string]any {
	described := make([]map[string]any, 0, len(s.order))

	for _, name := range s.order {
		tool := s.tools[name]

		described = append(described, map[string]any{
			"name":        tool.Name,
			"title":       tool.Title,
			"description": tool.Description,
			"inputSchema": tool.InputSchema,
			"annotations": map[string]any{
				"readOnlyHint": tool.ReadOnly,
				"title":        tool.Title,
			},
		})
	}

	return described
}

// callParams is the body of tools/call.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// callTool runs one tool.
//
// A tool that fails because of what the caller asked for comes back as a
// successful response with isError set, so the model reads the reason and tries
// again. Only a missing tool or a missing credential is a protocol error: those
// are things the model cannot fix by rephrasing.
func (s *Server) callTool(ctx context.Context, key *apikeys.Key, request *rpcRequest) *rpcResponse {
	var params callParams

	if err := json.Unmarshal(request.Params, &params); err != nil {
		return failure(request.ID, codeInvalidParams, "tools/call needs a name and an arguments object")
	}

	tool, ok := s.tools[params.Name]
	if !ok {
		return failure(request.ID, codeMethodNotFound, "no tool named %q — call tools/list to see what exists", params.Name)
	}

	if key == nil {
		return failure(request.ID, codeUnauthorized, "this connection is not authenticated")
	}

	answer, err := tool.Handler(ctx, key, params.Arguments)
	if err != nil {
		if s.Log != nil {
			s.Log.Error("mcp tool failed", "tool", params.Name, "error", err)
		}

		// Whatever went wrong, the model gets a sentence rather than silence.
		// A tool call that vanishes is a conversation that stalls, and the
		// person watching cannot tell a permission problem from an outage.
		return result(request.ID, toolFailure("%s could not run: %s", params.Name, readableError(err)))
	}

	return result(request.ID, answer)
}

// readableError turns an internal failure into something safe and useful to put
// in front of a model. A SQLite error tells the model nothing it can act on and
// tells whoever is watching more about our internals than they should see.
func readableError(err error) string {
	switch {
	case errors.Is(err, publicapi.ErrForbidden):
		return "that site is not available to this credential"
	case errors.Is(err, publicapi.ErrNotFound):
		return "that does not exist"
	case errors.Is(err, publicapi.ErrConflict):
		return err.Error()
	}

	return "the request could not be completed"
}

// decodeArgs reads a tool's arguments into a typed struct, refusing anything it
// does not recognise.
//
// Refusing unknown fields matters more here than anywhere else in the product: a
// model that invents a plausible argument name gets told immediately, where
// silently ignoring it would give a confidently wrong answer built from a filter
// that never applied.
func decodeArgs(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}

	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%s", readableArgError(err))
	}

	return nil
}

// readableArgError phrases a decode failure for a model.
func readableArgError(err error) string {
	message := err.Error()

	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		return fmt.Sprintf("the argument %q should be a %s", mismatch.Field, mismatch.Type)
	}

	if strings.Contains(message, "unknown field") {
		return message + " — call tools/list and use the argument names in the schema"
	}

	return message
}

// sortedKeys returns a map's keys in order, so that anything rendered from a map
// reads the same way twice. An assistant comparing two runs of the same tool
// should not see a diff that is only key order.
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return keys
}
