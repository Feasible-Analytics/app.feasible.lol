//
// webhook_test.go
// Duplicate, out-of-order and contradictory deliveries, all landing on one answer.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package billing

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"
)

// The fixed values every test in this file uses.
const (
	webhookSecret = "whsec_test_only"
	customerID    = "cus_test_1"
	teamID        = 1
)

// now is the clock the whole harness runs on. Every signature, every phase and
// every stored timestamp derives from it, so nothing here depends on when the
// suite runs.
var now = time.Date(2026, time.March, 3, 12, 0, 0, 0, time.UTC)

// provider is a stand-in for the payment provider's API.
//
// It exists because the handler under test is defined by one property: it reads
// the provider's *current* state rather than the payload it was handed. Testing
// that needs a provider whose state the test can change between deliveries,
// which a canned payload cannot give.
type provider struct {
	mu sync.Mutex

	status      string
	paused      bool
	priceID     string
	periodEnd   int64
	cancelAtEnd bool

	// calls counts subscription reads, so a test can prove the handler asked
	// rather than trusted.
	calls int
}

// set changes what the provider will report from now on.
func (p *provider) set(status string, paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.status = status
	p.paused = paused
}

// reads reports how many times the handler asked for the subscription.
func (p *provider) reads() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.calls
}

// ServeHTTP answers the three endpoints the reconciler calls.
func (p *provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/customers/"):
		fmt.Fprintf(w, `{"id":%q,"email":"owner@example.com","object":"customer"}`, customerID)

	case r.URL.Path == "/v1/subscriptions":
		p.calls++

		pause := "null"
		if p.paused {
			pause = `{"behavior":"void"}`
		}

		fmt.Fprintf(w, `{"object":"list","has_more":false,"data":[{
			"id":"sub_test_1","object":"subscription","customer":%q,"status":%q,
			"current_period_end":%d,"cancel_at_period_end":%t,"pause_collection":%s,
			"items":{"data":[{"id":"si_1","price":{"id":%q,"object":"price"}}]},
			"metadata":{"feasible_team_id":"1"}
		}]}`, customerID, p.status, p.periodEnd, p.cancelAtEnd, pause, p.priceID)

	default:
		http.NotFound(w, r)
	}
}

// harness is a billing service wired to the fake provider and a real control
// database, with a real lifecycle machine behind it.
type harness struct {
	t        *testing.T
	control  *sql.DB
	service  *Service
	webhook  *Webhook
	provider *provider
	clock    time.Time
	mu       sync.Mutex
}

// newHarness builds the whole stack.
func newHarness(t *testing.T) *harness {
	t.Helper()

	control, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(context.Background(), control, migrate.Control()); err != nil {
		t.Fatal(err)
	}

	stamp := now.Unix()

	if _, err := control.Exec(`INSERT INTO users (id, email, name, created_at, updated_at) VALUES (1, 'owner@example.com', 'Owner', ?, ?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(`INSERT INTO teams (id, name, created_at, updated_at) VALUES (1, 'Example Co', ?, ?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(`INSERT INTO team_memberships (team_id, user_id, role, created_at) VALUES (1, 1, 'owner', ?)`, stamp); err != nil {
		t.Fatal(err)
	}

	fake := &provider{status: stripe.StatusActive, priceID: "price_monthly", periodEnd: now.AddDate(0, 1, 0).Unix()}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	h := &harness{t: t, control: control, provider: fake, clock: now}

	client := stripe.New("sk_test_fake")
	client.BaseURL = server.URL

	lifecycleStore := lifecycle.NewStore(control)

	lifecycleService := &lifecycle.Service{
		Store:  lifecycleStore,
		Notify: lifecycle.NotifierFunc(func(context.Context, lifecycle.Notice) (string, error) { return "captured", nil }),
		Purger: &lifecycle.Purger{Store: lifecycleStore, DataDir: t.TempDir()},
		Links:  lifecycle.Links{BaseURL: "https://feasible.lol"},
		Now:    func() time.Time { return h.now() },
	}

	h.service = &Service{
		Stripe:        client,
		Store:         NewStore(control),
		Lifecycle:     lifecycleService,
		Plans:         Plans{Monthly: "price_monthly", Yearly: "price_yearly"},
		WebhookSecret: webhookSecret,
		BaseURL:       "https://feasible.lol",
		Now:           func() time.Time { return h.now() },
	}

	h.service.Store.Now = func() time.Time { return h.now() }
	h.webhook = NewWebhook(h.service, nil)

	return h
}

// now is the injected clock.
func (h *harness) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.clock
}

// travel moves the clock forward.
func (h *harness) travel(d time.Duration) {
	h.mu.Lock()
	h.clock = h.clock.Add(d)
	h.mu.Unlock()
}

// deliver posts one signed event and returns the response.
func (h *harness) deliver(id, eventType, object string) *httptest.ResponseRecorder {
	h.t.Helper()

	body := fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":%s}}`, id, eventType, h.now().Unix(), object)

	request := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(body))
	request.Header.Set("Stripe-Signature", stripe.SignPayload([]byte(body), webhookSecret, h.now()))

	recorder := httptest.NewRecorder()
	h.webhook.ServeHTTP(recorder, request)

	return recorder
}

