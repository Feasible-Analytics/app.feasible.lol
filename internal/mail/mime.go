//
// mime.go
// Turning a rendered message into something an SMTP server will accept.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"fmt"
	"strings"
	"time"
)

// mimeMessage builds the multipart/alternative body. Both parts carry the same
// content because a mail client that refuses HTML — and a screen reader working
// through a plain-text part — still has to be able to show a verification code
// that the recipient cannot get any other way.
func mimeMessage(from string, msg Message) string {
	boundary := fmt.Sprintf("feasible-%d", time.Now().UnixNano())

	var b strings.Builder

	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + msg.To + "\r\n")
	b.WriteString("Subject: " + msg.Subject + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(dotStuff(msg.Text) + "\r\n\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n\r\n")
	b.WriteString(dotStuff(msg.HTML) + "\r\n\r\n")

	b.WriteString("--" + boundary + "--\r\n")

	return b.String()
}

// dotStuff escapes a line that is a single full stop. SMTP ends the DATA
// command with exactly that line, so a body containing one would truncate the
// message at that point and the rest would be interpreted as SMTP commands.
func dotStuff(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")

	for i, line := range lines {
		if strings.HasPrefix(line, ".") {
			lines[i] = "." + line
		}
	}

	return strings.Join(lines, "\r\n")
}

// addressOnly strips a display name off an address. The envelope commands take
// a bare address, and passing `Name <a@b>` to MAIL FROM is rejected by strict
// servers with an error that names neither the address nor the reason.
func addressOnly(address string) string {
	if start := strings.LastIndex(address, "<"); start >= 0 {
		if end := strings.Index(address[start:], ">"); end > 0 {
			return strings.TrimSpace(address[start+1 : start+end])
		}
	}

	return strings.TrimSpace(address)
}

// textFromHTML derives the plain-text part from the rendered HTML. It is a
// crude tag strip rather than a real converter, which is honest for four
// templates we write ourselves: a dependency that renders arbitrary HTML to
// text would be a lot of code to make our own known markup slightly prettier.
func textFromHTML(html string) string {
	var out strings.Builder
	depth := 0

	for _, r := range html {
		switch {
		case r == '<':
			depth++
		case r == '>':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			out.WriteRune(r)
		}
	}

	// Collapse the run of blank lines that stripping block tags leaves behind,
	// so the text part reads as paragraphs rather than as a column of gaps.
	lines := strings.Split(out.String(), "\n")
	kept := make([]string, 0, len(lines))
	blank := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}

		kept = append(kept, line)
	}

	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// safeFilename turns an address into something that can be a filename on every
// platform. Only the log transport uses it, and only so a developer can find
// the message they are looking for in a directory listing.
func safeFilename(address string) string {
	var out strings.Builder

	for _, r := range address {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			out.WriteRune(r)
		default:
			out.WriteRune('-')
		}
	}

	return out.String()
}
