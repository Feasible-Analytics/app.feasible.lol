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
	"slices"
	"strconv"
	"strings"
	"testing"
)

// setProductionOperator supplies a concrete self-hosted legal identity and a
// real ingest salt to production tests whose subject is neither of those.
func setProductionOperator(t *testing.T) {
	t.Helper()
	t.Setenv("FEASIBLE_APP_HOSTED", "false")
	t.Setenv("FEASIBLE_OPERATOR_NAME", "Example Operator, Inc.")
	t.Setenv("FEASIBLE_OPERATOR_ADDRESS", "123 Example Street")
	t.Setenv("FEASIBLE_OPERATOR_EMAIL", "privacy@example.test")
	t.Setenv("FEASIBLE_INGEST_SALT", "a-production-salt-nobody-else-knows")
}

// TestProductionRefusesAShortSalt covers the gap the default-value check leaves.
// A salt somebody shortened by hand is not the shipped default, so only a length
// floor stops a value small enough to enumerate against the stored hashes.
func TestProductionRefusesAShortSalt(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", EnvProduction)
	setProductionOperator(t)
	t.Setenv("FEASIBLE_INGEST_SALT", "short")

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "FEASIBLE_INGEST_SALT") {
		t.Fatalf("production accepted a five-character salt: err = %v", err)
	}

	t.Setenv("FEASIBLE_INGEST_SALT", strings.Repeat("s", MinIngestSaltLength))
	if _, err := LoadFrom(loader); err != nil {
		t.Fatalf("production rejected a salt at the floor: %v", err)
	}
}

// TestProductionRefusesTheDevelopmentSalt is the check that keeps a forgotten
// variable from shipping a public salt: with it, every visitor's daily hash is
// brute-forceable by anyone holding the fact rows.
func TestProductionRefusesTheDevelopmentSalt(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", EnvProduction)
	setProductionOperator(t)
	t.Setenv("FEASIBLE_INGEST_SALT", DefaultIngestSalt)

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "FEASIBLE_INGEST_SALT") {
		t.Fatalf("production accepted the development salt: err = %v", err)
	}

	t.Setenv("FEASIBLE_ENV", EnvDevelopment)
	if _, err := LoadFrom(loader); err != nil {
		t.Fatalf("development rejected its own default salt: %v", err)
	}
}

// TestProductionRefusesAShortInternalKey keeps the HMAC key that authenticates
// ingesters to app shards from being a guessable placeholder.
func TestProductionRefusesAShortInternalKey(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", EnvProduction)
	setProductionOperator(t)
	t.Setenv("FEASIBLE_INTERNAL_KEY", "dev-only-change-me")

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "FEASIBLE_INTERNAL_KEY") {
		t.Fatalf("production accepted an %d-character internal key: err = %v", len("dev-only-change-me"), err)
	}

	t.Setenv("FEASIBLE_INTERNAL_KEY", strings.Repeat("k", MinInternalKeyLength))
	if _, err := LoadFrom(loader); err != nil {
		t.Fatalf("a %d-character key was rejected: %v", MinInternalKeyLength, err)
	}
}

// TestIntFailsLoudly is the "never fail silently" rule applied to numbers: a
// shard id of "abc" quietly becoming shard 1 would have two app processes claim
// the same position, and a rate limit of zero would lock every customer out.
func TestIntFailsLoudly(t *testing.T) {
	cases := map[string]string{
		"FEASIBLE_APP_SHARD_ID":            "abc",
		"FEASIBLE_API_RATE_LIMIT":          "0",
		"FEASIBLE_SMTP_PORT":               "-25",
		"FEASIBLE_WEBHOOK_TIMEOUT_SECONDS": "soon",
		"FEASIBLE_QUERY_SAMPLE_THRESHOLD":  "lots",
	}

	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)

			loader, err := NewLoader("", "")
			if err != nil {
				t.Fatal(err)
			}

			if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("%s=%q was accepted: err = %v", name, value, err)
			}
		})
	}
}

