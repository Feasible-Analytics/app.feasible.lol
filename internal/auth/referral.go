//
// referral.go
// Where a new account came from, carried to sign-up on the link and nowhere else.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// The parameters a link to the sign-up page may carry. The campaign labels are
// the standard ones. The other two are stated by the page that sent the
// visitor, because by the time a browser reaches the form the only referrer it
// reports is that page.
const (
	paramSource   = "utm_source"
	paramMedium   = "utm_medium"
	paramCampaign = "utm_campaign"
	paramReferrer = "referrer"
	paramLanding  = "landing"
)

// The bounds on every stored and reported value. These arrive on a public URL,
// so they are attacker-controlled text that ends up in a database row and a
// chat message.
const (
	maxReferralLabel   = 80
	maxReferralDomain  = 253
	maxReferralLanding = 200
)

// referralDomain is the shape of a hostname. Anything else in the referrer
// field is dropped rather than stored.
var referralDomain = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// Referral is where one person came from. Every field is optional: many
// browsers and most apps send no referrer, and most visits carry no campaign.
type Referral struct {
	// Referrer is the domain that sent them, never a full address.
	Referrer string `json:"r,omitempty"`

	Source   string `json:"s,omitempty"`
	Medium   string `json:"m,omitempty"`
	Campaign string `json:"c,omitempty"`

	// LandingPage is the path of the first marketing page they saw, without
	// its query string.
	LandingPage string `json:"l,omitempty"`
}

// Empty reports whether we learned nothing at all.
func (r Referral) Empty() bool {
	return r.Referrer == "" && r.Source == "" && r.Medium == "" &&
		r.Campaign == "" && r.LandingPage == ""
}

// Values renders the referral as the link parameters it arrived in, so a form
// or a redirect can hand it to the next request.
func (r Referral) Values() url.Values {
	values := url.Values{}

	for _, field := range []struct{ name, value string }{
		{paramSource, r.Source},
		{paramMedium, r.Medium},
		{paramCampaign, r.Campaign},
		{paramReferrer, r.Referrer},
		{paramLanding, r.LandingPage},
	} {
		if field.value != "" {
			values.Set(field.name, field.value)
		}
	}

	return values
}

// LinkSuffix is the referral as extra query parameters for a link that already
// has a query string. It is typed as a URL so the template writes it as
// parameters rather than escaping it into the value before it.
func (r Referral) LinkSuffix() template.URL {
	encoded := r.Values().Encode()
	if encoded == "" {
		return ""
	}

	return template.URL("&" + encoded) //nolint:gosec // url.Values.Encode escapes every name and value
}

// requestReferral reads what one request says about where its visitor came
// from. The link parameters and the form's hidden fields are the same names, so
// the page load, the posted form and the federated sign-in link all read here.
func requestReferral(r *http.Request) Referral {
	if r == nil {
		return Referral{}
	}

	query := r.URL.Query()

	// The first non-empty value wins, with the address ahead of the form body.
	pick := func(names ...string) string {
		for _, name := range names {
			if value := strings.TrimSpace(query.Get(name)); value != "" {
				return value
			}
			if value := strings.TrimSpace(r.PostFormValue(name)); value != "" {
				return value
			}
		}

		return ""
	}

	referral := Referral{
		Source:      clipField(pick(paramSource, "ref", "via"), maxReferralLabel),
		Medium:      clipField(pick(paramMedium), maxReferralLabel),
		Campaign:    clipField(pick(paramCampaign), maxReferralLabel),
		Referrer:    cleanDomain(pick(paramReferrer)),
		LandingPage: cleanLanding(pick(paramLanding)),
	}

	// A visitor who followed a link straight to this deployment has no page of
	// ours to state the referrer for them, so the browser's own header is read.
	if referral.Referrer == "" {
		referral.Referrer = cleanDomain(r.Referer())
	}

	// Our own pages are not a referral. Reporting them would make every
	// sign-up look self-referred and hide whoever actually sent the visitor.
	if ownDomain(referral.Referrer, r.Host) {
		referral.Referrer = ""
	}

	return referral
}

// encodeReferral packs a referral into a token that is safe inside a URL.
func encodeReferral(referral Referral) (string, error) {
	payload, err := json.Marshal(referral)
	if err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(payload), nil
}

