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
)

// Defaults are the values used when a variable is absent. They are deliberately
// the safe, single-machine, self-hoster values: someone who runs the binary with
// no configuration at all gets a working process bound to loopback.
const (
	DefaultEnv               = "development"
	DefaultAppListen         = "127.0.0.1:19301"
	DefaultAppInternalListen = "127.0.0.1:19401"
	DefaultAppDataDir        = "./data"
	DefaultAppBaseURL        = "http://localhost:19300"
	DefaultAppTransport      = TransportDirect
	DefaultAppMailTransport  = MailTransportLog
	DefaultIngestListen      = "127.0.0.1:19302"
	DefaultIngestShards      = "http://127.0.0.1:19401"
	DefaultIngestBufferPath  = "./data/ingest/buffer.db"
)

// Layout of the data directory. These are constants because both the migrate
// and backup commands have to agree on where a database lives, and a mismatch
// would silently skip every account.
const (
	ControlDatabaseName = "control.db"
	AccountDatabaseDir  = "accounts"
)

// Transport names how the app process receives events. Direct means one process
// does everything (the self-hoster path); http means a separate ingest tier
// forwards over the network (the production shape).
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

// InternalKey is one shared secret used to sign requests between the ingest tier
// and a shard. Keys carry an id so that a rotation can add the new key
// everywhere before any signer starts using it.
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
}

// App holds the values only the `serve` process reads.
type App struct {
	Listen         string
	InternalListen string
	DataDir        string
	BaseURL        string
	Transport      string
	MailTransport  string
}

// Ingest holds the values only the `ingest` process reads.
type Ingest struct {
	Listen     string
	Shards     []string
	BufferPath string
}

// Config is the whole configuration for the binary. Both sections are always
// loaded, even in single-process mode, because `serve` with the direct
// transport runs the ingest path in-process.
type Config struct {
	Shared Shared
	App    App
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
		},
		App: App{
			Listen:         l.String("FEASIBLE_APP_LISTEN", DefaultAppListen),
			InternalListen: l.String("FEASIBLE_APP_INTERNAL_LISTEN", DefaultAppInternalListen),
			DataDir:        l.String("FEASIBLE_APP_DATA_DIR", DefaultAppDataDir),
			BaseURL:        strings.TrimRight(l.String("FEASIBLE_APP_BASE_URL", DefaultAppBaseURL), "/"),
			Transport:      strings.ToLower(l.String("FEASIBLE_APP_TRANSPORT", DefaultAppTransport)),
			MailTransport:  strings.ToLower(l.String("FEASIBLE_APP_MAIL_TRANSPORT", DefaultAppMailTransport)),
		},
		Ingest: Ingest{
			Listen:     l.String("FEASIBLE_INGEST_LISTEN", DefaultIngestListen),
			BufferPath: l.String("FEASIBLE_INGEST_BUFFER_PATH", DefaultIngestBufferPath),
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

// parseShards splits the shard list and checks each entry is an absolute URL.
// A shard address that is missing its scheme fails at the first forward attempt
// with an unhelpful error, hours after boot; catching it here costs nothing.
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

	base, err := url.Parse(c.App.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return fmt.Errorf("FEASIBLE_APP_BASE_URL: %q is not an absolute URL", c.App.BaseURL)
	}

	return nil
}
