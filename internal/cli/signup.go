//
// signup.go
// Reporting each new account to the installation's own analytics as a Signup event.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/auth"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// SignupEventName is the custom event a new account is recorded as. A goal on
// the receiving site matches it by this name.
const SignupEventName = "Signup"

// signupEventTimeout bounds how long one report may wait on the write buffer.
const signupEventTimeout = 10 * time.Second

// signupReporter turns an account being created into one event on a site this
// installation tracks.
//
// The event goes through the same ingest handler a browser's would, carrying
// the address and user agent of the request that created the account. That is
// what gives it the same visitor identity as the pages that person just read
// on the marketing site, so a funnel from a landing page to a sign-up joins up.
// The address is used to derive that identity and is not stored.
type signupReporter struct {
	// Ingest is the event endpoint of this process.
	Ingest http.Handler

	// Domain is the site the event is recorded on.
	Domain string

	// PageURL is the address the event is reported from.
	PageURL string

	Log *logger.Logger
}

// newSignupReporter builds the reporter, or returns nil when no site was named
// to receive the event.
func newSignupReporter(e *env, service *ingest.Service) *signupReporter {
	if e.cfg.App.SignupEventDomain == "" || service == nil || service.Handler == nil {
		return nil
	}

	return &signupReporter{
		Ingest:  service.Handler,
		Domain:  e.cfg.App.SignupEventDomain,
		PageURL: e.cfg.App.BaseURL + "/register",
		Log:     e.log,
	}
}

// Hook is the function the account handler calls. It reports in the background
// so a sign-up never waits on the write buffer.
func (s *signupReporter) Hook() func(*http.Request, string, auth.Referral) {
	if s == nil {
		return nil
	}

	return func(r *http.Request, method string, referral auth.Referral) {
		event, err := s.request(r, method, referral)
		if err != nil {
			s.failed("the event could not be built", err.Error())
			return
		}

		go s.send(event)
	}
}

// request builds the event as a request to the ingest handler. It copies what
// identifies the visitor from the request that created the account, and leaves
// behind everything that identifies the account.
func (s *signupReporter) request(r *http.Request, method string, referral auth.Referral) (*http.Request, error) {
	payload := map[string]any{
		"n": SignupEventName,
		"d": s.Domain,
		"u": s.PageURL,
		"p": map[string]string{"method": method},
	}

	// A visitor whose first request to this site is the sign-up itself has no
	// session to inherit a source from, so the event states its own.
	for name, value := range map[string]string{
		"utm_source":   referral.Source,
		"utm_medium":   referral.Medium,
		"utm_campaign": referral.Campaign,
	} {
		if value != "" {
			payload[name] = value
		}
	}
	if referral.Referrer != "" {
		payload["referrer"] = "https://" + referral.Referrer + "/"
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	// The event outlives the request that caused it, which ends in a redirect
	// as soon as the account exists.
	event, err := http.NewRequestWithContext(context.WithoutCancel(r.Context()),
		http.MethodPost, "/api/event", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	event.RemoteAddr = r.RemoteAddr
	event.Header = r.Header.Clone()
	for _, name := range []string{"Cookie", "Authorization", "Content-Length", "Referer", "Origin", ingest.HeaderDebug} {
		event.Header.Del(name)
	}
	event.Header.Set("Content-Type", "application/json")

	return event, nil
}

// send hands the event to the ingest handler and logs any answer that means it
// was not counted.
func (s *signupReporter) send(event *http.Request) {
	ctx, cancel := context.WithTimeout(event.Context(), signupEventTimeout)
	defer cancel()

	answer := &signupAnswer{header: http.Header{}}
	s.Ingest.ServeHTTP(answer, event.WithContext(ctx))

	if answer.status != http.StatusAccepted {
		s.failed("the event was refused", answer.body.String())
		return
	}

	if reason := answer.header.Get(ingest.HeaderDropped); reason != "" {
		s.failed("the event was dropped", reason)
	}
}

// failed logs a sign-up that did not reach the analytics site.
func (s *signupReporter) failed(message, detail string) {
	if s.Log != nil {
		s.Log.Error("a sign-up was not recorded as an event: "+message, "domain", s.Domain, "detail", detail)
	}
}

// signupAnswer collects what the ingest handler answered.
type signupAnswer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

// Header returns the answer's headers.
func (a *signupAnswer) Header() http.Header { return a.header }

// WriteHeader records the answer's status once.
func (a *signupAnswer) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
}

// Write records the answer's body, and a status of 200 when none was set.
func (a *signupAnswer) Write(body []byte) (int, error) {
	a.WriteHeader(http.StatusOK)

	return a.body.Write(body)
}
