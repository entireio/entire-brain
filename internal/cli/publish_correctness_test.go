package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/brainwire"
)

func TestPublishRefusesMalformedOrAmbiguousFactStreams(t *testing.T) {
	for _, data := range []string{"not-json\n", "not-json\n{\"branch\":\"main\"}\n", "{}\n", "null\n", "{\"branch\":\"main\"}\n{\"branch\":\"other\"}\n", "{\"branch\":\"main\"}\n{}\n"} {
		t.Run(data, func(t *testing.T) {
			brainDir := t.TempDir()
			writePublishBrainFixture(t, brainDir)
			writePublishFile(t, filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath("main"))), []byte(data))
			art := brainwire.NewBrainArtifact("gh/example/repo", "main", time.Now())
			got, err := collectFactArtifacts(art, brainDir)
			if err == nil || len(got) != 0 {
				t.Fatalf("bad stream accepted: %q; %d artifacts, %v", data, len(got), err)
			}
		})
	}
}

func TestPublishRequiresExactAcknowledgements(t *testing.T) {
	body := publishRequestBody{Artifacts: []publishArtifact{{Kind: "manifest", Ref: "head"}, {Kind: "facts", Ref: "main"}}}
	for _, tc := range []struct {
		name, stored string
		success      bool
	}{
		{"partial", `[{"kind":"manifest","ref":"head"}]`, false},
		{"unrelated", `[{"kind":"facts","ref":"other"}]`, false},
		{"duplicate", `[{"kind":"manifest","ref":"head"},{"kind":"manifest","ref":"head"},{"kind":"facts","ref":"main"}]`, false},
		{"extra", `[{"kind":"manifest","ref":"head"},{"kind":"facts","ref":"main"},{"kind":"facts","ref":"other"}]`, false},
		{"complete-reordered", `[{"kind":"facts","ref":"main"},{"kind":"manifest","ref":"head"}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"stored":` + tc.stored + `}`))
			}))
			defer server.Close()
			_, err := postBrainArtifacts(context.Background(), server.URL, "repo", "audit-token", body)
			if (err == nil) != tc.success {
				t.Fatalf("success=%v, error=%v", tc.success, err)
			}
		})
	}
}

func TestPublishAcknowledgementErrorRedactsToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "audit-token\x1b[31m", "stored": []any{}})
	}))
	defer server.Close()
	_, err := postBrainArtifacts(context.Background(), server.URL, "repo", "audit-token", publishRequestBody{Artifacts: []publishArtifact{{Kind: "facts", Ref: "main"}}})
	if err == nil {
		t.Fatal("missing acknowledgement succeeded")
	}
	if strings.Contains(err.Error(), "audit-token") || strings.ContainsRune(err.Error(), '\x1b') {
		t.Fatalf("unsafe error: %q", err)
	}
}

func TestPublishRefusesDuplicateBranchStreams(t *testing.T) {
	brainDir := t.TempDir()
	writePublishBrainFixture(t, brainDir)
	writePublishFile(t, filepath.Join(brainDir, factsDirName, "duplicate", factsFileName), []byte("{\"branch\":\"main\",\"text\":\"second stream\"}\n"))
	art := brainwire.NewBrainArtifact("gh/example/repo", "main", time.Now())
	got, err := collectFactArtifacts(art, brainDir)
	if err == nil || len(got) != 0 {
		t.Fatalf("ambiguous branch accepted: %d artifacts, %v", len(got), err)
	}
}

func TestPublishEmptyFactStreamRemainsOptional(t *testing.T) {
	brainDir := t.TempDir()
	writePublishBrainFixture(t, brainDir)
	writePublishFile(t, filepath.Join(brainDir, filepath.FromSlash(factsFileRelPath("main"))), []byte("\n \n"))
	art := brainwire.NewBrainArtifact("gh/example/repo", "main", time.Now())
	got, err := collectFactArtifacts(art, brainDir)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty stream: %d artifacts, %v", len(got), err)
	}
}
