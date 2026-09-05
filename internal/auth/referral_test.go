//
// referral_test.go
// Tests for capturing first touch and keeping it until the account exists.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// pageRequest is a browser asking for an HTML page.
func pageRequest(t *testing.T, target, referer string) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

	return req
}

// signupRequest is the registration form being posted.
func signupRequest(t *testing.T, target, referer string, form url.Values) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

	return req
}

// firstTouchCookie runs a page load through the recorder and returns the cookie
// it set, or nil.
func firstTouchCookie(t *testing.T, req *http.Request) *http.Cookie {
	t.Helper()

	recorder := httptest.NewRecorder()
	(&Handler{BaseURL: "https://app.example.test"}).recordFirstTouch(recorder, req)

	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == referralCookie {
			return cookie
		}
	}

	return nil
}

// TestFirstTouchSurvivesToTheSignup is the whole reason this exists. The
// registration form is posted from our own page, so without carrying the
// original visit across, every account would look self-referred.
func TestFirstTouchSurvivesToTheSignup(t *testing.T) {
	landing := pageRequest(t,
		"https://app.example.test/pricing?utm_source=hn&utm_medium=social&utm_campaign=launch",
		"https://news.example.test/item?id=1")
	landing.Host = "app.example.test"

	cookie := firstTouchCookie(t, landing)
	if cookie == nil {
		t.Fatal("no first touch was recorded")
	}

	// Days later, on our own form.
	signup := signupRequest(t, "https://app.example.test/register", "https://app.example.test/pricing", nil)
	signup.Host = "app.example.test"
	signup.AddCookie(cookie)

	referral := signupReferral(signup)

	if referral.Referrer != "https://news.example.test/item?id=1" {
		t.Fatalf("the original referrer was lost: %+v", referral)
	}
	if referral.Source != "hn" || referral.Medium != "social" || referral.Campaign != "launch" {
		t.Fatalf("the original campaign was lost: %+v", referral)
	}
	if referral.LandingPage != "/pricing" {
		t.Fatalf("the landing page was lost: %+v", referral)
	}
}

// TestFirstTouchIsNotOverwritten covers the "first" in first touch. Somebody who
// arrives from one place and wanders the site must stay credited to the place
// they arrived from.
func TestFirstTouchIsNotOverwritten(t *testing.T) {
	first := pageRequest(t, "https://app.example.test/", "https://news.example.test/")
	first.Host = "app.example.test"

	cookie := firstTouchCookie(t, first)
	if cookie == nil {
		t.Fatal("no first touch was recorded")
	}

	second := pageRequest(t, "https://app.example.test/pricing", "https://elsewhere.example.test/")
	second.Host = "app.example.test"
	second.AddCookie(cookie)

	if again := firstTouchCookie(t, second); again != nil {
		t.Fatalf("a later page overwrote the first touch: %q", again.Value)
	}
}

// TestSignedInVisitorsAreNotRecorded keeps the cookie off people who already
// have an account. They are not a prospect and their attribution was written
// the day they registered.
func TestSignedInVisitorsAreNotRecorded(t *testing.T) {
	req := pageRequest(t, "https://app.example.test/sites", "https://news.example.test/")
	req.Host = "app.example.test"
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "a-session"})

	if cookie := firstTouchCookie(t, req); cookie != nil {
		t.Fatalf("a signed-in request was recorded: %q", cookie.Value)
	}
}

// TestAssetsAreNotRecorded keeps the cookie off requests that are not a person
// arriving on a page.
func TestAssetsAreNotRecorded(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://app.example.test/static/app.css", nil)
	req.Header.Set("Accept", "text/css,*/*;q=0.1")

	if cookie := firstTouchCookie(t, req); cookie != nil {
		t.Fatalf("an asset request was recorded: %q", cookie.Value)
	}
}

// TestOurOwnPagesAreNotAReferral is what keeps the field meaningful. Every
// signup is posted from our own form.
func TestOurOwnPagesAreNotAReferral(t *testing.T) {
	req := pageRequest(t, "https://app.example.test/register", "https://app.example.test/pricing")
	req.Host = "app.example.test"

	if referral := requestReferral(req); referral.Referrer != "" {
		t.Fatalf("our own page was reported as a referral: %+v", referral)
	}
}

