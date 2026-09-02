//
// store.go
// Public sites and shared links: the tokens, the passwords, and what a link may pin.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package sharing lets somebody see a dashboard without an account: a site made
// fully public at a stable URL, or a tokenised link that can be password
// protected and pinned to one segment.
//
// One capability is deliberately absent and will stay absent. A
// password-protected dashboard cannot be embedded in an iframe, because making
// the password form work cross-origin means serving it without X-Frame-Options,
// which makes it clickjackable — a page that looks like a document but is
// actually a login sitting invisibly under somebody's cursor. We refuse the
// combination in Embeddable rather than building it and regretting it.
package sharing

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SlugBytes is how much randomness a share token carries. Sixteen bytes is 128
// bits: the URL is the entire credential for whoever holds it, so it has to be
// unguessable rather than merely unlikely, and 22 base64 characters is still
// short enough to paste into a chat message.
const SlugBytes = 16

// PBKDF2 parameters for a link password.
//
// A share password is typed by a person and handed to other people, so it is a
// real password with all a password's weaknesses — reused, short, and worth
// running a dictionary against if system.db ever leaks. A bare digest would
// fall to a wordlist in seconds. Two hundred thousand iterations costs a few
// milliseconds on the one request that checks it and turns that wordlist into
// weeks.
const (
	pbkdf2Iterations = 200_000
	pbkdf2KeyLength  = 32
	saltBytes        = 16

	// PasswordAttemptLimit bounds expensive derivations for one source and link.
	PasswordAttemptLimit = 6

	// PasswordLinkAttemptLimit bounds guesses at one link from every source
	// together. Per-source budgets stop one address from hammering a link;
	// this stops an attacker who has many addresses from doing the same.
	PasswordLinkAttemptLimit = 60

	// PasswordAttemptWindow resets a source's per-link budget.
	PasswordAttemptWindow = 15 * time.Minute

	// linkBudgetSource is the source key the per-link ceiling is counted
	// under. Real source keys are hex, so it can never collide with one.
	linkBudgetSource = "*"
)

// The failures a caller must be able to tell apart.
var (
	// ErrNotFound means no such link, or the site behind it is gone.
	ErrNotFound = errors.New("sharing: no such shared link")

	// ErrPasswordRequired means the link is real and needs a password.
	ErrPasswordRequired = errors.New("sharing: this link is password protected")

	// ErrWrongPassword means the password did not match.
	ErrWrongPassword = errors.New("sharing: that password is not correct")

	// ErrPasswordThrottled means this source exhausted this link's bounded
	// online password budget for the current window.
	ErrPasswordThrottled = errors.New("sharing: too many password attempts")

	// ErrEmbedNotAllowed is the refusal at the heart of this package: a
	// password-protected link may not be embedded, at all, ever.
	ErrEmbedNotAllowed = errors.New("sharing: a password-protected link cannot be embedded")
)

// Link is one shared view of a site.
type Link struct {
	ID     int64
	SiteID int64
	Domain string
	Name   string
	Slug   string

	// HasPassword says whether one is set, without carrying the hash out of
	// the database. Nothing outside this file ever needs the hash.
	HasPassword bool

	// SegmentID pins the link to one saved segment, or zero for the whole
	// site. It is how an agency shares "your campaign's traffic" rather than
	// "everything we know about this site".
	SegmentID int64

	CreatedBy int64
	CreatedAt int64
}

// Path is the URL prefix every request for this link lives under. It is a
// method rather than a string built at each call site because the front end has
// to keep this exact prefix on every URL it constructs — see Bootstrap.
func (l Link) Path() string {
	return SharePrefix + l.Slug
}

// Embeddable reports whether this link may be rendered inside an iframe.
//
// A password-protected link is never embeddable. The reason is not that it is
// hard: it is that the only way to make the password form work in a third-party
// frame is to serve it without X-Frame-Options, and a login form that any site
// may frame is a login form any site can overlay and steal. There is no
// configuration that makes it safe, so there is no configuration.
func (l Link) Embeddable() bool {
	return !l.HasPassword
}

// Store is the system-database side of sharing.
type Store struct {
	db *sql.DB

	// Now stamps creation, injectable for tests.
	Now func() time.Time
}

// NewStore builds a store over the system database.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, Now: func() time.Time { return time.Now().UTC() }}
}

// now reads the injected clock, falling back to the real one.
func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}

	return s.Now().UTC()
}

