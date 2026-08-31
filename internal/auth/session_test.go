//
// session_test.go
// The rolling idle window, the device label, and the cookie that must have no Domain.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// TestSessionExpiresAfterFourteenIdleDays walks the clock past the idle window
// and checks the session is gone — and that using it in between pushes the
// deadline forward, because the window is idle time rather than a fixed
// lifetime.
func TestSessionExpiresAfterFourteenIdleDays(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Now()
	s.SetClock(func() time.Time { return now })

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	token, _, err := s.CreateSession(ctx, user.ID, "Chrome on macOS")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	now = now.Add(13 * 24 * time.Hour)

	if _, err := s.SessionByToken(ctx, token); err != nil {
		t.Fatalf("session should still be live after 13 idle days: %v", err)
	}

	now = now.Add(13 * 24 * time.Hour)

	if _, err := s.SessionByToken(ctx, token); err != nil {
		t.Fatalf("using the session should have extended it: %v", err)
	}

	now = now.Add(SessionIdleWindow + time.Minute)

	if _, err := s.SessionByToken(ctx, token); err != ErrNotFound {
		t.Errorf("want ErrNotFound past the idle window, got %v", err)
	}
}

// TestSessionCookieHasNoDomain is the regression test for the bug that broke an
// incumbent's self-hosters for a whole release. A Domain attribute derived from
// the base URL makes the browser silently refuse the cookie on every other
// hostname the dashboard can be reached on, and login then fails with a bare
// 403 and nothing in any log.
func TestSessionCookieHasNoDomain(t *testing.T) {
	recorder := httptest.NewRecorder()

	SetSessionCookie(recorder, "token", "https://analytics.example.com")

	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("want one cookie, got %d", len(cookies))
	}

	if cookies[0].Domain != "" {
		t.Errorf("the session cookie must not carry a Domain, got %q", cookies[0].Domain)
	}

	if !cookies[0].HttpOnly {
		t.Error("the session cookie should be HttpOnly")
	}

	if !cookies[0].Secure {
		t.Error("an https base URL should produce a Secure cookie")
	}

	// Plain HTTP on a private network is a supported way to run this, and a
	// Secure cookie over plain HTTP is silently dropped.
	plain := httptest.NewRecorder()
	SetSessionCookie(plain, "token", "http://nas.local:19301")

	if plain.Result().Cookies()[0].Secure {
		t.Error("an http base URL must not produce a Secure cookie")
	}
}

// TestClearSessionCookieMatchesTheOneItSets checks the attributes line up. A
// delete that differs in Path or Domain creates a second cookie rather than
// removing the first, which is how a sign-out button ends up doing nothing.
func TestClearSessionCookieMatchesTheOneItSets(t *testing.T) {
	set := httptest.NewRecorder()
	SetSessionCookie(set, "token", "https://example.com")

	clear := httptest.NewRecorder()
	ClearSessionCookie(clear, "https://example.com")

	before, after := set.Result().Cookies()[0], clear.Result().Cookies()[0]

	if before.Path != after.Path || before.Domain != after.Domain ||
		before.Secure != after.Secure || before.SameSite != after.SameSite {
		t.Errorf("the clearing cookie does not match the one that was set: %+v vs %+v", before, after)
	}

	if after.MaxAge >= 0 {
		t.Errorf("the clearing cookie should expire immediately, got MaxAge %d", after.MaxAge)
	}
}

// TestListAndRevokeSessions checks what the login-management screen is for:
// seeing every signed-in browser, and being able to end one.
func TestListAndRevokeSessions(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	_, first, err := s.CreateSession(ctx, user.ID, "Chrome on macOS")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	secondToken, second, err := s.CreateSession(ctx, user.ID, "Safari on iPhone")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	list, err := s.ListSessions(ctx, user.ID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}

	if len(list) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(list))
	}

	if err := s.RevokeSession(ctx, user.ID, second.ID); err != nil {
		t.Fatalf("revoke session: %v", err)
	}

	if _, err := s.SessionByToken(ctx, secondToken); err != ErrNotFound {
		t.Errorf("the revoked session should be gone, got %v", err)
	}

	// Another user's id in the WHERE clause is what stops a forged form field
	// revoking somebody else's session.
	other, _, err := s.CreateUser(ctx, "b@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := s.RevokeSession(ctx, other.ID, first.ID); err != nil {
		t.Fatalf("revoke session: %v", err)
	}

	list, err = s.ListSessions(ctx, user.ID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}

	if len(list) != 1 {
		t.Errorf("another user must not be able to revoke this session, got %d left", len(list))
	}
}

// TestPruneSessionsDeletesTheExpired checks the sweep that clears the far more
// common case: a browser that is simply never used again.
func TestPruneSessionsDeletesTheExpired(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()

	now := time.Now()
	s.SetClock(func() time.Time { return now })

	user, _, err := s.CreateUser(ctx, "a@example.com", "", "hash", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if _, _, err := s.CreateSession(ctx, user.ID, "Chrome on macOS"); err != nil {
		t.Fatalf("create session: %v", err)
	}

	now = now.Add(SessionIdleWindow + time.Hour)

	if _, err := s.PruneSessions(ctx); err != nil {
		t.Fatalf("prune sessions: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM user_sessions").Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}

	if count != 0 {
		t.Errorf("want no sessions left, got %d", count)
	}
}

// TestDeviceLabelIsRecognisable checks the label a person reads on the sessions
// screen. An unreadable one makes that screen useless, because the only
// question it answers is "is one of these not me".
func TestDeviceLabelIsRecognisable(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36": "Chrome on macOS",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Version/17.0 Safari/604.1":             "Safari on iPhone",
		"Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0":                                            "Firefox on Linux",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0 Safari/537.36 Edg/120.0":                 "Edge on Windows",
		"": "Unknown device",
	}

	for agent, want := range cases {
		if got := DeviceLabel(agent); got != want {
			t.Errorf("DeviceLabel(%.40q) = %q, want %q", agent, got, want)
		}
	}
}

// TestHashTokenIsStableAndOneWay checks the transform every stored credential
// in this package goes through.
func TestHashTokenIsStableAndOneWay(t *testing.T) {
	token, hash, err := NewSessionToken()
	if err != nil {
		t.Fatalf("new session token: %v", err)
	}

	if hash == token {
		t.Fatal("the stored hash must not be the token")
	}

	if HashToken(token) != hash {
		t.Error("hashing the same token twice should give the same result")
	}

	other, _, err := NewSessionToken()
	if err != nil {
		t.Fatalf("new session token: %v", err)
	}

	if other == token {
		t.Error("two tokens should never be equal")
	}
}
