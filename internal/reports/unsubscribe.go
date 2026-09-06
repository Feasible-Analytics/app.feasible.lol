//
// unsubscribe.go
// Getting off a report or an alert somebody else signed you up for.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package reports

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	netmail "net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/logger"
)

// UnsubscribePath is where a token is redeemed. It is a path rather than a
// query parameter so a token cannot be dropped by a mail client that rewrites
// query strings.
const UnsubscribePath = "/unsubscribe/"

// The two things a recipient can be on.
const (
	ListReport = "report"
	ListAlert  = "alert"
)

// Sealer encrypts and reads a token.
//
// It is an interface so this package does not depend on internal/auth for one
// method pair. The implementation is *auth.Sealer, which uses the application
// key, so a token minted by one process is readable by every other process
// holding the same key and by none that do not.
type Sealer interface {
	SealToken(plaintext string) (string, error)
	OpenToken(token string) (string, error)
}

// Unsubscribed is who is being removed from what.
//
// The subscription is named by its site and kind rather than by its row id,
// because both tables are unique on that pair and a link that survives the row
// being rewritten is one fewer way for an unsubscribe to stop working.
type Unsubscribed struct {
	// List is ListReport or ListAlert.
	List   string
	SiteID int64

	// Kind is "weekly" or "monthly" for a report, "spike" or "drop" for an
	// alert.
	Kind string

	// Address is the recipient. It is inside the sealed token rather than in
	// the URL, so a link in a server log or a referrer header does not hand
	// somebody's address to a reader of that log.
	Address string
}

// Unsubscriber mints and redeems the links.
//
// The token carries the claim, sealed with the application key, so nothing is
// stored and nothing has to be tidied up when a recipient list changes. A link
// works for as long as the key does, which is what an unsubscribe clicked
// months after it arrived needs.
type Unsubscriber struct {
	Store   *Store
	Sealer  Sealer
	BaseURL string
	Log     *logger.Logger
}

// Link is where this recipient stops receiving this list, or empty when the
// unsubscriber is not configured — a self-hoster with no application key still
// gets their reports.
func (u *Unsubscriber) Link(who Unsubscribed) string {
	// No base URL means no absolute link, and a relative one in an email or a
	// List-Unsubscribe header points nowhere.
	if u == nil || u.Sealer == nil || who.Address == "" || strings.TrimSpace(u.BaseURL) == "" {
		return ""
	}

	token, err := u.Sealer.SealToken(claim(who))
	if err != nil {
		if u.Log != nil {
			u.Log.Error("could not mint an unsubscribe link", "list", who.List, "site", who.SiteID, "error", err)
		}

		return ""
	}

	return strings.TrimRight(u.BaseURL, "/") + UnsubscribePath + url.PathEscape(token)
}

// Describe reads a token and says what it would stop, without stopping it.
//
// A token that does not open, or names a row that is gone, is reported as
// already unsubscribed rather than as an error. Somebody clicking a link twice
// should see that they are off the list, not a failure page.
func (u *Unsubscriber) Describe(ctx context.Context, token string) (site string, list string, ok bool) {
	who, opened := u.read(token)
	if !opened {
		return "", "", false
	}

	domain, err := u.Store.domainFor(ctx, who)
	if err != nil {
		if u.Log != nil {
			u.Log.Error("could not read an unsubscribe target", "list", who.List, "site", who.SiteID, "error", err)
		}

		return "", "", false
	}

	// An identifier, not a phrase: the page names the list in the reader's own
	// language, and a string assembled here could never be translated.
	return domain, who.List + "_" + who.Kind, domain != ""
}

// Remove takes one address off one list and reports whether it was on it.
//
// Only that address, and only that list: a person may want the weekly report
// and not the spike alerts, and removing everything on one click is a decision
// the recipient cannot undo without asking the site's owner.
func (u *Unsubscriber) Remove(ctx context.Context, token string) error {
	who, opened := u.read(token)
	if !opened {
		// A forged or truncated token removes nothing, and says so no more
		// loudly than a second click on a real one.
		return nil
	}

	removed, err := u.Store.RemoveRecipient(ctx, who)
	if err != nil {
		return err
	}

	// The owner still believes the report goes out to everybody they typed in,
	// so the removal has to be somewhere it can be found.
	if u.Log != nil && removed {
		u.Log.Info("a recipient unsubscribed", "list", who.List, "kind", who.Kind,
			"site", who.SiteID, "address", who.Address)
	}

	return nil
}