// SetPublicForOwner turns a site's fully public dashboard on or off, but only
// while the expected team still owns the site, so a settings request that
// raced a transfer cannot publish the new owner's numbers. It is a single
// boolean rather than a token because "public" means exactly that: the site's
// numbers are readable by anybody who knows the domain, at a stable URL that
// can be linked to from a blog post and will still work next year.
func (s *Store) SetPublicForOwner(ctx context.Context, siteID, expectedOwnerTeamID int64, public bool) error {
	value := 0
	if public {
		value = 1
	}

	tx, err := s.ownerMutation(ctx, siteID, expectedOwnerTeamID)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	result, err := tx.ExecContext(ctx, `
		UPDATE sites SET is_public = ?, updated_at = ? WHERE id = ?
	`, value, s.now().Unix(), siteID)
	if err != nil {
		return fmt.Errorf("sharing: set public: %w", err)
	}

	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sharing: set public: %w", err)
	}

	return nil
}

// PublicSite resolves a domain to a site that has been made public, or
// ErrNotFound. A site that exists but is private answers the same as one that
// does not, so the endpoint cannot be used to enumerate domains.
func (s *Store) PublicSite(ctx context.Context, domain string) (Link, error) {
	var link Link

	err := s.db.QueryRowContext(ctx, `
		SELECT id, domain FROM sites WHERE domain = ? AND is_public = 1
	`, domain).Scan(&link.SiteID, &link.Domain)

	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("sharing: read public site: %w", err)
	}

	return link, nil
}

