//
// transport.go
// The two ways a message leaves: a file on disk, or a relay that has to answer.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// LogDir is where the log transport writes rendered bodies. It is relative to
// the working directory because it is a development affordance: the point is
// that `open tmp/mail/...` shows exactly what a customer would have received,
// with no SMTP service to run and no inbox to check.
const LogDir = "tmp/mail"

// LogTransport prints a message and writes its rendered HTML to disk. It is the
// default so that a laptop and a first-run self-hoster need no mail service at
// all — a verification email that cannot be sent would make the very first
// screen of the product a dead end — and so that the ten lifecycle emails can
// be reviewed as rendered pages rather than as escaped strings in a terminal.
type LogTransport struct {
	// Dir is where bodies are written. Empty means LogDir.
	Dir string

	// Log receives one line per message. It is optional so a test can use the
	// transport without a logger.
	Log *logger.Logger

	// Now is injectable so the filenames a test produces are deterministic.
	Now func() time.Time
}

// Send writes the message to disk and reports the file it wrote. The file path
// is the Detail, which is what makes the result honest: nothing was delivered
// to anybody, and the transport says so by naming a file rather than a relay.
func (t *LogTransport) Send(_ context.Context, msg Message) (Result, error) {
	dir := t.Dir
	if dir == "" {
		dir = LogDir
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{Transport: "log", Detail: err.Error()}, fmt.Errorf("mail: %w", err)
	}

	name := fmt.Sprintf("%s-%s-%s.html", t.stamp().Format("20060102-150405.000"), safeName(msg.Tag), safeName(msg.To))
	if msg.MessageID != "" {
		name = safeName(msg.MessageID) + ".html"
	}
	path := filepath.Join(dir, name)

	// The headers are written into the file as a comment rather than only
	// logged, so the artefact on disk is self-describing when somebody opens it
	// a week later with no terminal scrollback to go with it.
	body := fmt.Sprintf("<!--\nTo: %s\nSubject: %s\nTag: %s\nMessage-ID: %s\n-->\n%s",
		msg.To, msg.Subject, msg.Tag, msg.MessageID, msg.HTML)

	// The file is readable only by the account that wrote it. These artefacts
	// hold live password-reset links and verification codes, and tmp/ on a
	// shared development box is not somewhere to leave those world-readable.
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return Result{Transport: "log", Detail: err.Error()}, fmt.Errorf("mail: %w", err)
	}

	// The product's one shape for an outgoing-mail log line. It carries the
	// body path so a developer reading the log is one click from the rendered
	// HTML rather than from a description of it.
	if t.Log != nil {
		t.Log.EmailSent(msg.To, msg.Tag, path)
	}

	fmt.Printf("\n--- mail: %s\nTo: %s\nSubject: %s\nFile: %s\n\n%s\n", msg.Tag, msg.To, msg.Subject, path, msg.Text)

	return Result{Transport: "log", Accepted: true, Detail: path}, nil
}

// stamp returns the transport's clock, defaulting to the real one.
func (t *LogTransport) stamp() time.Time {
	if t.Now == nil {
		return time.Now().UTC()
	}

	return t.Now().UTC()
}

// safeName turns an address or a tag into something that can be a filename on
// every filesystem we might land on, while still naming the recipient so a
// developer can find the message they are looking for in a directory listing.
// A raw email address contains characters Windows refuses and a shell mangles,
// and a mail directory nobody can open is the same as no mail directory.
//
// A path separator must never survive, which is why the allowed set is a
// whitelist rather than a list of things to strip.
func safeName(value string) string {
	if value == "" {
		return "unknown"
	}

	var out strings.Builder
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			out.WriteRune(r)
		default:
			out.WriteByte('-')
		}
	}

	return out.String()
}

// SMTPConfig is a relay. Every field is something a self-hoster gets wrong
// once, so each is named rather than parsed out of a single URL where a typo
// becomes an unhelpful dial error.
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string

	// From is the envelope sender. A relay rejects a From it does not own, and
	// that rejection is the single most common reason a self-hoster's mail
	// silently stops.
	From string

	// StartTLS upgrades the connection after EHLO, which is what port 587
	// expects. Port 465 is TLS from the first byte and is detected from the
	// port rather than configured, because nobody remembers which is which.
	StartTLS bool

	// Timeout bounds the whole conversation. Without it a relay that accepts a
	// connection and then says nothing holds a lifecycle job open forever.
	Timeout time.Duration
}

