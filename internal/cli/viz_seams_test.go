package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/entireio/entire-brain/internal/cli/command/viz"
)

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