// subscriptionObject is the payload shape of a subscription event. Its status is
// deliberately settable so a test can send a payload that contradicts the
// provider's current state.
func subscriptionObject(status string) string {
	return fmt.Sprintf(`{"id":"sub_test_1","object":"subscription","customer":%q,"status":%q,"metadata":{"feasible_team_id":"1"}}`, customerID, status)
}

// invoiceObject is the payload shape of an invoice event.
func invoiceObject() string {
	return fmt.Sprintf(`{"id":"in_1","object":"invoice","customer":%q,"subscription":"sub_test_1","metadata":{"feasible_team_id":"1"}}`, customerID)
}

// phase reads the account's current lifecycle phase.
func (h *harness) phase() lifecycle.Phase {
	h.t.Helper()

	state, err := lifecycle.NewStore(h.control).Load(context.Background(), teamID)
	if err != nil {
		h.t.Fatal(err)
	}

	return state.At(h.now())
}

// clockStart reads day 0, or the zero time when no clock is running.
func (h *harness) clockStart() time.Time {
	h.t.Helper()

	state, err := lifecycle.NewStore(h.control).Load(context.Background(), teamID)
	if err != nil {
		h.t.Fatal(err)
	}

	return state.StartedAt
}

// TestCheckoutActivatesTheAccount is the happy path: money arrives, the mirror
// is written, and the machine says Active.
func TestCheckoutActivatesTheAccount(t *testing.T) {
	h := newHarness(t)

	response := h.deliver("evt_1", stripe.EventCheckoutCompleted,
		fmt.Sprintf(`{"id":"cs_1","object":"checkout.session","customer":%q,"subscription":"sub_test_1","metadata":{"feasible_team_id":"1"}}`, customerID))

	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}

	if got := h.phase(); got != lifecycle.PhaseActive {
		t.Fatalf("phase is %q, want active", got)
	}

	mirror, err := h.service.Store.Load(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}

	if mirror.CustomerID != customerID {
		t.Errorf("customer is %q", mirror.CustomerID)
	}
	if mirror.Status != stripe.StatusActive {
		t.Errorf("status is %q", mirror.Status)
	}
	if mirror.Plan != "monthly" {
		t.Errorf("plan is %q, want monthly", mirror.Plan)
	}
	if mirror.BillingEmail != "owner@example.com" {
		t.Errorf("billing email is %q", mirror.BillingEmail)
	}
}

