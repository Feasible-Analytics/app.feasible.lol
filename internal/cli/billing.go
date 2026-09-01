//
// billing.go
// The `billing` subcommand: what support needs to answer a billing question.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"context"
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/billing"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/lifecycle"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/stripe"

	volume "github.com/Feasible-Analytics/app.feasible.lol/internal/usage"
)

const billingHelp = `feasible billing — inspect and drive the account lifecycle.

Commands:
  status [<team>]   Show the clock, the plan and this month's usage.
  trial <team>      Start the trial clock for an account. Day 0 is now.
  sweep             Advance every running clock once, and run the volume ladder.
  replied <team>    Record that somebody replied about their volume, and unlock.
  events [<team>]   The last payment-provider webhooks, and what each one did.
  preflight         Verify Stripe product, prices, webhooks and Managed Payments.

Flags:
`

// runBilling dispatches the billing subcommands. They exist because every
// question support is asked about an account — what phase is it in, when does it
// get deleted, did the webhook arrive — has to be answerable from a shell on the
// box, without a dashboard and without guessing at SQL.
func runBilling(e *env, args []string) int {
	fs := newFlagSet("billing", e, billingHelp)
	dataDir := fs.String("data-dir", e.cfg.App.DataDir, "directory holding control.db and the account databases")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}

	e.cfg.App.DataDir = *dataDir

	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Fprint(e.stderr, billingHelp)
		return ExitUsage
	}

	// The subcommand is checked before anything is opened, so a typo answers
	// with the help rather than with an error about a database — which is a
	// confusing thing to be told when the problem is a missing letter.
	switch rest[0] {
	case "status", "trial", "sweep", "replied", "events", "preflight":
	default:
		fmt.Fprintf(e.stderr, "unknown billing command %q\n\n", rest[0])
		fmt.Fprint(e.stderr, billingHelp)
		return ExitUsage
	}

	ctx := context.Background()
	if rest[0] == "preflight" {
		return billingPreflight(ctx, e, rest[1:])
	}

	control, err := openControl(ctx, e.cfg.App.DataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}
	defer control.Close()

	manager := accounts.NewManager(e.cfg.App.DataDir)
	defer manager.CloseAll() //nolint:errcheck // the process is exiting either way

	// `billing sweep` sends the lifecycle emails, so the same mailer the server
	// uses is built here. A transport this binary does not know is refused now
	// rather than halfway through a sweep, with some accounts warned and some
	// not.
	mailer, err := buildMailer(e)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	com := buildCommerce(e, control, manager, nil, mailer)

	switch rest[0] {
	case "status":
		return billingStatus(ctx, e, com, rest[1:])
	case "trial":
		return billingTrial(ctx, e, com, rest[1:])
	case "sweep":
		return billingSweep(ctx, e, com)
	case "replied":
		return billingReplied(ctx, e, com, rest[1:])
	default:
		return billingEvents(ctx, e, com, rest[1:])
	}
}

const billingPreflightHelp = `feasible billing preflight — verify Stripe before customers reach checkout.

Usage:
  feasible billing preflight [--checkout-smoke]

The default performs read-only API checks but remains not-ready because Stripe
does not expose Managed Payments activation, accepted terms, or tax-code
eligibility as readable fields. --checkout-smoke creates no customer or charge;
it creates a Checkout Session with the production parameters and immediately
expires it.

Flags:
`

// billingPreflight runs without opening control.db, so it can gate a deployment
// before migrations, process startup, or customer traffic.
func billingPreflight(ctx context.Context, e *env, args []string) int {
	fs := newFlagSet("billing preflight", e, billingPreflightHelp)
	smoke := fs.Bool("checkout-smoke", false, "create and immediately expire a customerless Managed Payments Checkout Session")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(e.stderr, "unexpected billing preflight argument %q\n", fs.Arg(0))
		return ExitUsage
	}

	service := &billing.Service{
		Stripe: stripe.New(e.cfg.App.Stripe.SecretKey),
		Plans: billing.Plans{
			Product: e.cfg.App.Stripe.Product,
			Monthly: e.cfg.App.Stripe.PriceMonthly,
			Yearly:  e.cfg.App.Stripe.PriceYearly,
		},
		WebhookSecret: e.cfg.App.Stripe.WebhookSecret,
		BaseURL:       e.cfg.App.BaseURL,
	}

	report := service.Preflight(ctx, *smoke)
	w := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	for _, check := range report.Checks {
		fmt.Fprintf(w, "%s\t%s\t%s\n", check.Status, check.Name, check.Detail)
	}
	if code := flush(e, w); code != ExitOK {
		return code
	}

	if !report.Ready() {
		fmt.Fprintln(e.stderr, "Stripe Managed Payments preflight is not ready")
		return ExitError
	}

	fmt.Fprintln(e.stdout, "Stripe Managed Payments preflight passed")
	return ExitOK
}

