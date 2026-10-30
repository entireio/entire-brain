package factsync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpsertFactsWire(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotContentType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"added":2,"changed":1,"unchanged":3}`))
	}))
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}
	counts, err := h.UpsertFacts(context.Background(), "r1", []byte(`{"id":"f1"}`+"\n"))
	if err != nil {
		t.Fatalf("UpsertFacts: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/repos/r1/brain/facts" {
		t.Fatalf("request target = %s %s, want POST /api/v1/repos/r1/brain/facts", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotContentType != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q", gotContentType)
	}
	if gotBody != `{"id":"f1"}`+"\n" {
		t.Fatalf("body = %q", gotBody)
	}
	if counts != (UpsertCounts{Added: 2, Changed: 1, Unchanged: 3}) {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestUpsertFactsRejectsBadRepoID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request must not be sent for an invalid repo id")
	}))
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL}
	if _, err := h.UpsertFacts(context.Background(), "a/b", []byte("x")); err == nil {
		t.Fatal("want error for repo id with a path separator")
	}
}

func TestUpsertFactsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"push denied for secret-tok"}`))
	}))
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL, Token: "secret-tok"}
	_, err := h.UpsertFacts(context.Background(), "r1", []byte("x"))
	if err == nil {
		t.Fatal("want error on 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("error should carry the status: %v", err)
	}
	if strings.Contains(err.Error(), "secret-tok") {
		t.Fatalf("error must not echo the bearer token: %v", err)
	}
}

// 403 is the stale-binding shape (repo deleted or grant lost — COR-2279):
// both wire calls must surface the ErrNoAccess sentinel so the sync stops
// retrying. 401 stays a plain error — auth-plumbing breaks must keep looking
// like failures that reach the server-side monitor.
func TestForbiddenIsErrNoAccess(t *testing.T) {
	status := http.StatusForbidden
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"no repo grant"}`))
	}))
	defer srv.Close()
	h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}

	if _, err := h.UpsertFacts(context.Background(), "r1", []byte("x")); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("UpsertFacts(403) = %v, want ErrNoAccess", err)
	}
	if _, err := h.FactsFeedPage(context.Background(), "r1", "", 50); !errors.Is(err, ErrNoAccess) {
		t.Fatalf("FactsFeedPage(403) = %v, want ErrNoAccess", err)
	}

	status = http.StatusUnauthorized
	if _, err := h.UpsertFacts(context.Background(), "r1", []byte("x")); err == nil || errors.Is(err, ErrNoAccess) {
		t.Fatalf("UpsertFacts(401) = %v, want a plain error", err)
	}
	if _, err := h.FactsFeedPage(context.Background(), "r1", "", 50); err == nil || errors.Is(err, ErrNoAccess) {
		t.Fatalf("FactsFeedPage(401) = %v, want a plain error", err)
	}
}

func TestFactsFeedPageWire(t *testing.T) {
	var gotPath, gotToken, gotPageSize string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.URL.Query().Get("pageToken")
		gotPageSize = r.URL.Query().Get("pageSize")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{
				"record":          map[string]any{"id": "f1", "text": "t", "paths": []string{"architecture.core"}, "status": "active"},
				"trailId":         "trail-9",
				"createdBy":       "acct-1",
				"createdByHandle": "alice",
				"updatedAt":       "2026-10-17T00:00:00.000Z",
			}},
			"nextPageToken": "tok-2",
		})
	}))
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL, Token: "tok"}
	page, err := h.FactsFeedPage(context.Background(), "r1", "abc", 100)
	if err != nil {
		t.Fatalf("FactsFeedPage: %v", err)
	}
	if gotPath != "/api/v1/repos/r1/brain/facts" || gotToken != "abc" || gotPageSize != "100" {
		t.Fatalf("request = %s pageToken=%q pageSize=%q", gotPath, gotToken, gotPageSize)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(page.Items))
	}
	item := page.Items[0]
	if item.Record.ID != "f1" || item.Record.Text != "t" {
		t.Fatalf("record = %+v", item.Record)
	}
	if item.TrailID != "trail-9" || item.CreatedBy != "acct-1" || item.CreatedByHandle != "alice" {
		t.Fatalf("envelope = %+v", item)
	}
	if page.NextPageToken == nil || *page.NextPageToken != "tok-2" {
		t.Fatalf("nextPageToken = %v", page.NextPageToken)
	}
}

func TestFactsFeedPageInvalidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid pageToken"}`))
	}))
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL}
	_, err := h.FactsFeedPage(context.Background(), "r1", "stale", 50)
	if !errors.Is(err, ErrInvalidPageToken) {
		t.Fatalf("want ErrInvalidPageToken with a non-empty token, got %v", err)
	}
	_, err = h.FactsFeedPage(context.Background(), "r1", "", 50)
	if err == nil || errors.Is(err, ErrInvalidPageToken) {
		t.Fatalf("a 400 with no token sent is a plain error, got %v", err)
	}
}