// TestDuplicateDeliveryIsIgnored is the idempotency guarantee. The provider
// retries, and a handler that acted on every delivery would double-apply.
func TestDuplicateDeliveryIsIgnored(t *testing.T) {
	h := newHarness(t)

	first := h.deliver("evt_dup", stripe.EventInvoicePaymentSucceed, invoiceObject())
	if !strings.Contains(first.Body.String(), OutcomeApplied) {
		t.Fatalf("first delivery: %s", first.Body.String())
	}

	before := h.provider.reads()

	for i := 0; i < 5; i++ {
		again := h.deliver("evt_dup", stripe.EventInvoicePaymentSucceed, invoiceObject())

		if again.Code != http.StatusOK {
			t.Fatalf("redelivery %d: status %d", i, again.Code)
		}
		if !strings.Contains(again.Body.String(), OutcomeDuplicate) {
			t.Fatalf("redelivery %d was not recognised as a duplicate: %s", i, again.Body.String())
		}
	}

	// A duplicate must not even ask the provider. Recognising it late would
	// still be correct, but it would mean five network calls per retry storm.
	if after := h.provider.reads(); after != before {
		t.Errorf("duplicates cost %d extra provider reads", after-before)
	}

	var rows int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM stripe_events WHERE event_id = 'evt_dup'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("six deliveries wrote %d event rows, want 1", rows)
	}
}

// TestOutOfOrderDeliveryDoesNotUndoAPayment is the property the whole design
// exists for. A stale `payment_failed` arriving after the retry succeeded must
// not lapse a paying customer — and it does not, because the handler asks the
// provider what is true now rather than believing the payload.
func TestOutOfOrderDeliveryDoesNotUndoAPayment(t *testing.T) {
	h := newHarness(t)

	// The world: the subscription is active and paid.
	h.provider.set(stripe.StatusActive, false)

	// A failure event that was generated minutes ago, delayed in transit, and is
	// only arriving now. Its payload says past_due.
	response := h.deliver("evt_stale_fail", stripe.EventInvoicePaymentFailed, invoiceObject())
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}

	if got := h.phase(); got != lifecycle.PhaseActive {
		t.Fatalf("a stale failure lapsed a paying account: phase is %q", got)
	}
	if !h.clockStart().IsZero() {
		t.Fatal("a stale failure started the deletion clock on a paying account")
	}
}

// TestContradictoryUpdatesLandOnOneAnswer reproduces the race that has bitten
// this product category: two update events describing incompatible states,
// arriving together. Whichever order they are handled in, the answer is the
// provider's current state.
func TestContradictoryUpdatesLandOnOneAnswer(t *testing.T) {
	h := newHarness(t)

	h.provider.set(stripe.StatusActive, false)

	// Two events, one claiming the subscription is past due and one claiming it
	// is active, delivered back to back.
	if code := h.deliver("evt_a", stripe.EventSubscriptionUpdated, subscriptionObject(stripe.StatusPastDue)).Code; code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if code := h.deliver("evt_b", stripe.EventSubscriptionUpdated, subscriptionObject(stripe.StatusActive)).Code; code != http.StatusOK {
		t.Fatalf("status %d", code)
	}

	if got := h.phase(); got != lifecycle.PhaseActive {
		t.Fatalf("phase is %q, want active", got)
	}

	// And the reverse order, against a provider that now says the subscription
	// is genuinely past due.
	h2 := newHarness(t)
	h2.provider.set(stripe.StatusPastDue, false)

	h2.deliver("evt_b", stripe.EventSubscriptionUpdated, subscriptionObject(stripe.StatusActive))
	h2.deliver("evt_a", stripe.EventSubscriptionUpdated, subscriptionObject(stripe.StatusPastDue))

	if got := h2.phase(); got != lifecycle.PhaseGrace {
		t.Fatalf("phase is %q, want grace", got)
	}
}

// TestFirstFailureStartsTheClockAndLaterOnesDoNotMoveIt is the rule stated in
// the specification: day 0 is the first failed charge, not the end of the
// provider's retry window. Smart Retries produce a failure event every couple of
// days, and each one arrives here.
func TestFirstFailureStartsTheClockAndLaterOnesDoNotMoveIt(t *testing.T) {
	h := newHarness(t)

	h.provider.set(stripe.StatusPastDue, false)

	if code := h.deliver("evt_fail_1", stripe.EventInvoicePaymentFailed, invoiceObject()).Code; code != http.StatusOK {
		t.Fatalf("status %d", code)
	}

	dayZero := h.clockStart()
	if dayZero.IsZero() {
		t.Fatal("the first failure did not start the clock")
	}

	// The provider's own retries, on days 3, 5, 7 and 21.
	for i, days := range []int{3, 5, 7, 21} {
		h.travel(time.Duration(days) * 24 * time.Hour)

		id := fmt.Sprintf("evt_fail_retry_%d", i)
		if code := h.deliver(id, stripe.EventInvoicePaymentFailed, invoiceObject()).Code; code != http.StatusOK {
			t.Fatalf("retry %d: status %d", i, code)
		}
	}

	if got := h.clockStart(); !got.Equal(dayZero) {
		t.Fatalf("day 0 moved from %s to %s", dayZero, got)
	}
}

