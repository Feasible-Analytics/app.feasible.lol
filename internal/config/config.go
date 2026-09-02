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
	"sort"
	"strconv"
	"strings"
	"time"
)

// Defaults are the values used when a variable is absent. They are deliberately
// the safe, single-machine, self-hoster values: someone who runs the binary with
// no configuration at all gets a working process bound to loopback.
const (
	DefaultEnv              = "development"
	DefaultAppListen        = "127.0.0.1:19301"
	DefaultAppDataDir       = "./data"
	DefaultAppBaseURL       = "http://localhost:19300"
	DefaultAppTransport     = TransportDirect
	DefaultAppMailTransport = MailTransportLog
	DefaultAppMailFrom      = "feasible.lol <hello@feasible.lol>"
	DefaultAppSalesEmail    = "sales@feasible.lol"
	DefaultSMTPPort         = 587
	DefaultIngestListen     = "127.0.0.1:19302"
	DefaultIngestShards     = `["http://127.0.0.1:19301"]`
	DefaultIngestBufferPath = "./data/ingest/buffer.db"
	DefaultIngestSalt       = "dev-only-shared-salt-change-me"
	DefaultAppShardID       = 1

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

	// DefaultQuerySampleThreshold leaves the sampling ceiling to the query
	// engine, which is the one place the number is written down. Zero is the
	// sentinel for that rather than a copy of the figure: two constants that
	// have to agree are two constants that eventually do not.
	DefaultQuerySampleThreshold = 0
)

// Layout of the data directory. These are constants because both the migrate
// and backup commands have to agree on where a database lives, and a mismatch
// would silently skip every account.
const (
	SystemDatabaseName = "system.db"
	AccountDatabaseDir = "accounts"

	// LegacyDatabaseName is the filename used before the system database was
	// named for what it contains. Maintenance commands use it only to perform a
	// one-time, explicit layout upgrade; serving processes never open it.
	LegacyDatabaseName = "control.db"
)

// Transport names which public process owns the event endpoint. Direct mounts
// it in the app process; http leaves it to a separate store-and-forward ingest
// process that delivers durable batches to the owning app shard.
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

// Shared holds the values both processes read. Their values must match on every
// machine in a deployment, which is why they live in their own section of
// .env.sample rather than being duplicated per app.
type Shared struct {
	Env         string
	LogLevel    string
	LogFormat   string
	InternalKey string
	TraceEvents bool

	// IngestSalt is shared by every ingest process. Each process combines it
	// with the UTC day locally, so daily visitor identifiers agree without an
	// app-side salt authority or network request.
	IngestSalt string
}

// App holds the values only the `serve` process reads.
type App struct {
	Listen        string
	DataDir       string
	BaseURL       string
	Transport     string
	MailTransport string
	Hosted        bool
	// ShardID is this app's one-based stable position in every ingester's
	// ordered FEASIBLE_INGEST_SHARDS list.
	ShardID int

	// MailFrom is the envelope sender on every message the product sends. A
	// relay rejects a From it does not own, and that rejection is the most
	// common reason a self-hoster's mail stops arriving with nothing in our
	// logs to explain it.
	MailFrom string

	// SalesEmail is where the volume ladder points a growing customer. It is
	// configurable because a self-hoster's "talk to us" address is not ours.
	SalesEmail string

	// Operator identifies the legal entity responsible for a self-hosted
	// deployment. Hosted pages continue to identify Cloudmanic explicitly.
	OperatorName    string
	OperatorAddress string
	OperatorEmail   string

	// SecretKey encrypts the two-factor secrets and signs the short-lived
	// cookies, as 32 hex-encoded bytes. Empty means one is generated under the
	// data directory on first run, so encryption at rest is true by default
	// rather than only when somebody remembered to set a variable.
	SecretKey string

	// Worker decides whether this process drains the background queue. It is a
	// switch rather than always-on because a deployment with several app
	// replicas wants the scheduled reports and alerts running somewhere
	// specific, and because a developer poking at the dashboard should not have
	// a customer's report email going out from their laptop.
	Worker bool

	SMTP   SMTP
	Google GoogleOAuth
	Stripe Stripe

	// Subprocessors is the deployment's public legal inventory. Source code
	// cannot know which infrastructure a hosted operator chose, so production
	// hosted mode requires these facts explicitly instead of publishing category
	// placeholders as though they were legal entities.
	Subprocessors []Subprocessor
}

