//
// mail.go
// Sending the handful of transactional emails the product depends on.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package mail sends the messages an account cannot be created or recovered
// without: the verification code, the password reset link and the security
// notices that follow them.
//
// There are two transports and the difference between them is deliberate. The
// log transport writes the rendered message to disk and logs where it went, so
// local development and a first-run self-hoster need no SMTP service at all —
// a verification email that cannot be sent would otherwise make the very first
// screen of the product a dead end. The SMTP transport is the real one.
//
// Delivery failures are returned, never swallowed. A signup that silently sends
// no email looks identical to a signup that worked, and the person waiting for
// the code has no way to tell which happened.
package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"fmt"
	"html/template"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// templates holds the message bodies. They are embedded for the same reason
// every other asset is: a release is one binary, and a template directory that
// has to be copied alongside it is a directory that will be missing.
//
//go:embed templates
var templates embed.FS

// LogDir is where the log transport writes rendered messages, relative to the
// working directory. It is under tmp/ because these are development artefacts
// that must never be mistaken for something to back up.
const LogDir = "tmp/mail"

// dialTimeout caps how long a send waits on an unreachable SMTP host. Without
// it a misconfigured host makes the registration request hang until the
// browser gives up, which reads to the user as "the product is broken".
const dialTimeout = 10 * time.Second

// Message is one outgoing email. Both the subject and the body are rendered
// before a transport sees them, so a template error is caught in one place
// rather than once per transport.
type Message struct {
	To      string
	Subject string
	HTML    string
	Text    string
}

// Sender is the transport a Mailer writes through. It exists so tests can
// capture what would have been sent without a filesystem or a network.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// Mailer renders and sends the product's messages. It holds the base URL
// because every link in every email is absolute — a relative link in an email
// client resolves against nothing — and the one place that URL can be wrong is
// worth keeping to one.
type Mailer struct {
	sender  Sender
	from    string
	baseURL string
	tpl     *template.Template
}

// Options are the inputs to New. They are a struct rather than positional
// arguments because a mailer takes five strings that are all plausible values
// for each other, and swapping two of them silently sends every email from the
// wrong address.
type Options struct {
	Transport string
	From      string
	BaseURL   string
	SMTPHost  string
	SMTPPort  string
	SMTPUser  string
	SMTPPass  string
	Log       *logger.Logger
}

// New builds a mailer for the configured transport. An unknown transport is an
// error rather than a fallback to logging: a production box that quietly
// switched to writing emails into a temporary directory would look healthy
// while nobody could reset a password.
func New(opts Options) (*Mailer, error) {
	tpl, err := template.ParseFS(templates, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("mail: parse templates: %w", err)
	}

	from := strings.TrimSpace(opts.From)
	if from == "" {
		from = "feasible <no-reply@localhost>"
	}

	var sender Sender

	switch opts.Transport {
	case config.MailTransportLog:
		sender = &LogSender{Log: opts.Log, Dir: LogDir}

	case config.MailTransportSMTP:
		if opts.SMTPHost == "" {
			return nil, fmt.Errorf("mail: the smtp transport needs FEASIBLE_SMTP_HOST")
		}

		sender = &SMTPSender{
			Host:     opts.SMTPHost,
			Port:     opts.SMTPPort,
			Username: opts.SMTPUser,
			Password: opts.SMTPPass,
			From:     from,
			Log:      opts.Log,
		}

	default:
		return nil, fmt.Errorf("mail: %q is not a known transport", opts.Transport)
	}

	return &Mailer{sender: sender, from: from, baseURL: strings.TrimRight(opts.BaseURL, "/"), tpl: tpl}, nil
}

// NewWithSender builds a mailer over a supplied transport, which is how the
// tests assert on what was sent without touching disk or network.
func NewWithSender(sender Sender, from, baseURL string) (*Mailer, error) {
	tpl, err := template.ParseFS(templates, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("mail: parse templates: %w", err)
	}

	return &Mailer{sender: sender, from: from, baseURL: strings.TrimRight(baseURL, "/"), tpl: tpl}, nil
}

// BaseURL reports the origin every link in every message is built from.
func (m *Mailer) BaseURL() string {
	return m.baseURL
}

// data is what every template is rendered with. It carries the base URL on
// every message so a template author never has to remember to pass it, which
// is the mistake that produces an email full of links to "/reset-password".
type data struct {
	BaseURL string
	Name    string
	Code    string
	Link    string
	Device  string
	When    string
	Extra   string
}

// render turns one template into a message body. Both a plain-text and an HTML
// part are produced from the same template output: some clients refuse HTML
// outright, and a code the recipient cannot read is a code they cannot use.
func (m *Mailer) render(name string, d data) (string, string, error) {
	var buf bytes.Buffer

	if err := m.tpl.ExecuteTemplate(&buf, name, d); err != nil {
		return "", "", fmt.Errorf("mail: render %s: %w", name, err)
	}

	html := buf.String()

	return html, textFromHTML(html), nil
}

