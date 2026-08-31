//
// mail.go
// Messages, transports, and the difference between "sent" and "delivered".
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package mail renders and sends every message the product sends: the ones an
// account cannot be created or recovered without — the verification code, the
// password reset link, the security notices — and the ones that decide whether
// somebody keeps their data, which are the lifecycle and volume notices.
//
// They live in one package because three things about email have to be solved
// once rather than per call site, and two of them have broken this exact
// product category before.
//
// The first is line length. RFC 5321 limits a line to 998 octets, and many SMTP
// servers reject the entire message after DATA when one is longer. Generated
// HTML routinely produces a single enormous line, and the failure is silent —
// an incumbent's weekly reports simply never arrived for self-hosters. Every
// body this package sends goes through Wrap, in Mailer.Send, so there is exactly
// one path from a rendered template to a wire and a new transport cannot forget.
//
// The second is that a send function returning without an error is not
// delivery. Every transport returns what it actually observed, Mailer.Send
// turns a message the transport declined into an error, and the caller records
// the detail — because "the function returned" is not evidence a human received
// anything, and a signup that silently sent no email looks exactly like one
// that worked.
//
// The third is that a first run must need no mail service at all. The log
// transport writes the rendered message under tmp/mail/ and says which file it
// wrote, so a laptop and a fresh self-hosted install can both complete a
// registration and read the ten lifecycle emails as rendered pages.
package mail

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	htmlstd "html"
	"html/template"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// templates holds the account messages' bodies — the ones with their own
// markup rather than the shared lifecycle layout. They are embedded for the
// same reason every other asset is: a release is one binary, and a template
// directory that has to be copied alongside it is a directory that will be
// missing.
//
//go:embed templates
var templates embed.FS

// accountTemplates is parsed once at start-up. A broken embedded template is a
// programmer error the first test run catches, so panicking is honest: the
// binary cannot send the verification email and should not pretend it can.
var accountTemplates = template.Must(template.ParseFS(templates, "templates/*.html"))

// PostalAddress is the legal entity behind the service, in the form that goes
// in every footer. CAN-SPAM requires a valid physical postal address in
// marketing email; transactional email is exempt, but it is included in every
// message anyway because arguing about which category a dunning notice falls
// into costs more than the four lines.
const PostalAddress = "Cloudmanic Labs, LLC\n901 Brutscher Street, D112\nNewberg, OR 97132\nUnited States"

// The addresses the product writes from and points people at. Sales is the
// destination for the volume ladder: the whole point of warning at 70% is to
// turn a limit into a conversation while there is still a month of runway.
const (
	DefaultFrom  = "feasible.lol <hello@feasible.lol>"
	SalesAddress = "sales@feasible.lol"
)

// dialTimeout caps how long a send waits on an unreachable SMTP host. Without
// it a misconfigured host makes the registration request hang until the browser
// gives up, which reads to the user as "the product is broken".
const dialTimeout = 10 * time.Second

// Message is one email, ready to send. Both bodies are always populated: a
// plain-text alternative is what stops a message that carries a payment
// deadline landing in a spam folder, it is the only version some people will
// ever see, and it is the only one a screen reader can work through to find a
// verification code the recipient cannot get any other way.
type Message struct {
	To      string
	Subject string
	HTML    string
	Text    string

	// Tag names the template. It is carried through to the transport so a log
	// line says which message went out, rather than only that one did.
	Tag string
}

// Result is what a transport observed. It is deliberately not a boolean:
// "accepted by the relay" is the strongest claim any SMTP client can make, and
// recording the detail is what lets somebody answer "did they actually get it"
// three weeks later.
type Result struct {
	// Transport names which one handled it — "log" or "smtp".
	Transport string

	// Accepted is whether the transport took responsibility for the message.
	// It is still not proof of delivery, which is why Detail exists.
	Accepted bool

	// Detail is the transport's own words: the file a log transport wrote, or
	// the relay and response an SMTP transport saw.
	Detail string
}

// String renders a result for a log line and for the outcome column.
func (r Result) String() string {
	status := "rejected"
	if r.Accepted {
		status = "accepted"
	}

	return fmt.Sprintf("%s/%s: %s", r.Transport, status, r.Detail)
}

// Transport is somewhere a message can go. It is an interface so that local
// development needs no SMTP service at all and so that tests assert on what was
// sent rather than on what a network did.
type Transport interface {
	// Send delivers one message and reports what happened. A non-nil error and
	// an unaccepted result are the same fact told twice, on purpose: a caller
	// that only checks the error still records the truth.
	Send(ctx context.Context, msg Message) (Result, error)
}

// Sender is what the rest of the product depends on. Callers take this rather
// than a concrete mailer so a test can capture messages without a transport.
type Sender interface {
	Send(ctx context.Context, msg Message) (Result, error)
}

// Mailer wraps a transport with the guarantees this package exists for: every
// body is wrapped below the SMTP line limit, a message the transport declined
// is an error, and every send returns the transport's own answer rather than an
// assumption.
//
// It holds the base URL because every link in every email is absolute — a
// relative link in an email client resolves against nothing — and the one place
// that URL can be wrong is worth keeping to one.
type Mailer struct {
	transport Transport
	from      string
	baseURL   string
}

// Options are the inputs to New. They are a struct rather than positional
// arguments because a mailer takes half a dozen strings that are all plausible
// values for each other, and swapping two of them silently sends every email
// from the wrong address.
type Options struct {
	Transport    string
	From         string
	BaseURL      string
	SMTPHost     string
	SMTPPort     int
	SMTPUser     string
	SMTPPass     string
	SMTPStartTLS bool
	Log          *logger.Logger
}

