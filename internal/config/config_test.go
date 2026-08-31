//
// config_test.go
// Tests for the configuration loader.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLookupPrefersConfigDir is the Docker-secrets contract from the CLI issue:
// a file in $CONFIG_DIR beats the environment. If this ever regresses, a
// container would keep booting happily with a stale environment value instead
// of the mounted secret, which is not a failure anyone would notice.
func TestLookupPrefersConfigDir(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "FEASIBLE_APP_BASE_URL"), []byte("http://secret.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FEASIBLE_APP_BASE_URL", "http://from-environment.example")

	loader, err := NewLoader(dir, "")
	if err != nil {
		t.Fatal(err)
	}

	if got := loader.String("FEASIBLE_APP_BASE_URL", ""); got != "http://secret.example" {
		t.Fatalf("got %q, want the value from the secrets file with its newline trimmed", got)
	}
}

// TestLookupPrefersEnvironmentOverDotenv checks the other half of the ordering.
// A shell export has to win over the committed sample defaults, otherwise
// overriding one variable for one run would be impossible.
func TestLookupPrefersEnvironmentOverDotenv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	if err := os.WriteFile(path, []byte("FEASIBLE_APP_LISTEN=127.0.0.1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	loader, err := NewLoader("", path)
	if err != nil {
		t.Fatal(err)
	}

	if got := loader.String("FEASIBLE_APP_LISTEN", ""); got != "127.0.0.1:1" {
		t.Fatalf("dotenv value not used: got %q", got)
	}

	t.Setenv("FEASIBLE_APP_LISTEN", "127.0.0.1:2")

	if got := loader.String("FEASIBLE_APP_LISTEN", ""); got != "127.0.0.1:2" {
		t.Fatalf("environment did not override dotenv: got %q", got)
	}
}

// TestParseDotenv covers the shapes people actually write by hand. The JSON key
// list is the one that matters: it is full of quotes and colons, and losing its
// surrounding quotes would turn a working config into an unparseable one.
func TestParseDotenv(t *testing.T) {
	body := `
# a comment
export FEASIBLE_ENV=production

FEASIBLE_LOG_LEVEL = debug
FEASIBLE_INTERNAL_KEYS='[{"id":"dev-01","secret":"s"}]'
QUOTED="with spaces"
`

	got, err := ParseDotenv(body)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"FEASIBLE_ENV":           "production",
		"FEASIBLE_LOG_LEVEL":     "debug",
		"FEASIBLE_INTERNAL_KEYS": `[{"id":"dev-01","secret":"s"}]`,
		"QUOTED":                 "with spaces",
	}

	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s: got %q, want %q", name, got[name], value)
		}
	}
}

// TestParseDotenvRejectsGarbage makes sure a malformed file fails loudly. A
// dotenv parser that skips lines it does not understand is how a typo becomes a
// silently missing secret.
func TestParseDotenvRejectsGarbage(t *testing.T) {
	if _, err := ParseDotenv("this is not a variable\n"); err == nil {
		t.Fatal("expected an error for a line with no =")
	}
}

// TestLoadFromDefaults pins the zero-configuration behaviour: someone who runs
// the binary with nothing set gets a working loopback process, not an error.
func TestLoadFromDefaults(t *testing.T) {
	loader, err := NewLoader(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(loader)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Shared.Env != DefaultEnv {
		t.Errorf("env: got %q", cfg.Shared.Env)
	}
	if cfg.App.Listen != DefaultAppListen || cfg.App.InternalListen != DefaultAppInternalListen {
		t.Errorf("listen addresses: got %q and %q", cfg.App.Listen, cfg.App.InternalListen)
	}
	if cfg.App.Transport != TransportDirect {
		t.Errorf("transport: got %q, want the self-hoster default", cfg.App.Transport)
	}
	if cfg.Shared.LogLevel != "debug" || cfg.Shared.LogFormat != "text" {
		t.Errorf("development logging defaults: got %q/%q", cfg.Shared.LogLevel, cfg.Shared.LogFormat)
	}
	if len(cfg.Ingest.Shards) != 1 {
		t.Errorf("shards: got %v", cfg.Ingest.Shards)
	}
	if cfg.Litestream.ReplicaURL != "" {
		t.Errorf("replica URL: got %q, want empty — an install that configured no replication has none", cfg.Litestream.ReplicaURL)
	}
}

// TestLoadFromReplicationDefaults covers the one pairing among these values that
// produces a replica nobody can restore from: retention has to outlive the
// snapshot interval, or the snapshot a restore replays onto is deleted before
// its replacement exists.
func TestLoadFromReplicationDefaults(t *testing.T) {
	loader, err := NewLoader(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(loader)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Litestream.ConfigPath != DefaultLitestreamConfig {
		t.Errorf("config path: got %q", cfg.Litestream.ConfigPath)
	}
	if cfg.Litestream.SyncInterval != time.Second {
		t.Errorf("sync interval: got %s, want the one second the durability claim quotes", cfg.Litestream.SyncInterval)
	}
	if cfg.Litestream.Retention <= cfg.Litestream.SnapshotInterval {
		t.Errorf("retention %s does not outlive the snapshot interval %s", cfg.Litestream.Retention, cfg.Litestream.SnapshotInterval)
	}
	if cfg.Litestream.WatchInterval <= 0 {
		t.Errorf("watch interval: got %s — a new account would never be picked up", cfg.Litestream.WatchInterval)
	}
}

// TestLoadFromProductionLoggingDefaults checks that production flips the logging
// defaults on its own. Nobody should have to remember to set JSON logs on a
// production box, and text logs there are only discovered when someone tries to
// search them.
func TestLoadFromProductionLoggingDefaults(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", EnvProduction)

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(loader)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Shared.LogLevel != "info" || cfg.Shared.LogFormat != "json" {
		t.Fatalf("production defaults: got %q/%q, want info/json", cfg.Shared.LogLevel, cfg.Shared.LogFormat)
	}
	if !cfg.IsProduction() {
		t.Fatal("IsProduction should be true")
	}
}

// TestLoadFromParsesInternalKeys covers the signing key list, including the
// rotation case of more than one key being valid at once.
func TestLoadFromParsesInternalKeys(t *testing.T) {
	t.Setenv("FEASIBLE_INTERNAL_KEYS", `[{"id":"dev-01","secret":"a"},{"id":"dev-02","secret":"b"}]`)

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(loader)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Shared.InternalKeys) != 2 || cfg.Shared.InternalKeys[1].ID != "dev-02" {
		t.Fatalf("got %+v", cfg.Shared.InternalKeys)
	}
}

// TestLoadFromRejectsBadValues walks the validation rules. Each of these fails
// far away from its cause if it is allowed through: a bad transport drops every
// event, a relative base URL produces links nobody can click.
func TestLoadFromRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		"FEASIBLE_LOG_FORMAT":         "yaml",
		"FEASIBLE_LOG_LEVEL":          "chatty",
		"FEASIBLE_APP_TRANSPORT":      "carrier-pigeon",
		"FEASIBLE_APP_MAIL_TRANSPORT": "pigeon",
		"FEASIBLE_APP_BASE_URL":       "localhost:19300",
		"FEASIBLE_INGEST_SHARDS":      "127.0.0.1:19401",
		"FEASIBLE_INTERNAL_KEYS":      `[{"id":"dev-01"}]`,
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)

			loader, err := NewLoader("", "")
			if err != nil {
				t.Fatal(err)
			}

			if _, err := LoadFrom(loader); err == nil {
				t.Fatalf("%s=%q was accepted", name, value)
			}
		})
	}
}

