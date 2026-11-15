//
// referral_test.go
// Tests for carrying where a visitor came from to the account they create.
//
// Created: 2026-09-05
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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

// readBody returns a response's body and closes it.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer closeResponseBody(t, resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

// TestLinkParametersAreRead covers the link the marketing site builds: the
// campaign labels, the domain that sent the visitor and the page they landed on.
func TestLinkParametersAreRead(t *testing.T) {
	req := pageRequest(t,
		"https://app.example.test/register?utm_source=hn&utm_medium=social&utm_campaign=launch"+
			"&referrer=news.example.test&landing=%2Fpricing%2F",
		"https://example.test/")

	got := requestReferral(req)
	want := Referral{
		Referrer: "news.example.test", Source: "hn", Medium: "social",
		Campaign: "launch", LandingPage: "/pricing/",
	}
	if got != want {
		t.Fatalf("the link was read wrong:\n got %+v\nwant %+v", got, want)
	}
}

// TestHiddenFieldsAreRead covers the posted form, whose own referrer is always
// the sign-up page and whose address carries nothing.
func TestHiddenFieldsAreRead(t *testing.T) {
	req := signupRequest(t, "https://app.example.test/register", "https://app.example.test/register", url.Values{
		"utm_source": {"reddit"},
		"referrer":   {"www.reddit.example"},
		"landing":    {"/"},
	})

	got := requestReferral(req)
	want := Referral{Referrer: "www.reddit.example", Source: "reddit", LandingPage: "/"}
	if got != want {
		t.Fatalf("the form was read wrong:\n got %+v\nwant %+v", got, want)
	}
}

// TestOnlyTheReferringDomainIsKept is the privacy claim: a referrer's path and
// query string can hold a search term or a token, and neither is ever kept.
func TestOnlyTheReferringDomainIsKept(t *testing.T) {
	cases := map[string]string{
		"https://News.Example.test/item?id=1&token=secret": "news.example.test",
		"news.example.test/item?id=1":                      "news.example.test",
		"news.example.test:8443":                           "news.example.test",
		"android-app://com.example.reader/":                "com.example.reader",
		"not a domain":                                     "",
		"<script>":                                         "",
		"":                                                 "",
	}

	for input, want := range cases {
		if got := cleanDomain(input); got != want {
			t.Errorf("cleanDomain(%q) = %q, want %q", input, got, want)
		}
	}

	req := pageRequest(t, "https://app.example.test/register", "https://news.example.test/item?id=1&token=secret")
	if got := requestReferral(req).Referrer; got != "news.example.test" {
		t.Fatalf("the browser's referrer was stored as %q", got)
	}
}