// billingStatus prints one account's whole commercial position, or every
// account currently on a clock. It leads with the deletion date because that is
// the number somebody is calling about.
func billingStatus(ctx context.Context, e *env, com *commerce, args []string) int {
	now := time.Now().UTC()

	if len(args) == 0 {
		running, err := com.LifecycleStore.Running(ctx)
		if err != nil {
			fmt.Fprintf(e.stderr, "%v\n", err)
			return ExitError
		}

		if len(running) == 0 {
			fmt.Fprintln(e.stdout, "no account is on a lifecycle clock")
			return ExitOK
		}

		w := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TEAM\tNAME\tTRIGGER\tDAY\tPHASE\tDELETES ON\tCONTACT")

		for _, account := range running {
			fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%s\t%s\n",
				account.TeamID, account.TeamName, account.State.Trigger,
				account.State.DayAt(now), account.State.At(now),
				account.State.Boundary(lifecycle.PhaseDeleted).Format("2006-01-02"),
				account.Email)
		}

		return flush(e, w)
	}

	teamID, code := parseTeam(e, args[0])
	if code != ExitOK {
		return code
	}

	state, err := com.LifecycleStore.Load(ctx, teamID)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	mirror, err := com.Billing.Store.Load(ctx, teamID)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	period := volume.Period(now)

	counts, err := com.Usage.Get(ctx, teamID, period)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	overage, err := com.Usage.Overage(ctx, teamID)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	access := lifecycle.Capabilities(state.At(now))
	comped, err := com.LifecycleStore.IsComped(ctx, teamID)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	w := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)

	fmt.Fprintf(w, "account\t%d\n", teamID)
	fmt.Fprintf(w, "phase\t%s\n", state.At(now))
	fmt.Fprintf(w, "trigger\t%s\n", orNone(string(state.Trigger)))
	fmt.Fprintf(w, "comped\t%s\n", yesNo(comped))
	fmt.Fprintf(w, "dashboard\t%s\n", yesNo(access.Dashboard))
	fmt.Fprintf(w, "collecting\t%s\n", yesNo(access.Collect))
	fmt.Fprintf(w, "export\t%s\n", yesNo(access.Export))

	if state.Running() {
		fmt.Fprintf(w, "day\t%d\n", state.DayAt(now))
		fmt.Fprintf(w, "started\t%s\n", state.StartedAt.Format(time.RFC3339))
		fmt.Fprintf(w, "locks\t%s\n", state.Boundary(lifecycle.PhaseLocked).Format(time.RFC3339))
		fmt.Fprintf(w, "stops collecting\t%s\n", state.Boundary(lifecycle.PhaseDormant).Format(time.RFC3339))
		fmt.Fprintf(w, "deletes\t%s\n", state.Boundary(lifecycle.PhaseDeleted).Format(time.RFC3339))
	}

	fmt.Fprintf(w, "plan\t%s\n", orNone(mirror.Plan))
	fmt.Fprintf(w, "provider status\t%s\n", orNone(mirror.Status))
	fmt.Fprintf(w, "payment state\t%s\n", orNone(mirror.PaymentState))
	fmt.Fprintf(w, "customer\t%s\n", orNone(mirror.CustomerID))

	// The resolved contact rather than the stored billing address, because that
	// column is empty until an account has been to checkout — and "who would we
	// warn about a deletion" is the question actually being asked.
	if _, email, err := com.LifecycleStore.Contact(ctx, teamID); err == nil {
		fmt.Fprintf(w, "contact\t%s\n", orNone(email))
	}

	fmt.Fprintf(w, "usage %s\t%d of %d (%d%%)\n", period, counts.Billable(), volume.MonthlyLimit, volume.Percent(counts.Billable()))

	if overage.Period != "" {
		fmt.Fprintf(w, "volume conversation\topened %s, reply by %s\n",
			overage.AskedAt.Format("2006-01-02"), overage.ReplyDeadline.Format("2006-01-02"))
		fmt.Fprintf(w, "volume lock\t%s\n", yesNo(overage.Locked()))
	}

	sent, err := com.LifecycleStore.SentEmails(ctx, teamID, state.StartedAt)
	if err == nil && len(sent) > 0 {
		for _, entry := range lifecycle.Sequence {
			if sent[entry.Template] {
				fmt.Fprintf(w, "email day %d\t%s sent\n", entry.Day, entry.Template)
			}
		}
	}

	return flush(e, w)
}

