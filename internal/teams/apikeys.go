//
// apikeys.go
// API keys that belong to a team, not to the person who made them.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package teams

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/apikeys"
)

// KeyPrefix marks our keys so that one pasted into a chat window, a log line or
// a support ticket is recognisable as a credential at a glance — which is what
// makes a secret scanner able to catch it before somebody else does.
//
// It is the public API's own prefix rather than one of this package's. There is
// one api_keys table and one authenticator, and that authenticator refuses a
// presented key whose prefix it does not recognise — so a second prefix here
// would be a screen that issues credentials the API rejects.
const KeyPrefix = apikeys.Prefix

// DefaultHourlyLimit is the request budget a new key gets.
const DefaultHourlyLimit = 600

// APIKey is one issued credential, without its secret.
type APIKey struct {
	ID     int64
	TeamID int64
	UserID int64

	// Email is the address of the person who created it, carried alongside so a
	// key list reads as "whose key is this" rather than as a column of user ids.
	Email string

	Name        string
	Scopes      []string
	HourlyLimit int
	LastUsedAt  int64
	CreatedAt   int64
	RevokedAt   int64
}

// Active reports whether a key has not been revoked.
func (k APIKey) Active() bool {
	return k.RevokedAt == 0
}

// Authorisation is what a presented key resolves to. It carries the team
// explicitly because that team is the whole boundary of the key: a key issued
// against one team can never reach another team's sites, even when the person
// who made it is a member of both.
type Authorisation struct {
	KeyID  int64
	TeamID int64
	UserID int64

	// Role is the holder's role in the key's team, read at authentication time
	// rather than copied at creation time. A key is exactly as powerful as its
	// owner is right now, so demoting somebody demotes their keys with them.
	Role Role

	Scopes      []string
	HourlyLimit int
}

// ErrKeyNotAuthorised is what a presented key that no longer works resolves to.
// It is one error for four different causes — no such key, revoked, the team is
// gone, the holder has left the team — on purpose: telling an unauthenticated
// caller which of those it was is telling them how to probe.
var ErrKeyNotAuthorised = errors.New("teams: this API key is not authorised")

// CreateAPIKey issues a key against one team.
//
// Choosing the team at creation is the whole design. The incumbent's keys
// inherit every site their owner can see, so a key made for one client's
// reporting silently reads every other client on the same login, and revoking
// the person's access to one team leaves the key working against all of them.
// Here the team is part of the credential: a key reaches that team's sites and
// no others, and the moment its holder stops being a member it stops working.
//
// Viewer, Guest Editor and Guest Viewer cannot get here. That is the permission
// matrix rather than a check in this function, which is the point of having one.
func (s *Store) CreateAPIKey(ctx context.Context, actorID, teamID int64, name string, scopes []string) (string, APIKey, error) {
	if _, err := s.Authorise(ctx, teamID, actorID, PermCreateAPIKey); err != nil {
		return "", APIKey{}, err
	}

	// Minted and hashed by the package that authenticates them, so the key this
	// screen hands somebody is byte-for-byte the shape the API expects.
	secret, err := apikeys.Generate()
	if err != nil {
		return "", APIKey{}, fmt.Errorf("teams: generate key: %w", err)
	}

	if scopes == nil {
		scopes = []string{}
	}

	encoded, err := json.Marshal(scopes)
	if err != nil {
		return "", APIKey{}, fmt.Errorf("teams: encode scopes: %w", err)
	}

	key := APIKey{
		TeamID:      teamID,
		UserID:      actorID,
		Name:        strings.TrimSpace(name),
		Scopes:      scopes,
		HourlyLimit: DefaultHourlyLimit,
		CreatedAt:   s.now().Unix(),
	}

	// key_prefix is the readable head of the key, and it is what a list on this
	// screen or in the API shows so somebody can tell two keys apart without
	// either of them being recoverable.
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (team_id, user_id, name, key_hash, key_prefix, scopes, hourly_limit, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, teamID, actorID, key.Name, apikeys.Hash(secret), apikeys.Display(secret),
		string(encoded), key.HourlyLimit, key.CreatedAt)
	if err != nil {
		return "", APIKey{}, fmt.Errorf("teams: create api key: %w", err)
	}

	key.ID, _ = result.LastInsertId()

	// The secret is returned here and never again. Storing something we could
	// show a second time would mean a stolen control.db is a stolen set of live
	// credentials.
	return secret, key, nil
}

