//
// outcome.go
// Making "the job did nothing" a statement a handler has to write down.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package jobs

import (
	"context"
	"errors"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// ErrSilentSuccess is recorded against a job whose handler reported neither
// work done nor a reason for doing none.
var ErrSilentSuccess = errors.New("jobs: the handler reported success without doing anything and without saying why")

// Outcome is what a reporting handler answers with. Both counts exist so that
// "nothing happened" is a statement rather than an absence.
type Outcome struct {
	// Handled is how many units of real work the run did — reports sent,
	// alerts delivered, rows purged.
	Handled int

	// Skipped is work the handler deliberately declined, such as a site whose
	// report has already gone out for this period.
	Skipped int

	// Note is why nothing was handled. It is required whenever Handled is
	// zero, and it is the whole mechanism: a handler that returns an empty
	// Outcome is a bug, and this turns that bug into a recorded failure rather
	// than a silent success nobody looks at again.
	Note string
}

// Validate rejects an Outcome that claims success without evidence.
func (o Outcome) Validate() error {
	if o.Handled == 0 && strings.TrimSpace(o.Note) == "" {
		return ErrSilentSuccess
	}

	return nil
}

// Nothing builds the Outcome a handler returns when it correctly did no work.
// The reason is a parameter so that "nothing to do" cannot be expressed without
// saying why.
func Nothing(note string) Outcome {
	return Outcome{Note: note}
}

// Reporter is a handler that has to say what it did.
type Reporter func(ctx context.Context, job Job) (Outcome, error)

// Reporting adapts a Reporter to the Worker the runner dispatches to.
//
// It is an adapter rather than a second worker type because there is one queue
// and one runner in this binary, and two of either would mean two answers to
// "is anything stuck". What it adds is the check: a run that reports no work
// and no reason is turned into a failure on the row, which is exactly the shape
// of the notifier that returned an empty list and sent nothing for months.
func Reporting(log *logger.Logger, handler Reporter) Worker {
	return WorkerFunc(func(ctx context.Context, job Job) error {
		outcome, err := handler(ctx, job)
		if err != nil {
			return err
		}

		if err := outcome.Validate(); err != nil {
			// Logged as well as recorded, and at error level with the kind
			// named, because the bug is in the handler and seeing it here is
			// the only way anybody finds it.
			if log != nil {
				log.Error("a job reported success without doing anything", "kind", job.Kind, "job", job.ID)
			}

			return err
		}

		if log != nil {
			log.Info("job done",
				"kind", job.Kind, "job", job.ID,
				"handled", outcome.Handled, "skipped", outcome.Skipped, "note", outcome.Note)
		}

		return nil
	})
}