// SMTPTransport sends through a relay. It is written against net/smtp rather
// than a library because the product's promise is one binary, and the
// conversation below is the whole of what we need.
type SMTPTransport struct {
	Config SMTPConfig
	Log    *logger.Logger
}

// Send delivers one message and returns what the relay said. The relay's
// acceptance is recorded verbatim in Detail: it is the strongest claim that can
// honestly be made, and calling it "delivered" is the mistake this package was
// written to avoid.
func (t *SMTPTransport) Send(ctx context.Context, msg Message) (result Result, err error) {
	result = Result{Transport: "smtp"}

	if t.Config.Host == "" {
		return result, fmt.Errorf("mail: no SMTP host configured")
	}

	timeout := t.Config.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	addr := net.JoinHostPort(t.Config.Host, fmt.Sprintf("%d", t.Config.Port))

	dialer := &net.Dialer{Timeout: timeout}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("mail: dial %s: %w", addr, err)
	}
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conversationErr := fmt.Errorf("mail: set %s conversation deadline: %w", addr, err)
		if closeErr := conn.Close(); closeErr != nil {
			conversationErr = errors.Join(conversationErr, fmt.Errorf("mail: close %s after deadline failure: %w", addr, closeErr))
		}
		result.Detail = err.Error()
		return result, conversationErr
	}

	// Port 465 speaks TLS from the first byte; 587 and 25 negotiate it after
	// EHLO. Getting this backwards produces a handshake error that names
	// neither, which is why it is decided from the port here rather than left
	// to whoever wrote the configuration file.
	if t.Config.Port == 465 {
		conn = tls.Client(conn, &tls.Config{ServerName: t.Config.Host, MinVersion: tls.VersionTLS12})
	}

	client, err := smtp.NewClient(conn, t.Config.Host)
	if err != nil {
		conversationErr := fmt.Errorf("mail: %s: %w", addr, err)
		if closeErr := conn.Close(); closeErr != nil {
			conversationErr = errors.Join(conversationErr, fmt.Errorf("mail: close %s after SMTP greeting failure: %w", addr, closeErr))
		}
		result.Detail = err.Error()
		return result, conversationErr
	}
	clientClosed := false
	defer func() {
		if clientClosed {
			return
		}
		if closeErr := client.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("mail: close %s SMTP connection: %w", addr, closeErr))
			result.Detail = err.Error()
		}
	}()

	if t.Config.StartTLS && t.Config.Port != 465 {
		// A relay that does not offer STARTTLS after the operator asked for it
		// is refused rather than continued in the clear. Every message this
		// transport carries holds a password-reset link or a verification code,
		// and sending one unencrypted because the server declined to advertise
		// the extension is exactly the silent downgrade the setting exists to
		// prevent.
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			result.Detail = "the relay does not offer STARTTLS"
			return result, fmt.Errorf("mail: %s does not offer STARTTLS, and sending in the clear was not asked for", addr)
		}

		if err := client.StartTLS(&tls.Config{ServerName: t.Config.Host, MinVersion: tls.VersionTLS12}); err != nil {
			result.Detail = err.Error()
			return result, fmt.Errorf("mail: starttls %s: %w", addr, err)
		}
	}

	if t.Config.Username != "" {
		auth := smtp.PlainAuth("", t.Config.Username, t.Config.Password, t.Config.Host)
		if err := client.Auth(auth); err != nil {
			result.Detail = err.Error()
			return result, fmt.Errorf("mail: auth %s: %w", addr, err)
		}
	}

	from := t.Config.From
	if from == "" {
		from = DefaultFrom
	}

	if err := client.Mail(envelopeAddress(from)); err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("mail: MAIL FROM: %w", err)
	}

	if err := client.Rcpt(envelopeAddress(msg.To)); err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("mail: RCPT TO %s: %w", msg.To, err)
	}

	writer, err := client.Data()
	if err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("mail: DATA: %w", err)
	}

	if _, err := writer.Write([]byte(Render(from, msg))); err != nil {
		writeErr := fmt.Errorf("mail: write body: %w", err)
		if closeErr := writer.Close(); closeErr != nil {
			writeErr = errors.Join(writeErr, fmt.Errorf("mail: close rejected DATA body: %w", closeErr))
		}
		result.Detail = err.Error()
		return result, writeErr
	}

	// The relay's verdict arrives on the close of DATA, not on the write. A
	// transport that ignored this error would report every message as sent
	// including the ones that were rejected outright — which is exactly the
	// failure this package exists to make impossible.
	if err := writer.Close(); err != nil {
		result.Detail = err.Error()
		return result, fmt.Errorf("mail: relay rejected the message: %w", err)
	}

	if quitErr := client.Quit(); quitErr != nil {
		if t.Log != nil {
			t.Log.Warn("smtp quit failed after the message was accepted", "error", quitErr, "to", msg.To)
		}
		if closeErr := client.Close(); closeErr != nil && t.Log != nil {
			t.Log.Warn("smtp close failed after the message was accepted", "error", closeErr, "to", msg.To)
		}
	}
	clientClosed = true

	result.Accepted = true
	result.Detail = "accepted by " + addr

	// The relay is named rather than left to the outcome column, because "which
	// server accepted this" is the first question asked when a self-hoster's
	// mail stops arriving and the last one the logs usually answer.
	if t.Log != nil {
		t.Log.Info("mail sent", "to", msg.To, "subject", msg.Subject, "tag", msg.Tag, "relay", addr)
	}

	return result, nil
}