// TestQuerySampleThresholdAcceptsANegativeValue pins the documented meaning of
// the threshold's sign: negative turns automatic sampling off for an operator
// who would rather wait than estimate.
func TestQuerySampleThresholdAcceptsANegativeValue(t *testing.T) {
	t.Setenv("FEASIBLE_QUERY_SAMPLE_THRESHOLD", "-1")

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(loader)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.API.QuerySampleThreshold != -1 {
		t.Fatalf("threshold = %d, want -1", cfg.API.QuerySampleThreshold)
	}
}

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

// TestParseDotenv covers the shapes people actually write by hand, including a
// quoted shared key whose spaces must survive parsing.
func TestParseDotenv(t *testing.T) {
	body := `
# a comment
export FEASIBLE_ENV=production

FEASIBLE_LOG_LEVEL = debug
FEASIBLE_INTERNAL_KEY='secret with spaces'
QUOTED="with spaces"
`

	got, err := ParseDotenv(body)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"FEASIBLE_ENV":          "production",
		"FEASIBLE_LOG_LEVEL":    "debug",
		"FEASIBLE_INTERNAL_KEY": "secret with spaces",
		"QUOTED":                "with spaces",
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

// TestLoadFromRejectsUnknownEnvironment prevents a typo from silently taking
// development defaults on a production host.
func TestLoadFromRejectsUnknownEnvironment(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", "prodution")
	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "FEASIBLE_ENV") {
		t.Fatalf("unknown environment error = %v", err)
	}
}

// TestLoadFromRejectsMalformedBooleans covers every boolean whose fallback can
// alter telemetry, legal mode, or SMTP transport security.
func TestLoadFromRejectsMalformedBooleans(t *testing.T) {
	for _, name := range []string{"FEASIBLE_TRACE_EVENTS", "FEASIBLE_APP_HOSTED", "FEASIBLE_SMTP_STARTTLS"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "truthy")
			loader, err := NewLoader("", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("malformed boolean error = %v", err)
			}
		})
	}
}

// TestHostedProductionAllowsLogOnlyMail keeps a hosted deployment usable while
// its transactional provider is deliberately deferred and messages stay local.
func TestHostedProductionAllowsLogOnlyMail(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", EnvProduction)
	t.Setenv("FEASIBLE_APP_HOSTED", "true")
	t.Setenv("FEASIBLE_INGEST_SALT", "a-production-salt-nobody-else-knows")
	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}
	config, err := LoadFrom(loader)
	if err != nil {
		t.Fatalf("hosted log-mail configuration: %v", err)
	}
	if config.App.MailTransport != MailTransportLog {
		t.Fatalf("mail transport = %q, want %q", config.App.MailTransport, MailTransportLog)
	}
}

// TestStripeConfigurationRequiresWebhookSecret makes all-or-none provider
// configuration include inbound authenticity, not only checkout fields.
func TestStripeConfigurationRequiresWebhookSecret(t *testing.T) {
	for name, value := range map[string]string{
		"FEASIBLE_STRIPE_SECRET_KEY": "sk_test", "FEASIBLE_STRIPE_PUBLISHABLE_KEY": "pk_test",
		"FEASIBLE_STRIPE_PRODUCT": "prod_test", "FEASIBLE_STRIPE_PRICE_MONTHLY": "price_month",
		"FEASIBLE_STRIPE_PRICE_YEARLY": "price_year",
	} {
		t.Setenv(name, value)
	}
	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "FEASIBLE_STRIPE_WEBHOOK_SECRET") {
		t.Fatalf("incomplete Stripe error = %v", err)
	}
}

// TestSelfHostedModeIgnoresStripeConfiguration makes the mode flag
// authoritative even when a machine still carries an incomplete old secret.
func TestSelfHostedModeIgnoresStripeConfiguration(t *testing.T) {
	t.Setenv("FEASIBLE_APP_HOSTED", "false")
	t.Setenv("FEASIBLE_STRIPE_SECRET_KEY", "stale-secret")

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(loader); err != nil {
		t.Fatalf("self-hosted configuration rejected an ignored Stripe secret: %v", err)
	}
}

