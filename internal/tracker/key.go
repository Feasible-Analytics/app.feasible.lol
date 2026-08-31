//
// key.go
// The per-site script token: how a path is derived, and how one is read back.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package tracker

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SecretSize is the length of the key the tokens are derived with.
const SecretSize = 32

// SecretFileName is where a generated secret is kept. It sits beside the
// databases because it is worthless without them: back up the data directory
// and every customer's snippet URL still resolves after a restore.
const SecretFileName = "script.key"

// TokenBytes is how much of the HMAC ends up in the path. Ten bytes is eighty
// bits, which is far past guessing, and base32-encodes to exactly sixteen
// characters with no padding — a path a person can read down the phone.
const TokenBytes = 10

// IndexTTL is how long a resolved token map is trusted before it is rebuilt.
// It matches the site cache's own refresh interval, so a site added in the
// dashboard starts serving a script on the same schedule that it starts
// accepting events. Two different windows would be two different bug reports.
const IndexTTL = 15 * time.Second

// encoding is lowercase base32 without padding. Base32 rather than hex because
// it is a third shorter, and rather than base64 because a path segment that
// survives being read aloud, pasted into a spreadsheet and lowercased by a CMS
// is worth more than four characters.
var encoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Keyer turns a domain into a script path and back again.
//
// The derivation is keyed rather than a plain hash of the domain. An unkeyed
// hash is a path anybody can compute for any site, which would let a filter
// list enumerate every customer's script URL from their domain list — the exact
// outcome per-site paths exist to prevent.
type Keyer struct {
	secret []byte
	sites  DomainSource

	// mu guards the reverse index. Lookups are rare — one per uncached script
	// fetch — so a mutex is cheaper and clearer than the copy-on-write dance the
	// event hot path needs.
	mu      sync.Mutex
	index   map[string]string
	builtAt time.Time
}

// NewKeyer builds a keyer over a routing map. A nil map is usable: it derives
// tokens correctly, which is all the dashboard needs to render a snippet, and
// simply resolves nothing.
func NewKeyer(secret []byte, sites DomainSource) *Keyer {
	return &Keyer{secret: secret, sites: sites}
}

// LoadSecret resolves the derivation secret, generating and storing one on
// first run.
//
// Rotating it is the documented remedy for a script path that has been added to
// a filter list: delete the file, restart, and every site gets a new path. That
// is why it is a file rather than a value derived from something else — a
// remedy nobody can perform is not a remedy.
func LoadSecret(dataDir string) ([]byte, error) {
	return secretFromFile(filepath.Join(dataDir, SecretFileName))
}

// secretFromFile reads the secret, creating it if it is not there. It is
// written with O_EXCL so that two processes starting at once cannot each
// generate one and hand out two different sets of paths for the same sites.
func secretFromFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		secret, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(secret) != SecretSize {
			return nil, fmt.Errorf("script key %s is corrupt — every site's script path derives from it", path)
		}

		return secret, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("script key %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	secret := make([]byte, SecretSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("script key: %w", err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// Losing the race means another process wrote a secret a moment ago,
		// and that one is the one every path already derives from.
		if os.IsExist(err) {
			return secretFromFile(path)
		}

		return nil, fmt.Errorf("script key %s: %w", path, err)
	}
	defer file.Close()

	if _, err := file.WriteString(hex.EncodeToString(secret)); err != nil {
		return nil, fmt.Errorf("script key %s: %w", path, err)
	}

	return secret, nil
}

// Token derives the opaque path segment for one domain.
//
// The domain is normalised first, and identically to the routing map. A site
// registered as "example.com" whose snippet says "www.example.com" has to reach
// the same script, or the customer sees a 404 for a snippet that looks right.
func (k *Keyer) Token(domain string) string {
	mac := hmac.New(sha256.New, k.secret)
	_, _ = mac.Write([]byte(NormaliseDomain(domain)))

	return encoding.EncodeToString(mac.Sum(nil)[:TokenBytes])
}

// Path is the full URL path a snippet points at, which is what the dashboard
// renders. It is here rather than string-formatted at the call site so that the
// path shape can never drift from the one ServeHTTP parses.
func (k *Keyer) Path(domain string) string {
	return PathPrefix + sitePrefix + k.Token(domain) + siteSuffix
}

// Resolve reads a token back to the domain it was derived from.
//
// The derivation is one-way, so this is a lookup over the sites we know about
// rather than a decode. That is the right trade: it costs one HMAC per site on
// a rebuild that happens at most every fifteen seconds, and it means a token
// for a deleted site resolves to nothing instead of to a domain that no longer
// has an account behind it.
func (k *Keyer) Resolve(token string) (string, bool) {
	if k.sites == nil || token == "" {
		return "", false
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	if k.index == nil || time.Since(k.builtAt) > IndexTTL {
		domains := k.sites.Domains()

		index := make(map[string]string, len(domains))
		for _, domain := range domains {
			index[k.Token(domain)] = domain
		}

		k.index = index
		k.builtAt = time.Now()
	}

	domain, ok := k.index[token]

	return domain, ok
}

// NormaliseDomain puts a domain into the one form tokens are derived from. It
// mirrors the routing map's own normalisation, because a token that resolves to
// a domain the routing map does not hold is a script that reports to nowhere.
func NormaliseDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	domain = strings.TrimSuffix(domain, ".")
	domain = strings.TrimPrefix(domain, "www.")

	return domain
}
