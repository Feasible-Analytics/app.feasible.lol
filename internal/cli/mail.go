//
// mail.go
// The `mail preview` subcommand.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/mailsample"
)

const mailHelp = `feasible mail — what the product sends.

Commands:
  preview  Render every email to HTML files with sample data.
`

const mailPreviewHelp = `feasible mail preview — render every email to disk.

Writes one .html and one .txt per message, plus an index.html that shows them
all side by side. Nothing is sent and no account is read: the data is a fixture,
so two runs produce the same bytes and a change to the shared layout can be
looked at rather than described.

Flags:
`

// runMail dispatches the mail subcommands.
func runMail(e *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, mailHelp)
		return ExitUsage
	}

	switch args[0] {
	case "preview":
		return mailPreview(e, args[1:])
	default:
		fmt.Fprintf(e.stderr, "unknown mail command %q\n\n", args[0])
		fmt.Fprint(e.stderr, mailHelp)
		return ExitUsage
	}
}

// mailPreview writes every message to a directory.
func mailPreview(e *env, args []string) int {
	fs := newFlagSet("mail preview", e, mailPreviewHelp)
	out := fs.String("out", "tmp/mail-preview", "directory to write the rendered messages into")

	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(e.stderr, "unexpected mail preview argument %q\n", fs.Arg(0))
		return ExitUsage
	}

	messages, err := mailsample.Messages()
	if err != nil {
		fmt.Fprintf(e.stderr, "%v\n", err)
		return ExitError
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintf(e.stderr, "mail preview: %v\n", err)
		return ExitError
	}

	// A renamed message would otherwise leave its old file behind, unlinked
	// from the index and indistinguishable from a current one.
	stale, err := filepath.Glob(filepath.Join(*out, "*"))
	if err != nil {
		fmt.Fprintf(e.stderr, "mail preview: %v\n", err)
		return ExitError
	}

	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(e.stderr, "mail preview: %v\n", err)
			return ExitError
		}
	}

	tags := make([]string, 0, len(messages))
	for tag := range messages {
		tags = append(tags, tag)
	}

	sort.Strings(tags)

	for _, tag := range tags {
		message := messages[tag]

		for suffix, body := range map[string]string{".html": message.HTML, ".txt": message.Text} {
			if err := os.WriteFile(filepath.Join(*out, tag+suffix), []byte(body), 0o644); err != nil {
				fmt.Fprintf(e.stderr, "mail preview: %v\n", err)
				return ExitError
			}
		}
	}

	if err := os.WriteFile(filepath.Join(*out, "index.html"), []byte(mailsample.Index(tags)), 0o644); err != nil {
		fmt.Fprintf(e.stderr, "mail preview: %v\n", err)
		return ExitError
	}

	fmt.Fprintf(e.stdout, "wrote %d messages to %s — open %s\n",
		len(tags), *out, filepath.Join(*out, "index.html"))

	return ExitOK
}
