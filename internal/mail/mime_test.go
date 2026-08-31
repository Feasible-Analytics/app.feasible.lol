//
// mime_test.go
// Turning a rendered message into something an SMTP server will accept.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"strings"
	"testing"
)

// TestMimeMessageHasBothParts checks the multipart envelope. A client that
// refuses HTML still has to be able to show a verification code the recipient
// cannot get any other way.
func TestMimeMessageHasBothParts(t *testing.T) {
	body := mimeMessage("feasible <no-reply@example.com>", Message{
		To:      "a@example.com",
		Subject: "Your code",
		HTML:    "<p>12345678</p>",
		Text:    "12345678",
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

// TestAddressOnlyStripsTheDisplayName checks what goes in the envelope
// commands, since strict servers reject `Name <a@b>` there with an error that
// names neither the address nor the reason.
func TestAddressOnlyStripsTheDisplayName(t *testing.T) {
	cases := map[string]string{
		"feasible <no-reply@example.com>": "no-reply@example.com",
		"no-reply@example.com":            "no-reply@example.com",
		"  a@b.co  ":                      "a@b.co",
	}

	for input, want := range cases {
		if got := addressOnly(input); got != want {
			t.Errorf("addressOnly(%q) = %q, want %q", input, got, want)
		}
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

// TestSafeFilename checks the log transport's filename can be written on every
// platform while still naming the recipient.
func TestSafeFilename(t *testing.T) {
	if got := safeFilename("a+tag@example.com"); got != "a-tag-example.com" {
		t.Errorf("want %q, got %q", "a-tag-example.com", got)
	}

	if strings.ContainsAny(safeFilename("../../etc/passwd"), "/\\") {
		t.Error("a path separator must never survive into a filename")
	}
}
