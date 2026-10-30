package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/command/viz"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
)

func TestVizReloadsPrivacyStateForEveryRequest(t *testing.T) {
	const canary = "PRIVATE-LIVE-VIZ-CANARY"
	brainDir := t.TempDir()
	manifest := &exportManifest{Sources: &brainSources{Sessions: &sessionSourceManifest{Sessions: []exportSession{
		{SessionID: "private", TranscriptPath: "sessions/private.jsonl", Summary: &checkpointSummary{Intent: canary}},
		{SessionID: "public", TranscriptPath: "sessions/public.jsonl", Summary: &checkpointSummary{Intent: "safe"}},
	}}}}
	srv := &viz.Server{BrainDir: brainDir, Branch: "main", Manifest: manifest}
	request := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.HandleSessions(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
		return rec
	}

	before := request()
	if before.Code != http.StatusOK || !strings.Contains(before.Body.String(), canary) {
		t.Fatalf("pre-exclusion response = %d %s", before.Code, before.Body.String())
	}
	stones := brainstore.EmptySessionTombstones()
	stones.Excluded["private"] = sessionTombstone{At: time.Now().UTC()}
	if err := brainstore.SaveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	after := request()
	if after.Code != http.StatusOK || strings.Contains(after.Body.String(), canary) || strings.Contains(after.Body.String(), "private") {
		t.Fatalf("same server exposed a newly excluded session: %d %s", after.Code, after.Body.String())
	}
	if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(brainstore.SessionTombstonesPath))); err != nil {
		t.Fatal(err)
	}
	disappeared := request()
	if disappeared.Code != http.StatusOK || strings.Contains(disappeared.Body.String(), canary) || strings.Contains(disappeared.Body.String(), "private") {
		t.Fatalf("disappeared policy resurrected an excluded session: %d %s", disappeared.Code, disappeared.Body.String())
	}

	writeSessionTombstoneBytes(t, brainDir, []byte("{broken"))
	corrupt := request()
	if corrupt.Code != http.StatusServiceUnavailable || strings.Contains(corrupt.Body.String(), canary) || !strings.Contains(corrupt.Body.String(), brainstore.StateCorruptCode) {
		t.Fatalf("corrupt live policy did not fail closed: %d %s", corrupt.Code, corrupt.Body.String())
	}
}

// The CSP/no-egress headers must be present on REAL /api responses, not just when
// the middleware is exercised in isolation.
func TestVizSecurityHeaders_AppliedToMux(t *testing.T) {
	t.Parallel()
	srv := &viz.Server{BrainDir: t.TempDir(), Branch: "main", Deps: vizDeps()}
	m, err := srv.Mux()
	if err != nil {
		t.Fatalf("mux: %v", err)
	}
	h := viz.SecurityHeaders(m)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("CSP header not applied to /api/summary through the mux")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("nosniff header not applied through the mux")
	}
}

// Every feature endpoint must degrade to a valid 200 on an unbuilt brain, never 500.
func TestVizFeatureHandlers_UnbuiltBrain(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		path    string
		handler func(*viz.Server) http.HandlerFunc
	}{
		{"summary", "/api/summary", func(s *viz.Server) http.HandlerFunc { return s.HandleSummary }},
		{"search", "/api/search?q=foo", func(s *viz.Server) http.HandlerFunc { return s.HandleSearch }},
		{"facts", "/api/facts", func(s *viz.Server) http.HandlerFunc { return s.HandleFacts }},
		{"sessions", "/api/sessions", func(s *viz.Server) http.HandlerFunc { return s.HandleSessions }},
		{"history", "/api/history?limit=0", func(s *viz.Server) http.HandlerFunc { return s.HandleHistory }},
		{"docs", "/api/docs", func(s *viz.Server) http.HandlerFunc { return s.HandleDocs }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := &viz.Server{BrainDir: t.TempDir(), Branch: "main", Deps: vizDeps()}
			rec := httptest.NewRecorder()
			tc.handler(srv)(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", tc.name, rec.Code)
			}
			if !json.Valid(rec.Body.Bytes()) {
				t.Fatalf("%s: response is not valid JSON", tc.name)
			}
		})
	}
}