// decodeReferral reads a token back. Every field is cleaned again on the way
// out, because the token travels through the browser and the length limits are
// what protect the row and the chat message.
func decodeReferral(value string) (Referral, error) {
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return Referral{}, err
	}

	var referral Referral
	if err := json.Unmarshal(payload, &referral); err != nil {
		return Referral{}, err
	}

	referral.Referrer = cleanDomain(referral.Referrer)
	referral.Source = clipField(referral.Source, maxReferralLabel)
	referral.Medium = clipField(referral.Medium, maxReferralLabel)
	referral.Campaign = clipField(referral.Campaign, maxReferralLabel)
	referral.LandingPage = cleanLanding(referral.LandingPage)

	return referral, nil
}

// stateWithReferral appends a referral to a federated sign-in's state value.
// The identity provider returns the state unchanged on the callback link, which
// is what carries the referral across a redirect that leaves this deployment.
func stateWithReferral(state string, referral Referral) string {
	if referral.Empty() {
		return state
	}

	encoded, err := encodeReferral(referral)
	if err != nil {
		return state
	}

	return state + "." + encoded
}

// referralFromState reads back what stateWithReferral appended. A state with
// nothing appended, or with something unreadable, is an empty referral.
func referralFromState(state string) Referral {
	_, encoded, found := strings.Cut(state, ".")
	if !found {
		return Referral{}
	}

	referral, err := decodeReferral(encoded)
	if err != nil {
		return Referral{}
	}

	return referral
}

// cleanDomain reduces a referrer to its hostname. It accepts a bare hostname or
// a full address and keeps only the host, lower-cased, so no path, query string
// or port is ever stored.
func cleanDomain(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}

	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return ""
		}
		value = parsed.Hostname()
	} else {
		if cut := strings.IndexAny(value, "/?#"); cut >= 0 {
			value = value[:cut]
		}
		if host, _, found := strings.Cut(value, ":"); found {
			value = host
		}
	}

	if len(value) > maxReferralDomain || !referralDomain.MatchString(value) {
		return ""
	}

	return value
}

// cleanLanding reduces a landing page to its path. Anything that is not a
// local path is dropped, and the query string and fragment are cut off.
func cleanLanding(value string) string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return ""
	}

	if cut := strings.IndexAny(value, "?#"); cut >= 0 {
		value = value[:cut]
	}

	return clipField(value, maxReferralLanding)
}

// ownDomain reports whether a referring domain is this deployment or the site
// it sits under, so app.example.com treats example.com and www.example.com as
// its own pages.
func ownDomain(domain, host string) bool {
	if domain == "" || host == "" {
		return false
	}

	host = strings.ToLower(host)
	if name, _, found := strings.Cut(host, ":"); found {
		host = name
	}

	domain = strings.TrimPrefix(domain, "www.")

	return host == domain || strings.HasSuffix(host, "."+domain)
}

// clipField bounds one value and strips the control characters that would let a
// crafted link forge extra lines in a chat message.
func clipField(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}

		return r
	}, strings.TrimSpace(value))

	if len(value) > limit {
		return strings.ToValidUTF8(value[:limit], "")
	}

	return value
}

// SaveReferral records where one account came from. The first write for an
// account is the only one kept.
func (s *Store) SaveReferral(ctx context.Context, userID int64, referral Referral) error {
	if referral.Empty() {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_referrals
			(user_id, referrer, source, medium, campaign, landing_page, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO NOTHING`,
		userID, referral.Referrer, referral.Source, referral.Medium,
		referral.Campaign, referral.LandingPage, s.Now().UTC().Unix(),
	)

	return err
}

// Referral reads back where one account came from.
func (s *Store) Referral(ctx context.Context, userID int64) (Referral, bool, error) {
	var referral Referral

	err := s.db.QueryRowContext(ctx, `
		SELECT referrer, source, medium, campaign, landing_page
		FROM user_referrals WHERE user_id = ?`, userID).
		Scan(&referral.Referrer, &referral.Source, &referral.Medium,
			&referral.Campaign, &referral.LandingPage)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Referral{}, false, nil
		}

		return Referral{}, false, err
	}

	return referral, true, nil
}

// captureReferral stores where a brand-new account came from. A failed write
// is logged and the registration continues, because an attribution row is not
// worth failing a sign-up over.
func (h *Handler) captureReferral(ctx context.Context, userID int64, referral Referral) {
	if referral.Empty() || h.Store == nil {
		return
	}

	if err := h.Store.SaveReferral(ctx, userID, referral); err != nil && h.Log != nil {
		h.Log.Error("could not store where an account came from", "user", userID, "error", err)
	}
}
