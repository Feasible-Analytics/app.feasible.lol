//
// config.go
// Typed configuration, read from Docker secrets first, then the environment.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package config turns the process environment into typed structs the rest of
// the binary can rely on. Every value is read in one place so that a variable
// cannot be introduced without also being documented in .env.sample, which
// `make check-env` enforces.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults are the values used when a variable is absent. They are deliberately
// the safe, single-machine, self-hoster values: someone who runs the binary with
// no configuration at all gets a working process bound to loopback.
const (
	DefaultEnv                  = "development"
	DefaultAppListen            = "127.0.0.1:19301"
	DefaultAppInternalListen    = "127.0.0.1:19401"
	DefaultAppDataDir           = "./data"
	DefaultAppBaseURL           = "http://localhost:19300"
	DefaultAppTransport         = TransportDirect
	DefaultAppMailTransport     = MailTransportLog
	DefaultAppMailFrom          = "feasible.lol <hello@feasible.lol>"
	DefaultAppSalesEmail        = "sales@feasible.lol"
	DefaultSMTPPort             = 587
	DefaultIngestListen         = "127.0.0.1:19302"
	DefaultIngestInternalListen = "127.0.0.1:19402"
	DefaultIngestShards         = "http://127.0.0.1:19401"
	DefaultIngestBufferPath     = "./data/ingest/buffer.db"

	// DefaultAPIRateLimit is how many public-API requests one key may make an
	// hour. It is configurable at all because the incumbent's equivalent is
	// hard-coded at 600 even in the build people run on their own hardware, and
	// the workaround people actually use is editing their database by hand.
	DefaultAPIRateLimit = 10000

	// DefaultWebhookTimeout bounds one delivery attempt, in seconds.
	DefaultWebhookTimeout = 10

	// DefaultWebhookPoll is how long the delivery worker waits when the queue is
	// empty, in seconds. It loops without pausing while there is work.
	DefaultWebhookPoll = 5

	// DefaultLitestreamConfig is where the generated replication configuration
	// is written. It is outside the data directory on purpose: the file
	// describes how to recover the data directory, and a copy that only exists
	// inside the thing it recovers is not a recovery plan.
	DefaultLitestreamConfig = "/etc/litestream.yml"

	// DefaultLitestreamSync is how often each database's new write-ahead log
	// pages are shipped, in seconds. One second is the replica recovery point;
	// the public 202 already waited for the local account commit.
	DefaultLitestreamSync = 1

	// DefaultLitestreamSnapshot is how often a full copy is taken, in hours.
	DefaultLitestreamSnapshot = 6

	// DefaultLitestreamRetention is how long snapshots and log segments are
	// kept, in hours. It must stay longer than the snapshot interval or a
	// restore has no snapshot to replay onto.
	DefaultLitestreamRetention = 72

	// DefaultLitestreamWatch is how often the watcher re-reads the account
	// directory, in seconds. It is the window in which a brand-new account's
	// database is on disk and not yet replicated.
	DefaultLitestreamWatch = 60
)

// Layout of the data directory. These are constants because both the migrate
// and backup commands have to agree on where a database lives, and a mismatch
// would silently skip every account.
const (
	ControlDatabaseName = "control.db"
	AccountDatabaseDir  = "accounts"
)

// Transport names which public process owns the event endpoint. Direct mounts
// it in the app process; http leaves it to a separate ingest process that uses
// the same shared control and account storage.
const (
	TransportDirect = "direct"
	TransportHTTP   = "http"
)

// Mail transports. The log transport keeps local development free of an SMTP
// service by printing the message and writing the rendered body to disk.
const (
	MailTransportLog  = "log"
	MailTransportSMTP = "smtp"
)

// Environments. Production is singled out because it changes logging defaults
// and disables the .env file.
const (
	EnvProduction  = "production"
	EnvDevelopment = "development"
)

