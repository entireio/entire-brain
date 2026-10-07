package factsync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func authorsFixtureServer(t *testing.T, payload map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
}

func TestCurrentWithAuthorsDecodesAdditiveAuthorsMap(t *testing.T) {
	blob := []byte("{}\n")
	srv := authorsFixtureServer(t, map[string]any{
		"found": true, "ref": "facts-x", "version": 1,
		"data":    base64.StdEncoding.EncodeToString(blob),
		"authors": map[string]string{"fact:abc": "evisdren"},
	})
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL}
	ref, plaintext, authors, found, err := h.CurrentWithAuthors(context.Background(), "repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !found || ref != "facts-x" || string(plaintext) != "{}\n" {
		t.Fatalf("head mismatch: found=%v ref=%q data=%q", found, ref, plaintext)
	}
	if authors["fact:abc"] != "evisdren" {
		t.Fatalf("authors map must decode, got %v", authors)
	}
}

func TestCurrentWithAuthorsToleratesMissingField(t *testing.T) {
	srv := authorsFixtureServer(t, map[string]any{
		"found": true, "ref": "facts-x", "version": 1,
		"data": base64.StdEncoding.EncodeToString([]byte("{}\n")),
	})
	defer srv.Close()

	h := &HTTPServer{BaseURL: srv.URL}
	_, _, authors, found, err := h.CurrentWithAuthors(context.Background(), "repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("head must be found")
	}
	if authors != nil {
		t.Fatalf("a server without the authors field must yield a nil map, got %v", authors)
	}
}

func TestCurrentDelegatesToCurrentWithAuthors(t *testing.T) {
	srv := authorsFixtureServer(t, map[string]any{
		"found": true, "ref": "facts-y", "version": 1,
		"data":    base64.StdEncoding.EncodeToString([]byte("{}\n")),
		"authors": map[string]string{"fact:abc": "evisdren"},
	})
	defer srv.Close()

	// Current must keep satisfying the Server seam and ignore the additive field.
	var seam Server = &HTTPServer{BaseURL: srv.URL}
	ref, plaintext, found, err := seam.Current(context.Background(), "repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !found || ref != "facts-y" || string(plaintext) != "{}\n" {
		t.Fatalf("head mismatch: found=%v ref=%q data=%q", found, ref, plaintext)
	}
}