// SendVerification emails the code and the one-click link that prove an
// address. Both are in the same message on purpose: the link is one tap on a
// phone, and the code is what someone types when they opened the email on a
// different device from the one they registered on.
func (m *Mailer) SendVerification(ctx context.Context, to, name, code, link string) error {
	html, text, err := m.render("verify_email.html", data{BaseURL: m.baseURL, Name: name, Code: code, Link: link})
	if err != nil {
		return err
	}

	return m.sender.Send(ctx, Message{To: to, Subject: "Your feasible.lol verification code", HTML: html, Text: text})
}

// SendPasswordReset emails a single-use reset link. There is no code variant:
// a reset grants far more than proving an address does, so it is worth making
// the recipient come back through a URL we minted rather than something they
// can read out over the phone to whoever asked them for it.
func (m *Mailer) SendPasswordReset(ctx context.Context, to, name, link string) error {
	html, text, err := m.render("password_reset.html", data{BaseURL: m.baseURL, Name: name, Link: link})
	if err != nil {
		return err
	}

	return m.sender.Send(ctx, Message{To: to, Subject: "Reset your feasible.lol password", HTML: html, Text: text})
}

// SendPasswordChanged tells someone their password moved. It is sent after the
// change rather than before, because its only job is to be the alarm that goes
// off when the person reading it did not do it.
func (m *Mailer) SendPasswordChanged(ctx context.Context, to, name string) error {
	html, text, err := m.render("password_changed.html", data{BaseURL: m.baseURL, Name: name})
	if err != nil {
		return err
	}

	return m.sender.Send(ctx, Message{To: to, Subject: "Your feasible.lol password was changed", HTML: html, Text: text})
}

// SendNewLogin reports a sign-in from a device we have not seen before. The
// device label and time are the two things that let someone recognise their own
// login at a glance and act on one that is not.
func (m *Mailer) SendNewLogin(ctx context.Context, to, name, device string, when time.Time) error {
	html, text, err := m.render("new_login.html", data{
		BaseURL: m.baseURL,
		Name:    name,
		Device:  device,
		When:    when.UTC().Format("2 January 2006 at 15:04 MST"),
	})
	if err != nil {
		return err
	}

	return m.sender.Send(ctx, Message{To: to, Subject: "New sign-in to your feasible.lol account", HTML: html, Text: text})
}

// LogSender writes messages to disk instead of sending them. The file path is
// logged so that a developer, or a self-hoster with no mail service yet, can
// open the verification email rather than being locked out of the account they
// just created.
type LogSender struct {
	Log *logger.Logger
	Dir string

	// Sent keeps every message in memory so a test — and the local development
	// flow — can read back the last code without parsing a file.
	Sent []Message
}

// Send writes one message under the log directory and records it. A failure to
// write is returned rather than logged and forgotten, because in this transport
// writing the file *is* the delivery.
func (s *LogSender) Send(_ context.Context, msg Message) error {
	s.Sent = append(s.Sent, msg)

	dir := s.Dir
	if dir == "" {
		dir = LogDir
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mail: create %s: %w", dir, err)
	}

	name := fmt.Sprintf("%d-%s.html", time.Now().UnixNano(), safeFilename(msg.To))
	path := filepath.Join(dir, name)

	if err := os.WriteFile(path, []byte(msg.HTML), 0o600); err != nil {
		return fmt.Errorf("mail: write %s: %w", path, err)
	}

	if s.Log != nil {
		s.Log.EmailSent(msg.To, msg.Subject, path)
	}

	return nil
}

// SMTPSender is the real transport. It dials with an explicit timeout and
// upgrades to TLS when the server offers it, which covers both the submission
// port every hosted provider uses and the plain relay a self-hoster runs on the
// same box.
type SMTPSender struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
	Log      *logger.Logger
}

// Send delivers one message over SMTP. The whole conversation happens inside
// one dial so that a hung server fails on the connection rather than mid-DATA,
// where a partially sent message can be delivered twice.
func (s *SMTPSender) Send(ctx context.Context, msg Message) error {
	port := s.Port
	if port == "" {
		port = "587"
	}

	addr := net.JoinHostPort(s.Host, port)

	dialer := &net.Dialer{Timeout: dialTimeout}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mail: dial %s: %w", addr, err)
	}

	client, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mail: %s: %w", addr, err)
	}
	defer client.Close()

	// STARTTLS is attempted whenever the server advertises it. Sending
	// credentials in the clear because the server merely did not insist is not
	// a trade worth making.
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("mail: starttls %s: %w", addr, err)
		}
	}

	if s.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("mail: authenticate to %s: %w", addr, err)
		}
	}

	if err := client.Mail(addressOnly(s.From)); err != nil {
		return fmt.Errorf("mail: from %s: %w", s.From, err)
	}

	if err := client.Rcpt(addressOnly(msg.To)); err != nil {
		return fmt.Errorf("mail: to %s: %w", msg.To, err)
	}

	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("mail: data: %w", err)
	}

	if _, err := writer.Write([]byte(mimeMessage(s.From, msg))); err != nil {
		writer.Close()
		return fmt.Errorf("mail: write body: %w", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("mail: close body: %w", err)
	}

	if s.Log != nil {
		s.Log.EmailSent(msg.To, msg.Subject, "")
	}

	return client.Quit()
}