// InternalKey is retained only for configuration compatibility with retired
// internal delivery deployments. The consolidated runtime does not sign an
// ingest-to-writer network hop.
type InternalKey struct {
	ID     string `json:"id"`
	Secret string `json:"secret"`
}

// Shared holds the values both processes read. Their values must match on every
// machine in a deployment, which is why they live in their own section of
// .env.sample rather than being duplicated per app.
type Shared struct {
	Env          string
	LogLevel     string
	LogFormat    string
	InternalKeys []InternalKey
	TraceEvents  bool

	// SaltKey encrypts the fingerprint salts at rest, as 32 hex-encoded bytes.
	// Empty means one is generated under the data directory on first run, so
	// encryption at rest is true by default rather than only when somebody
	// remembered to set a variable.
	SaltKey string
}

// App holds the values only the `serve` process reads.
type App struct {
	Listen         string
	InternalListen string
	DataDir        string
	BaseURL        string
	Transport      string
	MailTransport  string

	// MailFrom is the envelope sender on every message the product sends. A
	// relay rejects a From it does not own, and that rejection is the most
	// common reason a self-hoster's mail stops arriving with nothing in our
	// logs to explain it.
	MailFrom string

	// SalesEmail is where the volume ladder points a growing customer. It is
	// configurable because a self-hoster's "talk to us" address is not ours.
	SalesEmail string

	// SecretKey encrypts the two-factor secrets and signs the short-lived
	// cookies, as 32 hex-encoded bytes. Empty means one is generated under the
	// data directory on first run, so encryption at rest is true by default
	// rather than only when somebody remembered to set a variable.
	SecretKey string

	SMTP   SMTP
	Google GoogleOAuth
	Stripe Stripe
}

// SMTP is the relay the smtp mail transport uses. It is a nested struct so that
// "which of these belongs to mail" is answered by the type rather than by a
// naming convention, and none of it is read unless MailTransport is "smtp".
type SMTP struct {
	Host string

	// Port decides the encryption as well as the destination: 465 is TLS from
	// the first byte and everything else negotiates it after EHLO. It is a
	// number rather than a string so that decision can be made by comparison
	// rather than by parsing it again at the point of use.
	Port int

	Username string
	Password string
	StartTLS bool
}

// GoogleOAuth is the one OAuth application every Google feature shares: signing
// in with Google, importing history from Analytics, and reading Search Console
// are three scopes on one client, not three clients.
//
// Both values being empty is a supported state, not an error: the credentials
// are issued out of band, and a binary that refused to boot without them would
// make the whole product wait on a Google console form.
type GoogleOAuth struct {
	ClientID     string
	ClientSecret string
}

// Configured reports whether the Google features can be offered. A half-filled
// client is unusable rather than partly usable — Google rejects a request
// missing either value — so the features hide themselves instead.
func (g GoogleOAuth) Configured() bool {
	return g.ClientID != "" && g.ClientSecret != ""
}

// Stripe is the payment provider's configuration. Every field is empty on a
// self-hosted install, which is a supported state rather than a broken one:
// billing degrades to "not available here" instead of failing to boot.
//
// The secret key on its own is enough for the delete-account flow to remove a
// customer record; taking money needs the complete catalogue and a verified
// webhook path, which is what Enabled reports.
type Stripe struct {
	SecretKey      string
	PublishableKey string
	Product        string
	PriceMonthly   string
	PriceYearly    string

	// WebhookSecret verifies every delivery. Empty makes the endpoint refuse
	// everything, which is correct: an unverified webhook endpoint is a public
	// URL that changes billing state.
	WebhookSecret string
}

// Enabled reports whether this install can safely take money. Checkout is not
// offered unless its signed fulfillment path and complete catalogue are ready;
// charging while webhooks are rejected would strand a paying account.
func (s Stripe) Enabled() bool {
	return s.SecretKey != "" && s.Product != "" && s.PriceMonthly != "" &&
		s.PriceYearly != "" && s.WebhookSecret != ""
}

