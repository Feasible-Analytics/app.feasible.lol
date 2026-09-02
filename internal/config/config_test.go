//
// config_test.go
// Tests for the configuration loader.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/replica"
)

// setValidReplicaAttestation writes the atomic provider bundle required by
// production startup. The canonical renderer keeps the fixture aligned with
// the deployed lifecycle contract.
func setValidReplicaAttestation(t *testing.T, replicaURL string) {
	t.Helper()

	policy, err := replica.Render(replicaURL)
	if err != nil {
		t.Fatal(err)
	}

	location, err := replica.ParseLocation(replicaURL)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := json.Marshal(replica.Attestation{
		Version: replica.AttestationVersion, FetchedAt: time.Now().UTC(), ReplicaURL: replicaURL,
		Bucket: location.Bucket, Prefix: location.Prefix,
		BucketLocation: json.RawMessage(`{"LocationConstraint":null}`),
		Lifecycle:      policy, Versioning: json.RawMessage(`{}`), ObjectLock: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "replica-attestation.json")
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FEASIBLE_LITESTREAM_ATTESTATION", path)
}

// setProductionOperator supplies a concrete self-hosted legal identity to
// production tests whose subject is unrelated to legal-mode validation.
func setProductionOperator(t *testing.T) {
	t.Helper()
	t.Setenv("FEASIBLE_APP_HOSTED", "false")
	t.Setenv("FEASIBLE_OPERATOR_NAME", "Example Operator, Inc.")
	t.Setenv("FEASIBLE_OPERATOR_ADDRESS", "123 Example Street")
	t.Setenv("FEASIBLE_OPERATOR_EMAIL", "privacy@example.test")
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

// TestHostedProductionRejectsLogOnlyMail ensures a deployment cannot publish
// deletion-warning promises while writing every message only to local disk.
func TestHostedProductionRejectsLogOnlyMail(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", EnvProduction)
	t.Setenv("FEASIBLE_APP_HOSTED", "true")
	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "log-only") {
		t.Fatalf("hosted log-mail error = %v", err)
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

// TestHostedProductionRequiresConcreteSubprocessors keeps the public legal list
// from degrading into category placeholders on a real hosted deployment.
func TestHostedProductionRequiresConcreteSubprocessors(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", EnvProduction)
	t.Setenv("FEASIBLE_APP_HOSTED", "true")
	t.Setenv("FEASIBLE_APP_MAIL_TRANSPORT", MailTransportSMTP)
	t.Setenv("FEASIBLE_SMTP_HOST", "smtp.example.test")

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "compute") {
		t.Fatalf("missing hosted inventory error = %v", err)
	}

	t.Setenv("FEASIBLE_HOSTED_SUBPROCESSORS_JSON", `[
		{"role":"compute","legal_entity":"Compute Corp","service":"Virtual machines","data":"Encrypted visitor analytics","region":"US"},
		{"role":"object_storage","legal_entity":"Storage Corp","service":"Object storage","data":"Encrypted database replicas","region":"US"},
		{"role":"email","legal_entity":"Mail Corp","service":"Transactional email","data":"Account addresses and service messages","region":"US"}
	]`)
	replicaURL := "s3://replicas/shard-01"
	t.Setenv("FEASIBLE_LITESTREAM_REPLICA_URL", replicaURL)
	t.Setenv("FEASIBLE_LITESTREAM_ON_CHANGE", "systemctl restart litestream")
	setValidReplicaAttestation(t, replicaURL)

	cfg, err := LoadFrom(loader)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.App.Subprocessors) != 3 || cfg.App.Subprocessors[1].Role != "object_storage" {
		t.Fatalf("subprocessors = %+v", cfg.App.Subprocessors)
	}
}

// TestHostedProductionRequiresReplicaEnforcement keeps the live legal promise
// fail-closed when provider retention checks or timely Litestream reloads are
// absent from a hosted deployment.
func TestHostedProductionRequiresReplicaEnforcement(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", EnvProduction)
	t.Setenv("FEASIBLE_APP_HOSTED", "true")
	t.Setenv("FEASIBLE_APP_MAIL_TRANSPORT", MailTransportSMTP)
	t.Setenv("FEASIBLE_SMTP_HOST", "smtp.example.test")
	t.Setenv("FEASIBLE_HOSTED_SUBPROCESSORS_JSON", `[
		{"role":"compute","legal_entity":"Compute Corp","service":"Virtual machines","data":"Encrypted visitor analytics","region":"US"},
		{"role":"object_storage","legal_entity":"Storage Corp","service":"Object storage","data":"Encrypted database replicas","region":"US"},
		{"role":"email","legal_entity":"Mail Corp","service":"Transactional email","data":"Account addresses and service messages","region":"US"}
	]`)

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "REPLICA_URL") {
		t.Fatalf("missing replica configuration error = %v", err)
	}

	t.Setenv("FEASIBLE_LITESTREAM_REPLICA_URL", "s3://replicas/shard-01")
	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "ON_CHANGE") {
		t.Fatalf("missing reload command error = %v", err)
	}

	t.Setenv("FEASIBLE_LITESTREAM_ON_CHANGE", "systemctl restart litestream")
	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "ATTESTATION") {
		t.Fatalf("missing provider attestation error = %v", err)
	}

	setValidReplicaAttestation(t, "s3://replicas/shard-01")
	t.Setenv("FEASIBLE_LITESTREAM_WATCH_SECONDS", "61")
	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "within 60 seconds") {
		t.Fatalf("slow replica watch error = %v", err)
	}
}

