//
// service.go
// The sweeper: send each new account the one onboarding email it is owed.
//
// Created: 2026-11-15
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package onboarding

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// SweepInterval is how often new accounts are looked at. It bounds how long
// after verifying their address somebody waits for the welcome.
const SweepInterval = 5 * time.Minute

// SkippedOutcome is what the outbox records for an install-help email that was
// settled without being sent, because the account had started collecting.
const SkippedOutcome = "skipped: the account has data"

// Notice is one onboarding email the sweeper has decided is due.
type Notice struct {
	TeamID   int64
	Template string

	// To is the account owner, and FirstName is how they are greeted. It is
	// empty when their name gives nothing safe to greet them by.
	To        string
	FirstName string

	// MessageKey is stable across retries of the same outbox row.
	MessageKey string

	// SiteDomain is the account's first site, InstallURL is that site's
	// install screen and DashboardURL is its dashboard. All three are empty
	// when no site has been added yet.
	SiteDomain   string
	InstallURL   string
	DashboardURL string

	// Collecting is set on a welcome that goes out after the account has
	// already counted an event, which happens to anybody who installs within
	// a sweep of verifying. Their install is done, so the welcome says so.
	Collecting bool

	// AddSiteURL is where a site is added.
	AddSiteURL string

	// StopURL is the one-click way out of these emails.
	StopURL string
}

// Notifier sends one notice and reports what the transport observed.
type Notifier interface {
	Notify(ctx context.Context, notice Notice) (string, error)
}

// NotifierFunc adapts a function to the interface.
type NotifierFunc func(ctx context.Context, notice Notice) (string, error)

// Notify calls the function.
func (f NotifierFunc) Notify(ctx context.Context, notice Notice) (string, error) {
	return f(ctx, notice)
}

// Traffic answers whether an account has counted anything yet.
type Traffic interface {
	HasEvents(ctx context.Context, teamID int64) (bool, error)
}

// Outbox is the leased sent-marker table. It is *lifecycle.Store, the same
// markers the billing lifecycle emails use, which is what makes a send happen
// once across restarts and across several processes.
type Outbox interface {
	ClaimEmail(ctx context.Context, teamID int64, startedAt time.Time, template, recipient string, now time.Time) (lifecycle.EmailClaim, bool, error)
	FinishEmail(ctx context.Context, teamID int64, startedAt time.Time, template string, claim lifecycle.EmailClaim, outcome string, now time.Time) error
	FailEmail(ctx context.Context, teamID int64, startedAt time.Time, template string, claim lifecycle.EmailClaim, outcome string, now time.Time) error
}

// Linker mints the stop link for one team.
type Linker interface {
	Link(teamID int64) string
}

// Service sweeps new accounts on a timer.
type Service struct {
	Store   *Store
	Outbox  Outbox
	Traffic Traffic
	Notify  Notifier
	Stop    Linker
	Log     *logger.Logger

	// BaseURL is the address people type. Every link in every email hangs off
	// it.
	BaseURL string

	// Now is injectable so a test can walk an account through its first week.
	Now func() time.Time

	// mu serialises sweeps inside one process.
	mu sync.Mutex
}