// read opens a token into the claim it carries.
func (u *Unsubscriber) read(token string) (Unsubscribed, bool) {
	if u == nil || u.Sealer == nil || u.Store == nil {
		return Unsubscribed{}, false
	}

	plaintext, err := u.Sealer.OpenToken(strings.TrimSpace(token))
	if err != nil {
		return Unsubscribed{}, false
	}

	return parseClaim(plaintext)
}

// claim is what the token carries: the list, the row and the address, in a form
// with one unambiguous separator.
func claim(who Unsubscribed) string {
	return strings.Join([]string{who.List, strconv.FormatInt(who.SiteID, 10), who.Kind, who.Address}, "\x00")
}

// parseClaim reads a claim back, refusing anything it did not write.
func parseClaim(plaintext string) (Unsubscribed, bool) {
	parts := strings.Split(plaintext, "\x00")
	if len(parts) != 4 {
		return Unsubscribed{}, false
	}

	if parts[0] != ListReport && parts[0] != ListAlert {
		return Unsubscribed{}, false
	}

	site, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || site < 1 || parts[2] == "" || parts[3] == "" {
		return Unsubscribed{}, false
	}

	return Unsubscribed{List: parts[0], SiteID: site, Kind: parts[2], Address: parts[3]}, true
}

// domainFor names the site a subscription or rule belongs to, or empty when the
// row is gone.
func (s *Store) domainFor(ctx context.Context, who Unsubscribed) (string, error) {
	table := "report_subscriptions"
	if who.List == ListAlert {
		table = "alert_rules"
	}

	var domain string

	//nolint:gosec // the table name is chosen from two constants above, not from input
	err := s.db.QueryRowContext(ctx,
		"SELECT s.domain FROM "+table+" r JOIN sites s ON s.id = r.site_id WHERE r.site_id = ? AND r.kind = ?",
		who.SiteID, who.Kind).Scan(&domain)

	// A row that is gone is a person who is already off the list, which is a
	// page that says so rather than an error.
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("reports: read unsubscribe target: %w", err)
	}

	return domain, nil
}

// RemoveRecipient takes one address off one subscription or rule and reports
// whether it was there.
//
// The read and the write are one transaction: two people unsubscribing from the
// same report at the same time would otherwise each write the list they read,
// and one of them would put the other back.
func (s *Store) RemoveRecipient(ctx context.Context, who Unsubscribed) (bool, error) {
	table := "report_subscriptions"
	if who.List == ListAlert {
		table = "alert_rules"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("reports: unsubscribe: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var raw string

	//nolint:gosec // the table name is chosen from two constants above, not from input
	err = tx.QueryRowContext(ctx,
		"SELECT recipients FROM "+table+" WHERE site_id = ? AND kind = ?", who.SiteID, who.Kind).Scan(&raw)

	// A row that is gone is a person who is already off the list. Anything else
	// is a failure, and reporting it as a successful unsubscribe would send the
	// next report to somebody who believes they left.
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("reports: unsubscribe: %w", err)
	}

	kept, removed := without(decodeRecipients(raw), who.Address)
	if !removed {
		return false, nil
	}

	// Marshalled rather than re-validated. Every address here was validated
	// when it was stored, and refusing to remove one person because a different
	// address in the list is old would be a 500 on a working link.
	encoded, err := json.Marshal(kept)
	if err != nil {
		return false, fmt.Errorf("reports: unsubscribe: %w", err)
	}

	//nolint:gosec // the table name is chosen from two constants above, not from input
	if _, err := tx.ExecContext(ctx,
		"UPDATE "+table+" SET recipients = ?, updated_at = ? WHERE site_id = ? AND kind = ?",
		string(encoded), s.now().Unix(), who.SiteID, who.Kind); err != nil {
		return false, fmt.Errorf("reports: unsubscribe: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("reports: unsubscribe: %w", err)
	}

	return true, nil
}

// without drops one address from a list.
//
// The comparison is on the mailbox rather than the whole string, so a link
// minted while the list said `Anna <anna@example.com>` still works after the
// owner rewrites it to `anna@example.com`. These links are clicked months
// later, and one that silently removes nobody is the complaint this exists to
// prevent.
func without(recipients []string, address string) (kept []string, removed bool) {
	want := mailbox(address)
	kept = make([]string, 0, len(recipients))

	for _, recipient := range recipients {
		if mailbox(recipient) == want {
			removed = true

			continue
		}

		kept = append(kept, recipient)
	}

	return kept, removed
}

// mailbox is the comparable form of an address: the part inside the angle
// brackets, lower-cased. Anything unparseable compares as itself.
func mailbox(address string) string {
	address = strings.TrimSpace(address)

	if parsed, err := netmail.ParseAddress(address); err == nil {
		return strings.ToLower(parsed.Address)
	}

	return strings.ToLower(address)
}
