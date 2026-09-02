//
// http.go
// Streamable HTTP at /mcp, for remote clients.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
)

// Path is where the endpoint is mounted.
const Path = "/mcp"

// MaxBodyBytes bounds one request. A JSON-RPC call is a few hundred bytes;
// anything near this is either a mistake or an attempt to make the decoder the
// most expensive thing on the box.
const MaxBodyBytes = 1 << 20

// sessionHeader carries the session id a client is told to echo back. Sessions
// hold nothing here — every call is authenticated on its own and every tool is a
// function of its arguments — but the header is part of the transport, and a
// client that is not given one has no way to tell two servers apart behind a
// load balancer.
const sessionHeader = "Mcp-Session-Id"

// versionHeader is the protocol revision a client declares.
const versionHeader = "MCP-Protocol-Version"

// Handler serves the streamable HTTP transport.
type Handler struct {
	Server *Server

	// Authenticate turns a bearer token into a credential. It is a function
	// rather than a store because the same endpoint accepts two kinds of token
	// — an API key and an OAuth access token — and which one arrived is the
	// authenticator's problem, not this handler's.
	Authenticate func(ctx context.Context, token string) (*apikeys.Key, error)

	// ResourceMetadataURL is advertised in the challenge on a failed
	// authentication. It is what makes a client discover the authorisation
	// server on its own instead of the person having to paste a second URL.
	ResourceMetadataURL string
}

// ServeHTTP answers one MCP request.
//
// POST carries a JSON-RPC message and gets a JSON response. GET is refused
// rather than upgraded to an event stream: this server never speaks first — it
// has no sampling requests, no progress notifications and no subscriptions — so
// holding a stream open would be a connection per client that carries nothing.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.post(w, r)

	case http.MethodGet:
		// 405 with Allow is the specification's own answer for a server that
		// does not offer the server-to-client stream, and clients handle it.
		w.Header().Set("Allow", "POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "this server does not open a server-to-client stream; POST your JSON-RPC messages instead")

	case http.MethodDelete:
		// There is no session state to discard, so ending one always succeeds.
		w.WriteHeader(http.StatusNoContent)

	case http.MethodOptions:
		w.Header().Set("Allow", "POST, DELETE, OPTIONS")
		w.WriteHeader(http.StatusNoContent)

	default:
		w.Header().Set("Allow", "POST, DELETE, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "POST a JSON-RPC message to this endpoint")
	}
}

// post handles one JSON-RPC message.
func (h *Handler) post(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		// A body over the limit is refused as too large, not reported as bad
		// JSON: truncating it and then failing to parse the remainder would
		// name the wrong problem.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "the request body is larger than "+strconv.Itoa(MaxBodyBytes)+" bytes")
			return
		}

		writeError(w, http.StatusBadRequest, "could not read the request body")
		return
	}

	// The credential is resolved before the method is even parsed, except for
	// initialize: a client has to be able to discover what this server is and
	// which authorisation server to use before it has a token, and refusing the
	// handshake outright is how a client ends up with no way to start.
	key, authErr := h.credential(r)

	if authErr != nil && !isInitialize(body) {
		h.challenge(w, authErr)
		return
	}

	response := h.Server.Handle(r.Context(), key, body)

	// A notification gets no body. Answering one with an empty object is a
	// response the client cannot match to anything it sent.
	if response == nil {
		w.Header().Set(sessionHeader, sessionID(r))
		w.WriteHeader(http.StatusAccepted)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(sessionHeader, sessionID(r))
	w.Header().Set(versionHeader, ProtocolVersion)
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(response); err != nil && h.Server.Log != nil {
		h.Server.Log.Error("mcp response could not be written", "error", err)
	}
}

// isInitialize reports whether a body is the handshake, without committing to
// parsing it properly — a malformed body is not the handshake either way, and
// the real parse happens a line later.
func isInitialize(body []byte) bool {
	var probe struct {
		Method string `json:"method"`
	}

	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}

	return probe.Method == "initialize"
}

// credential authenticates the bearer token.
func (h *Handler) credential(r *http.Request) (*apikeys.Key, error) {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return nil, errors.New("this endpoint needs a bearer token")
	}

	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return nil, errors.New("the Authorization header must be of the form: Bearer <token>")
	}

	if h.Authenticate == nil {
		return nil, errors.New("this server has no authenticator configured")
	}

	return h.Authenticate(r.Context(), strings.TrimSpace(token))
}

// challenge refuses an unauthenticated call and points at the metadata that
// says how to get a token.
//
// The resource-metadata parameter is what turns "unauthorised" into a working
// connection: a client that reads it discovers the authorisation server,
// registers itself and comes back with a token, with nobody pasting anything.
func (h *Handler) challenge(w http.ResponseWriter, err error) {
	value := `Bearer realm="feasible"`
	if h.ResourceMetadataURL != "" {
		value += `, resource_metadata="` + h.ResourceMetadataURL + `"`
	}

	w.Header().Set("WWW-Authenticate", value)
	writeError(w, http.StatusUnauthorized, err.Error())
}

// writeError answers a transport-level failure. It is a JSON body rather than
// plain text because a client that is expecting JSON everywhere else will
// happily report "unexpected token" instead of the actual reason.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// sessionID echoes the client's session or mints one. It is minted rather than
// omitted so that a client which stores and replays it behaves consistently
// whether or not it sent one first.
func sessionID(r *http.Request) string {
	if existing := r.Header.Get(sessionHeader); existing != "" {
		return existing
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "feasible"
	}

	return base64.RawURLEncoding.EncodeToString(raw)
}