// CreateLinkForOwner mints a shared link, but only while the expected team
// still owns the site at the moment of the write. The password is optional;
// when it is set the link is permanently non-embeddable, which the caller is
// told by Embeddable rather than discovering when the iframe is blank.
func (s *Store) CreateLinkForOwner(ctx context.Context, siteID, expectedOwnerTeamID int64,
	name, password string, segmentID, createdBy int64) (Link, error) {
	if segmentID != 0 {
		if _, err := s.SegmentFilters(ctx, siteID, segmentID); err != nil {
			return Link{}, err
		}
	}

	slug, err := newSlug()
	if err != nil {
		return Link{}, err
	}

	hash, salt := "", ""

	if strings.TrimSpace(password) != "" {
		hash, salt, err = hashPassword(password)
		if err != nil {
			return Link{}, err
		}
	}

	link := Link{
		SiteID:      siteID,
		Name:        strings.TrimSpace(name),
		Slug:        slug,
		HasPassword: hash != "",
		SegmentID:   segmentID,
		CreatedBy:   createdBy,
		CreatedAt:   s.now().Unix(),
	}

	var segment any
	if segmentID != 0 {
		segment = segmentID
	}

	var creator any
	if createdBy != 0 {
		creator = createdBy
	}

	tx, err := s.ownerMutation(ctx, siteID, expectedOwnerTeamID)
	if err != nil {
		return Link{}, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	result, err := tx.ExecContext(ctx, `
		INSERT INTO shared_links
			(site_id, name, slug, password_hash, password_salt, segment_id, created_by_user_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, siteID, link.Name, slug, hash, salt, segment, creator, link.CreatedAt)
	if err != nil {
		return Link{}, fmt.Errorf("sharing: create link: %w", err)
	}

	link.ID, _ = result.LastInsertId()
	if err := tx.Commit(); err != nil {
		return Link{}, fmt.Errorf("sharing: create link: %w", err)
	}

	return link, nil
}

// Links lists a site's shared links.
func (s *Store) Links(ctx context.Context, siteID int64) ([]Link, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT shared_links.id, shared_links.site_id, sites.domain, shared_links.name, shared_links.slug,
		       shared_links.password_hash <> '', COALESCE(shared_links.segment_id, 0),
		       COALESCE(shared_links.created_by_user_id, 0), shared_links.created_at
		FROM shared_links
		JOIN sites ON sites.id = shared_links.site_id
		WHERE shared_links.site_id = ?
		ORDER BY shared_links.created_at DESC
	`, siteID)
	if err != nil {
		return nil, fmt.Errorf("sharing: list links: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var links []Link

	for rows.Next() {
		var link Link

		if err := rows.Scan(&link.ID, &link.SiteID, &link.Domain, &link.Name, &link.Slug,
			&link.HasPassword, &link.SegmentID, &link.CreatedBy, &link.CreatedAt); err != nil {
			return nil, fmt.Errorf("sharing: list links: %w", err)
		}

		links = append(links, link)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sharing: list links: %w", err)
	}

	return links, nil
}

// RevokeLinkForOwner deletes a shared link, but only while the expected team
// still owns the site. There is no soft delete: a link is a live credential,
// and "revoked but still in the table" is one query bug away from still
// working.
func (s *Store) RevokeLinkForOwner(ctx context.Context, siteID, expectedOwnerTeamID, linkID int64) error {
	tx, err := s.ownerMutation(ctx, siteID, expectedOwnerTeamID)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	result, err := tx.ExecContext(ctx, `DELETE FROM shared_links WHERE id = ? AND site_id = ?`, linkID, siteID)
	if err != nil {
		return fmt.Errorf("sharing: revoke link: %w", err)
	}

	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sharing: revoke link: %w", err)
	}

	return nil
}

// ownerMutation validates current ownership inside the transaction it returns,
// so a publication mutation and a site transfer serialise on system.db's
// writer rather than both authorising from stale reads.
func (s *Store) ownerMutation(ctx context.Context, siteID, expectedOwnerTeamID int64) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sharing: begin owner mutation: %w", err)
	}
	var ownerTeamID int64
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(owner_team_id, account_id) FROM sites WHERE id = ?
	`, siteID).Scan(&ownerTeamID)
	if errors.Is(err, sql.ErrNoRows) || ownerTeamID != expectedOwnerTeamID {
		tx.Rollback() //nolint:errcheck // transaction cannot be reused
		return nil, ErrSiteOwnerChanged
	}
	if err != nil {
		tx.Rollback() //nolint:errcheck // transaction cannot be reused
		return nil, fmt.Errorf("sharing: verify site owner: %w", err)
	}

	return tx, nil
}

// ErrSiteOwnerChanged means a transfer committed after request authorization.
var ErrSiteOwnerChanged = errors.New("sharing: the site owner changed; reload and try again")

// Resolve looks up a link by its slug.
func (s *Store) Resolve(ctx context.Context, slug string) (Link, error) {
	var link Link

	err := s.db.QueryRowContext(ctx, `
		SELECT shared_links.id, shared_links.site_id, sites.domain, shared_links.name, shared_links.slug,
		       shared_links.password_hash <> '', COALESCE(shared_links.segment_id, 0),
		       COALESCE(shared_links.created_by_user_id, 0), shared_links.created_at
		FROM shared_links
		JOIN sites ON sites.id = shared_links.site_id
		WHERE shared_links.slug = ?
	`, slug).Scan(&link.ID, &link.SiteID, &link.Domain, &link.Name, &link.Slug,
		&link.HasPassword, &link.SegmentID, &link.CreatedBy, &link.CreatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("sharing: resolve link: %w", err)
	}

	return link, nil
}

// CheckPasswordForSource durably reserves one attempt before paying PBKDF2's
// CPU cost. The writer transaction serializes concurrent guesses against the
// same row and resets an expired window atomically.
func (s *Store) CheckPasswordForSource(ctx context.Context, linkID int64, sourceKey, password string) error {
	hash, salt, err := s.reservePasswordAttempt(ctx, linkID, sourceKey)
	if err != nil {
		return err
	}
	if hash == "" {
		return nil
	}
	if salt == "" {
		if err := s.checkLegacyPassword(ctx, linkID, password, hash); err != nil {
			return err
		}
		return s.clearPasswordAttempts(ctx, linkID, sourceKey)
	}

	expected, err := deriveKey(password, salt)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(hash)) != 1 {
		return ErrWrongPassword
	}

	return s.clearPasswordAttempts(ctx, linkID, sourceKey)
}

// reservePasswordAttempt reads the current credential and consumes one slot in
// the source-link window under a SQLite writer reservation.
func (s *Store) reservePasswordAttempt(ctx context.Context, linkID int64, sourceKey string) (string, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("sharing: reserve password attempt: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	if _, err := tx.ExecContext(ctx, `UPDATE share_password_attempts SET attempts = attempts WHERE link_id = -1`); err != nil {
		return "", "", fmt.Errorf("sharing: reserve password attempt: %w", err)
	}

	var hash, salt string
	err = tx.QueryRowContext(ctx, `
		SELECT password_hash, password_salt FROM shared_links WHERE id = ?
	`, linkID).Scan(&hash, &salt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("sharing: reserve password attempt: %w", err)
	}
	if hash == "" {
		if err := tx.Commit(); err != nil {
			return "", "", fmt.Errorf("sharing: reserve password attempt: %w", err)
		}
		return "", "", nil
	}

	now := s.now()
	if err := reserveAttemptSlot(ctx, tx, linkID, sourceKey, PasswordAttemptLimit, now); err != nil {
		return "", "", err
	}
	if err := reserveAttemptSlot(ctx, tx, linkID, linkBudgetSource, PasswordLinkAttemptLimit, now); err != nil {
		return "", "", err
	}
	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("sharing: reserve password attempt: %w", err)
	}

	return hash, salt, nil
}

// reserveAttemptSlot consumes one attempt in a (link, key) window, opening or
// resetting the window as needed, or reports the budget spent. Rows whose
// window has lapsed are deleted whenever a new window opens, so the table can
// never hold more than the windows currently open: nothing else ever reads
// them, and unauthenticated traffic must not grow system.db without bound.
func reserveAttemptSlot(ctx context.Context, tx *sql.Tx, linkID int64, key string, limit int, now time.Time) error {
	expiry := now.Add(-PasswordAttemptWindow).Unix()
	windowStart, attempts := int64(0), 0
	err := tx.QueryRowContext(ctx, `
		SELECT window_started_at, attempts FROM share_password_attempts
		WHERE link_id = ? AND source_key = ?
	`, linkID, key).Scan(&windowStart, &attempts)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM share_password_attempts WHERE window_started_at <= ?
		`, expiry); err != nil {
			return fmt.Errorf("sharing: purge password attempts: %w", err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO share_password_attempts (link_id, source_key, window_started_at, attempts)
			VALUES (?, ?, ?, 1)
		`, linkID, key, now.Unix())
	case err != nil:
		return fmt.Errorf("sharing: reserve password attempt: %w", err)
	case windowStart <= expiry:
		_, err = tx.ExecContext(ctx, `
			UPDATE share_password_attempts SET window_started_at = ?, attempts = 1
			WHERE link_id = ? AND source_key = ?
		`, now.Unix(), linkID, key)
	case attempts >= limit:
		return ErrPasswordThrottled
	default:
		_, err = tx.ExecContext(ctx, `
			UPDATE share_password_attempts SET attempts = attempts + 1
			WHERE link_id = ? AND source_key = ?
		`, linkID, key)
	}
	if err != nil {
		return fmt.Errorf("sharing: reserve password attempt: %w", err)
	}

	return nil
}

// clearPasswordAttempts gives a source a fresh window after successful proof.
// The per-link ceiling is left alone: it counts every guess, right or wrong,
// which is what makes it a backstop rather than a budget one success resets.
func (s *Store) clearPasswordAttempts(ctx context.Context, linkID int64, sourceKey string) error {
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM share_password_attempts WHERE link_id = ? AND source_key = ?
	`, linkID, sourceKey); err != nil {
		return fmt.Errorf("sharing: clear password attempts: %w", err)
	}

	return nil
}