// New builds a mailer for the configured transport. An unknown transport is an
// error rather than a fallback to logging: a production box that quietly
// switched to writing emails into a temporary directory would look healthy
// while nobody could reset a password or hear that their data is about to be
// deleted.
func New(opts Options) (*Mailer, error) {
	from := strings.TrimSpace(opts.From)
	if from == "" {
		from = DefaultFrom
	}

	var transport Transport

	switch opts.Transport {
	case config.MailTransportLog:
		transport = &LogTransport{Log: opts.Log, Dir: LogDir}

	case config.MailTransportSMTP:
		if opts.SMTPHost == "" {
			return nil, fmt.Errorf("mail: the smtp transport needs FEASIBLE_SMTP_HOST")
		}

		transport = &SMTPTransport{
			Config: SMTPConfig{
				Host:     opts.SMTPHost,
				Port:     opts.SMTPPort,
				Username: opts.SMTPUser,
				Password: opts.SMTPPass,
				From:     from,
				StartTLS: opts.SMTPStartTLS,
				Timeout:  dialTimeout,
			},
			Log: opts.Log,
		}

	default:
		return nil, fmt.Errorf("mail: %q is not a known transport", opts.Transport)
	}

	return NewWithTransport(transport, from, opts.BaseURL), nil
}

// NewWithTransport builds a mailer over a supplied transport, which is how the
// tests assert on what was sent without touching disk or network.
//
// The from address is a parameter rather than a constant because a self-hoster's
// relay will reject a From it does not own, and finding that out means reading
// the relay's logs.
func NewWithTransport(transport Transport, from, baseURL string) *Mailer {
	if from == "" {
		from = DefaultFrom
	}

	return &Mailer{transport: transport, from: from, baseURL: strings.TrimRight(baseURL, "/")}
}

// From is the envelope sender this mailer uses.
func (m *Mailer) From() string {
	return m.from
}

// BaseURL reports the origin every link in every message is built from.
func (m *Mailer) BaseURL() string {
	return m.baseURL
}

// Send wraps the bodies, hands the message to the transport, and refuses to
// call a declined message sent.
//
// Wrapping here rather than in each transport is what makes it impossible to
// add a transport that forgets. Checking the result here is what makes it
// impossible for a caller to record a warning the customer never received.
func (m *Mailer) Send(ctx context.Context, msg Message) (Result, error) {
	if strings.TrimSpace(msg.To) == "" {
		return Result{Detail: "no recipient"}, fmt.Errorf("mail: %q has no recipient", msg.Tag)
	}

	msg.HTML = Wrap(msg.HTML, MaxLineLength)
	msg.Text = Wrap(msg.Text, MaxLineLength)

	result, err := m.transport.Send(ctx, msg)
	if err != nil {
		return result, err
	}

	if !result.Accepted {
		return result, fmt.Errorf("mail: %q to %s was not accepted: %s", msg.Tag, msg.To, result.Detail)
	}

	return result, nil
}

// data is what every account template is rendered with. It carries the base URL
// on every message so a template author never has to remember to pass it, which
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

// render turns one account template into a message body. Both a plain-text and
// an HTML part are produced from the same template output: some clients refuse
// HTML outright, and a code the recipient cannot read is a code they cannot use.
func (m *Mailer) render(name string, d data) (string, string, error) {
	var buf bytes.Buffer

	if err := accountTemplates.ExecuteTemplate(&buf, name, d); err != nil {
		return "", "", fmt.Errorf("mail: render %s: %w", name, err)
	}

	html := buf.String()

	return html, textFromHTML(html), nil
}

// textFromHTML derives the plain-text part from rendered HTML. The lifecycle
// and volume messages build their text part from the same Content the HTML came
// from, so this is only for the account templates, which are hand-written
// markup with no data behind them.
//
// It is a crude tag strip rather than a real converter, which is honest for four
// templates we write ourselves: a dependency that renders arbitrary HTML to text
// would be a lot of code to make our own known markup slightly prettier.
func textFromHTML(source string) string {
	var out strings.Builder
	depth := 0

	for _, r := range source {
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

	return htmlstd.UnescapeString(strings.TrimSpace(strings.Join(kept, "\n")))
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

	_, err = m.Send(ctx, Message{
		To:      to,
		Subject: "Your feasible.lol verification code",
		HTML:    html,
		Text:    text,
		Tag:     "verify_email",
	})

	return err
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

	_, err = m.Send(ctx, Message{
		To:      to,
		Subject: "Reset your feasible.lol password",
		HTML:    html,
		Text:    text,
		Tag:     "password_reset",
	})

	return err
}

// SendPasswordChanged tells someone their password moved. It is sent after the
// change rather than before, because its only job is to be the alarm that goes
// off when the person reading it did not do it.
func (m *Mailer) SendPasswordChanged(ctx context.Context, to, name string) error {
	html, text, err := m.render("password_changed.html", data{BaseURL: m.baseURL, Name: name})
	if err != nil {
		return err
	}

	_, err = m.Send(ctx, Message{
		To:      to,
		Subject: "Your feasible.lol password was changed",
		HTML:    html,
		Text:    text,
		Tag:     "password_changed",
	})

	return err
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

	_, err = m.Send(ctx, Message{
		To:      to,
		Subject: "New sign-in to your feasible.lol account",
		HTML:    html,
		Text:    text,
		Tag:     "new_login",
	})

	return err
}
