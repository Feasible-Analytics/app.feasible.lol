//
// transport_test.go
// The log transport's artefacts, and the wire format an SMTP server will accept.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSMTPConversationDeadlineBoundsAStalledRelay accepts TCP and then never
// sends the SMTP greeting. The whole conversation deadline, not only dialing,
// must release the outbox worker well before its five-minute lease can expire.
func TestSMTPConversationDeadlineBoundsAStalledRelay(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close SMTP listener: %v", err)
		}
	})

	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		close(accepted)
		time.Sleep(500 * time.Millisecond)
	}()

	address := listener.Addr().(*net.TCPAddr)
	transport := &SMTPTransport{Config: SMTPConfig{
		Host: "127.0.0.1", Port: address.Port, From: "sender@example.com", Timeout: 50 * time.Millisecond,
	}}
	started := time.Now()
	_, err = transport.Send(context.Background(), Message{To: "owner@example.com", Subject: "test", Text: "test", HTML: "<p>test</p>"})
	if err == nil {
		t.Fatal("stalled SMTP greeting did not time out")
	}
	<-accepted
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("stalled SMTP conversation lasted %s", elapsed)
	}
}

// TestLogTransportWritesTheRenderedBody is what makes local development need no
// mail service: the message is a file you can open, not an escaped string in a
// terminal that has already scrolled away.
func TestLogTransportWritesTheRenderedBody(t *testing.T) {
	dir := t.TempDir()
	transport := &LogTransport{Dir: dir}

	result, err := transport.Send(context.Background(), Message{
		To:      "owner@example.com",
		Subject: "Your dashboard is locked",
		HTML:    "<p>Nothing is lost.</p>",
		Text:    "Nothing is lost.",
		Tag:     "dashboard_locked",
	})
	if err != nil {
		t.Fatal(err)
	}

	if !result.Accepted {
		t.Fatal("the log transport did not accept the message")
	}
	if result.Transport != "log" {
		t.Errorf("transport is %q, want log", result.Transport)
	}

	// The detail names a file rather than a relay, which is the transport being
	// honest: nothing was delivered to anybody.
	body, err := os.ReadFile(result.Detail)
	if err != nil {
		t.Fatalf("the result named %q, which does not exist: %v", result.Detail, err)
	}

	for _, want := range []string{"owner@example.com", "Your dashboard is locked", "dashboard_locked", "Nothing is lost."} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the written file is missing %q", want)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("wrote %d files, want 1", len(entries))
	}
	if filepath.Ext(entries[0].Name()) != ".html" {
		t.Errorf("wrote %q, want a .html file", entries[0].Name())
	}
}

// TestLogTransportCreatesItsDirectory covers the first run, when tmp/mail does
// not exist yet. Failing there would make a fresh checkout look broken.
func TestLogTransportCreatesItsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "mail")

	transport := &LogTransport{Dir: dir}

	if _, err := transport.Send(context.Background(), Message{To: "a@example.com", Tag: "t"}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the directory was not created: %v", err)
	}
}