// Ingest holds the values only the `ingest` process reads.
type Ingest struct {
	Listen string

	// InternalListen is the loopback address serving /metrics and the health
	// probes. It is a second listener rather than a path on the first because
	// the first is the public front door, and an endpoint that tells the
	// internet our event rate, error rate and account count is free
	// reconnaissance for anybody deciding whether we are worth attacking.
	InternalListen string

	// Shards and BufferPath retain environment parsing compatibility with the
	// retired forwarding topology. Direct account writes do not consume them.
	Shards     []string
	BufferPath string

	// TrustedProxies may set X-Feasible-IP. Empty means nobody: on a
	// directly-exposed instance an unconditionally trusted override lets
	// anyone forge their own geolocation and split their own fingerprint.
	TrustedProxies []string
}

// API holds the values the public API, the MCP server and the webhook worker
// read. They are their own section rather than part of App because the ingest
// tier never reads any of them, and a section is how this file says which
// process a variable belongs to.
type API struct {
	// RateLimit is requests per hour per key, for keys that carry no limit of
	// their own.
	RateLimit int

	// WebhookTimeout bounds one delivery attempt. It is the only place in this
	// system where we wait on somebody else's server, which is why it is short
	// and why deliveries never happen on a request path.
	WebhookTimeout time.Duration

	// WebhookPoll is the idle interval of the delivery worker.
	WebhookPoll time.Duration

	// MCPKey is the API key `feasible mcp` runs its stdio session as. It is an
	// environment variable rather than a flag because a desktop assistant
	// launches the binary itself and can set one, where a secret passed on the
	// command line is visible in the process list to every user on the machine.
	MCPKey string
}

// Litestream holds the values the `litestream` command reads. It is its own
// section because no serving process reads any of it: replication is a daemon
// beside us, and this binary's only part in it is writing the file that daemon
// reads.
type Litestream struct {
	// ConfigPath is the file to generate.
	ConfigPath string

	// ReplicaURL is the prefix every database is replicated under. Empty means
	// replication is not configured, which is normal for a self-hoster and an
	// error for a hosted storage group.
	ReplicaURL string

	SyncInterval     time.Duration
	SnapshotInterval time.Duration
	Retention        time.Duration

	// WatchInterval is how often `litestream config -watch` re-reads the
	// account directory.
	WatchInterval time.Duration

	// OnChange is the shell command run after the file changes, which is how
	// the daemon picks up a newly created account database. It is empty by
	// default because the right command depends on how Litestream is
	// supervised, and guessing would produce a watcher that silently reloads
	// nothing.
	OnChange string
}

// Config is the whole configuration for the binary. Both sections are always
// loaded, even in single-process mode, because `serve` with the direct
// transport runs the ingest path in-process.
type Config struct {
	Shared     Shared
	App        App
	API        API
	Ingest     Ingest
	Litestream Litestream
}

// IsProduction reports whether this process is running in production, which is
// the one environment where we refuse to read a .env file and default to
// machine-readable logs.
func (c *Config) IsProduction() bool {
	return c.Shared.Env == EnvProduction
}

// Loader resolves one variable name through the three places a value can come
// from. It exists as a type rather than a package-level function so tests can
// point it at a temporary directory instead of the real process environment.
type Loader struct {
	// configDir is the Docker/systemd secrets directory. A file named after the
	// variable wins over the environment, which is the whole of our secrets
	// support: an orchestrator mounts a file, nothing else changes.
	configDir string

	// dotenv holds values parsed from a .env file. It is the lowest priority
	// layer so that an explicit export in the shell always overrides the file.
	dotenv map[string]string
}