// now returns the service's clock.
func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// Sweep looks at every account inside the window once and sends what is due.
// It returns how many emails were sent, and it does not stop at the first
// failure: one account that cannot be mailed must not hold up the rest.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	start, err := s.Store.Start(ctx)
	if err != nil {
		return 0, err
	}
	if start.IsZero() {
		return 0, nil
	}

	// Never earlier than the start, so an account that predates these emails
	// is never written to, and never earlier than the window.
	since := now.Add(-WindowDays * Day)
	if since.Before(start) {
		since = start
	}

	teams, err := s.Store.Candidates(ctx, since)
	if err != nil {
		return 0, err
	}

	var (
		sent     int
		firstErr error
	)

	for _, team := range teams {
		delivered, err := s.advance(ctx, team, now)
		if delivered {
			sent++
		}
		if err != nil {
			if s.Log != nil {
				s.Log.Error("onboarding sweep failed for one account", "team", team.ID, "error", err)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return sent, firstErr
}

// advance sends one account the email it is owed, if any, and reports whether
// one went out.
func (s *Service) advance(ctx context.Context, team Team, now time.Time) (bool, error) {
	settled, err := s.Store.Sent(ctx, team)
	if err != nil {
		return false, err
	}

	template, noData := Next(Progress{CreatedAt: team.CreatedAt, Verified: team.Verified, Sent: settled}, now)
	if template == "" {
		return false, nil
	}

	site, hasSite, err := s.Store.FirstSite(ctx, team.ID)
	if err != nil {
		return false, err
	}

	if noData && hasSite {
		collecting, err := s.Traffic.HasEvents(ctx, team.ID)
		if err != nil {
			return false, err
		}
		if collecting {
			return false, s.settleRemaining(ctx, team, settled, now)
		}
	}

	welcomeCollecting := false
	if template == TemplateWelcome && hasSite {
		welcomeCollecting = s.collectingAtWelcome(ctx, team.ID)
	}

	if strings.TrimSpace(team.OwnerEmail) == "" {
		return false, fmt.Errorf("onboarding: account %d has no owner address and is due %s", team.ID, template)
	}

	// An email with no way out is not sent. The link needs the application
	// key, so this fails loudly on a box that cannot read it.
	stop := ""
	if s.Stop != nil {
		stop = s.Stop.Link(team.ID)
	}
	if stop == "" {
		return false, fmt.Errorf("onboarding: no stop link could be made for account %d", team.ID)
	}

	claim, claimed, err := s.Outbox.ClaimEmail(ctx, team.ID, team.CreatedAt, template, team.OwnerEmail, now)
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}

	notice := Notice{
		TeamID:     team.ID,
		Template:   template,
		To:         team.OwnerEmail,
		FirstName:  FirstName(team.OwnerName),
		MessageKey: claim.MessageKey,
		AddSiteURL: s.BaseURL + "/sites/new",
		StopURL:    stop,
	}
	if hasSite {
		notice.SiteDomain = site.Domain
		notice.InstallURL = s.BaseURL + "/onboarding/" + strconv.FormatInt(site.ID, 10)
		notice.DashboardURL = s.BaseURL + "/dashboard/" + site.Domain
		notice.Collecting = welcomeCollecting
	}

	outcome, err := s.Notify.Notify(ctx, notice)
	if err != nil {
		return false, errors.Join(err,
			s.Outbox.FailEmail(ctx, team.ID, team.CreatedAt, template, claim, outcomeText(outcome, err), s.now()))
	}

	if err := s.Outbox.FinishEmail(ctx, team.ID, team.CreatedAt, template, claim, outcomeText(outcome, nil), s.now()); err != nil {
		return true, err
	}

	if s.Log != nil {
		s.Log.Info("onboarding email sent", "team", team.ID, "template", template, "outcome", outcome)
	}

	return true, nil
}

// collectingAtWelcome reports whether an account has counted an event by the
// time its welcome goes out. A failed read answers no and is logged: the
// welcome still goes, in its install wording, rather than waiting on a database
// that may stay unreadable.
func (s *Service) collectingAtWelcome(ctx context.Context, teamID int64) bool {
	collecting, err := s.Traffic.HasEvents(ctx, teamID)
	if err != nil {
		if s.Log != nil {
			s.Log.Warn("could not tell whether a new account is collecting; sending the install welcome",
				"team", teamID, "error", err)
		}

		return false
	}

	return collecting
}

// settleRemaining marks every install-help email not yet sent as skipped, once
// an account has data. The markers are what keep any of them from going out
// later, even if the account's sites are removed again.
func (s *Service) settleRemaining(ctx context.Context, team Team, settled map[string]time.Time, now time.Time) error {
	for _, step := range NoData {
		if _, done := settled[step.Template]; done {
			continue
		}

		claim, claimed, err := s.Outbox.ClaimEmail(ctx, team.ID, team.CreatedAt, step.Template, team.OwnerEmail, now)
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}

		if err := s.Outbox.FinishEmail(ctx, team.ID, team.CreatedAt, step.Template, claim, SkippedOutcome, now); err != nil {
			return err
		}
	}

	if s.Log != nil {
		s.Log.Info("onboarding install help stopped, the account has data", "team", team.ID)
	}

	return nil
}

// FirstName picks what to greet somebody by: the first word of their name when
// it is a plain word, and nothing otherwise. A name that is an email address,
// a single letter or a sentence greets worse than no name at all.
func FirstName(name string) string {
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return ""
	}

	first := fields[0]
	if len([]rune(first)) < 2 || len([]rune(first)) > 20 {
		return ""
	}

	for _, r := range first {
		letter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127
		if !letter && r != '-' && r != '\'' {
			return ""
		}
	}

	return first
}

// outcomeText turns a transport answer and an error into the one string stored
// on the outbox row.
func outcomeText(outcome string, err error) string {
	if err != nil {
		if outcome == "" {
			return "failed: " + err.Error()
		}

		return outcome + " (failed: " + err.Error() + ")"
	}

	if outcome == "" {
		return "sent"
	}

	return outcome
}

// Run sweeps on a ticker until the context is cancelled, starting with one
// sweep straight away so a restart does not delay anybody's welcome.
func (s *Service) Run(ctx context.Context) {
	if s.Log != nil {
		s.Log.Info("onboarding emails running", "interval", SweepInterval.String())
	}

	s.tick(ctx)

	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick runs one sweep and logs what it did. A quiet sweep logs nothing, so the
// log shows the emails rather than the timer.
func (s *Service) tick(ctx context.Context) {
	sent, err := s.Sweep(ctx)
	if s.Log == nil {
		return
	}

	if err != nil {
		s.Log.Error("onboarding sweep failed", "sent", sent, "error", err)
		return
	}

	if sent > 0 {
		s.Log.Info("onboarding sweep finished", "sent", sent)
	}
}