// TestStripeRequiresACompleteFulfillmentConfiguration prevents checkout from
// being exposed when charges can be created but signed webhooks cannot fulfill
// them, and rejects every other partial catalogue combination as well.
func TestStripeRequiresACompleteFulfillmentConfiguration(t *testing.T) {
	complete := Stripe{
		SecretKey:     "sk_test",
		Product:       "prod_test",
		PriceMonthly:  "price_monthly",
		PriceYearly:   "price_yearly",
		WebhookSecret: "whsec_test",
	}

	for name, clear := range map[string]func(*Stripe){
		"secret":  func(s *Stripe) { s.SecretKey = "" },
		"product": func(s *Stripe) { s.Product = "" },
		"monthly": func(s *Stripe) { s.PriceMonthly = "" },
		"yearly":  func(s *Stripe) { s.PriceYearly = "" },
		"webhook": func(s *Stripe) { s.WebhookSecret = "" },
	} {
		t.Run(name, func(t *testing.T) {
			loader, err := NewLoader("", "")
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadFrom(loader)
			if err != nil {
				t.Fatal(err)
			}

			cfg.App.Stripe = complete
			clear(&cfg.App.Stripe)
			if cfg.App.Stripe.Enabled() {
				t.Fatal("partial Stripe configuration reported enabled")
			}
			if err := cfg.Validate(); err == nil {
				t.Fatal("partial Stripe configuration passed validation")
			}
		})
	}

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFrom(loader)
	if err != nil {
		t.Fatal(err)
	}
	cfg.App.Stripe = complete
	if err := cfg.Validate(); err != nil {
		t.Fatalf("complete Stripe configuration failed validation: %v", err)
	}
	if !cfg.App.Stripe.Enabled() {
		t.Fatal("complete Stripe configuration reported disabled")
	}
}

// TestLoadIgnoresDotenvInProduction protects against a stray .env on a
// production box quietly overriding the real deployment configuration.
func TestLoadIgnoresDotenvInProduction(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("FEASIBLE_APP_LISTEN=127.0.0.1:9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	chdir(t, dir)
	t.Setenv("FEASIBLE_ENV", EnvProduction)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	if cfg.App.Listen != DefaultAppListen {
		t.Fatalf("production read the .env file: listen is %q", cfg.App.Listen)
	}
}

// TestLoadReadsDotenvInDevelopment is the same check from the other side: on a
// laptop the file is the whole point.
func TestLoadReadsDotenvInDevelopment(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("FEASIBLE_APP_LISTEN=127.0.0.1:9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	chdir(t, dir)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}

	if cfg.App.Listen != "127.0.0.1:9999" {
		t.Fatalf("development ignored the .env file: listen is %q", cfg.App.Listen)
	}
}

// chdir moves the process into a directory for the duration of one test and
// puts it back afterwards. Load() reads .env relative to the working directory,
// so testing that behaviour means moving; testing.Chdir would do this for us but
// arrived after the Go version this module targets.
func chdir(t *testing.T, dir string) {
	t.Helper()

	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Fatal(err)
		}
	})
}