// NewLoader builds a loader for the given secrets directory and .env file. The
// .env file is optional and a missing one is not an error, because production
// deploys have no such file and must not fail because of it.
func NewLoader(configDir, dotenvPath string) (*Loader, error) {
	l := &Loader{configDir: configDir, dotenv: map[string]string{}}

	if dotenvPath == "" {
		return l, nil
	}

	raw, err := os.ReadFile(dotenvPath)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dotenvPath, err)
	}

	values, err := ParseDotenv(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", dotenvPath, err)
	}
	l.dotenv = values

	return l, nil
}

// ParseDotenv turns the contents of a .env file into a map. We parse it
// ourselves rather than taking a dependency, because the format we need is
// small and a project whose pitch is "one binary" should not grow a module for
// forty lines of string handling.
func ParseDotenv(body string) (map[string]string, error) {
	out := map[string]string{}

	for i, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// "export FOO=bar" is common in hand-written files, and rejecting it
		// would be a pointless papercut.
		line = strings.TrimPrefix(line, "export ")

		name, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("line %d: expected NAME=value", i+1)
		}

		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("line %d: empty variable name", i+1)
		}

		out[name] = unquote(strings.TrimSpace(value))
	}

	return out, nil
}

// unquote strips one layer of matching quotes from a .env value. Values such as
// the internal key JSON contain characters people instinctively quote, and
// keeping the quotes would silently produce an unparseable value.
func unquote(value string) string {
	if len(value) < 2 {
		return value
	}

	first, last := value[0], value[len(value)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return value[1 : len(value)-1]
	}

	return value
}

// lookup resolves a variable, preferring a file in the secrets directory over
// the environment over the .env file. Reading the file first is what lets a
// container mount a secret at /run/secrets/NAME without the app knowing the
// difference.
func (l *Loader) lookup(name string) (string, bool) {
	if l.configDir != "" {
		raw, err := os.ReadFile(filepath.Join(l.configDir, name))
		if err == nil {
			// Trailing newlines are near-universal in mounted secret files and
			// would otherwise end up inside HMAC keys and URLs.
			return strings.TrimSpace(string(raw)), true
		}
	}

	if value, ok := os.LookupEnv(name); ok {
		return value, true
	}

	if value, ok := l.dotenv[name]; ok {
		return value, true
	}

	return "", false
}

// String returns the value of a variable or the supplied fallback. An empty
// value counts as absent so that blanking a variable in a deploy template gives
// the default rather than an empty listen address.
func (l *Loader) String(name, fallback string) string {
	if value, ok := l.lookup(name); ok && value != "" {
		return value
	}

	return fallback
}

// Bool reads a boolean variable using Go's own truthiness rules, so 1/true/TRUE
// all work. An unparseable value is treated as the fallback rather than an
// error, because a typo in a debug flag should never stop a process booting.
func (l *Loader) Bool(name string, fallback bool) bool {
	value, ok := l.lookup(name)
	if !ok || value == "" {
		return fallback
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}

	return parsed
}

// Int reads a whole-number variable.
//
// An unparseable or non-positive value falls back to the default rather than
// failing the boot. Every number read through this is a limit, an interval or a
// port, and a typo in one must not stop a process starting — a rate limit of
// zero would lock every customer out of the API it exists to protect, and a
// port of zero would bind the mail relay to nothing.
func (l *Loader) Int(name string, fallback int) int {
	value, ok := l.lookup(name)
	if !ok || value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}

	return parsed
}

// Load reads the whole configuration. The .env file is only consulted outside
// production: a stray .env on a production box silently overriding the real
// deployment configuration is a failure mode worth designing out.
func Load() (*Config, error) {
	configDir := os.Getenv("CONFIG_DIR")

	// The environment has to be resolved before we know whether to read .env,
	// so this first loader deliberately has no dotenv layer.
	bootstrap, err := NewLoader(configDir, "")
	if err != nil {
		return nil, err
	}

	env := bootstrap.String("FEASIBLE_ENV", DefaultEnv)

	dotenvPath := ".env"
	if env == EnvProduction {
		dotenvPath = ""
	}

	loader, err := NewLoader(configDir, dotenvPath)
	if err != nil {
		return nil, err
	}

	return LoadFrom(loader)
}

