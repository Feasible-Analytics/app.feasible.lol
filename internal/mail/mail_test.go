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
	"github.com/Feasible-Analytics/app.feasible.lol/internal/timefmt"
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

	// The name is deliberately absent: the layout leads with a heading, and a
	// greeting on four messages out of twenty-three is two house styles.
	for _, fragment := range []string{"12345678", "verify-email/confirm?token=abc"} {
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

	if err := mailer.SendNewLogin(ctx, "a@example.com", "Sam", "Chrome on macOS", timefmt.Cycle24, time.Now()); err != nil {
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

// TestInvitationCarriesRecipientRoleExpiryAndBearerLink checks the message the
// team screen relies on instead of exposing an invitation secret in the UI.
func TestInvitationCarriesRecipientRoleExpiryAndBearerLink(t *testing.T) {
	mailer, sender := newTestMailer(t)
	expires := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	err := mailer.SendInvitation(context.Background(), "new@example.com", "Acme", "Sam",
		"Editor", "https://example.com/invitations/bearer-secret", expires)
	if err != nil {
		t.Fatalf("send invitation: %v", err)
	}

	if len(sender.messages) != 1 {
		t.Fatalf("want one message, got %d", len(sender.messages))
	}

	message := sender.messages[0]
	for _, fragment := range []string{"Acme", "Sam", "Editor", "bearer-secret", expires.Format(DateFormat)} {
		if !strings.Contains(message.HTML, fragment) || !strings.Contains(message.Text, fragment) {
			t.Errorf("invitation is missing %q", fragment)
		}
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

// TestTheNewLoginMailFollowsTheReadersClock covers the mail that is read in a
// hurry. "Was that three in the afternoon or three in the morning" is the first
// question somebody has about a sign-in they do not recognise.
func TestTheNewLoginMailFollowsTheReadersClock(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 9, 4, 15, 4, 0, 0, time.UTC)

	for cycle, want := range map[string]string{
		timefmt.Cycle12: "3:04 PM UTC",
		timefmt.Cycle24: "15:04 UTC",
		timefmt.System:  "15:04 UTC",
	} {
		sender := &capture{}
		mailer := NewWithTransport(sender, "feasible <no-reply@example.com>", "https://feasible.lol")

		if err := mailer.SendNewLogin(ctx, "a@example.com", "Sam", "Chrome on macOS", cycle, at); err != nil {
			t.Fatalf("send new login: %v", err)
		}

		if len(sender.messages) != 1 {
			t.Fatalf("sent %d messages, want 1", len(sender.messages))
		}

		if !strings.Contains(sender.messages[0].Text, want) {
			t.Errorf("the %q dial produced %q, want it to contain %q", cycle, sender.messages[0].Text, want)
		}
	}
}

// accountMessages builds one of each account email, for the assertions that
// have to hold across all four.
func accountMessages(t *testing.T) map[string]Message {
	t.Helper()

	mailer, sender := newTestMailer(t)
	ctx := context.Background()

	if err := mailer.SendVerification(ctx, "a@example.com", "Sam", "12345678", "https://example.com/verify?token=abc"); err != nil {
		t.Fatal(err)
	}
	if err := mailer.SendPasswordReset(ctx, "a@example.com", "Sam", "https://example.com/reset?token=xyz"); err != nil {
		t.Fatal(err)
	}
	if err := mailer.SendPasswordChanged(ctx, "a@example.com", "Sam"); err != nil {
		t.Fatal(err)
	}
	if err := mailer.SendNewLogin(ctx, "a@example.com", "Sam", "Chrome on macOS", timefmt.Cycle24,
		time.Date(2026, 9, 4, 15, 4, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}

	byTag := map[string]Message{}
	for _, message := range sender.messages {
		byTag[message.Tag] = message
	}

	if len(byTag) != 4 {
		t.Fatalf("built %d distinct messages, want 4", len(byTag))
	}

	return byTag
}

// TestTheAccountEmailsCarryTheFooter is the assertion that was missing. These
// four skipped the shared layout, so they had no wordmark, no company name and
// no postal address — which is the shape a phishing email takes, on exactly the
// messages that ask somebody to click a link and type a password.
func TestTheAccountEmailsCarryTheFooter(t *testing.T) {
	for tag, message := range accountMessages(t) {
		for _, fragment := range []string{"Cloudmanic Labs, LLC", "901 Brutscher Street, D112", "Newberg, OR 97132"} {
			if !strings.Contains(message.HTML, fragment) {
				t.Errorf("%s: the HTML part is missing %q", tag, fragment)
			}

			if !strings.Contains(message.Text, fragment) {
				t.Errorf("%s: the text part is missing %q", tag, fragment)
			}
		}

		if !strings.Contains(message.HTML, "Feasible<span") {
			t.Errorf("%s: the HTML part has no wordmark", tag)
		}
	}
}

// TestTheAccountEmailsHaveAReadablePlainTextPart proves nothing regressed when
// the crude tag stripper was deleted. The text part is built from the same
// data the HTML is now, rather than scraped back out of it.
func TestTheAccountEmailsHaveAReadablePlainTextPart(t *testing.T) {
	for tag, message := range accountMessages(t) {
		if strings.TrimSpace(message.Text) == "" {
			t.Errorf("%s: the text part is empty", tag)
		}

		if strings.ContainsAny(message.Text, "<>") {
			t.Errorf("%s: the text part carries markup:\n%s", tag, message.Text)
		}

		if message.Subject == "" {
			t.Errorf("%s: no subject", tag)
		}
	}
}

// TestTheResetEmailShowsItsLink covers the one email that hands over account
// control. A button hides where it goes and there is no hover on a phone, so
// the URL is in the body as well as behind the button.
func TestTheResetEmailShowsItsLink(t *testing.T) {
	message := accountMessages(t)["password_reset"]
	link := "https://example.com/reset?token=xyz"

	if !strings.Contains(message.HTML, `href="`+link+`"`) {
		t.Error("the reset email has no button pointing at the link")
	}

	// Visible text, not only an href: html/template escapes the & in a query,
	// so the body copy is checked on the escaped form the reader sees.
	if !strings.Contains(message.HTML, ">"+link+"<") && !strings.Contains(message.HTML, link+"</p>") {
		t.Errorf("the reset link is not shown as text:\n%s", message.HTML)
	}

	if !strings.Contains(message.Text, link) {
		t.Error("the plain-text part does not carry the reset link")
	}
}

// TestTheNewSignInEmailRendersItsFacts checks the device and the time land in
// the facts table, which is what it is for.
func TestTheNewSignInEmailRendersItsFacts(t *testing.T) {
	message := accountMessages(t)["new_login"]

	for _, fragment := range []string{"Device", "Chrome on macOS", "Signed in", "4 September 2026 at 15:04 UTC"} {
		if !strings.Contains(message.HTML, fragment) {
			t.Errorf("the HTML part is missing %q", fragment)
		}

		if !strings.Contains(message.Text, fragment) {
			t.Errorf("the text part is missing %q", fragment)
		}
	}
}