// FuzzBoolFailsClosed proves no malformed spelling is silently converted to a
// fallback. Only values accepted by strconv.ParseBool may load successfully.
func FuzzBoolFailsClosed(f *testing.F) {
	for _, value := range []string{"true", "false", "1", "0", "truthy", "", " TRUE "} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		loader := &Loader{dotenv: map[string]string{"FLAG": value}}
		_, parseErr := strconv.ParseBool(value)
		_, err := loader.Bool("FLAG", false)
		if value == "" {
			if err != nil {
				t.Fatalf("empty value should use fallback: %v", err)
			}
			return
		}
		if (err == nil) != (parseErr == nil) {
			t.Fatalf("value %q parse error=%v loader error=%v", value, parseErr, err)
		}
	})
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
	if cfg.App.Listen != DefaultAppListen {
		t.Errorf("listen address: got %q", cfg.App.Listen)
	}
	if cfg.App.Transport != TransportDirect {
		t.Errorf("transport: got %q, want the single-process default", cfg.App.Transport)
	}
	if !cfg.App.Hosted {
		t.Error("hosted mode should default to true")
	}
	if cfg.Shared.LogLevel != "debug" || cfg.Shared.LogFormat != "text" {
		t.Errorf("development logging defaults: got %q/%q", cfg.Shared.LogLevel, cfg.Shared.LogFormat)
	}
	if len(cfg.Ingest.Shards) != 1 {
		t.Errorf("shards: got %v", cfg.Ingest.Shards)
	}
	if cfg.App.ShardID != DefaultAppShardID {
		t.Errorf("app shard: got %d", cfg.App.ShardID)
	}
	if cfg.Shared.IngestSalt != DefaultIngestSalt {
		t.Errorf("ingest salt: got %q", cfg.Shared.IngestSalt)
	}
	if cfg.Ingest.BufferPath != DefaultIngestBufferPath {
		t.Errorf("outbox path: got %q", cfg.Ingest.BufferPath)
	}
}

// TestLoadFromProductionLoggingDefaults checks that production flips the logging
// defaults on its own. Nobody should have to remember to set JSON logs on a
// production box, and text logs there are only discovered when someone tries to
// search them.
func TestLoadFromProductionLoggingDefaults(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", EnvProduction)
	setProductionOperator(t)

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

// TestSelfHostedProductionRequiresOperatorIdentity prevents public privacy and
// DPA pages from booting with a generic URL where a legal operator must appear.
func TestSelfHostedProductionRequiresOperatorIdentity(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", EnvProduction)
	t.Setenv("FEASIBLE_APP_HOSTED", "false")
	t.Setenv("FEASIBLE_INGEST_SALT", "a-production-salt-nobody-else-knows")
	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "FEASIBLE_OPERATOR_NAME") || !strings.Contains(err.Error(), "FEASIBLE_OPERATOR_ADDRESS") || !strings.Contains(err.Error(), "FEASIBLE_OPERATOR_EMAIL") {
		t.Fatalf("missing self-hosted operator error = %v", err)
	}

	setProductionOperator(t)
	if _, err := LoadFrom(loader); err != nil {
		t.Fatalf("configured self-hosted operator was rejected: %v", err)
	}
}

// TestLoadFromReadsInternalKey verifies both processes receive the same plain
// signing value without a JSON wrapper or key identifier.
func TestLoadFromReadsInternalKey(t *testing.T) {
	t.Setenv("FEASIBLE_INTERNAL_KEY", "shared-signing-key")

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(loader)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Shared.InternalKey != "shared-signing-key" {
		t.Fatalf("internal key = %q", cfg.Shared.InternalKey)
	}
}