// TestStableMessageIDSurvivesTransportRetries documents the provider-accepted
// before local-ack window. SMTP may deliver twice, but every retry carries the
// same Message-ID; the local transport also overwrites one stable artifact
// instead of pretending two logical notices were created.
func TestStableMessageIDSurvivesTransportRetries(t *testing.T) {
	dir := t.TempDir()
	transport := &LogTransport{Dir: dir}
	message := Message{
		To: "owner@example.com", Subject: "Deletion tomorrow", Tag: "deletion_tomorrow",
		HTML: "<p>One notice.</p>", Text: "One notice.",
		MessageID: "lifecycle-1-1772538000-deletion_tomorrow",
	}
	first, err := transport.Send(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	second, err := transport.Send(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Detail != second.Detail || len(entries) != 1 {
		t.Fatalf("stable retry paths are %q and %q with %d files", first.Detail, second.Detail, len(entries))
	}
	raw := Render("feasible <no-reply@example.com>", message)
	want := "Message-ID: <lifecycle-1-1772538000-deletion-tomorrow@feasible.lol>"
	if !strings.Contains(raw, want) {
		t.Fatalf("SMTP retry is missing %q", want)
	}
}

// TestMailerRefusesAMessageWithNoRecipient is the one send failure that must be
// loud. A deletion warning addressed to nobody is exactly the case where a
// silent success would be unforgivable.
func TestMailerRefusesAMessageWithNoRecipient(t *testing.T) {
	mailer := NewWithTransport(&LogTransport{Dir: t.TempDir()}, "", "")

	if _, err := mailer.Send(context.Background(), Message{Subject: "x", Tag: "t"}); err == nil {
		t.Fatal("a message with no recipient was accepted")
	}
}

// TestMailerWrapsEveryBody is why wrapping lives in the mailer rather than in
// each transport: there is exactly one path from a rendered template to a wire,
// so a new transport cannot forget.
func TestMailerWrapsEveryBody(t *testing.T) {
	var captured Message

	mailer := NewWithTransport(transportFunc(func(_ context.Context, msg Message) (Result, error) {
		captured = msg
		return Result{Transport: "test", Accepted: true}, nil
	}), "", "")

	long := strings.Repeat("<span>x</span>", 5000)

	if _, err := mailer.Send(context.Background(), Message{To: "a@example.com", HTML: long, Text: long, Tag: "t"}); err != nil {
		t.Fatal(err)
	}

	if got := LongestLine(captured.HTML); got > MaxLineLength {
		t.Errorf("the HTML reached the transport with a %d byte line", got)
	}
	if got := LongestLine(captured.Text); got > MaxLineLength {
		t.Errorf("the text reached the transport with a %d byte line", got)
	}
}

// TestRenderUsesCRLFThroughout pins the wire format. SMTP's line terminator is
// CRLF, and a body with bare newlines is one of the ways a message ends up
// mangled or rejected by a strict relay.
func TestRenderUsesCRLFThroughout(t *testing.T) {
	raw := Render("feasible.lol <hello@feasible.lol>", Message{
		To:      "owner@example.com",
		Subject: "Test",
		HTML:    "<p>one</p>\n<p>two</p>",
		Text:    "one\ntwo",
		Tag:     "test",
	})

	// Every LF must be preceded by a CR.
	for i, c := range []byte(raw) {
		if c == '\n' && (i == 0 || raw[i-1] != '\r') {
			t.Fatalf("a bare newline at byte %d", i)
		}
	}

	for _, want := range []string{
		"From: feasible.lol <hello@feasible.lol>",
		"To: owner@example.com",
		"Subject: Test",
		"Content-Type: multipart/alternative",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Type: text/html; charset=UTF-8",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("the rendered message is missing %q", want)
		}
	}

	// The plain-text part comes first: that is the order the standard defines
	// as least-to-most preferred, and getting it backwards shows HTML source to
	// anyone whose client picks the first part.
	if strings.Index(raw, "text/plain") > strings.Index(raw, "text/html") {
		t.Error("the HTML part comes before the plain-text part")
	}
}

// TestRenderKeepsEveryHeaderOnOneLine is the header injection case. A team
// name is customer-typed text that ends up in an invitation subject, and a
// line break inside it would otherwise start a header of the customer's own.
func TestRenderKeepsEveryHeaderOnOneLine(t *testing.T) {
	raw := Render("feasible <no-reply@example.com>", Message{
		To:      "owner@example.com\r\nBcc: everyone@example.com",
		Subject: "You're invited to Acme\r\nBcc: list@example.com\r\nX-Injected: yes on feasible.lol",
		HTML:    "<p>hi</p>",
		Text:    "hi",
		Tag:     "test",
	})

	headers, _, _ := strings.Cut(raw, "\r\n\r\n")

	// An injected name is only a header when it begins a line. The same text
	// carried inline on the To or Subject line is inert, and is what
	// flattening is meant to produce — so the check is per line rather than
	// over the whole block.
	for _, line := range strings.Split(headers, "\r\n") {
		for _, injected := range []string{"Bcc:", "X-Injected:"} {
			if strings.HasPrefix(line, injected) {
				t.Fatalf("an injected %s header survived into the message:\n%s", injected, headers)
			}
		}
	}

	if !strings.Contains(headers, "To: owner@example.com Bcc: everyone@example.com\r\n") {
		t.Fatalf("the recipient was not flattened onto one line:\n%s", headers)
	}

	if !strings.Contains(headers, "Subject: You're invited to Acme Bcc: list@example.com X-Injected: yes on feasible.lol\r\n") {
		t.Fatalf("the subject was not flattened onto one line:\n%s", headers)
	}
}

// TestRenderEncodesANonASCIISubject pins RFC 2047: a subject with an em dash
// or an accented letter must not go out as raw UTF-8 in a header, which is
// invalid and renders as mojibake in strict clients.
func TestRenderEncodesANonASCIISubject(t *testing.T) {
	raw := Render("feasible <no-reply@example.com>", Message{
		To: "owner@example.com", Subject: "Weekly report — acme.example", HTML: "<p>hi</p>", Text: "hi", Tag: "test",
	})

	if !strings.Contains(raw, "Subject: =?utf-8?q?Weekly_report_=E2=80=94_acme.example?=\r\n") {
		t.Fatalf("the subject was not RFC 2047 encoded:\n%s", raw)
	}

	plain := Render("feasible <no-reply@example.com>", Message{
		To: "owner@example.com", Subject: "Weekly report", HTML: "<p>hi</p>", Text: "hi", Tag: "test",
	})

	if !strings.Contains(plain, "Subject: Weekly report\r\n") {
		t.Fatal("an ASCII subject must be left readable rather than encoded")
	}
}

// TestEnvelopeAddressStripsTheDisplayName covers the SMTP conversation. MAIL
// FROM takes an address, not "Name <address>", and a relay handed the display
// form answers with a syntax error that names nothing useful.
func TestEnvelopeAddressStripsTheDisplayName(t *testing.T) {
	cases := map[string]string{
		"feasible.lol <hello@feasible.lol>": "hello@feasible.lol",
		"hello@feasible.lol":                "hello@feasible.lol",
		"  spaced@example.com  ":            "spaced@example.com",
	}

	for input, want := range cases {
		if got := envelopeAddress(input); got != want {
			t.Errorf("%q became %q, want %q", input, got, want)
		}
	}
}

// TestRenderHasBothParts checks the multipart envelope. A client that refuses
// HTML still has to be able to show a verification code the recipient cannot
// get any other way.
func TestRenderHasBothParts(t *testing.T) {
	body := Render("feasible <no-reply@example.com>", Message{
		To:      "a@example.com",
		Subject: "Your code",
		HTML:    "<p>12345678</p>",
		Text:    "12345678",
		Tag:     "verify_email",
	})

	for _, fragment := range []string{
		"From: feasible <no-reply@example.com>",
		"To: a@example.com",
		"Subject: Your code",
		"Content-Type: multipart/alternative",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Type: text/html; charset=UTF-8",
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("the message is missing %q", fragment)
		}
	}

	// Every header line has to end CRLF, or strict servers reject the message.
	if !strings.Contains(body, "\r\n") {
		t.Error("headers should be CRLF-terminated")
	}

	// A message with no Date is scored as spam by most filters, which for a
	// deletion warning means it left the building and still did not arrive.
	if !strings.Contains(body, "\r\nDate: ") {
		t.Error("the message has no Date header")
	}
}