// LoadFrom builds the config from an already-constructed loader. Splitting it
// out keeps Load free of test hooks while letting tests drive the same parsing
// and validation code with a temporary secrets directory.
func LoadFrom(l *Loader) (*Config, error) {
	env := l.String("FEASIBLE_ENV", DefaultEnv)

	// Production wants machine-readable logs at info; a laptop wants readable
	// logs at debug. Deriving both from the environment means a developer never
	// has to set them, and a production box never accidentally ships text logs.
	defaultLevel, defaultFormat := "debug", "text"
	if env == EnvProduction {
		defaultLevel, defaultFormat = "info", "json"
	}

	cfg := &Config{
		Shared: Shared{
			Env:         env,
			LogLevel:    strings.ToLower(l.String("FEASIBLE_LOG_LEVEL", defaultLevel)),
			LogFormat:   strings.ToLower(l.String("FEASIBLE_LOG_FORMAT", defaultFormat)),
			TraceEvents: l.Bool("FEASIBLE_TRACE_EVENTS", false),
			SaltKey:     strings.TrimSpace(l.String("FEASIBLE_SALT_KEY", "")),
		},
		App: App{
			Listen:         l.String("FEASIBLE_APP_LISTEN", DefaultAppListen),
			InternalListen: l.String("FEASIBLE_APP_INTERNAL_LISTEN", DefaultAppInternalListen),
			DataDir:        l.String("FEASIBLE_APP_DATA_DIR", DefaultAppDataDir),
			BaseURL:        strings.TrimRight(l.String("FEASIBLE_APP_BASE_URL", DefaultAppBaseURL), "/"),
			Transport:      strings.ToLower(l.String("FEASIBLE_APP_TRANSPORT", DefaultAppTransport)),
			MailTransport:  strings.ToLower(l.String("FEASIBLE_APP_MAIL_TRANSPORT", DefaultAppMailTransport)),
			MailFrom:       l.String("FEASIBLE_APP_MAIL_FROM", DefaultAppMailFrom),
			SalesEmail:     l.String("FEASIBLE_APP_SALES_EMAIL", DefaultAppSalesEmail),
			SecretKey:      strings.TrimSpace(l.String("FEASIBLE_APP_SECRET_KEY", "")),
			SMTP: SMTP{
				Host:     strings.TrimSpace(l.String("FEASIBLE_SMTP_HOST", "")),
				Port:     l.Int("FEASIBLE_SMTP_PORT", DefaultSMTPPort),
				Username: l.String("FEASIBLE_SMTP_USERNAME", ""),
				Password: l.String("FEASIBLE_SMTP_PASSWORD", ""),
				StartTLS: l.Bool("FEASIBLE_SMTP_STARTTLS", true),
			},
			Google: GoogleOAuth{
				ClientID:     strings.TrimSpace(l.String("FEASIBLE_GOOGLE_CLIENT_ID", "")),
				ClientSecret: strings.TrimSpace(l.String("FEASIBLE_GOOGLE_CLIENT_SECRET", "")),
			},
			Stripe: Stripe{
				SecretKey:      strings.TrimSpace(l.String("FEASIBLE_STRIPE_SECRET_KEY", "")),
				PublishableKey: strings.TrimSpace(l.String("FEASIBLE_STRIPE_PUBLISHABLE_KEY", "")),
				Product:        strings.TrimSpace(l.String("FEASIBLE_STRIPE_PRODUCT", "")),
				PriceMonthly:   strings.TrimSpace(l.String("FEASIBLE_STRIPE_PRICE_MONTHLY", "")),
				PriceYearly:    strings.TrimSpace(l.String("FEASIBLE_STRIPE_PRICE_YEARLY", "")),
				WebhookSecret:  strings.TrimSpace(l.String("FEASIBLE_STRIPE_WEBHOOK_SECRET", "")),
			},
		},
		API: API{
			RateLimit:      l.Int("FEASIBLE_API_RATE_LIMIT", DefaultAPIRateLimit),
			WebhookTimeout: time.Duration(l.Int("FEASIBLE_WEBHOOK_TIMEOUT_SECONDS", DefaultWebhookTimeout)) * time.Second,
			WebhookPoll:    time.Duration(l.Int("FEASIBLE_WEBHOOK_POLL_SECONDS", DefaultWebhookPoll)) * time.Second,
			MCPKey:         strings.TrimSpace(l.String("FEASIBLE_MCP_API_KEY", "")),
		},
		Litestream: Litestream{
			ConfigPath:       l.String("FEASIBLE_LITESTREAM_CONFIG", DefaultLitestreamConfig),
			ReplicaURL:       strings.TrimSpace(l.String("FEASIBLE_LITESTREAM_REPLICA_URL", "")),
			SyncInterval:     time.Duration(l.Int("FEASIBLE_LITESTREAM_SYNC_SECONDS", DefaultLitestreamSync)) * time.Second,
			SnapshotInterval: time.Duration(l.Int("FEASIBLE_LITESTREAM_SNAPSHOT_HOURS", DefaultLitestreamSnapshot)) * time.Hour,
			Retention:        time.Duration(l.Int("FEASIBLE_LITESTREAM_RETENTION_HOURS", DefaultLitestreamRetention)) * time.Hour,
			WatchInterval:    time.Duration(l.Int("FEASIBLE_LITESTREAM_WATCH_SECONDS", DefaultLitestreamWatch)) * time.Second,
			OnChange:         strings.TrimSpace(l.String("FEASIBLE_LITESTREAM_ON_CHANGE", "")),
		},
		Ingest: Ingest{
			Listen:         l.String("FEASIBLE_INGEST_LISTEN", DefaultIngestListen),
			InternalListen: l.String("FEASIBLE_INGEST_INTERNAL_LISTEN", DefaultIngestInternalListen),
			BufferPath:     l.String("FEASIBLE_INGEST_BUFFER_PATH", DefaultIngestBufferPath),
			TrustedProxies: parseList(l.String("FEASIBLE_INGEST_TRUSTED_PROXIES", "")),
		},
	}

	keys, err := parseInternalKeys(l.String("FEASIBLE_INTERNAL_KEYS", ""))
	if err != nil {
		return nil, err
	}
	cfg.Shared.InternalKeys = keys

	shards, err := parseShards(l.String("FEASIBLE_INGEST_SHARDS", DefaultIngestShards))
	if err != nil {
		return nil, err
	}
	cfg.Ingest.Shards = shards

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// parseInternalKeys decodes the signing keys. They are carried as JSON rather
// than a delimited string because a key list has to hold two fields per entry
// and survive rotation, and inventing a second mini-format for that is how you
// end up unable to put a comma in a secret.
func parseInternalKeys(raw string) ([]InternalKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var keys []InternalKey
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil, fmt.Errorf("FEASIBLE_INTERNAL_KEYS: not valid JSON: %w", err)
	}

	for i, key := range keys {
		if key.ID == "" || key.Secret == "" {
			return nil, fmt.Errorf("FEASIBLE_INTERNAL_KEYS: entry %d needs both an id and a secret", i)
		}
	}

	return keys, nil
}