// TestProductionRejectsInvalidReplicaAttestation proves application startup is
// itself fail-closed when a provider export is unreadable or no longer proves
// the public expiry bound; a separate scheduled check is not the only guard.
func TestProductionRejectsInvalidReplicaAttestation(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", EnvProduction)
	setProductionOperator(t)
	replicaURL := "s3://replicas/shard-01"
	t.Setenv("FEASIBLE_LITESTREAM_REPLICA_URL", replicaURL)
	t.Setenv("FEASIBLE_LITESTREAM_ATTESTATION", filepath.Join(t.TempDir(), "missing.json"))

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "read provider evidence") {
		t.Fatalf("unreadable attestation error = %v", err)
	}

	setValidReplicaAttestation(t, replicaURL)
	path := os.Getenv("FEASIBLE_LITESTREAM_ATTESTATION")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var evidence replica.Attestation
	if err := json.Unmarshal(body, &evidence); err != nil {
		t.Fatal(err)
	}
	evidence.Versioning = json.RawMessage(`{"Status":"Enabled"}`)
	body, err = json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "versioning") {
		t.Fatalf("noncompliant attestation error = %v", err)
	}
}

// TestHostedProductionRejectsSampleSubprocessors prevents the visibly local
// sample from being copied into a hosted production environment unchanged.
func TestHostedProductionRejectsSampleSubprocessors(t *testing.T) {
	t.Setenv("FEASIBLE_ENV", EnvProduction)
	t.Setenv("FEASIBLE_APP_HOSTED", "true")
	t.Setenv("FEASIBLE_APP_MAIL_TRANSPORT", MailTransportSMTP)
	t.Setenv("FEASIBLE_SMTP_HOST", "smtp.example.test")
	t.Setenv("FEASIBLE_HOSTED_SUBPROCESSORS_JSON", `[
		{"role":"compute","legal_entity":"LOCAL PLACEHOLDER","service":"VM","data":"data","region":"US"},
		{"role":"object_storage","legal_entity":"Storage Corp","service":"objects","data":"replicas","region":"US"},
		{"role":"email","legal_entity":"Mail Corp","service":"mail","data":"addresses","region":"US"}
	]`)

	loader, err := NewLoader("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(loader); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("sample hosted inventory error = %v", err)
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
