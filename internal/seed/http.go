//
// http.go
// A couple of hundred events over the real wire, to prove the path works end to end.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// DefaultHTTPEvents is how many events the wire check sends. It is a couple of
// hundred because this is a different tool for a different job than the bulk
// generator: it proves the whole path works — TCP, the handler, the parser, the
// derive, the buffer, the writer — and volume proves nothing about that.
const DefaultHTTPEvents = 200

// httpTimeout bounds one request. A wire check that hangs is a wire check
// nobody can put in a Makefile.
const httpTimeout = 10 * time.Second

// HTTPOptions configure the wire check.
type HTTPOptions struct {
	// Endpoint is the base URL of a running instance — through the reverse
	// proxy if there is one, since that is the path a browser takes.
	Endpoint string

	// Domain is the site to send events for. It has to be a site the instance
	// routes, or every event comes back with unknown_site — which is itself the
	// most common real misconfiguration and is reported rather than hidden.
	Domain string

	Events int
	Seed   int64

	Out io.Writer
}

// HTTPResult is what the wire check saw.
type HTTPResult struct {
	Sent     int
	Accepted int
	Dropped  map[string]int
	Statuses map[int]int
	Duration time.Duration
}

// SendHTTP posts events to a running instance over real HTTP. Everything the
// bulk generator skips is exercised here: the listener, the content type the
// tracker actually uses, the JSON wire shape, the response contract of always
// answering 202 with the reason in a header.
func SendHTTP(ctx context.Context, opts HTTPOptions) (*HTTPResult, error) {
	if opts.Events <= 0 {
		opts.Events = DefaultHTTPEvents
	}
	if opts.Seed == 0 {
		opts.Seed = DefaultSeed
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Endpoint == "" {
		return nil, fmt.Errorf("seed http: no endpoint to send to")
	}

	endpoint := strings.TrimRight(opts.Endpoint, "/") + "/api/event"

	rng := rand.New(rand.NewPCG(uint64(opts.Seed), 0x853c49e6748fea9b))
	agents := newChooser(zipf(len(agentCatalog), agentExponent))
	langs := newChooser(zipf(len(languages), 1.4))
	pages := pageCatalog(kindMarketing)
	pageWeights := newChooser(zipf(len(pages), pageExponent))

	client := &http.Client{Timeout: httpTimeout}
	result := &HTTPResult{Dropped: map[string]int{}, Statuses: map[int]int{}}

	started := time.Now()

	for i := 0; i < opts.Events; i++ {
		person := visitorFor(uint32(rng.IntN(4096)), agents, langs)
		path := pages[pageWeights.pick(rng.Float64())]

		body, err := wireBody(opts.Domain, path, person, rng)
		if err != nil {
			return nil, err
		}

		status, dropped, err := post(ctx, client, endpoint, body, person)
		if err != nil {
			return result, err
		}

		result.Sent++
		result.Statuses[status]++

		switch {
		case dropped != "":
			result.Dropped[dropped]++
		case status == http.StatusAccepted:
			result.Accepted++
		}
	}

	result.Duration = time.Since(started)

	return result, nil
}

// wireBody builds the JSON a tracker sends. The single-letter keys are the
// established payload shape byte for byte, which is what lets somebody migrate
// by changing one hostname — so the wire check has to send exactly that rather
// than something convenient.
func wireBody(domain, path string, person visitor, rng *rand.Rand) ([]byte, error) {
	payload := struct {
		Name     string          `json:"n"`
		URL      string          `json:"u"`
		Domain   string          `json:"d"`
		Referrer string          `json:"r,omitempty"`
		Title    string          `json:"t,omitempty"`
		Version  int             `json:"v"`
		Width    int             `json:"w"`
		Props    json.RawMessage `json:"p,omitempty"`
	}{
		Name:    ingest.EventPageview,
		URL:     "https://" + domain + path,
		Domain:  domain,
		Title:   pageTitle(path, domain),
		Version: 1,
		Width:   person.Width,
	}

	// A third of the events arrive from somewhere, so the check covers the
	// referrer and channel rules rather than only the direct path.
	if rng.Float64() < 0.34 {
		payload.Referrer = knownSources[rng.IntN(len(knownSources))]
	}

	if rng.Float64() < 0.15 {
		payload.Name = "Signup"
		payload.Props = json.RawMessage(`{"plan":"growth","source":"seed-http"}`)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("seed http: encode payload: %w", err)
	}

	return encoded, nil
}

// post sends one event and reads the contract back. The content type is
// text/plain because that is what the official trackers send to avoid a CORS
// preflight, and a wire check that sent application/json would not be checking
// the path real traffic takes.
func post(ctx context.Context, client *http.Client, endpoint string, body []byte, person visitor) (status int, dropped string, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("seed http: %w", err)
	}

	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("User-Agent", person.UA)
	request.Header.Set("Accept-Language", person.Language)

	// The visitor's address, exactly as a reverse proxy in front of the ingest
	// tier would present it. Without it every event in the check would come
	// from the loopback address and be one visitor.
	request.Header.Set(clientip.HeaderForwardedFor, person.IP)

	response, err := client.Do(request)
	if err != nil {
		return 0, "", fmt.Errorf("seed http: %w", err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("seed http: close response: %w", closeErr))
		}
	}()

	// The body is drained so the connection can be reused. Two hundred events
	// on two hundred connections is a slower and less realistic check.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))

	return response.StatusCode, response.Header.Get(ingest.HeaderDropped), nil
}