// billingTrial enrols an account. It is the signal a signup would send, exposed
// as a command so an operator can put an account onto the clock — and so the
// whole sequence can be exercised on a real install without waiting for one.
func billingTrial(ctx context.Context, e *env, com *commerce, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, "usage: feasible billing trial <team>")
		return ExitUsage
	}

	teamID, code := parseTeam(e, args[0])
	if code != ExitOK {
		return code
	}

	transition, err := com.Lifecycle.Signal(ctx, teamID, lifecycle.SignalTrialStarted)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	if !transition.Changed {
		fmt.Fprintf(e.stdout, "account %d was already on a clock — nothing changed\n", teamID)
		return ExitOK
	}

	fmt.Fprintf(e.stdout, "account %d: trial started, dashboard locks %s, deletes %s\n",
		teamID,
		transition.State.Boundary(lifecycle.PhaseLocked).Format("2006-01-02"),
		transition.State.Boundary(lifecycle.PhaseDeleted).Format("2006-01-02"))

	return ExitOK
}

// billingSweep runs both sweeps once and reports what they touched. It is what
// an operator runs after a process has been down, and what a cron job on a
// self-hosted install would call.
func billingSweep(ctx context.Context, e *env, com *commerce) int {
	code := ExitOK

	accountsSwept, err := com.Lifecycle.Sweep(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "lifecycle: %v\n", err)
		code = ExitError
	}

	fmt.Fprintf(e.stdout, "lifecycle: %d account(s) on a clock\n", accountsSwept)

	teamsSwept, err := com.Volume.Sweep(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "volume: %v\n", err)
		code = ExitError
	}

	fmt.Fprintf(e.stdout, "volume: %d account(s) with usage this month\n", teamsSwept)

	return code
}

// billingReplied records that a human answered the volume conversation, which
// unlocks the dashboard immediately. There is no automatic signal for this: an
// email thread is not machine-readable, and locking somebody who did reply would
// be unforgivable.
func billingReplied(ctx context.Context, e *env, com *commerce, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, "usage: feasible billing replied <team>")
		return ExitUsage
	}

	teamID, code := parseTeam(e, args[0])
	if code != ExitOK {
		return code
	}

	if err := com.Usage.MarkReplied(ctx, teamID); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	fmt.Fprintf(e.stdout, "account %d: reply recorded, dashboard unlocked\n", teamID)

	return ExitOK
}

// billingEvents prints the webhook log. It is the first thing to read when a
// customer says they paid and the account still says otherwise.
func billingEvents(ctx context.Context, e *env, com *commerce, args []string) int {
	var teamID int64

	if len(args) > 0 {
		parsed, code := parseTeam(e, args[0])
		if code != ExitOK {
			return code
		}
		teamID = parsed
	}

	events, err := com.Billing.Store.RecentEvents(ctx, teamID, 50)
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	if len(events) == 0 {
		fmt.Fprintln(e.stdout, "no webhook deliveries recorded")
		return ExitOK
	}

	w := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RECEIVED\tTYPE\tTEAM\tOUTCOME\tEVENT\tERROR")

	for _, event := range events {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n",
			event.ReceivedAt.Format("2006-01-02 15:04:05"), event.Type,
			event.TeamID, event.Outcome, event.EventID, event.Error)
	}

	return flush(e, w)
}

// parseTeam turns an argument into an account id.
func parseTeam(e *env, raw string) (int64, int) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		fmt.Fprintf(e.stderr, "%q is not an account id\n", raw)
		return 0, ExitUsage
	}

	return id, ExitOK
}

// flush writes a tabwriter out and maps its failure onto an exit code.
func flush(e *env, w *tabwriter.Writer) int {
	if err := w.Flush(); err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	return ExitOK
}

// yesNo renders a capability for a human.
func yesNo(value bool) string {
	if value {
		return "yes"
	}

	return "no"
}

// orNone renders an empty string as something visible, so a blank column is
// obviously "nothing here" rather than a truncated line.
func orNone(value string) string {
	if value == "" {
		return "—"
	}

	return value
}