// APIKeys lists a team's keys, revoked ones included. A revoked key stays
// visible because "did we turn that one off?" is a question with a real answer,
// and a list that hides them makes it unanswerable.
func (s *Store) APIKeys(ctx context.Context, actorID, teamID int64) ([]APIKey, error) {
	role, err := s.Authorise(ctx, teamID, actorID, PermCreateAPIKey)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT api_keys.id, api_keys.team_id, api_keys.user_id, COALESCE(users.email, ''),
		       api_keys.name, api_keys.scopes, api_keys.hourly_limit,
		       COALESCE(api_keys.last_used_at, 0), api_keys.created_at, COALESCE(api_keys.revoked_at, 0)
		FROM api_keys
		LEFT JOIN users ON users.id = api_keys.user_id
		WHERE api_keys.team_id = ? AND (? OR api_keys.user_id = ?)
		ORDER BY api_keys.created_at DESC
	`, teamID, Can(role, PermManageMembers), actorID)
	if err != nil {
		return nil, fmt.Errorf("teams: list api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var keys []APIKey

	for rows.Next() {
		var (
			key    APIKey
			scopes string
		)

		if err := rows.Scan(&key.ID, &key.TeamID, &key.UserID, &key.Email, &key.Name, &scopes,
			&key.HourlyLimit, &key.LastUsedAt, &key.CreatedAt, &key.RevokedAt); err != nil {
			return nil, fmt.Errorf("teams: list api keys: %w", err)
		}

		key.Scopes = decodeScopes(scopes)
		keys = append(keys, key)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("teams: list api keys: %w", err)
	}

	return keys, nil
}

// RevokeAPIKey turns a key off. Anybody who can manage members can revoke any
// of the team's keys, because the person who made one may well be the person
// who has to be locked out.
func (s *Store) RevokeAPIKey(ctx context.Context, actorID, teamID, keyID int64) error {
	role, err := s.RoleOf(ctx, teamID, actorID)
	if err != nil {
		return err
	}

	var ownerID int64

	err = s.db.QueryRowContext(ctx, `SELECT user_id FROM api_keys WHERE id = ? AND team_id = ?`,
		keyID, teamID).Scan(&ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("teams: revoke api key: %w", err)
	}

	// Everyone manages their own keys; managing somebody else's is a
	// member-management power.
	if ownerID != actorID && !Can(role, PermManageMembers) {
		return ErrForbidden
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE api_keys SET revoked_at = ? WHERE id = ? AND team_id = ? AND revoked_at IS NULL
	`, s.now().Unix(), keyID, teamID)
	if err != nil {
		return fmt.Errorf("teams: revoke api key: %w", err)
	}

	return nil
}

// AuthenticateAPIKey resolves a presented secret to what it may do.
//
// The join onto team_memberships is the load-bearing line in this file: a key
// is only live while its holder is still a member of the team it was issued
// against. Leaving a team therefore kills that person's keys for it without
// anybody having to remember to revoke them, which is the failure the incumbent
// leaves open.
func (s *Store) AuthenticateAPIKey(ctx context.Context, secret string) (Authorisation, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return Authorisation{}, ErrKeyNotAuthorised
	}

	var (
		auth   Authorisation
		scopes string
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT api_keys.id, api_keys.team_id, api_keys.user_id, api_keys.scopes,
		       api_keys.hourly_limit, team_memberships.role
		FROM api_keys
		JOIN team_memberships
		  ON team_memberships.team_id = api_keys.team_id
		 AND team_memberships.user_id = api_keys.user_id
		WHERE api_keys.key_hash = ? AND api_keys.revoked_at IS NULL
	`, hashToken(secret)).Scan(&auth.KeyID, &auth.TeamID, &auth.UserID, &scopes,
		&auth.HourlyLimit, &auth.Role)

	if errors.Is(err, sql.ErrNoRows) {
		return Authorisation{}, ErrKeyNotAuthorised
	}
	if err != nil {
		return Authorisation{}, fmt.Errorf("teams: authenticate api key: %w", err)
	}

	auth.Scopes = decodeScopes(scopes)

	// Recording use is deliberately not part of the authentication result. A
	// failure to write the timestamp must not turn a valid key into an invalid
	// one, and the column exists for a human reading a key list rather than for
	// anything the request depends on.
	_, _ = s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Unix(), auth.KeyID)

	return auth, nil
}

// KeyReadsSite reports whether an authenticated key may read one site. It asks
// the database rather than trusting a list carried on the authorisation,
// because a site moved to another team between two requests has to stop being
// readable on the second one.
func (s *Store) KeyReadsSite(ctx context.Context, auth Authorisation, siteID int64) (bool, error) {
	if !Can(auth.Role, PermViewDashboard) {
		return false, nil
	}

	var found int

	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sites WHERE id = ? AND COALESCE(owner_team_id, account_id) = ?
	`, siteID, auth.TeamID).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("teams: check key site: %w", err)
	}

	return found > 0, nil
}

// decodeScopes reads the stored JSON array, treating anything unreadable as no
// scopes at all. A key whose scope column is corrupt must not fall open to
// every scope, which is what a nil-on-error return would mean if the caller
// read nil as "unrestricted".
func decodeScopes(raw string) []string {
	var scopes []string

	if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
		return []string{}
	}

	if scopes == nil {
		return []string{}
	}

	return scopes
}
