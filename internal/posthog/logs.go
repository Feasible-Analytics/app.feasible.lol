//
// logs.go
// Copying every log line to PostHog Logs over OTLP.
//
// Created: 2026-11-16
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package posthog

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// logsPath is PostHog's OTLP/HTTP logs endpoint.
const logsPath = "/i/v1/logs"

// Defaults for the exporter's batching.
const (
	DefaultFlushInterval = 2 * time.Second
	DefaultBatchSize     = 500
	DefaultQueueSize     = 10000

	// lossReportInterval is the most often the exporter reports records it
	// could not ship, so an outage is one line a minute rather than one a batch.
	lossReportInterval = time.Minute
)

// LogOptions configures a log exporter.
type LogOptions struct {
	// Token is the project token. Empty makes NewLogExporter return nil.
	Token string

	// Host is the project's ingestion origin.
	Host string

	// Service, Env and Version describe this process on every record, as the
	// OpenTelemetry resource attributes service.name, deployment.environment
	// and service.version.
	Service string
	Env     string
	Version string

	// Level is the lowest level shipped.
	Level slog.Leveler

	// Log reports records the exporter could not ship. It must not itself ship
	// to this exporter, or a failure would log about itself forever.
	Log *logger.Logger

	Client        *http.Client
	FlushInterval time.Duration
	BatchSize     int
	QueueSize     int
}

// LogExporter batches log records and posts them to PostHog. A nil exporter is
// valid and ships nothing.
type LogExporter struct {
	endpoint string
	token    string
	resource []otlpAttribute
	level    slog.Leveler
	log      *logger.Logger
	client   *http.Client
	interval time.Duration
	batch    int

	queue chan otlpRecord
	stop  chan context.Context
	done  chan struct{}
	once  sync.Once

	mu         sync.Mutex
	dropped    int
	failed     int
	lastErr    error
	lastReport time.Time
}

// NewLogExporter starts an exporter, or returns nil when there is no token.
func NewLogExporter(opts LogOptions) *LogExporter {
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		return nil
	}

	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: PostTimeout}
	}

	level := opts.Level
	if level == nil {
		level = slog.LevelInfo
	}

	interval := opts.FlushInterval
	if interval <= 0 {
		interval = DefaultFlushInterval
	}

	batch := opts.BatchSize
	if batch <= 0 {
		batch = DefaultBatchSize
	}

	queue := opts.QueueSize
	if queue <= 0 {
		queue = DefaultQueueSize
	}

	resource := []otlpAttribute{stringAttribute("service.name", opts.Service)}
	if opts.Env != "" {
		resource = append(resource, stringAttribute("deployment.environment", opts.Env))
	}
	if opts.Version != "" {
		resource = append(resource, stringAttribute("service.version", opts.Version))
	}

	e := &LogExporter{
		endpoint: strings.TrimRight(strings.TrimSpace(opts.Host), "/") + logsPath,
		token:    token,
		resource: resource,
		level:    level,
		log:      opts.Log,
		client:   client,
		interval: interval,
		batch:    batch,
		queue:    make(chan otlpRecord, queue),
		stop:     make(chan context.Context, 1),
		done:     make(chan struct{}),
	}

	go e.run()

	return e
}

// Handler is the slog handler that feeds this exporter. It never blocks: a
// record that finds the queue full is counted and reported, not waited on.
func (e *LogExporter) Handler() slog.Handler {
	if e == nil {
		return nil
	}

	return &logHandler{exporter: e}
}

