//
// mail_test.go
// Rendering the four messages, and the transport that writes them to disk.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
)

// capture keeps messages in memory instead of sending them.
type capture struct {
	messages []Message
}

// Send records a message and reports the acceptance a real transport would.
// Anything else would be a transport that declined every message, which the
// mailer correctly turns into an error.
func (c *capture) Send(_ context.Context, msg Message) (Result, error) {
	c.messages = append(c.messages, msg)

	return Result{Transport: "capture", Accepted: true, Detail: "captured"}, nil
}

// newTestMailer builds a mailer over a capturing transport.
func newTestMailer(t *testing.T) (*Mailer, *capture) {
	t.Helper()

	sender := &capture{}

	mailer := NewWithTransport(sender, "feasible <no-reply@example.com>", "https://example.com")

	return mailer, sender
}

// TestVerificationCarriesBothTheCodeAndTheLink checks that one message serves
// both cases: typing a code on the machine you registered on, and tapping a
// link on the phone the email arrived on.
func TestVerificationCarriesBothTheCodeAndTheLink(t *testing.T) {
	mailer, sender := newTestMailer(t)

	err := mailer.SendVerification(context.Background(), "a@example.com", "Sam",
		"12345678", "https://example.com/verify-email/confirm?token=abc")
	if err != nil {
		t.Fatalf("send verification: %v", err)
	}

	if len(sender.messages) != 1 {
		t.Fatalf("want one message, got %d", len(sender.messages))
	}

	msg := sender.messages[0]

	if msg.To != "a@example.com" {
		t.Errorf("wrong recipient: %q", msg.To)
	}

	for _, fragment := range []string{"12345678", "verify-email/confirm?token=abc", "Sam"} {
		if !strings.Contains(msg.HTML, fragment) {
			t.Errorf("the HTML part is missing %q", fragment)
		}

		// The plain-text part matters as much: a client that refuses HTML must
		// still show a code the recipient cannot get any other way.
		if !strings.Contains(msg.Text, fragment) {
			t.Errorf("the text part is missing %q", fragment)
		}
	}
}

// TestEveryMessageRenders checks all four templates, since a template that
// fails to execute means an email nobody ever receives.
func TestEveryMessageRenders(t *testing.T) {
	mailer, sender := newTestMailer(t)
	ctx := context.Background()

	if err := mailer.SendPasswordReset(ctx, "a@example.com", "Sam", "https://example.com/reset"); err != nil {
		t.Fatalf("send reset: %v", err)
	}

	if err := mailer.SendPasswordChanged(ctx, "a@example.com", "Sam"); err != nil {
		t.Fatalf("send password changed: %v", err)
	}

	if err := mailer.SendNewLogin(ctx, "a@example.com", "Sam", "Chrome on macOS", time.Now()); err != nil {
		t.Fatalf("send new login: %v", err)
	}

	if len(sender.messages) != 3 {
		t.Fatalf("want three messages, got %d", len(sender.messages))
	}

	for _, msg := range sender.messages {
		if msg.Subject == "" {
			t.Error("a message with no subject reads as spam")
		}

		if strings.TrimSpace(msg.Text) == "" {
			t.Errorf("%q has an empty text part", msg.Subject)
		}
	}

	// The new-device notice has to name the device and the time, which are the
	// two things that let somebody recognise their own login or act on one that
	// is not.
	newLogin := sender.messages[2]

	if !strings.Contains(newLogin.HTML, "Chrome on macOS") {
		t.Error("the new-device email should name the device")
	}
}

// TestLogTransportNamesTheRecipientInTheFilename checks the transport that
// makes local development and a first-run self-hoster work with no SMTP service
// at all — a verification email that cannot be sent would make the first screen
// of the product a dead end.
//
// The body is checked by containment rather than equality because the written
// file also carries the recipient, subject and tag as an HTML comment, so that
// an artefact opened a week later says who it was for.
func TestLogTransportNamesTheRecipientInTheFilename(t *testing.T) {
	dir := t.TempDir()

	sender := &LogTransport{Dir: dir}

	_, err := sender.Send(context.Background(), Message{To: "a@example.com", Subject: "Hello", HTML: "<p>Hi</p>"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("want one file, got %d", len(entries))
	}

	body, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read message: %v", err)
	}

	if !strings.Contains(string(body), "<p>Hi</p>") {
		t.Errorf("the rendered message was not written: %q", body)
	}

	if !strings.Contains(entries[0].Name(), "a-example.com") {
		t.Errorf("the filename should name the recipient so it can be found: %q", entries[0].Name())
	}
}

// TestNewRejectsAnUnknownTransport checks that a misconfigured box fails at
// start-up rather than quietly writing every email into a temporary directory
// while looking healthy.
func TestNewRejectsAnUnknownTransport(t *testing.T) {
	if _, err := New(Options{Transport: "carrier-pigeon"}); err == nil {
		t.Error("an unknown transport should be refused")
	}

	if _, err := New(Options{Transport: config.MailTransportSMTP}); err == nil {
		t.Error("the smtp transport with no host should be refused")
	}

	if _, err := New(Options{Transport: config.MailTransportLog, BaseURL: "https://example.com/"}); err != nil {
		t.Errorf("the log transport should always build: %v", err)
	}
}

// TestBaseURLLosesItsTrailingSlash checks the one place a stray slash would
// produce a double one in every link in every email.
func TestBaseURLLosesItsTrailingSlash(t *testing.T) {
	mailer := NewWithTransport(&capture{}, "a@example.com", "https://example.com/")

	if mailer.BaseURL() != "https://example.com" {
		t.Errorf("want %q, got %q", "https://example.com", mailer.BaseURL())
	}
}

// TestTextFromHTMLKeepsTheContent checks the crude tag strip. It only has to
// handle four templates we wrote ourselves, and what matters is that the code
// and the link survive.
func TestTextFromHTMLKeepsTheContent(t *testing.T) {
	text := textFromHTML(`<div><p>Hi Sam,</p>

	<p>Your code is <strong>12345678</strong>.</p>

	<p><a href="https://example.com/x">https://example.com/x</a></p></div>`)

	for _, fragment := range []string{"Hi Sam,", "12345678", "https://example.com/x"} {
		if !strings.Contains(text, fragment) {
			t.Errorf("the text part lost %q:\n%s", fragment, text)
		}
	}

	if strings.Contains(text, "<") || strings.Contains(text, ">") {
		t.Errorf("tags should be stripped:\n%s", text)
	}

	// Stripping block tags leaves runs of blank lines, and a column of gaps is
	// not a readable message.
	if strings.Contains(text, "\n\n\n") {
		t.Errorf("blank lines should be collapsed:\n%q", text)
	}
}
