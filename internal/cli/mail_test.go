//
// mail_test.go
// That `mail preview` renders every message and says where it put them.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
)

// TestMailPreviewRendersEveryMessage is the CI check as much as the developer
// command: a template error in the shared layout breaks all twenty-four at
// once, and this is what catches it before anybody is sent one.
func TestMailPreviewRendersEveryMessage(t *testing.T) {
	dir := t.TempDir()

	code, stdout, stderr := run(t, "mail", "preview", "--out", dir)
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}

	for _, tag := range mail.Tags() {
		for _, suffix := range []string{".html", ".txt"} {
			body, err := os.ReadFile(filepath.Join(dir, tag+suffix))
			if err != nil {
				t.Errorf("%s%s was not written: %v", tag, suffix, err)

				continue
			}

			if len(body) == 0 {
				t.Errorf("%s%s is empty", tag, suffix)
			}
		}
	}

	index, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}

	for _, tag := range mail.Tags() {
		if !strings.Contains(string(index), tag+".html") {
			t.Errorf("the index does not link to %s", tag)
		}
	}

	// The command says where it put them, since the point is to go and look.
	if !strings.Contains(stdout, dir) {
		t.Errorf("the command does not name the directory: %q", stdout)
	}
}

// TestMailWithoutASubcommandExplainsItself keeps the command discoverable.
func TestMailWithoutASubcommandExplainsItself(t *testing.T) {
	code, _, stderr := run(t, "mail")
	if code != ExitUsage {
		t.Fatalf("code = %d, want a usage exit", code)
	}

	if !strings.Contains(stderr, "preview") {
		t.Errorf("the help does not name the one subcommand: %q", stderr)
	}
}