// Close ships what is queued and stops. It returns when the last batch is sent
// or ctx ends, whichever is first.
func (e *LogExporter) Close(ctx context.Context) error {
	if e == nil {
		return nil
	}

	e.once.Do(func() { e.stop <- ctx })

	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is the exporter's one goroutine. It sends a batch when it is full or when
// the interval passes, and on Close drains the queue before returning.
func (e *LogExporter) run() {
	defer close(e.done)

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	pending := make([]otlpRecord, 0, e.batch)

	for {
		select {
		case record := <-e.queue:
			pending = append(pending, record)
			if len(pending) >= e.batch {
				e.send(context.Background(), pending)
				pending = pending[:0]
			}

		case <-ticker.C:
			if len(pending) > 0 {
				e.send(context.Background(), pending)
				pending = pending[:0]
			}
			e.reportLosses(false)

		case ctx := <-e.stop:
			for drained := false; !drained; {
				select {
				case record := <-e.queue:
					pending = append(pending, record)
					if len(pending) >= e.batch {
						e.send(ctx, pending)
						pending = pending[:0]
					}
				default:
					drained = true
				}
			}
			if len(pending) > 0 {
				e.send(ctx, pending)
			}
			e.reportLosses(true)

			return
		}
	}
}

// enqueue hands one record to the exporter's goroutine.
func (e *LogExporter) enqueue(record otlpRecord) {
	select {
	case e.queue <- record:
	default:
		e.mu.Lock()
		e.dropped++
		e.mu.Unlock()
	}
}

// send posts one batch and counts it as lost if PostHog does not accept it.
func (e *LogExporter) send(parent context.Context, records []otlpRecord) {
	ctx, cancel := context.WithTimeout(parent, PostTimeout)
	defer cancel()

	if err := e.post(ctx, records); err != nil {
		e.mu.Lock()
		e.failed += len(records)
		e.lastErr = err
		e.mu.Unlock()
	}
}

// reportLosses writes one line about records that never reached PostHog. The
// local log still has every one of them.
func (e *LogExporter) reportLosses(final bool) {
	e.mu.Lock()
	if e.dropped == 0 && e.failed == 0 {
		e.mu.Unlock()
		return
	}
	if !final && time.Since(e.lastReport) < lossReportInterval {
		e.mu.Unlock()
		return
	}

	dropped, failed, lastErr := e.dropped, e.failed, e.lastErr
	e.dropped, e.failed, e.lastErr = 0, 0, nil
	e.lastReport = time.Now()
	e.mu.Unlock()

	if e.log == nil {
		return
	}

	args := []any{"queue_full", dropped, "send_failed", failed}
	if lastErr != nil {
		args = append(args, "error", lastErr)
	}
	e.log.Warn("log records not shipped to posthog", args...)
}

// post sends one batch as gzipped OTLP JSON.
func (e *LogExporter) post(ctx context.Context, records []otlpRecord) error {
	payload := otlpRequest{ResourceLogs: []otlpResourceLogs{{
		Resource: otlpResource{Attributes: e.resource},
		ScopeLogs: []otlpScopeLogs{{
			Scope:      otlpScope{Name: "feasible"},
			LogRecords: records,
		}},
	}}}

	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	if err := json.NewEncoder(zw).Encode(payload); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer "+e.token)

	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // the status is the whole answer

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("posthog: logs answered %s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}

	return nil
}

// logHandler turns slog records into OTLP log records. Attributes from With
// and groups from WithGroup are flattened into dotted keys.
type logHandler struct {
	exporter *LogExporter
	attrs    []otlpAttribute
	prefix   string
}

// Enabled reports whether a record at level is shipped.
func (h *logHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.exporter.level.Level()
}

// Handle converts one record and queues it.
func (h *logHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make([]otlpAttribute, len(h.attrs), len(h.attrs)+r.NumAttrs())
	copy(attrs, h.attrs)

	r.Attrs(func(a slog.Attr) bool {
		attrs = appendAttr(attrs, h.prefix, a)
		return true
	})

	when := r.Time
	if when.IsZero() {
		when = time.Now()
	}

	h.exporter.enqueue(otlpRecord{
		TimeUnixNano:   strconv.FormatInt(when.UnixNano(), 10),
		SeverityNumber: severityNumber(r.Level),
		SeverityText:   r.Level.String(),
		Body:           otlpValue{StringValue: r.Message},
		Attributes:     attrs,
	})

	return nil
}

// WithAttrs returns a handler that adds attrs to every record.
func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	next := *h
	next.attrs = make([]otlpAttribute, len(h.attrs), len(h.attrs)+len(attrs))
	copy(next.attrs, h.attrs)
	for _, a := range attrs {
		next.attrs = appendAttr(next.attrs, h.prefix, a)
	}

	return &next
}

// WithGroup returns a handler that nests later attributes under name.
func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	next := *h
	next.prefix = h.prefix + name + "."

	return &next
}