// envelopeAddress strips a display name down to the bare address. SMTP's
// MAIL FROM and RCPT TO take an address, not "Name <address>", and a relay
// handed the display form answers with a syntax error that names nothing
// useful.
func envelopeAddress(value string) string {
	if start := strings.LastIndex(value, "<"); start >= 0 {
		if end := strings.Index(value[start:], ">"); end > 0 {
			return value[start+1 : start+end]
		}
	}

	return strings.TrimSpace(value)
}

// Render builds the message for an SMTP DATA stream: headers, a
// multipart/alternative body, CRLF line endings, and dot-stuffing.
func Render(from string, msg Message) string {
	return renderMessage(from, msg, dotStuff)
}

// RenderMIME builds the same message as a standalone MIME document, for an API
// that takes one rather than a DATA stream.
//
// It exists because dot-stuffing is a property of the SMTP wire, not of the
// message: a relay strips the doubled full stop back off, and an API that never
// saw a DATA command delivers it to the reader.
func RenderMIME(from string, msg Message) string {
	return renderMessage(from, msg, crlf)
}

// renderMessage assembles the RFC 5322 message, escaping each body with the
// supplied function. The plain-text part comes first because that is the order
// the standard defines as least-to-most preferred, and a client that renders the
// wrong one is showing HTML source to a customer.
//
// The Date header is not optional in practice. A message without one is scored
// as spam by most filters, which for a deletion warning means the message left
// the building and still did not arrive.
func renderMessage(from string, msg Message, escape func(string) string) string {
	boundary := "feasible-" + safeName(msg.Tag) + "-boundary"

	var b strings.Builder

	b.WriteString("From: " + headerValue(from) + "\r\n")
	b.WriteString("To: " + headerValue(msg.To) + "\r\n")
	b.WriteString("Subject: " + encodeSubject(msg.Subject) + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	if msg.MessageID != "" {
		b.WriteString("Message-ID: <" + safeName(msg.MessageID) + "@feasible.lol>\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(escape(msg.Text))
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n\r\n")
	b.WriteString(escape(msg.HTML))
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "--\r\n")

	return b.String()
}

// headerValue flattens a value onto one header line. A line break inside a
// header ends that header, so a team name or an address carrying CR or LF
// would otherwise inject a header of its own — a Bcc, say — into the message.
//
// A CRLF pair becomes one space rather than two, so the flattened line reads
// the way the customer's own text does.
func headerValue(value string) string {
	value = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(value)

	return strings.TrimSpace(value)
}

// encodeSubject makes the subject a legal header: one line, and RFC 2047
// encoded when it holds anything outside ASCII. A raw UTF-8 subject reads as
// mojibake in the clients that follow the standard strictly.
func encodeSubject(subject string) string {
	subject = headerValue(subject)

	for i := 0; i < len(subject); i++ {
		if subject[i] >= 0x80 {
			return mime.QEncoding.Encode("utf-8", subject)
		}
	}

	return subject
}

// dotStuff escapes a line that begins with a full stop, and converts bare
// newlines to CRLF on the way.
//
// SMTP ends the DATA command with a line containing exactly one full stop, so a
// body line starting with one would truncate the message there and the rest
// would be read as SMTP commands. CRLF is done in the same pass because both
// are per-line rewrites of the same body, and a body with bare newlines is one
// of the ways a message ends up mangled or rejected by a strict relay.
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

// crlf normalises line endings without dot-stuffing, for a transport that hands
// over a MIME document rather than writing an SMTP DATA stream. RFC 5322 wants
// CRLF regardless of who carries the message, so this half of the rewrite
// applies either way.
func crlf(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")

	return strings.ReplaceAll(body, "\n", "\r\n")
}