// CountSince counts the events a site has written since an instant. It is how
// the wire check knows the events reached a database rather than only a 202:
// the whole failure this exists to catch is an event that was accepted, said
// nothing, and landed nowhere.
func CountSince(ctx context.Context, dataDir, domain string, since time.Time) (int64, error) {
	control, err := store.Open(filepath.Join(dataDir, config.SystemDatabaseName))
	if err != nil {
		return 0, err
	}
	defer control.Close()

	var accountID, siteID int64

	err = control.QueryRowContext(ctx, "SELECT account_id, id FROM sites WHERE domain = ?", domain).Scan(&accountID, &siteID)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("seed http: %s is not a site in this install", domain)
	}
	if err != nil {
		return 0, fmt.Errorf("seed http: %w", err)
	}

	manager := accounts.NewManager(dataDir)
	defer manager.CloseAll() //nolint:errcheck // the count is the answer; a close failure on the way out is not

	lease, err := manager.Acquire(ctx, accountID)
	if err != nil {
		return 0, err
	}
	defer lease.Release() //nolint:errcheck // the query result is more useful than an unlock error

	var count int64
	if err := lease.Account.Reader().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM events WHERE site_id = ? AND timestamp >= ?", siteID, since.Unix(),
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("seed http: %w", err)
	}

	return count, nil
}

// PrimaryDomain is the site the wire check sends to by default: the first
// traffic-carrying site in the fixture, which is the one a plain `make seed`
// always creates.
func PrimaryDomain() string {
	for _, account := range fixture {
		for _, site := range account.Sites {
			if site.Traffic {
				return site.Domain
			}
		}
	}

	return ""
}

// EnsureFixture creates system.db, brings it up to date and registers the
// accounts and sites, without generating any traffic. The wire check needs a
// site that routes before it can send anything, and making somebody run the
// bulk generator first would make the quick check the slow one.
func EnsureFixture(ctx context.Context, dataDir string, now time.Time) error {
	return EnsureFixtureWithMigrations(ctx, dataDir, now, migrate.System())
}

// EnsureFixtureWithMigrations creates the HTTP fixture with an explicit set.
// Tests may select an actual embedded prefix when they do not exercise newer
// control tables; production keeps the complete embedded set and its gap check.
func EnsureFixtureWithMigrations(ctx context.Context, dataDir string, now time.Time, systemMigrations migrate.Set) error {
	control, err := store.Open(filepath.Join(dataDir, config.SystemDatabaseName))
	if err != nil {
		return err
	}
	defer control.Close()

	if _, err := migrate.Run(ctx, control, systemMigrations); err != nil {
		return fmt.Errorf("seed http: %w", err)
	}

	if _, err := ensureFixture(ctx, control, selectFixture(DefaultSites), now.AddDate(0, 0, -DefaultDays), now); err != nil {
		return err
	}

	return nil
}

// Write prints what the wire check saw, including every drop reason. A run that
// sent two hundred events and had them all dropped as an unknown site has to
// read as a failure, not as two hundred successes.
func (r *HTTPResult) Write(out io.Writer) {
	fmt.Fprintf(out, "  sent      %d events in %s\n", r.Sent, r.Duration.Round(time.Millisecond))
	fmt.Fprintf(out, "  accepted  %d\n", r.Accepted)

	for status, count := range r.Statuses {
		if status != http.StatusAccepted {
			fmt.Fprintf(out, "  status    %d on %d event(s)\n", status, count)
		}
	}

	for reason, count := range r.Dropped {
		fmt.Fprintf(out, "  dropped   %-24s %d\n", reason, count)
	}
}