// checkLegacyPassword verifies a row still holding an unsalted SHA-256 digest
// and replaces it with PBKDF2 only after a constant-time successful comparison.
// The compare-and-swap predicate prevents a concurrent verifier from replacing
// a newer salted value or moving a row back to the legacy representation.
func (s *Store) checkLegacyPassword(ctx context.Context, linkID int64, password, legacyHash string) error {
	expected := legacyPasswordHash(password)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(legacyHash)) != 1 {
		return ErrWrongPassword
	}

	hash, salt, err := hashPassword(password)
	if err != nil {
		return err
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE shared_links SET password_hash = ?, password_salt = ?
		WHERE id = ? AND password_hash = ? AND password_salt = ''
	`, hash, salt, linkID, legacyHash)
	if err != nil {
		return fmt.Errorf("sharing: upgrade legacy password: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 1 {
		return nil
	}

	// Another successful request may have completed the same upgrade while this
	// one derived its replacement. Verify that committed value rather than
	// authenticating against a row whose credential changed underneath us.
	var currentHash, currentSalt string
	if err := s.db.QueryRowContext(ctx, `
		SELECT password_hash, password_salt FROM shared_links WHERE id = ?
	`, linkID).Scan(&currentHash, &currentSalt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}

		return fmt.Errorf("sharing: verify upgraded password: %w", err)
	}
	if currentSalt == "" {
		return errors.New("sharing: legacy password upgrade did not commit")
	}

	currentExpected, err := deriveKey(password, currentSalt)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(currentExpected), []byte(currentHash)) != 1 {
		return ErrWrongPassword
	}

	return nil
}

// legacyPasswordHash reproduces the unsalted digest older rows hold. It is
// verification-only: every write path uses hashPassword and a random salt.
func legacyPasswordHash(password string) string {
	digest := sha256.Sum256([]byte(password))

	return hex.EncodeToString(digest[:])
}

// newSlug mints a share token.
func newSlug() (string, error) {
	raw := make([]byte, SlugBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("sharing: generate slug: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// hashPassword derives a stored hash and its salt.
func hashPassword(password string) (hash, salt string, err error) {
	raw := make([]byte, saltBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("sharing: generate salt: %w", err)
	}

	salt = hex.EncodeToString(raw)

	hash, err = deriveKey(password, salt)
	if err != nil {
		return "", "", err
	}

	return hash, salt, nil
}

// deriveKey runs PBKDF2-HMAC-SHA256 at this package's parameters. The salt is
// fed in as its hex text rather than decoded, because that is what every
// stored hash was derived with; decoding it would invalidate all of them.
func deriveKey(password, salt string) (string, error) {
	if salt == "" {
		return "", errors.New("sharing: a password hash needs a salt")
	}

	key, err := pbkdf2.Key(sha256.New, password, []byte(salt), pbkdf2Iterations, pbkdf2KeyLength)
	if err != nil {
		return "", fmt.Errorf("sharing: derive key: %w", err)
	}

	return hex.EncodeToString(key), nil
}