// TestPaymentAfterALapseResetsTheClock is the recovery path through the webhook
// rather than through the machine directly.
func TestPaymentAfterALapseResetsTheClock(t *testing.T) {
	h := newHarness(t)

	h.provider.set(stripe.StatusPastDue, false)
	h.deliver("evt_fail", stripe.EventInvoicePaymentFailed, invoiceObject())

	if h.clockStart().IsZero() {
		t.Fatal("the failure did not start the clock")
	}

	h.travel(20 * 24 * time.Hour)
	h.provider.set(stripe.StatusActive, false)
	h.deliver("evt_paid", stripe.EventInvoicePaymentSucceed, invoiceObject())

	if got := h.phase(); got != lifecycle.PhaseActive {
		t.Fatalf("phase is %q, want active", got)
	}
	if !h.clockStart().IsZero() {
		t.Fatal("the clock is still running after a successful payment")
	}
}

// TestAPausedSubscriptionIsNotPaying covers the trap that a paused subscription
// can still report `active`. Reading only the status is how a paused customer is
// mistaken for a healthy one and keeps the product free.
func TestAPausedSubscriptionIsNotPaying(t *testing.T) {
	h := newHarness(t)

	h.provider.set(stripe.StatusActive, true)

	if code := h.deliver("evt_paused", stripe.EventSubscriptionPaused, subscriptionObject(stripe.StatusActive)).Code; code != http.StatusOK {
		t.Fatalf("status %d", code)
	}

	if got := h.phase(); got != lifecycle.PhaseGrace {
		t.Fatalf("phase is %q, want grace", got)
	}

	mirror, err := h.service.Store.Load(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.Status != stripe.StatusPaused {
		t.Errorf("the mirror says %q, want paused", mirror.Status)
	}

	// Resuming puts it back.
	h.provider.set(stripe.StatusActive, false)
	h.deliver("evt_resumed", stripe.EventSubscriptionResumed, subscriptionObject(stripe.StatusActive))

	if got := h.phase(); got != lifecycle.PhaseActive {
		t.Fatalf("after resuming, phase is %q, want active", got)
	}
}

// TestCancellationStartsTheClock covers the subscription being deleted outright,
// which is what cancelling at the end of a period eventually produces.
func TestCancellationStartsTheClock(t *testing.T) {
	h := newHarness(t)

	h.provider.set(stripe.StatusCanceled, false)

	if code := h.deliver("evt_cancel", stripe.EventSubscriptionDeleted, subscriptionObject(stripe.StatusCanceled)).Code; code != http.StatusOK {
		t.Fatalf("status %d", code)
	}

	if got := h.phase(); got != lifecycle.PhaseGrace {
		t.Fatalf("phase is %q, want grace", got)
	}
}

// TestAForgedDeliveryIsRefused checks the endpoint end to end, including the
// status code: a 400 stops the provider retrying a forgery forever.
func TestAForgedDeliveryIsRefused(t *testing.T) {
	h := newHarness(t)

	body := `{"id":"evt_forged","type":"invoice.payment_succeeded","data":{"object":{}}}`

	request := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(body))
	request.Header.Set("Stripe-Signature", stripe.SignPayload([]byte(body), "the-wrong-secret", h.now()))

	recorder := httptest.NewRecorder()
	h.webhook.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", recorder.Code)
	}

	var rows int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM stripe_events`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("a forged delivery was recorded as an event")
	}
}

// TestAnUnroutableEventIsRecordedAndIgnored covers somebody clicking around the
// provider's dashboard against a customer this install has never seen. It must
// be visible in the log and must not be guessed at.
func TestAnUnroutableEventIsRecordedAndIgnored(t *testing.T) {
	h := newHarness(t)

	response := h.deliver("evt_stranger", stripe.EventInvoicePaymentSucceed,
		`{"id":"in_x","object":"invoice","customer":"cus_someone_else"}`)

	if response.Code != http.StatusOK {
		t.Fatalf("status %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), OutcomeIgnored) {
		t.Fatalf("outcome was %s", response.Body.String())
	}

	events, err := h.service.Store.RecentEvents(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Outcome != OutcomeIgnored {
		t.Fatalf("the event log holds %+v", events)
	}
}

// TestAFailedHandlerCanBeRetried is why a failed attempt is not treated as a
// duplicate. The provider retries a 500, and that retry has to actually run —
// which is safe here because the handler reconciles from current state rather
// than replaying a stale event.
func TestAFailedHandlerCanBeRetried(t *testing.T) {
	h := newHarness(t)

	// Point the client at a provider that is not answering, so the reconcile
	// fails the way a real outage would.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider is down", http.StatusInternalServerError)
	}))
	defer broken.Close()

	working := h.service.Stripe.BaseURL
	h.service.Stripe.BaseURL = broken.URL

	response := h.deliver("evt_retry", stripe.EventInvoicePaymentSucceed, invoiceObject())
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 so the provider retries", response.Code)
	}

	// The provider comes back, and the retry lands.
	h.service.Stripe.BaseURL = working

	retry := h.deliver("evt_retry", stripe.EventInvoicePaymentSucceed, invoiceObject())
	if retry.Code != http.StatusOK {
		t.Fatalf("the retry failed: status %d, %s", retry.Code, retry.Body.String())
	}
	if !strings.Contains(retry.Body.String(), OutcomeApplied) {
		t.Fatalf("the retry was skipped as a duplicate: %s", retry.Body.String())
	}

	if got := h.phase(); got != lifecycle.PhaseActive {
		t.Fatalf("phase is %q, want active", got)
	}
}

// TestEveryHandledEventIsInTheLog is the "logged where support can read it"
// requirement. A person answering "they say they paid" needs the events, their
// order and what each one did.
func TestEveryHandledEventIsInTheLog(t *testing.T) {
	h := newHarness(t)

	h.deliver("evt_log_1", stripe.EventInvoicePaymentSucceed, invoiceObject())
	h.travel(time.Minute)
	h.deliver("evt_log_2", stripe.EventSubscriptionUpdated, subscriptionObject(stripe.StatusActive))
	h.travel(time.Minute)
	h.deliver("evt_log_3", "customer.discount.created", invoiceObject())

	events, err := h.service.Store.RecentEvents(context.Background(), teamID, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(events) != 3 {
		t.Fatalf("the log holds %d events, want 3", len(events))
	}

	// Newest first, so the last thing that happened is the first thing read.
	if events[0].EventID != "evt_log_3" {
		t.Errorf("the newest event is %q", events[0].EventID)
	}

	outcomes := map[string]string{}
	for _, event := range events {
		outcomes[event.EventID] = event.Outcome

		if event.HandledAt.IsZero() {
			t.Errorf("%s has no handled time", event.EventID)
		}
	}

	if outcomes["evt_log_1"] != OutcomeApplied || outcomes["evt_log_2"] != OutcomeApplied {
		t.Errorf("outcomes are %v", outcomes)
	}

	// A type we do not act on is recorded rather than dropped, so the log
	// answers "did it arrive" as well as "did it do anything".
	if outcomes["evt_log_3"] != OutcomeIgnored {
		t.Errorf("an unhandled type was recorded as %q", outcomes["evt_log_3"])
	}
}

// TestPayloadIsStoredVerbatim checks that support reads what the provider sent
// rather than what our structs could parse.
func TestPayloadIsStoredVerbatim(t *testing.T) {
	h := newHarness(t)

	h.deliver("evt_verbatim", stripe.EventInvoicePaymentSucceed, invoiceObject())

	var stored string
	if err := h.control.QueryRow(`SELECT payload FROM stripe_events WHERE event_id = 'evt_verbatim'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(stored, `"evt_verbatim"`) || !strings.Contains(stored, customerID) {
		t.Errorf("the stored payload is %q", stored)
	}
}