// TestLoadFromParsesOrderedShardArray verifies JSON order becomes stable shard
// identity and URL normalization does not disturb that order.
func TestLoadFromParsesOrderedShardArray(t *testing.T) {
	t.Setenv("FEASIBLE_INGEST_SHARDS", `["http://app-1:19301/","http://app-2:19301"]`)

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(loader)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"http://app-1:19301", "http://app-2:19301"}
	if !slices.Equal(cfg.Ingest.Shards, want) {
		t.Fatalf("shards = %v, want %v", cfg.Ingest.Shards, want)
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
	setProductionOperator(t)

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

// TestSESTransportNeedsItsCredentials refuses a half-configured SES setup at
// start-up. AWS answers an unsigned or half-signed request with a signature
// complaint that names no variable, so the missing ones are listed here while
// somebody is still looking at the configuration.
func TestSESTransportNeedsItsCredentials(t *testing.T) {
	t.Setenv("FEASIBLE_APP_MAIL_TRANSPORT", "ses")

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}

	_, err = LoadFrom(loader)
	if err == nil {
		t.Fatal("the ses transport was accepted with no credentials")
	}
	for _, want := range []string{"FEASIBLE_AWS_ACCESS_KEY_ID", "FEASIBLE_AWS_SECRET_ACCESS_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}

	t.Setenv("FEASIBLE_AWS_ACCESS_KEY_ID", "AKIAEXAMPLEKEYID0000")
	t.Setenv("FEASIBLE_AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")

	loader, err = NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(loader)
	if err != nil {
		t.Fatalf("a complete ses configuration was refused: %v", err)
	}

	// The region defaults rather than being required, because it has no safe
	// empty value: it becomes part of the endpoint hostname.
	if cfg.App.AWS.SESRegion != DefaultSESRegion {
		t.Fatalf("ses region = %q, want %q", cfg.App.AWS.SESRegion, DefaultSESRegion)
	}
	if cfg.App.AWS.SESConfigurationSet != "" {
		t.Fatalf("ses configuration set = %q, want it empty by default", cfg.App.AWS.SESConfigurationSet)
	}
}

// TestHostedProductionShardSchemes covers the one hop where TLS is
// confidentiality rather than authentication. The loopback and Tailscale cases
// are the ones a real deployment hits: the app port serves plain HTTP, so
// demanding https of them forces the internal call back out through the public
// edge, which is configured to refuse it.
func TestHostedProductionShardSchemes(t *testing.T) {
	for name, tc := range map[string]struct {
		shard   string
		allowed bool
	}{
		"public https":       {"https://app.example.com", true},
		"public plaintext":   {"http://app.example.com", false},
		"loopback v4":        {"http://127.0.0.1:19303", true},
		"loopback v6":        {"http://[::1]:19303", true},
		"localhost":          {"http://localhost:19301", true},
		"tailscale address":  {"http://100.101.102.103:19301", true},
		"tailscale magicdns": {"http://shard-one.tailnet.ts.net:19301", true},
		"private lan":        {"http://10.0.0.5:19301", false},
		"lookalike hostname": {"http://ts.net.example.com", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("FEASIBLE_ENV", EnvProduction)
			t.Setenv("FEASIBLE_APP_HOSTED", "true")
			// The scheme rule only governs the store-and-forward topology; a
			// direct app writes its own events and never makes this call.
			t.Setenv("FEASIBLE_APP_TRANSPORT", TransportHTTP)
			// Every internal request is signed with this key whatever the
			// scheme, which is why TLS on that hop is confidentiality alone.
			t.Setenv("FEASIBLE_INTERNAL_KEY", "an-internal-signing-key-nobody-else-knows")
			t.Setenv("FEASIBLE_INGEST_SALT", "a-production-salt-nobody-else-knows")
			t.Setenv("FEASIBLE_INGEST_SHARDS", `["`+tc.shard+`"]`)

			loader, err := NewLoader("", "")
			if err != nil {
				t.Fatal(err)
			}

			_, err = LoadFrom(loader)
			if tc.allowed && err != nil {
				t.Fatalf("shard %q rejected: %v", tc.shard, err)
			}
			if !tc.allowed {
				if err == nil {
					t.Fatalf("shard %q accepted, want rejection", tc.shard)
				}
				if !strings.Contains(err.Error(), "must use https") {
					t.Fatalf("shard %q error = %v", tc.shard, err)
				}
			}
		})
	}
}
