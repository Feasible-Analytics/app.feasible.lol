//
// handler_test.go
// The endpoint the graph reads its markers from.
//
// Created: 2026-08-31
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package annotations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/accounts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// newHandler builds a handler over a real routing snapshot, because the
// endpoint's whole job is turning a domain into a site and an account.
func newHandler(t *testing.T) (*Handler, string) {
	t.Helper()

	dir := t.TempDir()
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

	control, err := store.Open(filepath.Join(dir, "system.db"))
	if err != nil {
		t.Fatalf("open control: %v", err)
	}

	t.Cleanup(func() { control.Close() })

	if _, err := migrate.Run(context.Background(), control, migrate.System()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	team, err := control.Exec(`INSERT INTO teams (name, created_at, updated_at) VALUES ('Acme', ?, ?)`,
		now.Unix(), now.Unix())
	if err != nil {
		t.Fatalf("insert team: %v", err)
	}
	teamID, _ := team.LastInsertId()

	domain := "acme.example"

	if _, err := control.Exec(`INSERT INTO sites (account_id, domain, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		teamID, domain, now.Unix(), now.Unix()); err != nil {
		t.Fatalf("insert site: %v", err)
	}

	cache := sites.New(control)
	if err := cache.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	manager := accounts.NewManager(dir)
	t.Cleanup(func() {
		if err := manager.CloseAll(); err != nil {
			t.Errorf("close account manager: %v", err)
		}
	})

	handler := New(NewStore(manager), cache, nil)
	handler.Identity = func(*http.Request) (int64, string, bool) { return 42, "Sam", true }

	return handler, domain
}

// request builds one request with the path value the mux would have set.
func request(method, domain, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.SetPathValue("domain", domain)

	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}

	return r
}

// TestTheEndpointCreatesListsAndDeletes drives the whole surface.
func TestTheEndpointCreatesListsAndDeletes(t *testing.T) {
	handler, domain := newHandler(t)

	create := httptest.NewRecorder()
	handler.ServeHTTP(create, request(http.MethodPost, domain,
		"/api/sites/"+domain+"/annotations",
		`{"shown_on":"2026-08-14","body":"Launched the new pricing page"}`))

	if create.Code != http.StatusCreated {
		t.Fatalf("create answered %d: %s", create.Code, create.Body.String())
	}

	var created Annotation
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if created.ID == 0 {
		t.Fatal("the created annotation has no id")
	}
	if created.AuthorUserID != 42 || created.AuthorName != "Sam" {
		t.Fatalf("annotation author = %d/%q, want authenticated user 42/Sam", created.AuthorUserID, created.AuthorName)
	}

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, request(http.MethodGet, domain,
		"/api/sites/"+domain+"/annotations?from=2026-08-01&to=2026-08-31", ""))

	if list.Code != http.StatusOK {
		t.Fatalf("list answered %d", list.Code)
	}

	var body struct {
		Annotations []Annotation `json:"annotations"`
	}

	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v", err)
	}

	if len(body.Annotations) != 1 || body.Annotations[0].Body != "Launched the new pricing page" {
		t.Fatalf("the list is %+v", body.Annotations)
	}

	remove := httptest.NewRecorder()
	deletion := request(http.MethodDelete, domain, "/api/sites/"+domain+"/annotations/1", "")
	deletion.SetPathValue("id", "1")

	handler.ServeHTTP(remove, deletion)

	if remove.Code != http.StatusNoContent {
		t.Fatalf("delete answered %d: %s", remove.Code, remove.Body.String())
	}
}

// TestAnEmptyListIsAnEmptyArrayNotNull checks the shape the front end reads. A
// null would be a runtime error in a map over the result.
func TestAnEmptyListIsAnEmptyArrayNotNull(t *testing.T) {
	handler, domain := newHandler(t)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request(http.MethodGet, domain, "/api/sites/"+domain+"/annotations", ""))

	if !strings.Contains(recorder.Body.String(), `"annotations":[]`) {
		t.Fatalf("an empty list answered %s", recorder.Body.String())
	}
}

// TestAMisspeltFieldIsRefusedRatherThanIgnored checks that a note stored with a
// blank date because somebody typed `date` instead of `shown_on` is a 400
// naming the key.
func TestAMisspeltFieldIsRefusedRatherThanIgnored(t *testing.T) {
	handler, domain := newHandler(t)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request(http.MethodPost, domain,
		"/api/sites/"+domain+"/annotations", `{"date":"2026-08-14","body":"x"}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unknown field answered %d", recorder.Code)
	}

	if !strings.Contains(recorder.Body.String(), "date") {
		t.Fatalf("the error does not name the field: %s", recorder.Body.String())
	}
}

// TestCreateRefusesClientSuppliedAuthorIdentity proves the JSON body cannot
// impersonate another user. The only accepted identity is the principal the
// authenticated serving layer places on the request.
func TestCreateRefusesClientSuppliedAuthorIdentity(t *testing.T) {
	handler, domain := newHandler(t)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request(http.MethodPost, domain,
		"/api/sites/"+domain+"/annotations",
		`{"shown_on":"2026-08-14","body":"x","author_user_id":999,"author_name":"Imposter"}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("client-supplied author answered %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "author_user_id") {
		t.Fatalf("refusal does not name the forbidden field: %s", recorder.Body.String())
	}
}

// TestAnInvalidNoteAnswers400WithTheReason checks that a validation failure
// carries its own sentence rather than becoming a 500.
func TestAnInvalidNoteAnswers400WithTheReason(t *testing.T) {
	handler, domain := newHandler(t)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request(http.MethodPost, domain,
		"/api/sites/"+domain+"/annotations", `{"shown_on":"14/08/2026","body":"x"}`))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a bad date answered %d", recorder.Code)
	}

	if !strings.Contains(recorder.Body.String(), "YYYY-MM-DD") {
		t.Fatalf("the error does not say what shape a date takes: %s", recorder.Body.String())
	}
}

// TestAnUnknownSiteAnswers404 checks the routing lookup.
func TestAnUnknownSiteAnswers404(t *testing.T) {
	handler, _ := newHandler(t)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request(http.MethodGet, "nobody.example",
		"/api/sites/nobody.example/annotations", ""))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("an unknown site answered %d", recorder.Code)
	}
}