// TestTheLandingPageIsAPath keeps a query string, another site's address and
// anything that is not a path out of the landing page.
func TestTheLandingPageIsAPath(t *testing.T) {
	cases := map[string]string{
		"/pricing/":                 "/pricing/",
		"/pricing/?email=a@b.test":  "/pricing/",
		"/docs/#install":            "/docs/",
		"//evil.example.test/":      "",
		"https://evil.example.test": "",
		"pricing":                   "",
	}

	for input, want := range cases {
		if got := cleanLanding(input); got != want {
			t.Errorf("cleanLanding(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestOurOwnPagesAreNotAReferral covers both the deployment itself and the
// site it sits under, whether the browser or the link names them.
func TestOurOwnPagesAreNotAReferral(t *testing.T) {
	for _, referer := range []string{
		"https://app.example.test/login",
		"https://example.test/pricing/",
		"https://www.example.test/",
	} {
		req := pageRequest(t, "https://app.example.test/register", referer)
		req.Host = "app.example.test"

		if got := requestReferral(req); !got.Empty() {
			t.Errorf("%s was recorded as a referral: %+v", referer, got)
		}
	}

	req := pageRequest(t, "https://app.example.test/register?referrer=example.test", "")
	req.Host = "app.example.test"
	if got := requestReferral(req); got.Referrer != "" {
		t.Fatalf("a link naming our own site was recorded as a referral: %+v", got)
	}

	req = pageRequest(t, "https://app.example.test/register", "https://notexample.test/")
	req.Host = "app.example.test"
	if got := requestReferral(req); got.Referrer != "notexample.test" {
		t.Fatalf("a different site was mistaken for our own: %+v", got)
	}
}

// TestReferralFieldsAreBounded holds the limit on text that arrives on a public
// link and ends up in a row and a chat message.
func TestReferralFieldsAreBounded(t *testing.T) {
	long := strings.Repeat("a", 5000)

	req := pageRequest(t, "https://app.example.test/register?utm_source="+long+
		"&utm_campaign=line%0Abreak&landing=/"+long+"&referrer="+long, "")

	got := requestReferral(req)

	if len(got.Source) != maxReferralLabel {
		t.Errorf("the source is %d characters, want %d", len(got.Source), maxReferralLabel)
	}
	if len(got.LandingPage) != maxReferralLanding {
		t.Errorf("the landing page is %d characters, want %d", len(got.LandingPage), maxReferralLanding)
	}
	if got.Referrer != "" {
		t.Errorf("an oversized referrer was kept: %d characters", len(got.Referrer))
	}
	if strings.ContainsAny(got.Campaign, "\n\r") {
		t.Errorf("a line break survived in the campaign: %q", got.Campaign)
	}
}

// TestNoCookieIsSetForAVisitor is the promise this file keeps: a visitor who
// arrives on a tagged link is remembered by the link and nothing else.
func TestNoCookieIsSetForAVisitor(t *testing.T) {
	app := newTestApp(t)

	req := pageRequest(t, "/login?utm_source=hn", "https://news.example.test/item?id=1")
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, req)

	for _, cookie := range recorder.Result().Cookies() {
		if strings.Contains(cookie.Value, "hn") || strings.Contains(cookie.Name, "ref") {
			t.Fatalf("a visitor was given a cookie about where they came from: %s", cookie.Name)
		}
	}
}

// TestTheSignupFormCarriesTheLink checks the page hands the link's parameters
// to the form as hidden fields, which is the only place they are held.
func TestTheSignupFormCarriesTheLink(t *testing.T) {
	app := newTestApp(t)
	c := newClient(t, app)

	body := c.body("/register?utm_source=hn&utm_medium=social&utm_campaign=launch&referrer=news.example.test&landing=/pricing/")

	for _, field := range []string{
		`name="utm_source" value="hn"`,
		`name="utm_medium" value="social"`,
		`name="utm_campaign" value="launch"`,
		`name="referrer" value="news.example.test"`,
		`name="landing" value="/pricing/"`,
	} {
		if !strings.Contains(body, field) {
			t.Errorf("the form is missing %s", field)
		}
	}

	if plain := c.body("/register"); strings.Contains(plain, `name="utm_source"`) {
		t.Error("an untagged visit was given a source field")
	}
}

// TestASignupStoresItsSourceAndIsReported walks the whole path: a tagged link,
// the form, the account, the stored row and the one report that an account now
// exists.
func TestASignupStoresItsSourceAndIsReported(t *testing.T) {
	app := newTestApp(t)

	type report struct {
		method   string
		referral Referral
	}
	var reports []report
	app.SignupEvent = func(_ *http.Request, method string, referral Referral) {
		reports = append(reports, report{method, referral})
	}

	c := newClient(t, app)

	// A rejected form reports nothing and keeps the source for the next try.
	resp := c.post("/register", url.Values{
		"email": {"jane@example.test"}, "password": {"short"},
		"utm_source": {"hn"}, "referrer": {"news.example.test"}, "landing": {"/pricing/"},
	})
	rejected := readBody(t, resp)
	if !strings.Contains(rejected, `name="utm_source" value="hn"`) {
		t.Fatal("a rejected form lost where the visitor came from")
	}
	if len(reports) != 0 {
		t.Fatalf("a rejected form was reported as a sign-up: %+v", reports)
	}

	resp = c.post("/register", url.Values{
		"email": {"jane@example.test"}, "password": {"a long enough password"},
		"utm_source": {"hn"}, "utm_medium": {"social"}, "utm_campaign": {"launch"},
		"referrer": {"https://news.example.test/item?id=1"}, "landing": {"/pricing/?x=1"},
	})
	closeResponseBody(t, resp)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("registration answered %d", resp.StatusCode)
	}

	want := Referral{
		Referrer: "news.example.test", Source: "hn", Medium: "social",
		Campaign: "launch", LandingPage: "/pricing/",
	}

	user, err := app.store.UserByEmail(t.Context(), "jane@example.test")
	if err != nil {
		t.Fatal(err)
	}

	stored, found, err := app.store.Referral(t.Context(), user.ID)
	if err != nil || !found {
		t.Fatalf("the source was not stored: found=%v err=%v", found, err)
	}
	if stored != want {
		t.Fatalf("the stored source is wrong:\n got %+v\nwant %+v", stored, want)
	}

	if len(reports) != 1 || reports[0].method != SignupMethodPassword || reports[0].referral != want {
		t.Fatalf("the sign-up was not reported exactly once with its source: %+v", reports)
	}

	// A second attempt on the same address creates nothing and reports nothing.
	resp = newClient(t, app).post("/register", url.Values{
		"email": {"jane@example.test"}, "password": {"a long enough password"},
	})
	closeResponseBody(t, resp)
	if len(reports) != 1 {
		t.Fatalf("a refused registration was reported as a sign-up: %+v", reports)
	}
}

// TestTheSourceSurvivesAFederatedSignIn covers the redirect out to an identity
// provider and back, where the state value is the only thing returned.
func TestTheSourceSurvivesAFederatedSignIn(t *testing.T) {
	referral := Referral{Referrer: "news.example.test", Source: "hn", LandingPage: "/pricing/"}

	state := stateWithReferral("abc123", referral)
	if !strings.HasPrefix(state, "abc123.") {
		t.Fatalf("the state lost its random half: %q", state)
	}
	if got := referralFromState(state); got != referral {
		t.Fatalf("the source did not survive the state:\n got %+v\nwant %+v", got, referral)
	}

	if got := stateWithReferral("abc123", Referral{}); got != "abc123" {
		t.Fatalf("an empty source changed the state: %q", got)
	}
	if got := referralFromState("abc123"); !got.Empty() {
		t.Fatalf("a bare state produced a source: %+v", got)
	}
	if got := referralFromState("abc123.not-a-token!"); !got.Empty() {
		t.Fatalf("an unreadable state produced a source: %+v", got)
	}

	// A state edited in the browser is still cleaned on the way back in.
	forged, err := encodeReferral(Referral{Referrer: "https://news.example.test/item?id=1", LandingPage: "//evil.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := referralFromState("abc123." + forged); got.Referrer != "news.example.test" || got.LandingPage != "" {
		t.Fatalf("a forged state was not cleaned: %+v", got)
	}
}

// TestTheFederatedLinkCarriesTheSource checks the link written into the page
// keeps its own parameters apart from the ones it carries.
func TestTheFederatedLinkCarriesTheSource(t *testing.T) {
	suffix := string(Referral{Source: "a&b", LandingPage: "/pricing/"}.LinkSuffix())
	if suffix != "&landing=%2Fpricing%2F&utm_source=a%26b" {
		t.Fatalf("the link suffix is %q", suffix)
	}

	if got := (Referral{}).LinkSuffix(); got != "" {
		t.Fatalf("an empty source added %q to the link", got)
	}
}

// TestReferralIsStoredOnce covers the row itself: an account's origin is
// written when it is created and never revised.
func TestReferralIsStoredOnce(t *testing.T) {
	store, _ := newTestStore(t)

	user, _, err := store.CreateUser(t.Context(), "jane@example.test", "Jane Doe", "hash", "")
	if err != nil {
		t.Fatal(err)
	}

	first := Referral{
		Referrer: "news.example.test", Source: "hn", Medium: "social",
		Campaign: "launch", LandingPage: "/pricing/",
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

// TestReferralDiesWithTheAccount is a deletion claim, so it is checked rather
// than assumed.
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
