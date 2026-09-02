//
// internal_auth.go
// HMAC authentication for private ingest and routing requests.
//
// Created: 2026-09-01
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	internalKeyHeader  = "X-Feasible-Key"
	internalTimeHeader = "X-Feasible-Time"
	internalSigHeader  = "X-Feasible-Sig"
	internalClockSkew  = 5 * time.Minute
	internalKeyRate    = 1000
)

// InternalKey is one rotatable signing identity. Signers use the first key;
// verifiers accept every configured key so rotation requires no coordinated
// restart.
type InternalKey struct {
	ID     string
	Secret string
}

// InternalSigner signs requests sent from an ingester to an app shard.
type InternalSigner struct {
	Keys []InternalKey
	Now  func() time.Time
}

// Sign adds an identity, timestamp, and body-integrity signature to a request.
func (s *InternalSigner) Sign(request *http.Request, body []byte) error {
	if len(s.Keys) == 0 || s.Keys[0].ID == "" || s.Keys[0].Secret == "" {
		return fmt.Errorf("internal authentication has no signing key")
	}

	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	signature := internalSignature(s.Keys[0].Secret, request.Method, request.URL.EscapedPath(), timestamp, body)

	request.Header.Set(internalKeyHeader, s.Keys[0].ID)
	request.Header.Set(internalTimeHeader, timestamp)
	request.Header.Set(internalSigHeader, signature)

	return nil
}

// VerifyInternal authenticates a private request before handing it to the
// shard handler. The body is restored after verification so the real handler
// reads exactly the bytes whose digest was signed.
func VerifyInternal(keys []InternalKey, next http.Handler) http.Handler {
	byID := make(map[string]InternalKey, len(keys))
	for _, key := range keys {
		byID[key.ID] = key
	}
	type keyWindow struct {
		second int64
		count  int
	}
	var rateMu sync.Mutex
	rate := make(map[string]keyWindow, len(keys))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyID := strings.TrimSpace(r.Header.Get(internalKeyHeader))
		key, ok := byID[keyID]
		if !ok || key.Secret == "" {
			http.Error(w, "internal request identity is not accepted", http.StatusUnauthorized)
			return
		}

		stamp := strings.TrimSpace(r.Header.Get(internalTimeHeader))
		seconds, err := strconv.ParseInt(stamp, 10, 64)
		if err != nil {
			http.Error(w, "internal request timestamp is invalid", http.StatusUnauthorized)
			return
		}
		if skew := time.Since(time.Unix(seconds, 0)); skew > internalClockSkew || skew < -internalClockSkew {
			http.Error(w, fmt.Sprintf("internal request clock skew is %s", skew.Round(time.Second)), http.StatusUnauthorized)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes*1000))
		if err != nil {
			http.Error(w, "internal request body could not be read", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		want, err := hex.DecodeString(strings.TrimSpace(r.Header.Get(internalSigHeader)))
		if err != nil {
			http.Error(w, "internal request signature is invalid", http.StatusUnauthorized)
			return
		}
		got, err := hex.DecodeString(internalSignature(key.Secret, r.Method, r.URL.EscapedPath(), stamp, body))
		if err != nil || !hmac.Equal(got, want) {
			http.Error(w, "internal request signature is invalid", http.StatusUnauthorized)
			return
		}

		second := time.Now().Unix()
		rateMu.Lock()
		window := rate[keyID]
		if window.second != second {
			window = keyWindow{second: second}
		}
		window.count++
		rate[keyID] = window
		rateMu.Unlock()
		if window.count > internalKeyRate {
			http.Error(w, "internal request rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// internalSignature computes the canonical request signature shared by both
// sides of the private protocol.
func internalSignature(secret, method, path, timestamp string, body []byte) string {
	digest := sha256.Sum256(body)
	canonical := method + "\n" + path + "\n" + timestamp + "\n" + hex.EncodeToString(digest[:])

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))

	return hex.EncodeToString(mac.Sum(nil))
}