// TestDotStuffing checks the escape that stops a body truncating the message.
// SMTP ends DATA with a line containing a single full stop, so a body line that
// starts with one would end the message early and the rest would be read as
// SMTP commands.
func TestDotStuffing(t *testing.T) {
	got := dotStuff("hello\n.\nworld")

	if !strings.Contains(got, "..") {
		t.Errorf("a leading full stop should be doubled, got %q", got)
	}

	if strings.Contains("\r\n"+got+"\r\n", "\r\n.\r\n") {
		t.Errorf("the body still contains a bare terminating dot: %q", got)
	}
}

// TestEnvelopeAddressStripsTheAccountFormats is the account side's coverage of
// the same envelope rule, kept because the address forms the verification and
// reset emails use are not the ones the lifecycle emails use.
func TestEnvelopeAddressStripsTheAccountFormats(t *testing.T) {
	cases := map[string]string{
		"feasible <no-reply@example.com>": "no-reply@example.com",
		"no-reply@example.com":            "no-reply@example.com",
		"  a@b.co  ":                      "a@b.co",
	}

	for input, want := range cases {
		if got := envelopeAddress(input); got != want {
			t.Errorf("envelopeAddress(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestSafeName checks the log transport's filename can be written on every
// platform while still naming the recipient.
func TestSafeName(t *testing.T) {
	if got := safeName("a+tag@example.com"); got != "a-tag-example.com" {
		t.Errorf("want %q, got %q", "a-tag-example.com", got)
	}

	if strings.ContainsAny(safeName("../../etc/passwd"), "/\\") {
		t.Error("a path separator must never survive into a filename")
	}
}

// transportFunc adapts a function to the Transport interface.
type transportFunc func(ctx context.Context, msg Message) (Result, error)

// Send calls the function.
func (f transportFunc) Send(ctx context.Context, msg Message) (Result, error) {
	return f(ctx, msg)
}

// TestStartTLSIsNotSilentlySkipped answers a relay that greets but never
// advertises STARTTLS. Every message this transport carries holds a
// password-reset link or a verification code, so continuing in the clear
// because the server declined to offer the extension is exactly the silent
// downgrade the setting exists to prevent.
func TestStartTLSIsNotSilentlySkipped(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close SMTP listener: %v", err)
		}
	})

	delivered := make(chan struct{}, 1)

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		reader := bufio.NewReader(conn)
		write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

		write("220 relay.example ESMTP")

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}

			switch verb := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(verb, "EHLO"):
				// Deliberately no STARTTLS in the extension list.
				write("250-relay.example")
				write("250 SIZE 10240000")

			case strings.HasPrefix(verb, "MAIL FROM"), strings.HasPrefix(verb, "RCPT TO"):
				write("250 OK")

			case verb == "DATA":
				delivered <- struct{}{}
				write("354 go ahead")

			case verb == "QUIT":
				write("221 bye")
				return

			default:
				write("250 OK")
			}
		}
	}()

	address := listener.Addr().(*net.TCPAddr)
	transport := &SMTPTransport{Config: SMTPConfig{
		Host: "127.0.0.1", Port: address.Port, From: "sender@example.com",
		StartTLS: true, Timeout: 2 * time.Second,
	}}

	result, err := transport.Send(context.Background(), Message{
		To: "owner@example.com", Subject: "test", Text: "test", HTML: "<p>test</p>",
	})
	if err == nil {
		t.Fatal("the message was sent in the clear to a relay that does not offer STARTTLS")
	}

	if result.Accepted {
		t.Fatal("the result claims the relay accepted a message that was never sent")
	}

	select {
	case <-delivered:
		t.Fatal("the body reached the relay unencrypted")
	default:
	}
}