// TestStaleFirstTouchIsIgnored covers a visit old enough that it is no longer
// the reason somebody signed up.
func TestStaleFirstTouchIsIgnored(t *testing.T) {
	stale, err := encodeReferral(Referral{
		Referrer:    "https://news.example.test/",
		FirstSeenAt: time.Now().Add(-2 * ReferralWindow).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	req := signupRequest(t, "https://app.example.test/register", "", nil)
	req.Host = "app.example.test"
	req.AddCookie(&http.Cookie{Name: referralCookie, Value: stale})

	if referral := signupReferral(req); referral.Referrer != "" {
		t.Fatalf("a stale first touch was credited: %+v", referral)
	}
}

// TestReferralFieldsAreBounded covers text an attacker controls. A referrer is
// whatever a browser was told to send, and it reaches a database row and a chat
// message.
func TestReferralFieldsAreBounded(t *testing.T) {
	long := "https://example.test/" + strings.Repeat("a", maxReferralField*2)

	forged, err := encodeReferral(Referral{Referrer: long + "\nfake line"})
	if err != nil {
		t.Fatal(err)
	}

	// Through the cookie, which is the path that skipped the live clip.
	decoded, err := decodeReferral(forged)
	if err != nil {
		t.Fatal(err)
	}

	if strings.ContainsAny(decoded.Referrer, "\r\n") {
		t.Fatalf("a referrer kept its line breaks: %q", decoded.Referrer)
	}
	if len([]rune(decoded.Referrer)) > maxReferralField+1 {
		t.Fatalf("a referrer was not clipped: %d runes", len([]rune(decoded.Referrer)))
	}
}

// TestReferralIsStoredOnce covers the row itself, including the redelivery
// guard: an account's origin is written when it is created and never revised.
func TestReferralIsStoredOnce(t *testing.T) {
	store, _ := newTestStore(t)

	user, _, err := store.CreateUser(t.Context(), "jane@example.test", "Jane Doe", "hash", "")
	if err != nil {
		t.Fatal(err)
	}

	first := Referral{
		Referrer:    "https://news.example.test/item?id=1",
		Source:      "hn",
		Medium:      "social",
		Campaign:    "launch",
		LandingPage: "/pricing",
		FirstSeenAt: 1788591600,
	}
	if err := store.SaveReferral(t.Context(), user.ID, first); err != nil {
		t.Fatal(err)
	}

	if err := store.SaveReferral(t.Context(), user.ID, Referral{Source: "somewhere-else"}); err != nil {
		t.Fatal(err)
	}

	stored, found, err := store.Referral(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the referral was not stored")
	}
	if stored != first {
		t.Fatalf("the stored referral changed:\n got %+v\nwant %+v", stored, first)
	}
}

// TestNothingLearnedStoresNothing keeps the table free of rows that say only
// that somebody signed up, which the users table already says.
func TestNothingLearnedStoresNothing(t *testing.T) {
	store, _ := newTestStore(t)

	user, _, err := store.CreateUser(t.Context(), "jane@example.test", "Jane Doe", "hash", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SaveReferral(t.Context(), user.ID, Referral{}); err != nil {
		t.Fatal(err)
	}

	if _, found, err := store.Referral(t.Context(), user.ID); err != nil || found {
		t.Fatalf("an empty referral was stored: found=%v err=%v", found, err)
	}
}

// TestCaptureClearsTheCookie stops one browser's first touch being credited to
// every account created from it.
func TestCaptureClearsTheCookie(t *testing.T) {
	store, _ := newTestStore(t)
	h := &Handler{Store: store, BaseURL: "https://app.example.test"}

	user, _, err := store.CreateUser(t.Context(), "jane@example.test", "Jane Doe", "hash", "")
	if err != nil {
		t.Fatal(err)
	}

	value, err := encodeReferral(Referral{Source: "hn", LandingPage: "/pricing"})
	if err != nil {
		t.Fatal(err)
	}

	req := signupRequest(t, "https://app.example.test/register", "", nil)
	req.Host = "app.example.test"
	req.AddCookie(&http.Cookie{Name: referralCookie, Value: value})

	recorder := httptest.NewRecorder()
	if referral := h.captureReferral(recorder, req, user.ID); referral.Source != "hn" {
		t.Fatalf("the captured referral was wrong: %+v", referral)
	}

	var cleared bool
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == referralCookie && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the first-touch cookie was not cleared after it was stored")
	}

	if _, found, err := store.Referral(t.Context(), user.ID); err != nil || !found {
		t.Fatalf("the captured referral was not stored: found=%v err=%v", found, err)
	}
}

// TestReferralDiesWithTheAccount is a deletion claim, so it is checked rather
// than assumed. An attribution row that outlives the account it describes is
// data we told somebody we had destroyed.
func TestReferralDiesWithTheAccount(t *testing.T) {
	store, db := newTestStore(t)

	user, _, err := store.CreateUser(t.Context(), "jane@example.test", "Jane Doe", "hash", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SaveReferral(t.Context(), user.ID, Referral{Source: "hn"}); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(t.Context(), "DELETE FROM users WHERE id = ?", user.ID); err != nil {
		t.Fatal(err)
	}

	if _, found, err := store.Referral(t.Context(), user.ID); err != nil || found {
		t.Fatalf("the referral outlived the account: found=%v err=%v", found, err)
	}
}