// appendAttr flattens one slog attribute onto out. Every value is sent as a
// string, which is how PostHog stores log attributes. An attribute that carries
// an IP address is left out; see carriesAddress.
func appendAttr(out []otlpAttribute, prefix string, a slog.Attr) []otlpAttribute {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return out
	}

	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		if len(group) == 0 {
			return out
		}

		inner := prefix
		if a.Key != "" {
			inner = prefix + a.Key + "."
		}
		for _, member := range group {
			out = appendAttr(out, inner, member)
		}

		return out
	}

	key, value := prefix+a.Key, valueString(a.Value)
	if carriesAddress(key, value) {
		return out
	}

	return append(out, stringAttribute(key, value))
}

// addressKeys are attribute names that hold a network address, written in the
// form normalizeKey produces. The privacy page tells people we don't keep their
// IP address, and PostHog is a third party, so an attribute under one of these
// names never leaves the box. The local log still has it.
var addressKeys = map[string]bool{
	"ip": true, "ips": true, "ipaddr": true, "ipaddress": true,
	"addr": true, "remote": true, "remoteip": true, "remoteaddr": true, "remoteaddress": true,
	"clientip": true, "clientaddr": true, "clientaddress": true,
	"visitorip": true, "sourceip": true, "srcip": true, "realip": true, "xrealip": true,
	"forwardedfor": true, "xforwardedfor": true, "cfconnectingip": true, "xfeasibleip": true,
	"peer": true, "peerip": true, "peeraddr": true,
}

// carriesAddress reports whether an attribute must stay out of the shipped
// copy. A key is checked at every level, so a group or a dotted name such as
// request.remote_addr is caught, and the value is checked too, so an address
// logged under a name nobody thought of is caught as well.
func carriesAddress(key, value string) bool {
	for segment := range strings.SplitSeq(key, ".") {
		if addressKeys[normalizeKey(segment)] {
			return true
		}
	}

	return isAddress(value)
}

// normalizeKey lowercases a key and drops underscores and hyphens, so
// remoteAddr, remote_addr and Remote-Addr are one name.
func normalizeKey(key string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' {
			return -1
		}

		return unicode.ToLower(r)
	}, key)
}

// isAddress reports whether value is an IP address, bare, bracketed or with a
// port, or a comma-separated chain with one in it, the way X-Forwarded-For
// carries them.
func isAddress(value string) bool {
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if _, err := netip.ParseAddrPort(part); err == nil {
			return true
		}
		if _, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(part, "["), "]")); err == nil {
			return true
		}
	}

	return false
}

// valueString renders a resolved slog value.
func valueString(v slog.Value) string {
	switch v.Kind() {
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok && err != nil {
			return err.Error()
		}
	}

	return v.String()
}

// severityNumber maps a slog level onto the OpenTelemetry scale, where slog's
// four levels land on DEBUG 5, INFO 9, WARN 13 and ERROR 17.
func severityNumber(level slog.Level) int {
	return min(max(int(level)+9, 1), 24)
}

// stringAttribute is one key with a string value.
func stringAttribute(key, value string) otlpAttribute {
	return otlpAttribute{Key: key, Value: otlpValue{StringValue: value}}
}

// The OTLP/JSON shapes, cut down to the fields this exporter sends.
type (
	otlpRequest struct {
		ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
	}

	otlpResourceLogs struct {
		Resource  otlpResource    `json:"resource"`
		ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
	}

	otlpResource struct {
		Attributes []otlpAttribute `json:"attributes"`
	}

	otlpScopeLogs struct {
		Scope      otlpScope    `json:"scope"`
		LogRecords []otlpRecord `json:"logRecords"`
	}

	otlpScope struct {
		Name string `json:"name"`
	}

	otlpRecord struct {
		TimeUnixNano   string          `json:"timeUnixNano"`
		SeverityNumber int             `json:"severityNumber"`
		SeverityText   string          `json:"severityText"`
		Body           otlpValue       `json:"body"`
		Attributes     []otlpAttribute `json:"attributes,omitempty"`
	}

	otlpAttribute struct {
		Key   string    `json:"key"`
		Value otlpValue `json:"value"`
	}

	otlpValue struct {
		StringValue string `json:"stringValue"`
	}
)