// parseShards validates the retired compatibility list so an existing deploy
// does not silently accept malformed configuration during migration.
func parseShards(raw string) ([]string, error) {
	var out []string

	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		parsed, err := url.Parse(part)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("FEASIBLE_INGEST_SHARDS: %q is not an absolute URL", part)
		}

		out = append(out, strings.TrimRight(part, "/"))
	}

	return out, nil
}

// parseList splits a comma-separated variable into its entries, dropping the
// blanks. It exists because an operator writing a list by hand leaves trailing
// commas and spaces, and a stray empty entry in the trusted-proxy list would be
// an allow-list entry that matches nothing and looks like it matches something.
func parseList(raw string) []string {
	var out []string

	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

// Validate rejects values that would otherwise fail much later and much less
// clearly — a misspelled transport that silently drops every event, or a base
// URL without a scheme that produces links nobody can click.
func (c *Config) Validate() error {
	switch c.Shared.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("FEASIBLE_LOG_FORMAT: %q is not text or json", c.Shared.LogFormat)
	}

	switch c.Shared.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("FEASIBLE_LOG_LEVEL: %q is not debug, info, warn or error", c.Shared.LogLevel)
	}

	switch c.App.Transport {
	case TransportDirect, TransportHTTP:
	default:
		return fmt.Errorf("FEASIBLE_APP_TRANSPORT: %q is not direct or http", c.App.Transport)
	}

	switch c.App.MailTransport {
	case MailTransportLog, MailTransportSMTP:
	default:
		return fmt.Errorf("FEASIBLE_APP_MAIL_TRANSPORT: %q is not log or smtp", c.App.MailTransport)
	}

	// A relay with no host cannot send anything, and finding that out at the
	// moment a deletion warning is due is far too late.
	if c.App.MailTransport == MailTransportSMTP && c.App.SMTP.Host == "" {
		return fmt.Errorf("FEASIBLE_APP_MAIL_TRANSPORT is smtp but FEASIBLE_SMTP_HOST is empty")
	}

	// Any billing value opts into hosted billing, and hosted billing is usable
	// only as one complete unit. In particular, accepting checkout without the
	// signing secret charges a customer while rejecting the fulfillment event.
	stripeValues := []string{
		c.App.Stripe.SecretKey,
		c.App.Stripe.Product,
		c.App.Stripe.PriceMonthly,
		c.App.Stripe.PriceYearly,
		c.App.Stripe.WebhookSecret,
	}
	stripeConfigured := false
	stripeComplete := true
	for _, value := range stripeValues {
		stripeConfigured = stripeConfigured || value != ""
		stripeComplete = stripeComplete && value != ""
	}
	if stripeConfigured && !stripeComplete {
		return fmt.Errorf("Stripe billing requires FEASIBLE_STRIPE_SECRET_KEY, _PRODUCT, _PRICE_MONTHLY, _PRICE_YEARLY and _WEBHOOK_SECRET together")
	}

	if c.Shared.SaltKey != "" && len(c.Shared.SaltKey) != 64 {
		return fmt.Errorf("FEASIBLE_SALT_KEY: expected 64 hex characters, got %d", len(c.Shared.SaltKey))
	}

	if c.App.SecretKey != "" && len(c.App.SecretKey) != 64 {
		return fmt.Errorf("FEASIBLE_APP_SECRET_KEY: expected 64 hex characters, got %d", len(c.App.SecretKey))
	}

	// A half-configured OAuth client produces a button that starts a flow it
	// cannot finish, which is worse than no button at all.
	if (c.App.Google.ClientID == "") != (c.App.Google.ClientSecret == "") {
		return fmt.Errorf("FEASIBLE_GOOGLE_CLIENT_ID and FEASIBLE_GOOGLE_CLIENT_SECRET must be set together or not at all")
	}

	if c.App.MailTransport == MailTransportSMTP && c.App.SMTP.Host == "" {
		return fmt.Errorf("FEASIBLE_SMTP_HOST is required when FEASIBLE_APP_MAIL_TRANSPORT is smtp")
	}

	base, err := url.Parse(c.App.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return fmt.Errorf("FEASIBLE_APP_BASE_URL: %q is not an absolute URL", c.App.BaseURL)
	}

	return nil
}
