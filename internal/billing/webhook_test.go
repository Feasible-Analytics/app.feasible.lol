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
	"net/url"
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

	invoiceStatus      string
	invoiceAutoAdvance bool
	invoicePaidAt      int64
	settleOnCustomerAt int64
	settleOnVoidAt     int64
	customerDeleted    bool
	pauseStarted       chan struct{}
	continuePause      chan struct{}

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

// ServeHTTP answers the provider endpoints used by reconciliation and purge.
func (p *provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/invoices/in_test_1":
		p.invoiceStatus = ""
		p.invoiceAutoAdvance = false
		_, _ = w.Write([]byte(`{"id":"in_test_1","deleted":true}`))

	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/customers/"):
		p.customerDeleted = true
		fmt.Fprintf(w, `{"id":%q,"deleted":true}`, customerID)

	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/customers/"):
		if p.settleOnCustomerAt != 0 {
			p.invoiceStatus = "paid"
			p.invoiceAutoAdvance = false
			p.invoicePaidAt = p.settleOnCustomerAt
			p.settleOnCustomerAt = 0
		}
		fmt.Fprintf(w, `{"id":%q,"email":"owner@example.com","object":"customer"}`, customerID)

	case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions":
		_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[]}`))

	case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions":
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

	case r.Method == http.MethodPost && r.URL.Path == "/v1/subscriptions/sub_test_1":
		if p.pauseStarted != nil {
			close(p.pauseStarted)
			p.pauseStarted = nil
			continuePause := p.continuePause
			p.mu.Unlock()
			<-continuePause
			p.mu.Lock()
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.paused = r.PostForm.Get("pause_collection[behavior]") != ""
		pause := "null"
		if p.paused {
			pause = `{"behavior":"void"}`
		}
		fmt.Fprintf(w, `{"id":"sub_test_1","customer":%q,"status":%q,"pause_collection":%s}`, customerID, p.status, pause)

	case r.Method == http.MethodGet && r.URL.Path == "/v1/invoices":
		if p.invoiceStatus == "" {
			_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[]}`))
			return
		}
		fmt.Fprintf(w, `{"object":"list","has_more":false,"data":[{
			"id":"in_test_1","created":%d,"customer":%q,"status":%q,
			"auto_advance":%t,"paid":%t,"status_transitions":{"paid_at":%d},
			"parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_test_1"}}
		}]}`, now.Add(-time.Hour).Unix(), customerID, p.invoiceStatus,
			p.invoiceAutoAdvance, p.invoiceStatus == "paid", p.invoicePaidAt)

	case r.Method == http.MethodPost && r.URL.Path == "/v1/invoices/in_test_1":
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.invoiceAutoAdvance = r.PostForm.Get("auto_advance") == "true"
		fmt.Fprintf(w, `{"id":"in_test_1","customer":%q,"status":%q,"auto_advance":%t}`, customerID, p.invoiceStatus, p.invoiceAutoAdvance)

	case r.Method == http.MethodPost && r.URL.Path == "/v1/invoices/in_test_1/void":
		if p.settleOnVoidAt != 0 {
			p.invoiceStatus = "paid"
			p.invoicePaidAt = p.settleOnVoidAt
			p.status = stripe.StatusActive
			p.settleOnVoidAt = 0
		}
		if p.invoiceStatus == "paid" {
			http.Error(w, `{"error":{"type":"invalid_request_error","message":"invoice already paid"}}`, http.StatusBadRequest)
			return
		}
		p.invoiceStatus = "void"
		p.invoiceAutoAdvance = false
		fmt.Fprintf(w, `{"id":"in_test_1","customer":%q,"status":"void","auto_advance":false}`, customerID)

	default:
		http.NotFound(w, r)
	}
}

// harness is a billing service wired to the fake provider and a real control
// database, with a real lifecycle machine behind it.
type harness struct {
	t           *testing.T
	control     *sql.DB
	controlPath string
	service     *Service
	webhook     *Webhook
	provider    *provider
	clock       time.Time
	mu          sync.Mutex
}

// newHarness builds the whole stack.
func newHarness(t *testing.T) *harness {
	t.Helper()

	controlPath := filepath.Join(t.TempDir(), "control.db")
	control, err := store.Open(controlPath)
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

	h := &harness{t: t, control: control, controlPath: controlPath, provider: fake, clock: now}

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
		Plans:         Plans{Product: "prod_test", Monthly: "price_monthly", Yearly: "price_yearly"},
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
	return h.deliverCreated(id, eventType, object, h.now())
}

// deliverCreated posts one signed event whose provider creation time can differ
// from its delivery time, reproducing Stripe's documented out-of-order delivery.
func (h *harness) deliverCreated(id, eventType, object string, created time.Time) *httptest.ResponseRecorder {
	return h.deliverCreatedWith(h.webhook, id, eventType, object, created)
}

// deliverCreatedWith posts a signed event to a selected independently built
// webhook, which is how cross-process ordering is exercised in one test.
func (h *harness) deliverCreatedWith(webhook *Webhook, id, eventType, object string, created time.Time) *httptest.ResponseRecorder {
	h.t.Helper()

	body := fmt.Sprintf(`{"id":%q,"type":%q,"created":%d,"data":{"object":%s}}`, id, eventType, created.Unix(), object)

	request := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(body))
	request.Header.Set("Stripe-Signature", stripe.SignPayload([]byte(body), webhookSecret, h.now()))

	recorder := httptest.NewRecorder()
	webhook.ServeHTTP(recorder, request)

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
	return invoiceObjectFor("in_1", "sub_test_1", now.Add(-time.Hour))
}

// invoiceObjectFor builds the pinned Basil invoice parent shape with explicit
// provider creation time for evidence-order tests.
func invoiceObjectFor(invoiceID, subscriptionID string, created time.Time) string {
	return fmt.Sprintf(`{"id":%q,"object":"invoice","created":%d,"customer":%q,"parent":{"type":"subscription_details","subscription_details":{"subscription":%q,"metadata":{"feasible_team_id":"1"}}}}`, invoiceID, created.Unix(), customerID, subscriptionID)
}

// checkoutObject builds the session shape shared by immediate and delayed
// payment events.
func checkoutObject(paymentStatus string) string {
	return fmt.Sprintf(`{"id":"cs_1","object":"checkout.session","created":%d,"customer":%q,"subscription":"sub_test_1","payment_status":%q,"metadata":{"feasible_team_id":"1"}}`, now.Add(-time.Hour).Unix(), customerID, paymentStatus)
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
		checkoutObject("paid"))

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
	if mirror.PaymentState != PaymentPaid {
		t.Errorf("payment state is %q, want paid", mirror.PaymentState)
	}
}

