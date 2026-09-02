//
// outbound.go
// Where a customer-supplied URL is allowed to send this process.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package outbound guards every HTTP request whose destination a customer
// chose: webhook deliveries, Slack notifications, the site verification fetch.
// A URL typed into a form is a way to make this process connect somewhere on
// its own network, and the metadata endpoint at 169.254.169.254 or a database
// listening on loopback answers a request from us the way it would never
// answer one from the internet.
//
// Every such request goes through two checks. ValidateURL refuses a bad
// destination when the form is saved, so the customer sees why. NewClient
// refuses it again at connect time, after doing its own name resolution, so a
// name that pointed somewhere harmless when it was saved and at a private
// address when it was used is caught anyway.
//
// Build the policy with PolicyFor, which turns the loopback and HTTPS rules
// off for development and self-hosted installs and on for hosted production.
package outbound

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/clientip"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/config"
)

// lookupTimeout bounds the DNS query ValidateURL makes while a form submission
// waits on it. A resolver that hangs must not hang the settings page.
const lookupTimeout = 5 * time.Second

// Error is a refusal written for the customer: it names what to change and
// never carries a resolver or socket error, so a handler can render it in a
// form as it is. errors.As on it separates "the customer typed something we
// will not send to" from a failure of ours.
type Error string

// Error returns the message.
func (e Error) Error() string {
	return string(e)
}

// Policy says what an outbound destination may be.
type Policy struct {
	// AllowLoopback permits localhost / 127.0.0.1 / ::1 targets. It is on for
	// self-hosted and development installs so somebody can test a webhook
	// against a local receiver; hosted production leaves it off.
	AllowLoopback bool

	// RequireHTTPS refuses plain http (except loopback when AllowLoopback).
	RequireHTTPS bool

	// AllowedHosts, when non-empty, restricts destinations to exactly these
	// hostnames (used for Slack: hooks.slack.com).
	AllowedHosts []string
}

// PolicyFor derives the policy from the running configuration. Only hosted
// production refuses loopback: a self-hoster's webhook receiver is very often
// on the same box, and refusing it there protects nothing of ours.
func PolicyFor(cfg *config.Config) Policy {
	hostedProduction := cfg.IsProduction() && cfg.App.Hosted

	return Policy{
		AllowLoopback: !hostedProduction,
		RequireHTTPS:  cfg.IsProduction(),
	}
}

// ValidateURL parses raw, checks scheme/host against the policy, and refuses
// an IP literal or a hostname that resolves (via net.DefaultResolver.LookupNetIP
// with a short ctx timeout) to any private/loopback/link-local/unspecified/
// multicast address per clientip.IsPrivateOrLocal. Returns a user-facing error
// message (no internal details) — safe to show in a form.
func (p Policy) ValidateURL(ctx context.Context, raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.Hostname() == "" {
		return nil, Error("Enter a full URL, starting with https://")
	}

	// A username in the URL is nearly always a paste mistake, and the
	// characters it allows are the ones that make a hostname read as
	// something it is not.
	if parsed.User != nil {
		return nil, Error("The URL must not contain a username or password")
	}

	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	scheme := strings.ToLower(parsed.Scheme)

	if scheme != "http" && scheme != "https" {
		return nil, Error("The URL must start with https:// or http://")
	}

	if len(p.AllowedHosts) > 0 && !p.hostAllowed(host) {
		return nil, Error("The URL must point at " + strings.Join(p.AllowedHosts, " or "))
	}

	addrs, err := p.resolve(ctx, host)
	if err != nil {
		return nil, err
	}

	for _, addr := range addrs {
		if !p.addressAllowed(addr) {
			return nil, Error("The URL points at a private or local network address, which is not allowed")
		}
	}

	if scheme == "http" && p.RequireHTTPS && !isLoopbackSet(addrs) {
		return nil, Error("The URL must use https://")
	}

	return parsed, nil
}

// NewClient returns an *http.Client whose transport dials through a
// DialContext that resolves the host itself and refuses private/local
// addresses at connect time (so DNS rebinding between validation and send
// cannot reach an internal address), and which never follows redirects
// (CheckRedirect returns http.ErrUseLastResponse). timeout applies to the
// whole request. Loopback is permitted only if p.AllowLoopback.
func (p Policy) NewClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	// An environment proxy would receive the connection instead of the
	// destination, and the dialer below would then be checking the proxy's
	// address rather than the one the customer supplied.
	transport.Proxy = nil
	transport.DialContext = p.dialContext(timeout)

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// A redirect is a second destination nobody validated. The caller
			// gets the 3xx and can decide what to tell the customer.
			return http.ErrUseLastResponse
		},
	}
}

// dialContext builds the transport's dialer. It resolves the name itself and
// checks every candidate address, because the address a name resolves to at
// connect time is the only one that matters.
func (p Policy) dialContext(timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	if timeout <= 0 {
		dialer.Timeout = 30 * time.Second
	}

	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("outbound: %w", err)
		}

		addrs, err := p.resolveFor(ctx, network, host)
		if err != nil {
			return nil, err
		}

		var allowed []netip.Addr
		for _, addr := range addrs {
			if p.addressAllowed(addr) {
				allowed = append(allowed, addr)
			}
		}

		if len(allowed) == 0 {
			return nil, fmt.Errorf("outbound: %s resolves only to private or local addresses", host)
		}

		var lastErr error
		for _, addr := range allowed {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}

		return nil, lastErr
	}
}

// resolve turns a hostname into the addresses it currently points at, with
// the validation timeout applied. It is the form-time lookup; the dialer does
// its own.
func (p Policy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()

	addrs, err := p.resolveFor(ctx, "tcp", host)
	if err != nil {
		return nil, Error("The URL's hostname could not be resolved")
	}

	return addrs, nil
}

// resolveFor resolves a host for one network, so a transport asking for tcp4
// is not handed an IPv6 address it cannot use. An IP literal and localhost
// never touch the resolver: the first has nothing to look up, and the second
// must mean loopback even on a machine whose hosts file says otherwise.
func (p Policy) resolveFor(ctx context.Context, network, host string) ([]netip.Addr, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}

	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()}, nil
	}

	family := "ip"
	switch network {
	case "tcp4", "udp4", "ip4":
		family = "ip4"
	case "tcp6", "udp6", "ip6":
		family = "ip6"
	}

	addrs, err := net.DefaultResolver.LookupNetIP(ctx, family, host)
	if err != nil {
		return nil, fmt.Errorf("outbound: resolve %s: %w", host, err)
	}

	if len(addrs) == 0 {
		return nil, fmt.Errorf("outbound: %s has no addresses", host)
	}

	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}

	return addrs, nil
}

// addressAllowed is the one decision both checks share.
func (p Policy) addressAllowed(addr netip.Addr) bool {
	if !clientip.IsPrivateOrLocal(addr) {
		return true
	}

	return p.AllowLoopback && addr.Unmap().IsLoopback()
}

// hostAllowed matches a hostname against the allow-list exactly, so
// hooks.slack.com.evil.example does not pass as hooks.slack.com.
func (p Policy) hostAllowed(host string) bool {
	for _, allowed := range p.AllowedHosts {
		if strings.EqualFold(strings.TrimSuffix(allowed, "."), host) {
			return true
		}
	}

	return false
}

// isLoopbackSet reports whether every address is loopback, which is the one
// case plain http is tolerated under RequireHTTPS: there is no wire for a
// packet to loopback to be read from.
func isLoopbackSet(addrs []netip.Addr) bool {
	for _, addr := range addrs {
		if !addr.Unmap().IsLoopback() {
			return false
		}
	}

	return len(addrs) > 0
}