// Subprocessor is one legal entity that can process hosted-service data.
type Subprocessor struct {
	Role        string `json:"role"`
	LegalEntity string `json:"legal_entity"`
	Service     string `json:"service"`
	Data        string `json:"data"`
	Region      string `json:"region"`
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

	Shards     []string
	BufferPath string

	// TrustedProxies may supply client-address headers. Empty means all
	// forwarded headers are ignored so a direct client cannot forge its
	// fingerprint, geolocation or shield address.
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

	// QuerySampleThreshold is the repeated event and session fact-row estimate
	// above which a report is answered from deterministic fact-row buckets and
	// labelled as sampled. Zero takes the query engine's own default and a
	// negative value turns automatic sampling off, for an operator who would
	// rather wait than estimate.
	//
	// It applies to the dashboard and the public API alike, because two
	// thresholds would mean two answers to one question.
	QuerySampleThreshold int64
}

// Config is the whole configuration for the binary. Both sections are always
// loaded, even in single-process mode, because `serve` with the direct
// transport runs the ingest path in-process.
type Config struct {
	Shared Shared
	App    App
	API    API
	Ingest Ingest
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

// Bool reads a boolean variable using Go's truthiness rules. A malformed value
// fails startup: silently turning a mistyped hosted or TLS flag into its
// fallback changes security and legal behavior while the process looks healthy.
func (l *Loader) Bool(name string, fallback bool) (bool, error) {
	value, ok := l.lookup(name)
	if !ok || value == "" {
		return fallback, nil
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s: %q is not a boolean", name, value)
	}

	return parsed, nil
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
	if env != EnvDevelopment && env != EnvProduction {
		return nil, fmt.Errorf("FEASIBLE_ENV: %q is not development or production", env)
	}

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
	traceEvents, err := l.Bool("FEASIBLE_TRACE_EVENTS", false)
	if err != nil {
		return nil, err
	}
	hosted, err := l.Bool("FEASIBLE_APP_HOSTED", true)
	if err != nil {
		return nil, err
	}
	worker, err := l.Bool("FEASIBLE_APP_WORKER", true)
	if err != nil {
		return nil, err
	}
	startTLS, err := l.Bool("FEASIBLE_SMTP_STARTTLS", true)
	if err != nil {
		return nil, err
	}

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
			TraceEvents: traceEvents,
			InternalKey: strings.TrimSpace(l.String("FEASIBLE_INTERNAL_KEY", "")),
			IngestSalt:  l.String("FEASIBLE_INGEST_SALT", DefaultIngestSalt),
		},
		App: App{
			Listen:          l.String("FEASIBLE_APP_LISTEN", DefaultAppListen),
			DataDir:         l.String("FEASIBLE_APP_DATA_DIR", DefaultAppDataDir),
			BaseURL:         strings.TrimRight(l.String("FEASIBLE_APP_BASE_URL", DefaultAppBaseURL), "/"),
			Transport:       strings.ToLower(l.String("FEASIBLE_APP_TRANSPORT", DefaultAppTransport)),
			MailTransport:   strings.ToLower(l.String("FEASIBLE_APP_MAIL_TRANSPORT", DefaultAppMailTransport)),
			Hosted:          hosted,
			ShardID:         l.Int("FEASIBLE_APP_SHARD_ID", DefaultAppShardID),
			MailFrom:        l.String("FEASIBLE_APP_MAIL_FROM", DefaultAppMailFrom),
			SalesEmail:      l.String("FEASIBLE_APP_SALES_EMAIL", DefaultAppSalesEmail),
			OperatorName:    strings.TrimSpace(l.String("FEASIBLE_OPERATOR_NAME", "")),
			OperatorAddress: strings.TrimSpace(l.String("FEASIBLE_OPERATOR_ADDRESS", "")),
			OperatorEmail:   strings.TrimSpace(l.String("FEASIBLE_OPERATOR_EMAIL", "")),
			SecretKey:       strings.TrimSpace(l.String("FEASIBLE_APP_SECRET_KEY", "")),
			Worker:          worker,
			SMTP: SMTP{
				Host:     strings.TrimSpace(l.String("FEASIBLE_SMTP_HOST", "")),
				Port:     l.Int("FEASIBLE_SMTP_PORT", DefaultSMTPPort),
				Username: l.String("FEASIBLE_SMTP_USERNAME", ""),
				Password: l.String("FEASIBLE_SMTP_PASSWORD", ""),
				StartTLS: startTLS,
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

			QuerySampleThreshold: int64(l.Int("FEASIBLE_QUERY_SAMPLE_THRESHOLD", DefaultQuerySampleThreshold)),
		},
		Ingest: Ingest{
			Listen:         l.String("FEASIBLE_INGEST_LISTEN", DefaultIngestListen),
			BufferPath:     l.String("FEASIBLE_INGEST_BUFFER_PATH", DefaultIngestBufferPath),
			TrustedProxies: parseList(l.String("FEASIBLE_INGEST_TRUSTED_PROXIES", "")),
		},
	}

	subprocessors, err := parseSubprocessors(l.String("FEASIBLE_HOSTED_SUBPROCESSORS_JSON", ""))
	if err != nil {
		return nil, err
	}
	cfg.App.Subprocessors = subprocessors

	shards, err := parseShards(l.String("FEASIBLE_INGEST_SHARDS", DefaultIngestShards))
	if err != nil {
		return nil, err
	}
	cfg.Ingest.Shards = shards
	if !cfg.App.Hosted && cfg.Shared.Env == EnvDevelopment {
		if cfg.App.OperatorName == "" {
			cfg.App.OperatorName = "Operator of " + cfg.App.BaseURL
		}
		if cfg.App.OperatorAddress == "" {
			cfg.App.OperatorAddress = cfg.App.BaseURL
		}
		if cfg.App.OperatorEmail == "" {
			cfg.App.OperatorEmail = cfg.App.SalesEmail
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// parseSubprocessors decodes the public hosted-provider inventory. JSON keeps
// the five fields in each legal disclosure together and avoids a parallel set
// of numbered environment variables that can drift across deployments.
func parseSubprocessors(raw string) ([]Subprocessor, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	var subprocessors []Subprocessor
	if err := json.Unmarshal([]byte(raw), &subprocessors); err != nil {
		return nil, fmt.Errorf("FEASIBLE_HOSTED_SUBPROCESSORS_JSON: not valid JSON: %w", err)
	}

	for i, entry := range subprocessors {
		if strings.TrimSpace(entry.Role) == "" || strings.TrimSpace(entry.LegalEntity) == "" ||
			strings.TrimSpace(entry.Service) == "" || strings.TrimSpace(entry.Data) == "" || strings.TrimSpace(entry.Region) == "" {
			return nil, fmt.Errorf("FEASIBLE_HOSTED_SUBPROCESSORS_JSON: entry %d needs role, legal_entity, service, data and region", i)
		}
	}

	return subprocessors, nil
}

// parseShards validates the complete, ordered JSON array of app-shard URLs.
// Completeness is what lets an ingester distinguish an unknown domain from a
// domain hidden behind an unavailable shard, while explicit JSON preserves the
// order that defines each shard's stable identity.
func parseShards(raw string) ([]string, error) {
	var entries []string
	if strings.TrimSpace(raw) == "" || !strings.HasPrefix(strings.TrimSpace(raw), "[") {
		return nil, fmt.Errorf("FEASIBLE_INGEST_SHARDS: expected a JSON array of absolute URLs")
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("FEASIBLE_INGEST_SHARDS: expected a JSON array of absolute URLs: %w", err)
	}

	out := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, part := range entries {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("FEASIBLE_INGEST_SHARDS: entries cannot be empty")
		}

		parsed, err := url.Parse(part)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("FEASIBLE_INGEST_SHARDS: %q is not an absolute URL", part)
		}

		part = strings.TrimRight(part, "/")
		if _, exists := seen[part]; exists {
			return nil, fmt.Errorf("FEASIBLE_INGEST_SHARDS: duplicate shard URL %q", part)
		}
		seen[part] = struct{}{}
		out = append(out, part)
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
	switch c.Shared.Env {
	case EnvDevelopment, EnvProduction:
	default:
		return fmt.Errorf("FEASIBLE_ENV: %q is not development or production", c.Shared.Env)
	}

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
	if c.App.Transport == TransportHTTP {
		if c.Shared.InternalKey == "" {
			return fmt.Errorf("FEASIBLE_INTERNAL_KEY: http transport requires a signing key")
		}
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

	if c.App.Hosted {
		// Any billing value opts a hosted deployment into Stripe, and hosted
		// billing is usable only as one complete unit. Self-hosted deployments
		// ignore these variables because billing is disabled in that mode.
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
			return fmt.Errorf("Stripe billing requires FEASIBLE_STRIPE_SECRET_KEY, FEASIBLE_STRIPE_PRODUCT, FEASIBLE_STRIPE_PRICE_MONTHLY, FEASIBLE_STRIPE_PRICE_YEARLY and FEASIBLE_STRIPE_WEBHOOK_SECRET together")
		}
	}

	if c.IsProduction() && c.App.Hosted {
		if c.App.MailTransport == MailTransportLog {
			return fmt.Errorf("FEASIBLE_APP_MAIL_TRANSPORT: hosted production cannot use log-only mail")
		}
		required := []string{"compute", "object_storage", "email"}
		if c.App.Stripe.Enabled() {
			required = append(required, "billing")
		}
		present := make(map[string]bool, len(required))

		for _, entry := range c.App.Subprocessors {
			role := strings.ToLower(strings.TrimSpace(entry.Role))
			for _, requiredRole := range required {
				if role == requiredRole {
					present[role] = true
				}
			}

			text := strings.ToLower(entry.LegalEntity + " " + entry.Service + " " + entry.Data + " " + entry.Region)
			if strings.Contains(text, "placeholder") || strings.Contains(text, "non-production") || strings.Contains(text, "example.invalid") {
				return fmt.Errorf("FEASIBLE_HOSTED_SUBPROCESSORS_JSON: production entry %q is still a sample placeholder", entry.Role)
			}
		}

		for _, role := range required {
			if !present[role] {
				return fmt.Errorf("FEASIBLE_HOSTED_SUBPROCESSORS_JSON: hosted production requires a %s entry", role)
			}
		}

	}
	if c.IsProduction() && !c.App.Hosted {
		operatorFields := map[string]string{
			"FEASIBLE_OPERATOR_NAME":    c.App.OperatorName,
			"FEASIBLE_OPERATOR_ADDRESS": c.App.OperatorAddress,
			"FEASIBLE_OPERATOR_EMAIL":   c.App.OperatorEmail,
		}
		var missing []string
		for name, value := range operatorFields {
			if strings.TrimSpace(value) == "" {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return fmt.Errorf("self-hosted production operator identity is incomplete; missing %s", strings.Join(missing, ", "))
		}
	}
	if strings.TrimSpace(c.Shared.IngestSalt) == "" {
		return fmt.Errorf("FEASIBLE_INGEST_SALT cannot be empty")
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
	if c.IsProduction() && c.App.Hosted && c.App.Transport == TransportHTTP {
		for _, endpoint := range c.Ingest.Shards {
			if endpoint == "" {
				continue
			}
			parsed, _ := url.Parse(endpoint)
			if parsed.Scheme != "https" {
				return fmt.Errorf("private app-shard URL %q must use https in hosted production", endpoint)
			}
		}
	}

	return nil
}
