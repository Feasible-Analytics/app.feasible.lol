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
	internalTimeHeader = "X-Feasible-Time"
	internalSigHeader  = "X-Feasible-Sig"
	internalClockSkew  = 5 * time.Minute
	internalKeyRate    = 1000
)

// InternalSigner signs requests sent from an ingester to an app shard.
type InternalSigner struct {
	Key string
	Now func() time.Time
}

// Sign adds a timestamp and body-integrity signature to a request. The shared
// key itself never leaves the process.
func (s *InternalSigner) Sign(request *http.Request, body []byte) error {
	if s.Key == "" {
		return fmt.Errorf("internal authentication has no signing key")
	}

	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	signature := internalSignature(s.Key, request.Method, request.URL.EscapedPath(), timestamp, body)

	request.Header.Set(internalTimeHeader, timestamp)
	request.Header.Set(internalSigHeader, signature)

	return nil
}

// VerifyInternal authenticates a private request before handing it to the
// shard handler. The body is restored after verification so the real handler
// reads exactly the bytes whose digest was signed.
func VerifyInternal(key string, next http.Handler) http.Handler {
	type keyWindow struct {
		second int64
		count  int
	}
	var rateMu sync.Mutex
	var rate keyWindow

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key == "" {
			http.Error(w, "internal signing key is not configured", http.StatusUnauthorized)
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
		got, err := hex.DecodeString(internalSignature(key, r.Method, r.URL.EscapedPath(), stamp, body))
		if err != nil || !hmac.Equal(got, want) {
			http.Error(w, "internal request signature is invalid", http.StatusUnauthorized)
			return
		}

		second := time.Now().Unix()
		rateMu.Lock()
		if rate.second != second {
			rate = keyWindow{second: second}
		}
		rate.count++
		count := rate.count
		rateMu.Unlock()
		if count > internalKeyRate {
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