// TestAsyncCheckoutWaitsForPayment proves checkout completion is not payment
// evidence for a delayed method, while the later success event is.
func TestAsyncCheckoutWaitsForPayment(t *testing.T) {
	h := newHarness(t)

	if _, err := h.service.StartTrial(context.Background(), teamID); err != nil {
		t.Fatal(err)
	}

	if code := h.deliver("evt_pending", stripe.EventCheckoutCompleted, checkoutObject("unpaid")).Code; code != http.StatusOK {
		t.Fatalf("pending checkout status %d", code)
	}
	if got := h.phase(); got != lifecycle.PhaseGrace {
		t.Fatalf("pending payment changed trial access to %q", got)
	}

	// Subscription events can arrive after checkout completion while Stripe's
	// subscription object already says active. They still are not payment proof.
	h.deliver("evt_active_while_pending", stripe.EventSubscriptionUpdated, subscriptionObject(stripe.StatusActive))
	if got := h.phase(); got != lifecycle.PhaseGrace {
		t.Fatalf("active subscription bypassed pending payment: phase is %q", got)
	}

	mirror, err := h.service.Store.Load(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.PaymentState != PaymentPending {
		t.Fatalf("payment state is %q, want pending", mirror.PaymentState)
	}

	h.deliver("evt_async_paid", stripe.EventCheckoutAsyncPaymentSucceeded, checkoutObject("paid"))
	if got := h.phase(); got != lifecycle.PhaseActive {
		t.Fatalf("async success left phase %q, want active", got)
	}

	// A delayed completion delivery cannot downgrade final success back to
	// pending. Stripe does not guarantee webhook delivery order.
	h.deliver("evt_pending_arrived_late", stripe.EventCheckoutCompleted, checkoutObject("unpaid"))
	mirror, err = h.service.Store.Load(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.PaymentState != PaymentPaid || h.phase() != lifecycle.PhaseActive {
		t.Fatalf("late completion changed final success: payment=%q phase=%q", mirror.PaymentState, h.phase())
	}
}

// TestAsyncSuccessWaitsForSubscriptionState handles the provider API lagging
// behind its success webhook. Payment evidence is retained, but access waits
// until the subscription itself is healthy too.
func TestAsyncSuccessWaitsForSubscriptionState(t *testing.T) {
	h := newHarness(t)

	if _, err := h.service.StartTrial(context.Background(), teamID); err != nil {
		t.Fatal(err)
	}
	h.provider.set(stripe.StatusIncomplete, false)

	h.deliver("evt_async_paid", stripe.EventCheckoutAsyncPaymentSucceeded, checkoutObject("paid"))
	if got := h.phase(); got != lifecycle.PhaseGrace {
		t.Fatalf("success with an incomplete subscription changed phase to %q", got)
	}

	mirror, err := h.service.Store.Load(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.PaymentState != PaymentPaid {
		t.Fatalf("payment evidence was lost while subscription lagged: %q", mirror.PaymentState)
	}

	h.deliver("evt_subscription_still_lagging", stripe.EventSubscriptionUpdated, subscriptionObject(stripe.StatusIncomplete))
	mirror, err = h.service.Store.Load(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.PaymentState != PaymentPaid || h.phase() != lifecycle.PhaseGrace {
		t.Fatalf("incomplete update lost payment evidence: payment=%q phase=%q", mirror.PaymentState, h.phase())
	}

	h.provider.set(stripe.StatusActive, false)
	h.deliver("evt_subscription_caught_up", stripe.EventSubscriptionUpdated, subscriptionObject(stripe.StatusActive))
	if got := h.phase(); got != lifecycle.PhaseActive {
		t.Fatalf("healthy subscription did not apply retained payment: phase is %q", got)
	}
}

// TestAsyncCheckoutFailureStaysFailed proves Stripe's active subscription
// status cannot reactivate access after the asynchronous payment fails.
func TestAsyncCheckoutFailureStaysFailed(t *testing.T) {
	h := newHarness(t)

	if _, err := h.service.StartTrial(context.Background(), teamID); err != nil {
		t.Fatal(err)
	}

	h.deliver("evt_pending", stripe.EventCheckoutCompleted, checkoutObject("unpaid"))
	h.deliver("evt_async_failed", stripe.EventCheckoutAsyncPaymentFailed, checkoutObject("unpaid"))

	if got := h.phase(); got != lifecycle.PhaseGrace {
		t.Fatalf("failed async payment left phase %q, want grace", got)
	}

	// A later active snapshot must preserve the failed payment gate.
	h.deliver("evt_active_after_failure", stripe.EventSubscriptionUpdated, subscriptionObject(stripe.StatusActive))
	if got := h.phase(); got != lifecycle.PhaseGrace {
		t.Fatalf("active subscription undid async failure: phase is %q", got)
	}

	mirror, err := h.service.Store.Load(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.Status != stripe.StatusActive || mirror.PaymentState != PaymentFailed {
		t.Fatalf("mirror is status=%q payment=%q", mirror.Status, mirror.PaymentState)
	}
}

// TestTerminalPaymentEvidenceUsesProviderOrder covers every paid/failed source
// order and delivery order. The newer invoice must win whether it arrives first
// or last, so arrival scheduling cannot decide account access.
func TestTerminalPaymentEvidenceUsesProviderOrder(t *testing.T) {
	cases := []struct {
		name         string
		newerState   string
		newerArrives bool
	}{
		{name: "newer paid arrives first", newerState: PaymentPaid, newerArrives: true},
		{name: "newer paid arrives last", newerState: PaymentPaid, newerArrives: false},
		{name: "newer failed arrives first", newerState: PaymentFailed, newerArrives: true},
		{name: "newer failed arrives last", newerState: PaymentFailed, newerArrives: false},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			h.provider.set(stripe.StatusActive, false)

			olderState := PaymentFailed
			if test.newerState == PaymentFailed {
				olderState = PaymentPaid
			}

			olderType := stripe.EventInvoicePaymentFailed
			if olderState == PaymentPaid {
				olderType = stripe.EventInvoicePaymentSucceed
			}
			newerType := stripe.EventInvoicePaymentFailed
			if test.newerState == PaymentPaid {
				newerType = stripe.EventInvoicePaymentSucceed
			}

			olderAt := h.now().Add(-2 * time.Hour)
			newerAt := h.now().Add(-time.Hour)
			older := func() {
				h.deliverCreated("evt_older", olderType,
					invoiceObjectFor("in_older", "sub_test_1", olderAt), olderAt.Add(time.Minute))
			}
			newer := func() {
				h.deliverCreated("evt_newer", newerType,
					invoiceObjectFor("in_newer", "sub_test_1", newerAt), newerAt.Add(time.Minute))
			}

			if test.newerArrives {
				newer()
				older()
			} else {
				older()
				newer()
			}

			mirror, err := h.service.Store.Load(context.Background(), teamID)
			if err != nil {
				t.Fatal(err)
			}
			if mirror.PaymentState != test.newerState {
				t.Fatalf("payment state is %q, want newer %q", mirror.PaymentState, test.newerState)
			}

			wantPhase := lifecycle.PhaseGrace
			if test.newerState == PaymentPaid {
				wantPhase = lifecycle.PhaseActive
			}
			if got := h.phase(); got != wantPhase {
				t.Fatalf("phase is %q, want %q", got, wantPhase)
			}
		})
	}
}

// TestExactPaymentTimestampTieUsesSettlementSemantics makes the deterministic
// tie rule explicit: proof of settlement outranks a failure for the same Stripe
// object and second, regardless of which event is examined first.
func TestExactPaymentTimestampTieUsesSettlementSemantics(t *testing.T) {
	paid := PaymentUpdate{State: PaymentPaid, SourceID: "in_tie", SourceCreated: 10, EventCreated: 20}
	failed := PaymentUpdate{State: PaymentFailed, SourceID: "in_tie", SourceCreated: 10, EventCreated: 20}

	if !paymentUpdateAfter(paid, failed) {
		t.Fatal("settlement did not outrank failure at an exact provider timestamp tie")
	}
	if paymentUpdateAfter(failed, paid) {
		t.Fatal("failure outranked settlement at an exact provider timestamp tie")
	}
}

// TestConcurrentPaymentTransitionsUseProviderOrder exercises the durable
// account lease through separate database handles while contradictory terminal
// events enter independently constructed handlers at the same time.
func TestConcurrentPaymentTransitionsUseProviderOrder(t *testing.T) {
	for _, newerState := range []string{PaymentPaid, PaymentFailed} {
		t.Run(newerState, func(t *testing.T) {
			h := newHarness(t)
			h.provider.set(stripe.StatusActive, false)

			secondControl, err := store.Open(h.controlPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { secondControl.Close() })

			secondLifecycleStore := lifecycle.NewStore(secondControl)
			secondLifecycle := &lifecycle.Service{
				Store:  secondLifecycleStore,
				Notify: h.service.Lifecycle.Notify,
				Purger: &lifecycle.Purger{Store: secondLifecycleStore, DataDir: t.TempDir()},
				Links:  h.service.Lifecycle.Links,
				Now:    func() time.Time { return h.now() },
			}
			secondService := *h.service
			secondService.Store = NewStore(secondControl)
			secondService.Store.Now = func() time.Time { return h.now() }
			secondService.Lifecycle = secondLifecycle
			secondWebhook := NewWebhook(&secondService, nil)

			olderType := stripe.EventInvoicePaymentFailed
			newerType := stripe.EventInvoicePaymentSucceed
			if newerState == PaymentFailed {
				olderType = stripe.EventInvoicePaymentSucceed
				newerType = stripe.EventInvoicePaymentFailed
			}

			olderAt := h.now().Add(-2 * time.Hour)
			newerAt := h.now().Add(-time.Hour)
			start := make(chan struct{})
			responses := make(chan *httptest.ResponseRecorder, 2)
			var workers sync.WaitGroup

			workers.Add(2)
			go func() {
				defer workers.Done()
				<-start
				responses <- h.deliverCreated("evt_concurrent_older", olderType,
					invoiceObjectFor("in_concurrent_older", "sub_test_1", olderAt), olderAt.Add(time.Minute))
			}()
			go func() {
				defer workers.Done()
				<-start
				responses <- h.deliverCreatedWith(secondWebhook, "evt_concurrent_newer", newerType,
					invoiceObjectFor("in_concurrent_newer", "sub_test_1", newerAt), newerAt.Add(time.Minute))
			}()

			close(start)
			workers.Wait()
			close(responses)

			for response := range responses {
				if response.Code != http.StatusOK {
					t.Errorf("concurrent transition returned status %d: %s", response.Code, response.Body.String())
				}
			}

			mirror, err := h.service.Store.Load(context.Background(), teamID)
			if err != nil {
				t.Fatal(err)
			}
			if mirror.PaymentState != newerState {
				t.Fatalf("concurrent payment state is %q, want newer %q", mirror.PaymentState, newerState)
			}
		})
	}
}

// TestMalformedAuthenticatedEvidenceDoesNotPoisonLaterReconciliation records a
// signed but structurally invalid invoice as failed, then proves a later valid
// payment can reconcile without decoding that historical row again.
func TestMalformedAuthenticatedEvidenceDoesNotPoisonLaterReconciliation(t *testing.T) {
	h := newHarness(t)

	malformed := fmt.Sprintf(`{"id":"in_bad","object":"invoice","created":"not-a-timestamp","customer":%q,"metadata":{"feasible_team_id":"1"}}`, customerID)
	bad := h.deliver("evt_bad_history", stripe.EventInvoicePaymentFailed, malformed)
	if bad.Code != http.StatusInternalServerError {
		t.Fatalf("malformed authenticated event answered %d: %s", bad.Code, bad.Body.String())
	}

	var outcome string
	if err := h.control.QueryRow(`SELECT outcome FROM stripe_events WHERE event_id = 'evt_bad_history'`).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != OutcomeError {
		t.Fatalf("malformed event outcome is %q, want error", outcome)
	}

	good := h.deliver("evt_after_bad", stripe.EventInvoicePaymentSucceed, invoiceObject())
	if good.Code != http.StatusOK || !strings.Contains(good.Body.String(), OutcomeApplied) {
		t.Fatalf("valid event after malformed history answered %d: %s", good.Code, good.Body.String())
	}

	mirror, err := h.service.Store.Load(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.PaymentState != PaymentPaid {
		t.Fatalf("valid event after malformed history left payment %q", mirror.PaymentState)
	}
}

// TestDelayedFailureUsesStripeEventTimeForDayZero proves delayed and out-of-
// order failures start the lifecycle at Stripe's first event in the lapse, not
// local receipt or the newest failed attempt.
func TestDelayedFailureUsesStripeEventTimeForDayZero(t *testing.T) {
	h := newHarness(t)
	h.provider.set(stripe.StatusPastDue, false)
	failedAt := h.now().Add(-7 * 24 * time.Hour)
	newerFailure := h.now().Add(-2 * 24 * time.Hour)

	response := h.deliverCreated("evt_newer_failure_first", stripe.EventInvoicePaymentFailed,
		invoiceObjectFor("in_newer", "sub_test_1", newerFailure), newerFailure)
	if response.Code != http.StatusOK {
		t.Fatalf("newer failure answered %d: %s", response.Code, response.Body.String())
	}

	response = h.deliverCreated("evt_delayed_failure", stripe.EventInvoicePaymentFailed,
		invoiceObjectFor("in_delayed", "sub_test_1", failedAt), failedAt)
	if response.Code != http.StatusOK {
		t.Fatalf("delayed failure answered %d: %s", response.Code, response.Body.String())
	}

	if got := h.clockStart(); !got.Equal(failedAt) {
		t.Fatalf("lifecycle day zero is %s, want Stripe event time %s", got, failedAt)
	}
}

// TestDelayedOldFailureDoesNotCrossARecoveredPayment proves reconstructing day
// zero stops at the latest settlement instead of reviving an earlier lapse.
func TestDelayedOldFailureDoesNotCrossARecoveredPayment(t *testing.T) {
	h := newHarness(t)
	oldFailure := h.now().Add(-7 * 24 * time.Hour)
	paidAt := h.now().Add(-5 * 24 * time.Hour)
	currentFailure := h.now().Add(-2 * 24 * time.Hour)

	h.provider.set(stripe.StatusActive, false)
	response := h.deliverCreated("evt_recovered", stripe.EventInvoicePaymentSucceed,
		invoiceObjectFor("in_recovered", "sub_test_1", paidAt), paidAt)
	if response.Code != http.StatusOK {
		t.Fatalf("recovery answered %d: %s", response.Code, response.Body.String())
	}

	h.provider.set(stripe.StatusPastDue, false)
	response = h.deliverCreated("evt_current_failure", stripe.EventInvoicePaymentFailed,
		invoiceObjectFor("in_current", "sub_test_1", currentFailure), currentFailure)
	if response.Code != http.StatusOK {
		t.Fatalf("current failure answered %d: %s", response.Code, response.Body.String())
	}

	response = h.deliverCreated("evt_old_failure_late", stripe.EventInvoicePaymentFailed,
		invoiceObjectFor("in_old", "sub_test_1", oldFailure), oldFailure)
	if response.Code != http.StatusOK {
		t.Fatalf("old failure answered %d: %s", response.Code, response.Body.String())
	}

	if got := h.clockStart(); !got.Equal(currentFailure) {
		t.Fatalf("old lapse moved day zero to %s, want %s", got, currentFailure)
	}
}

// TestConcurrentCheckoutAcrossStoresCreatesOneStripeSession proves repeated
// requests, plan changes, and a process restart all recover the durable claim
// and provider idempotency key instead of creating a second subscription.
func TestConcurrentCheckoutAcrossStoresCreatesOneStripeSession(t *testing.T) {
	h := newHarness(t)
	secondControl, err := store.Open(h.controlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { secondControl.Close() })

	var providerMu sync.Mutex
	creates := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
			providerMu.Lock()
			creates++
			providerMu.Unlock()
			time.Sleep(25 * time.Millisecond)
			_, _ = w.Write([]byte(`{"id":"cs_single","status":"open","url":"https://checkout.example/single"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions/cs_single":
			_, _ = w.Write([]byte(`{"id":"cs_single","status":"open","url":"https://checkout.example/single"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	first := *h.service
	first.Stripe = stripe.New("sk_test_fake")
	first.Stripe.BaseURL = server.URL
	first.Store = NewStore(h.control)
	first.Store.Now = func() time.Time { return h.now() }

	second := first
	second.Store = NewStore(secondControl)
	second.Store.Now = func() time.Time { return h.now() }

	start := make(chan struct{})
	results := make(chan *stripe.CheckoutSession, 2)
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for _, request := range []struct {
		service *Service
		plan    string
	}{{&first, "monthly"}, {&second, "yearly"}} {
		workers.Add(1)
		go func(service *Service, plan string) {
			defer workers.Done()
			<-start
			session, err := service.Checkout(context.Background(), teamID, plan, "owner@example.com")
			results <- session
			errors <- err
		}(request.service, request.plan)
	}

	close(start)
	workers.Wait()
	close(results)
	close(errors)

	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for session := range results {
		if session == nil || session.ID != "cs_single" {
			t.Fatalf("concurrent checkout returned %+v", session)
		}
	}

	// A fresh Store represents a restarted process and must still recover the
	// same open session.
	restarted := first
	restarted.Store = NewStore(h.control)
	restarted.Store.Now = func() time.Time { return h.now() }
	session, err := restarted.Checkout(context.Background(), teamID, "yearly", "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "cs_single" {
		t.Fatalf("restarted checkout returned %q", session.ID)
	}

	providerMu.Lock()
	defer providerMu.Unlock()
	if creates != 1 {
		t.Fatalf("concurrent and restarted checkout created %d Stripe sessions, want 1", creates)
	}
}

// TestCheckoutRecoversAfterSessionPersistenceFailure simulates a crash after
// Stripe creates a session but before its id is stored. A restarted process
// must reuse the durable pre-provider idempotency key and recover that session.
func TestCheckoutRecoversAfterSessionPersistenceFailure(t *testing.T) {
	h := newHarness(t)

	var providerMu sync.Mutex
	requests := 0
	keys := make(map[string]int)
	var prices []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/checkout/sessions" {
			http.NotFound(w, r)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		providerMu.Lock()
		requests++
		keys[r.Header.Get("Idempotency-Key")]++
		prices = append(prices, r.PostForm.Get("line_items[0][price]"))
		providerMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cs_recovered","status":"open","url":"https://checkout.example/recovered"}`))
	}))
	t.Cleanup(server.Close)

	service := *h.service
	service.Stripe = stripe.New("sk_test_fake")
	service.Stripe.BaseURL = server.URL
	service.Store = NewStore(h.control)
	service.Store.Now = func() time.Time { return h.now() }

	if _, err := h.control.Exec(`
		CREATE TRIGGER fail_checkout_session_save
		BEFORE UPDATE OF status ON billing_checkouts
		WHEN NEW.status = 'open'
		BEGIN
			SELECT RAISE(FAIL, 'simulated checkout persistence failure');
		END
	`); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Checkout(context.Background(), teamID, "monthly", "owner@example.com"); err == nil {
		t.Fatal("checkout unexpectedly survived the simulated persistence failure")
	}
	if _, err := h.control.Exec(`DROP TRIGGER fail_checkout_session_save`); err != nil {
		t.Fatal(err)
	}

	secondControl, err := store.Open(h.controlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { secondControl.Close() })

	restarted := service
	restarted.Store = NewStore(secondControl)
	restarted.Store.Now = func() time.Time { return h.now() }
	session, err := restarted.Checkout(context.Background(), teamID, "yearly", "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if session.ID != "cs_recovered" {
		t.Fatalf("restarted checkout returned %q", session.ID)
	}

	providerMu.Lock()
	defer providerMu.Unlock()
	if requests != 2 || len(keys) != 1 || keys[""] != 0 {
		t.Fatalf("provider requests=%d idempotency keys=%v, want two requests with one non-empty key", requests, keys)
	}
	if len(prices) != 2 || prices[0] != "price_monthly" || prices[1] != "price_monthly" {
		t.Fatalf("live claim changed plans across restart: %v", prices)
	}
}

// TestExpiredCheckoutClaimIsReplacedAtTheBoundary exercises failure, restart,
// exact expiry, concurrent reclaim, a monthly-to-yearly change, and a late
// response from the abandoned provider call. Stripe idempotency is deliberately
// not reused after expiry; the old session is explicitly neutralized instead.
func TestExpiredCheckoutClaimIsReplacedAtTheBoundary(t *testing.T) {
	h := newHarness(t)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})

	var providerMu sync.Mutex
	var forms []url.Values
	var keys []string
	expiredOld := 0
	oldExpired := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			providerMu.Lock()
			index := len(forms)
			forms = append(forms, r.PostForm)
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			providerMu.Unlock()
			if index == 0 {
				close(firstStarted)
				<-releaseFirst
				_, _ = w.Write([]byte(`{"id":"cs_old","status":"open","url":"https://checkout.example/old"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"cs_new","status":"open","url":"https://checkout.example/new"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions":
			providerMu.Lock()
			expired := oldExpired
			providerMu.Unlock()
			if expired {
				_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[]}`))
			} else {
				_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[{"id":"cs_old","status":"open","metadata":{"feasible_team_id":"1"}}]}`))
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions/cs_new":
			_, _ = w.Write([]byte(`{"id":"cs_new","status":"open","url":"https://checkout.example/new"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions/cs_old":
			providerMu.Lock()
			expired := oldExpired
			providerMu.Unlock()
			status := "open"
			if expired {
				status = "expired"
			}
			fmt.Fprintf(w, `{"id":"cs_old","status":%q,"metadata":{"feasible_team_id":"1"}}`, status)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions/cs_old/expire":
			providerMu.Lock()
			expiredOld++
			oldExpired = true
			providerMu.Unlock()
			_, _ = w.Write([]byte(`{"id":"cs_old","status":"expired"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	first := *h.service
	first.Stripe = stripe.New("sk_test_fake")
	first.Stripe.BaseURL = server.URL
	first.Store = NewStore(h.control)
	first.Store.Now = func() time.Time { return h.now() }
	firstResult := make(chan error, 1)
	go func() {
		_, err := first.Checkout(context.Background(), teamID, "monthly", "owner@example.com")
		firstResult <- err
	}()
	<-firstStarted

	secondControl, err := store.Open(h.controlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { secondControl.Close() })
	second := first
	second.Store = NewStore(secondControl)
	second.Store.Now = func() time.Time { return h.now() }

	// One second before expiry, a restarted process cannot steal the live
	// account lease or replace its checkout claim.
	h.travel(accountLeaseDuration - time.Second)
	liveCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := second.Checkout(liveCtx, teamID, "yearly", "owner@example.com"); err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("live claim replacement returned %v", err)
	}

	thirdControl, err := store.Open(h.controlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { thirdControl.Close() })
	third := first
	third.Store = NewStore(thirdControl)
	third.Store.Now = func() time.Time { return h.now() }

	// At the exact boundary, two restarted workers race to replace monthly with
	// yearly. The durable lease permits one create; the other reads its session.
	h.travel(time.Second)
	start := make(chan struct{})
	results := make(chan *stripe.CheckoutSession, 2)
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for _, service := range []*Service{&second, &third} {
		workers.Add(1)
		go func(service *Service) {
			defer workers.Done()
			<-start
			session, err := service.Checkout(context.Background(), teamID, "yearly", "owner@example.com")
			results <- session
			errors <- err
		}(service)
	}
	close(start)
	workers.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for session := range results {
		if session == nil || session.ID != "cs_new" {
			t.Fatalf("concurrent expiry reclaim returned %+v", session)
		}
	}

	close(releaseFirst)
	if err := <-firstResult; err == nil ||
		(!strings.Contains(err.Error(), "claim was replaced") && !strings.Contains(err.Error(), "lease expired or was replaced")) {
		t.Fatalf("late old checkout returned %v", err)
	}

	claim, found, err := second.Store.CheckoutClaimForAccount(context.Background(), teamID)
	if err != nil || !found {
		t.Fatalf("replacement claim found=%t error=%v", found, err)
	}
	var cleanupRows int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM billing_checkout_cleanup`).Scan(&cleanupRows); err != nil {
		t.Fatal(err)
	}
	providerMu.Lock()
	defer providerMu.Unlock()
	if len(forms) != 2 || len(keys) != 2 {
		t.Fatalf("provider creates forms=%d keys=%d, want old plus one replacement", len(forms), len(keys))
	}
	if forms[0].Get("line_items[0][price]") != "price_monthly" || forms[1].Get("line_items[0][price]") != "price_yearly" {
		t.Fatalf("provider plan attempts are %q then %q", forms[0].Get("line_items[0][price]"), forms[1].Get("line_items[0][price]"))
	}
	if keys[0] == "" || keys[1] == "" || keys[0] == keys[1] || claim.IdempotencyKey != keys[1] {
		t.Fatalf("replacement keys=%v stored=%q", keys, claim.IdempotencyKey)
	}
	if claim.Plan != "yearly" || claim.PriceID != "price_yearly" || claim.SessionID != "cs_new" || expiredOld != 2 || cleanupRows != 0 {
		t.Fatalf("replacement claim=%+v expired_old=%d cleanup=%d", claim, expiredOld, cleanupRows)
	}
}

// TestExpiredCheckoutRestartPersistsCleanupFailure proves recovery does not
// depend on Stripe retaining an idempotency key. An orphan is discovered by
// metadata, persisted before expiration, and blocks replacement until cleanup
// succeeds on a later process attempt.
func TestExpiredCheckoutRestartPersistsCleanupFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	old, err := h.service.Store.NewCheckoutClaim(ctx, teamID, "monthly", "price_monthly", "", "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	h.travel(accountLeaseDuration)

	var mu sync.Mutex
	expireCalls := 0
	created := 0
	expired := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions":
			if expired {
				_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[]}`))
			} else {
				_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[{"id":"cs_orphan","status":"open","metadata":{"feasible_team_id":"1"}}]}`))
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions/cs_orphan":
			status := "open"
			if expired {
				status = "expired"
			}
			fmt.Fprintf(w, `{"id":"cs_orphan","status":%q}`, status)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions/cs_orphan/expire":
			expireCalls++
			if expireCalls == 1 {
				http.Error(w, `{"error":{"type":"api_error","message":"temporary"}}`, http.StatusInternalServerError)
				return
			}
			expired = true
			_, _ = w.Write([]byte(`{"id":"cs_orphan","status":"expired"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
			created++
			_, _ = w.Write([]byte(`{"id":"cs_new","status":"open","url":"https://checkout.example/new"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	h.service.Stripe = stripe.New("sk_test_fake")
	h.service.Stripe.BaseURL = server.URL

	if _, err := h.service.Checkout(ctx, teamID, "yearly", "owner@example.com"); err == nil || !strings.Contains(err.Error(), "temporary") {
		t.Fatalf("first orphan cleanup returned %v", err)
	}
	var cleanup int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM billing_checkout_cleanup WHERE session_id = 'cs_orphan'`).Scan(&cleanup); err != nil {
		t.Fatal(err)
	}
	if cleanup != 1 || created != 0 {
		t.Fatalf("failed cleanup rows=%d provider creates=%d", cleanup, created)
	}

	session, err := h.service.Checkout(ctx, teamID, "yearly", "owner@example.com")
	if err != nil || session == nil || session.ID != "cs_new" {
		t.Fatalf("recovered checkout session=%+v error=%v", session, err)
	}
	claim, found, err := h.service.Store.CheckoutClaimForAccount(ctx, teamID)
	if err != nil || !found {
		t.Fatalf("replacement claim found=%t error=%v", found, err)
	}
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM billing_checkout_cleanup`).Scan(&cleanup); err != nil {
		t.Fatal(err)
	}
	if cleanup != 0 || created != 1 || claim.Plan != "yearly" || claim.IdempotencyKey == old.IdempotencyKey {
		t.Fatalf("cleanup=%d creates=%d claim=%+v old_key=%q", cleanup, created, claim, old.IdempotencyKey)
	}
}

// TestCompletedCheckoutTruthBlocksOnlyAnUntrackedSubscription distinguishes an
// orphan completion from harmless history. A first customer's completed session
// blocks replacement, while an existing customer's terminal subscription may
// start a new checkout after its historical completion is inspected.
func TestCompletedCheckoutTruthBlocksOnlyAnUntrackedSubscription(t *testing.T) {
	for _, tc := range []struct {
		name             string
		existingCustomer bool
		wantCreate       int
		wantError        string
	}{
		{name: "untracked first customer", wantError: "completed before it could be recorded locally"},
		{name: "terminal existing customer", existingCustomer: true, wantCreate: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			customer := ""
			if tc.existingCustomer {
				customer = customerID
				if err := h.service.Store.Save(ctx, Subscription{
					TeamID: teamID, CustomerID: customerID, SubscriptionID: "sub_terminal",
					Status: stripe.StatusCanceled, Plan: "monthly", PriceID: "price_monthly",
					PaymentState: PaymentFailed,
				}); err != nil {
					t.Fatal(err)
				}
			}
			claim, err := h.service.Store.NewCheckoutClaim(ctx, teamID, "monthly", "price_monthly", customer, "owner@example.com")
			if err != nil {
				t.Fatal(err)
			}
			if tc.existingCustomer {
				if err := h.service.Store.SaveCheckoutSession(ctx, claim, "cs_complete", "", "complete"); err != nil {
					t.Fatal(err)
				}
			} else {
				h.travel(accountLeaseDuration)
			}

			creates := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions":
					_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[{"id":"sub_terminal","status":"canceled"}]}`))
				case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions":
					_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[{"id":"cs_complete","status":"complete","subscription":"sub_terminal","metadata":{"feasible_team_id":"1"}}]}`))
				case r.Method == http.MethodGet && r.URL.Path == "/v1/checkout/sessions/cs_complete":
					_, _ = w.Write([]byte(`{"id":"cs_complete","status":"complete","subscription":"sub_terminal","metadata":{"feasible_team_id":"1"}}`))
				case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
					creates++
					_, _ = w.Write([]byte(`{"id":"cs_replacement","status":"open","url":"https://checkout.example/replacement"}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			h.service.Stripe = stripe.New("sk_test_fake")
			h.service.Stripe.BaseURL = server.URL

			session, err := h.service.Checkout(ctx, teamID, "yearly", "owner@example.com")
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("completed orphan session=%+v error=%v", session, err)
				}
			} else if err != nil || session == nil || session.ID != "cs_replacement" {
				t.Fatalf("terminal replacement session=%+v error=%v", session, err)
			}
			if creates != tc.wantCreate {
				t.Fatalf("provider creates=%d, want %d", creates, tc.wantCreate)
			}
		})
	}
}

// TestExpiredAccountLeaseCannotContinueOrReleaseReplacement proves token and
// deadline fencing across independent Store connections.
func TestExpiredAccountLeaseCannotContinueOrReleaseReplacement(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first, err := h.service.Store.AcquireAccountLease(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	h.travel(accountLeaseDuration)
	if err := first.Renew(ctx); err == nil || !strings.Contains(err.Error(), "expired or was replaced") {
		t.Fatalf("expired lease renewed with %v", err)
	}

	secondControl, err := store.Open(h.controlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { secondControl.Close() })
	secondStore := NewStore(secondControl)
	secondStore.Now = func() time.Time { return h.now() }
	second, err := secondStore.AcquireAccountLease(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	var leases int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM billing_account_leases WHERE team_id = 1`).Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if leases != 1 {
		t.Fatalf("expired release removed replacement: rows=%d", leases)
	}
	second.Release()
}

// TestCheckoutRefusesAnExistingChargeableSubscription ensures plan changes use
// the portal and cannot create a second subscription beside the mirrored one.
func TestCheckoutRefusesAnExistingChargeableSubscription(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.service.Store.Save(ctx, Subscription{
		TeamID:         teamID,
		CustomerID:     customerID,
		SubscriptionID: "sub_test_1",
		Status:         stripe.StatusActive,
		Plan:           "monthly",
		PriceID:        "price_monthly",
		PaymentState:   PaymentPaid,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.service.Checkout(ctx, teamID, "yearly", "owner@example.com"); err == nil || !strings.Contains(err.Error(), "billing portal") {
		t.Fatalf("existing subscription checkout returned %v", err)
	}

	var claims int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM billing_checkouts WHERE team_id = 1`).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 0 {
		t.Fatalf("blocked plan change wrote %d checkout claims", claims)
	}
}

// TestMixedSubscriptionHistoryCannotHideChargeableTruth proves every page and
// every subscription participates in billing decisions. An older canceled
// annual plan cannot hide a newer active monthly plan, and payment evidence for
// the canceled subscription cannot be attached to the selected live one.
func TestMixedSubscriptionHistoryCannotHideChargeableTruth(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	creates := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions" && r.URL.Query().Get("starting_after") == "":
			_, _ = w.Write([]byte(`{"object":"list","has_more":true,"data":[{
				"id":"sub_old_annual","created":100,"customer":"cus_test_1","status":"canceled",
				"current_period_end":1000,"items":{"data":[{"price":{"id":"price_yearly"}}]}
			}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions":
			_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[{
				"id":"sub_new_monthly","created":200,"customer":"cus_test_1","status":"active",
				"current_period_end":2000,"items":{"data":[{"price":{"id":"price_monthly"}}]}
			}]}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/customers/"):
			_, _ = w.Write([]byte(`{"id":"cus_test_1","email":"owner@example.com"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
			creates++
			_, _ = w.Write([]byte(`{"id":"cs_forbidden","status":"open"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	h.service.Stripe = stripe.New("sk_test_fake")
	h.service.Stripe.BaseURL = server.URL

	if err := h.service.Store.Save(ctx, Subscription{
		TeamID: teamID, CustomerID: customerID, SubscriptionID: "sub_old_annual",
		Status: stripe.StatusCanceled, Plan: "yearly", PriceID: "price_yearly",
		PaymentState: PaymentFailed,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Checkout(ctx, teamID, "monthly", "owner@example.com"); err == nil || !strings.Contains(err.Error(), "sub_new_monthly") {
		t.Fatalf("mixed history checkout returned %v", err)
	}
	if creates != 0 {
		t.Fatalf("mixed history created %d checkout sessions", creates)
	}

	applied, err := h.service.Reconcile(ctx, teamID, customerID, PaymentUpdate{
		State: PaymentPaid, SubscriptionID: "sub_old_annual", SourceID: "in_old",
		SourceCreated: 300, EventCreated: 300,
		Trigger: stripe.EventInvoicePaymentSucceed, RequireSubscriptionMatch: true,
	})
	if err != nil || !applied {
		t.Fatalf("historical evidence reconciliation applied=%t error=%v", applied, err)
	}
	mirror, err := h.service.Store.Load(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.SubscriptionID != "sub_new_monthly" || mirror.Plan != "monthly" || mirror.PaymentState != PaymentPending {
		t.Fatalf("historical invoice selected misleading mirror %+v", mirror)
	}
}

// TestReturnURLPreservesStripePlaceholderAndEscapesValues pins the only
// deliberate exception to ordinary query escaping: Stripe requires literal
// braces around its Checkout Session placeholder, while every other value must
// remain URL encoded.
func TestReturnURLPreservesStripePlaceholderAndEscapesValues(t *testing.T) {
	h := newHarness(t)
	got := h.service.returnURL("/billing/done", url.Values{
		"session": {"{CHECKOUT_SESSION_ID}"},
		"team":    {"2"},
		"label":   {"month & year"},
	})
	want := "https://feasible.lol/billing/done?label=month+%26+year&session={CHECKOUT_SESSION_ID}&team=2"
	if got != want {
		t.Fatalf("return URL is %q, want %q", got, want)
	}
}

// TestPendingNeverReplacesTerminalEvidence pins the monotonic source rule for a
// Checkout Session even when its non-terminal completion event is created later.
func TestPendingNeverReplacesTerminalEvidence(t *testing.T) {
	for _, terminal := range []string{PaymentPaid, PaymentFailed} {
		t.Run(terminal, func(t *testing.T) {
			h := newHarness(t)

			eventType := stripe.EventCheckoutAsyncPaymentSucceeded
			if terminal == PaymentFailed {
				eventType = stripe.EventCheckoutAsyncPaymentFailed
			}

			h.deliverCreated("evt_terminal", eventType, checkoutObject("unpaid"), h.now().Add(-time.Minute))
			h.deliverCreated("evt_pending_late", stripe.EventCheckoutCompleted, checkoutObject("unpaid"), h.now())

			mirror, err := h.service.Store.Load(context.Background(), teamID)
			if err != nil {
				t.Fatal(err)
			}
			if mirror.PaymentState != terminal {
				t.Fatalf("late pending changed %q terminal evidence to %q", terminal, mirror.PaymentState)
			}
		})
	}
}

// TestDuplicateDeliveryIsIgnored is the concurrent idempotency guarantee. Many
// copies can race through claim, but exactly one may read the provider and apply.
func TestDuplicateDeliveryIsIgnored(t *testing.T) {
	h := newHarness(t)

	const deliveries = 16
	responses := make(chan *httptest.ResponseRecorder, deliveries)
	start := make(chan struct{})
	var workers sync.WaitGroup

	for i := 0; i < deliveries; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			responses <- h.deliver("evt_dup", stripe.EventInvoicePaymentSucceed, invoiceObject())
		}()
	}

	close(start)
	workers.Wait()
	close(responses)

	applied := 0
	duplicates := 0
	processing := 0
	for response := range responses {
		switch {
		case response.Code == http.StatusOK && strings.Contains(response.Body.String(), OutcomeApplied):
			applied++
		case response.Code == http.StatusOK && strings.Contains(response.Body.String(), OutcomeDuplicate):
			duplicates++
		case response.Code == http.StatusInternalServerError:
			processing++
		default:
			t.Errorf("unexpected status %d and outcome: %s", response.Code, response.Body.String())
		}
	}

	if applied != 1 || duplicates+processing != deliveries-1 {
		t.Errorf("outcomes are applied=%d duplicate=%d processing=%d", applied, duplicates, processing)
	}
	if reads := h.provider.reads(); reads != 1 {
		t.Errorf("concurrent duplicates caused %d provider reads, want 1", reads)
	}

	var rows int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM stripe_events WHERE event_id = 'evt_dup'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("%d deliveries wrote %d event rows, want 1", deliveries, rows)
	}
}

// TestStaleEventClaimCanBeRecovered ensures an in-progress duplicate is
// retryable and that only the claimant holding the renewed lease may finish.
func TestStaleEventClaimCanBeRecovered(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	payload := []byte(`{"id":"evt_lease"}`)

	first, err := h.service.Store.ClaimEvent(ctx, "evt_lease", stripe.EventInvoicePaymentSucceed, teamID, payload)
	if err != nil || !first.Claimed {
		t.Fatalf("first claim is %+v, error %v", first, err)
	}

	inProgress, err := h.service.Store.ClaimEvent(ctx, "evt_lease", stripe.EventInvoicePaymentSucceed, teamID, payload)
	if err != nil || inProgress.Claimed || !inProgress.Processing {
		t.Fatalf("in-progress claim is %+v, error %v", inProgress, err)
	}

	h.travel(eventClaimLease + time.Second)
	recovered, err := h.service.Store.ClaimEvent(ctx, "evt_lease", stripe.EventInvoicePaymentSucceed, teamID, payload)
	if err != nil || !recovered.Claimed {
		t.Fatalf("recovered claim is %+v, error %v", recovered, err)
	}

	if err := h.service.Store.FinishEvent(ctx, "evt_lease", first, OutcomeApplied, teamID, nil); err == nil {
		t.Fatal("expired claimant was allowed to finish the renewed lease")
	}
	if err := h.service.Store.FinishEvent(ctx, "evt_lease", recovered, OutcomeApplied, teamID, nil); err != nil {
		t.Fatalf("recovered claimant could not finish: %v", err)
	}
}

// TestOutOfOrderDeliveryDoesNotUndoAPayment is the property the whole design
// exists for. A stale `payment_failed` arriving after the retry succeeded must
// not lapse a paying customer — and it does not, because the handler asks the
// provider what is true now rather than believing the payload.
func TestOutOfOrderDeliveryDoesNotUndoAPayment(t *testing.T) {
	h := newHarness(t)

	// The world: the subscription is active, and a settlement event is newer than
	// the failed attempt that is still in transit.
	h.provider.set(stripe.StatusActive, false)
	invoice := invoiceObjectFor("in_recovered", "sub_test_1", h.now().Add(-10*time.Minute))
	paidAt := h.now().Add(-time.Minute)
	failedAt := h.now().Add(-2 * time.Minute)

	paid := h.deliverCreated("evt_paid_first", stripe.EventInvoicePaymentSucceed, invoice, paidAt)
	if paid.Code != http.StatusOK {
		t.Fatalf("paid status %d: %s", paid.Code, paid.Body.String())
	}

	// A failure event that was generated minutes ago, delayed in transit, and is
	// only arriving now. Its payload says past_due.
	response := h.deliverCreated("evt_stale_fail", stripe.EventInvoicePaymentFailed, invoice, failedAt)
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

// TestInvoiceFinalizationFailureRevokesPaidEvidence covers Stripe's documented
// state where the subscription remains active although the invoice cannot be
// finalized or collected.
func TestInvoiceFinalizationFailureRevokesPaidEvidence(t *testing.T) {
	h := newHarness(t)
	h.provider.set(stripe.StatusActive, false)

	paidAt := h.now().Add(-2 * time.Hour)
	h.deliverCreated("evt_initial_paid", stripe.EventInvoicePaymentSucceed,
		invoiceObjectFor("in_initial", "sub_test_1", paidAt), paidAt.Add(time.Minute))
	if got := h.phase(); got != lifecycle.PhaseActive {
		t.Fatalf("initial settlement left phase %q", got)
	}

	failedAt := h.now().Add(-time.Hour)
	response := h.deliverCreated("evt_finalize_failed", stripe.EventInvoiceFinalizationFailed,
		invoiceObjectFor("in_uncollectable", "sub_test_1", failedAt), failedAt.Add(time.Minute))
	if response.Code != http.StatusOK {
		t.Fatalf("finalization failure status %d: %s", response.Code, response.Body.String())
	}

	mirror, err := h.service.Store.Load(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.Status != stripe.StatusActive || mirror.PaymentState != PaymentFailed {
		t.Fatalf("active uncollectable subscription is status=%q payment=%q", mirror.Status, mirror.PaymentState)
	}
	if got := h.phase(); got != lifecycle.PhaseGrace {
		t.Fatalf("finalization failure retained access in phase %q", got)
	}
}

// TestInvoiceEvidenceRequiresCurrentSubscriptionID rejects one-off invoices,
// malformed Basil parents, and invoices for another subscription on a customer.
func TestInvoiceEvidenceRequiresCurrentSubscriptionID(t *testing.T) {
	h := newHarness(t)
	h.deliver("evt_paid", stripe.EventInvoicePaymentSucceed, invoiceObject())

	missing := fmt.Sprintf(`{"id":"in_missing","object":"invoice","customer":%q,"metadata":{"feasible_team_id":"1"}}`, customerID)
	wrongParent := fmt.Sprintf(`{"id":"in_quote","object":"invoice","customer":%q,"subscription":"sub_test_1","parent":{"type":"quote_details"},"metadata":{"feasible_team_id":"1"}}`, customerID)
	mismatch := invoiceObjectFor("in_other", "sub_other", h.now())

	for i, object := range []string{missing, wrongParent, mismatch} {
		response := h.deliver(fmt.Sprintf("evt_ignored_invoice_%d", i), stripe.EventInvoicePaymentFailed, object)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), OutcomeIgnored) {
			t.Errorf("invoice %d was not ignored: status=%d body=%s", i, response.Code, response.Body.String())
		}
	}

	mirror, err := h.service.Store.Load(context.Background(), teamID)
	if err != nil {
		t.Fatal(err)
	}
	if mirror.PaymentState != PaymentPaid || h.phase() != lifecycle.PhaseActive {
		t.Fatalf("unmatched invoice changed payment=%q phase=%q", mirror.PaymentState, h.phase())
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

	// Resuming changes subscription collection state, but it does not prove a
	// charge settled and therefore cannot clear the lapse by itself.
	dayZero := h.clockStart()
	h.provider.set(stripe.StatusActive, false)
	h.deliver("evt_resumed", stripe.EventSubscriptionResumed, subscriptionObject(stripe.StatusActive))

	if got := h.phase(); got != lifecycle.PhaseGrace {
		t.Fatalf("resumed without settlement changed phase to %q", got)
	}
	if got := h.clockStart(); !got.Equal(dayZero) {
		t.Fatalf("resumed moved the lapse clock from %s to %s", dayZero, got)
	}

	// A later invoice settlement is the evidence that restores access.
	h.deliver("evt_paid_after_resume", stripe.EventInvoicePaymentSucceed, invoiceObject())
	if got := h.phase(); got != lifecycle.PhaseActive {
		t.Fatalf("settlement after resume left phase %q, want active", got)
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

// TestDay90RevalidatesSettlementBeforeDeletion reproduces the provider/local
// race exactly: payment settles after reconciliation reads the subscription but
// before it writes the failed mirror. Day 90 must recover from paid_at evidence,
// and a delayed webhook must repair a mirror write that failed after lifecycle
// activation instead of leaving the account locked or deleting it.
func TestDay90RevalidatesSettlementBeforeDeletion(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	failureAt := h.now().Add(-lifecycle.DeletionDays * lifecycle.Day)
	paidAt := h.now().Add(-time.Hour)

	h.provider.mu.Lock()
	h.provider.status = stripe.StatusActive
	h.provider.invoiceStatus = "open"
	h.provider.invoiceAutoAdvance = true
	h.provider.settleOnCustomerAt = paidAt.Unix()
	h.provider.mu.Unlock()

	applied, err := h.service.Reconcile(ctx, teamID, customerID, PaymentUpdate{
		State:                    PaymentFailed,
		SubscriptionID:           "sub_test_1",
		SourceID:                 "in_failed",
		SourceCreated:            failureAt.Unix(),
		EventCreated:             failureAt.Unix(),
		Trigger:                  stripe.EventInvoicePaymentFailed,
		RequireSubscriptionMatch: true,
	})
	if err != nil || !applied {
		t.Fatalf("failed reconciliation applied=%t error=%v", applied, err)
	}
	if mirror, err := h.service.Store.Load(ctx, teamID); err != nil || mirror.PaymentState != PaymentFailed {
		t.Fatalf("local mirror after provider-side settlement is %+v error=%v", mirror, err)
	}

	// Make the first post-recovery mirror write crash. The lifecycle transition
	// occurs first, so this fault must not leave an otherwise-paid account locked.
	if _, err := h.control.Exec(`
		CREATE TRIGGER fail_paid_mirror
		BEFORE UPDATE OF payment_state ON subscriptions
		WHEN NEW.payment_state = 'paid'
		BEGIN
			SELECT RAISE(FAIL, 'simulated paid mirror crash');
		END
	`); err != nil {
		t.Fatal(err)
	}

	h.service.Lifecycle.Purger.Payments = h.service
	h.service.Lifecycle.Purger.Customers = h.service
	state, err := lifecycle.NewStore(h.control).Load(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	err = h.service.Lifecycle.Purger.Purge(ctx, lifecycle.Account{
		TeamID: 1, TeamName: "Example Co", Email: "owner@example.com",
		CustomerID: customerID, State: state,
	}, h.now())
	if err == nil || !strings.Contains(err.Error(), "simulated paid mirror crash") {
		t.Fatalf("day-90 recovery returned %v", err)
	}
	if got := h.phase(); got != lifecycle.PhaseActive {
		t.Fatalf("failed mirror write left paid lifecycle in %q, want active", got)
	}
	var deletions int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM account_deletions WHERE team_id = 1`).Scan(&deletions); err != nil {
		t.Fatal(err)
	}
	if deletions != 0 {
		t.Fatalf("provider-paid account acquired %d deletion audits", deletions)
	}

	if _, err := h.control.Exec(`DROP TRIGGER fail_paid_mirror`); err != nil {
		t.Fatal(err)
	}
	response := h.deliverCreated("evt_paid_after_day_90", stripe.EventInvoicePaymentSucceed,
		invoiceObjectFor("in_test_1", "sub_test_1", paidAt), paidAt)
	if response.Code != http.StatusOK {
		t.Fatalf("delayed paid event answered %d: %s", response.Code, response.Body.String())
	}
	if mirror, err := h.service.Store.Load(ctx, teamID); err != nil || mirror.PaymentState != PaymentPaid {
		t.Fatalf("delayed paid event left mirror %+v error=%v", mirror, err)
	}

	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if h.provider.paused || h.provider.customerDeleted {
		t.Fatalf("recovered provider state paused=%t deleted=%t", h.provider.paused, h.provider.customerDeleted)
	}
}

// TestDay90RecoversSettlementAtTheVoidFence covers the last provider race: an
// invoice settles after the final pre-mutation read, exactly when deletion tries
// to void it. The failed void is re-read as paid_at evidence and recovery wins.
func TestDay90RecoversSettlementAtTheVoidFence(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	failureAt := h.now().Add(-lifecycle.DeletionDays * lifecycle.Day)
	paidAt := h.now().Add(-time.Second)

	if err := h.service.Store.Save(ctx, Subscription{
		TeamID: teamID, CustomerID: customerID, SubscriptionID: "sub_test_1",
		Status: stripe.StatusPastDue, Plan: "monthly", PriceID: "price_monthly",
		PaymentState: PaymentFailed, PaymentFailedAt: failureAt,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Lifecycle.SignalAt(ctx, teamID, lifecycle.SignalPaymentFailed, failureAt); err != nil {
		t.Fatal(err)
	}
	h.provider.mu.Lock()
	h.provider.status = stripe.StatusPastDue
	h.provider.invoiceStatus = "open"
	h.provider.invoiceAutoAdvance = true
	h.provider.settleOnVoidAt = paidAt.Unix()
	h.provider.mu.Unlock()

	h.service.Lifecycle.Purger.Payments = h.service
	h.service.Lifecycle.Purger.Customers = h.service
	state, err := lifecycle.NewStore(h.control).Load(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.Lifecycle.Purger.Purge(ctx, lifecycle.Account{
		TeamID: teamID, TeamName: "Example Co", Email: "owner@example.com",
		CustomerID: customerID, State: state,
	}, h.now()); err != nil {
		t.Fatal(err)
	}

	var teams, deletions, quiescence int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM teams WHERE id = 1`:                           &teams,
		`SELECT COUNT(*) FROM account_deletions WHERE team_id = 1`:          &deletions,
		`SELECT COUNT(*) FROM billing_quiescence_objects WHERE team_id = 1`: &quiescence,
	} {
		if err := h.control.QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if teams != 1 || deletions != 0 || quiescence != 0 || h.phase() != lifecycle.PhaseActive {
		t.Fatalf("settled-at-void recovery teams=%d deletions=%d quiescence=%d phase=%q",
			teams, deletions, quiescence, h.phase())
	}
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if h.provider.paused || h.provider.customerDeleted || h.provider.invoiceStatus != "paid" {
		t.Fatalf("settled-at-void provider paused=%t deleted=%t invoice=%q",
			h.provider.paused, h.provider.customerDeleted, h.provider.invoiceStatus)
	}
}

// TestDay90RestoresDurableQuiescenceAfterProcessCrash seeds the exact state left
// by a crash after provider mutation. A paid mirror causes the replacement
// worker to restore every recorded object and clear the durable recovery rows.
func TestDay90RestoresDurableQuiescenceAfterProcessCrash(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	failureAt := h.now().Add(-lifecycle.DeletionDays * lifecycle.Day)
	if _, err := h.service.Lifecycle.SignalAt(ctx, teamID, lifecycle.SignalPaymentFailed, failureAt); err != nil {
		t.Fatal(err)
	}
	if err := h.service.Store.Save(ctx, Subscription{
		TeamID: teamID, CustomerID: customerID, SubscriptionID: "sub_test_1",
		Status: stripe.StatusActive, Plan: "monthly", PriceID: "price_monthly",
		PaymentState: PaymentPaid, EvidenceEventAt: h.now().Add(-time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, object := range []QuiescenceObject{{Type: "subscription", ID: "sub_test_1"}, {Type: "invoice", ID: "in_test_1"}} {
		if err := h.service.Store.RememberQuiescence(ctx, teamID, object.Type, object.ID); err != nil {
			t.Fatal(err)
		}
	}
	h.provider.mu.Lock()
	h.provider.status = stripe.StatusActive
	h.provider.paused = true
	h.provider.invoiceStatus = "draft"
	h.provider.invoiceAutoAdvance = false
	h.provider.mu.Unlock()

	h.service.Lifecycle.Purger.Payments = h.service
	state, err := lifecycle.NewStore(h.control).Load(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.Lifecycle.Purger.Purge(ctx, lifecycle.Account{
		TeamID: teamID, TeamName: "Example Co", Email: "owner@example.com",
		CustomerID: customerID, State: state,
	}, h.now()); err != nil {
		t.Fatal(err)
	}

	var recoveryRows int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM billing_quiescence_objects WHERE team_id = 1`).Scan(&recoveryRows); err != nil {
		t.Fatal(err)
	}
	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if recoveryRows != 0 || h.provider.paused || !h.provider.invoiceAutoAdvance || h.phase() != lifecycle.PhaseActive {
		t.Fatalf("crash recovery rows=%d paused=%t invoice_auto=%t phase=%q",
			recoveryRows, h.provider.paused, h.provider.invoiceAutoAdvance, h.phase())
	}
}

// TestPurgeLeaseWinsAgainstIndependentReconciliation proves the day-90 worker
// and a second process share one durable account lease. Reconciliation waits
// while provider collection is quiesced, then observes the immutable deletion
// audit even though the team and its lease row have already cascaded away.
func TestPurgeLeaseWinsAgainstIndependentReconciliation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	failureAt := h.now().Add(-lifecycle.DeletionDays * lifecycle.Day)

	if err := h.service.Store.Save(ctx, Subscription{
		TeamID: teamID, CustomerID: customerID, SubscriptionID: "sub_test_1",
		Status: stripe.StatusPastDue, Plan: "monthly", PriceID: "price_monthly",
		PaymentState: PaymentFailed, PaymentFailedAt: failureAt,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Lifecycle.SignalAt(ctx, teamID, lifecycle.SignalPaymentFailed, failureAt); err != nil {
		t.Fatal(err)
	}

	h.provider.mu.Lock()
	h.provider.status = stripe.StatusPastDue
	h.provider.invoiceStatus = "open"
	h.provider.invoiceAutoAdvance = true
	h.provider.pauseStarted = make(chan struct{})
	h.provider.continuePause = make(chan struct{})
	pauseStarted := h.provider.pauseStarted
	continuePause := h.provider.continuePause
	h.provider.mu.Unlock()

	h.service.Lifecycle.Purger.Payments = h.service
	h.service.Lifecycle.Purger.Customers = h.service
	state, err := lifecycle.NewStore(h.control).Load(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}

	secondControl, err := store.Open(h.controlPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { secondControl.Close() })
	second := *h.service
	second.Store = NewStore(secondControl)
	second.Store.Now = func() time.Time { return h.now() }

	purgeErr := make(chan error, 1)
	go func() {
		purgeErr <- h.service.Lifecycle.Purger.Purge(ctx, lifecycle.Account{
			TeamID: 1, TeamName: "Example Co", Email: "owner@example.com",
			CustomerID: customerID, State: state,
		}, h.now())
	}()
	<-pauseStarted

	type reconcileResult struct {
		applied bool
		err     error
	}
	reconciled := make(chan reconcileResult, 1)
	go func() {
		applied, err := second.Reconcile(ctx, teamID, customerID, PaymentUpdate{
			State: PaymentPaid, SubscriptionID: "sub_test_1", SourceID: "in_late",
			SourceCreated: h.now().Unix(), EventCreated: h.now().Unix(),
			Trigger: stripe.EventInvoicePaymentSucceed, RequireSubscriptionMatch: true,
		})
		reconciled <- reconcileResult{applied: applied, err: err}
	}()

	select {
	case result := <-reconciled:
		t.Fatalf("reconciliation bypassed live purge lease: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	close(continuePause)
	if err := <-purgeErr; err != nil {
		t.Fatal(err)
	}
	result := <-reconciled
	if result.err != nil || result.applied {
		t.Fatalf("post-deletion reconciliation is %+v, want ignored", result)
	}

	var completed int64
	if err := h.control.QueryRow(`SELECT completed_at FROM account_deletions WHERE team_id = 1`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if completed == 0 {
		t.Fatal("deletion audit was not completed")
	}
}

// TestLostDeletionClaimRestoresProviderCollection covers the final local race:
// a paid mirror committed before purge took the lease, but the stale lifecycle
// snapshot still reached provider quiescence. Losing the conditional deletion
// claim must restore both the subscription and its pre-existing open invoice.
func TestLostDeletionClaimRestoresProviderCollection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	failureAt := h.now().Add(-lifecycle.DeletionDays * lifecycle.Day)
	if _, err := h.service.Lifecycle.SignalAt(ctx, teamID, lifecycle.SignalPaymentFailed, failureAt); err != nil {
		t.Fatal(err)
	}
	if err := h.service.Store.Save(ctx, Subscription{
		TeamID: teamID, CustomerID: customerID, SubscriptionID: "sub_test_1",
		Status: stripe.StatusPastDue, Plan: "monthly", PriceID: "price_monthly",
		PaymentState: PaymentPaid,
	}); err != nil {
		t.Fatal(err)
	}
	h.provider.mu.Lock()
	h.provider.status = stripe.StatusPastDue
	h.provider.invoiceStatus = "open"
	h.provider.invoiceAutoAdvance = true
	h.provider.mu.Unlock()

	h.service.Lifecycle.Purger.Payments = h.service
	state, err := lifecycle.NewStore(h.control).Load(ctx, teamID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.Lifecycle.Purger.Purge(ctx, lifecycle.Account{
		TeamID: 1, TeamName: "Example Co", Email: "owner@example.com",
		CustomerID: customerID, State: state,
	}, h.now()); err != nil {
		t.Fatal(err)
	}

	h.provider.mu.Lock()
	defer h.provider.mu.Unlock()
	if h.provider.paused || !h.provider.invoiceAutoAdvance || h.provider.customerDeleted {
		t.Fatalf("lost claim left provider paused=%t invoice_auto_advance=%t deleted=%t",
			h.provider.paused, h.provider.invoiceAutoAdvance, h.provider.customerDeleted)
	}
	var teams, deletions int
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM teams WHERE id = 1`).Scan(&teams); err != nil {
		t.Fatal(err)
	}
	if err := h.control.QueryRow(`SELECT COUNT(*) FROM account_deletions WHERE team_id = 1`).Scan(&deletions); err != nil {
		t.Fatal(err)
	}
	if teams != 1 || deletions != 0 {
		t.Fatalf("lost claim teams=%d deletions=%d", teams, deletions)
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
