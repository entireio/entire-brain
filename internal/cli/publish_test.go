package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

const (
	testPublishRepoID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	testPublishToken  = "tok-abc-123"
	testPublishCommit = "1111111111111111111111111111111111111111"
	testPublishHead   = "2222222222222222222222222222222222222222"
)

// capturedPublish records the requests a fake publish endpoint receives.
type capturedPublish struct {
	count    int
	method   string
	path     string
	auth     string
	rawBodie [][]byte
}

// wirePublishArtifact mirrors the on-the-wire artifact so a test can decode the
// request the client sent (Data is []byte, so json base64-decodes it).
type wirePublishArtifact struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
	Data   []byte `json:"data"`
}

type wirePublishBody struct {
	Artifacts []wirePublishArtifact `json:"artifacts"`
}

func newPublishFixtureRunner() *fakeCommandRunner {
	return &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: "https://github.com/example/repo\n"},
		fakeCommandKey("git", "rev-parse", "HEAD"):           {stdout: testPublishHead + "\n"},
	}}
}

// writePublishBrainFixture lays down a minimal but complete on-disk brain: a
// manifest, one snapshot, one (base..head) overlay plus an all-branches report
// (which must be skipped), and one branch facts stream.
func writePublishBrainFixture(t *testing.T, brainDir string) {
	t.Helper()

	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC),
		RepoKey:       "gh/example/repo",
		DefaultBranch: "main",
		Sources: &brainSources{
			Semantic: &semanticSourceManifest{
				GeneratedAt:     time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC),
				Provider:        "entire-sem",
				ProviderVersion: "0.3.1",
				SchemaVersion:   "1.0",
			},
		},
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	writePublishFile(t, filepath.Join(brainDir, exportManifestFileName), append(manifestData, '\n'))

	writePublishFile(t,
		filepath.Join(brainDir, semanticDirName, semanticSnapshotsDir, testPublishCommit, semanticSnapshotName),
		[]byte("{\"symbol\":\"Foo\",\"kind\":\"func\"}\n"))

	overlayName := testPublishCommit + ".." + testPublishHead + ".json"
	writePublishFile(t,
		filepath.Join(brainDir, semanticDirName, "overlays", overlayName),
		[]byte("{\"base\":\""+testPublishCommit+"\",\"head\":\""+testPublishHead+"\",\"branch\":\"feature/x\"}\n"))
	// all-branches.json is a refresh report, not a (base..head) overlay: must be skipped.
	writePublishFile(t,
		filepath.Join(brainDir, semanticDirName, "overlays", "all-branches.json"),
		[]byte("{\"branches\":[]}\n"))

	factsRel := factsFileRelPath("main")
	writePublishFile(t,
		filepath.Join(brainDir, filepath.FromSlash(factsRel)),
		[]byte("{\"branch\":\"main\",\"text\":\"the repo uses go\"}\n"))
}

func writePublishFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newPublishCmd(t *testing.T, repoDir, dataDir string, runner *fakeCommandRunner) *cobra.Command {
	t.Helper()
	return NewRootCommand(Options{
		Version: "test-version",
		Env: EntireEnv{
			RepoRoot:        repoDir,
			PluginConfigDir: filepath.Join(t.TempDir(), "config"),
			PluginDataDir:   dataDir,
			PluginStateDir:  filepath.Join(t.TempDir(), "state"),
			PluginCacheDir:  filepath.Join(t.TempDir(), "cache"),
		},
		Runner: runner,
		Now:    func() time.Time { return time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC) },
	})
}

func publishBrainDir(dataDir string) string {
	return filepath.Join(dataDir, repoStoreDirName, "gh", "example", "repo")
}

func expectedDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestPublishRefusesWithoutOptIn is the load-bearing no-egress test: with the
// opt-in gate unset the command must refuse and make NO HTTP request, even though
// a reachable API URL, repo id, and token are all configured.
func TestPublishRefusesWithoutOptIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("publish endpoint must not be called when opt-in is off; got %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	repoDir := t.TempDir()
	dataDir := t.TempDir()
	writePublishBrainFixture(t, publishBrainDir(dataDir))

	// Opt-in and local-only master switch both explicitly off; full config set so
	// the ONLY thing keeping the brain local is the missing opt-in.
	t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "")
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	t.Setenv("ENTIRE_API_URL", server.URL)
	t.Setenv("ENTIRE_API_TOKEN", testPublishToken)
	t.Setenv("ENTIRE_REPO_ID", testPublishRepoID)

	cmd := newPublishCmd(t, repoDir, dataDir, newPublishFixtureRunner())
	out, err := execute(t, cmd, "publish")
	if err == nil {
		t.Fatalf("publish succeeded without opt-in; want refusal. output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "opt-in") || !strings.Contains(err.Error(), envBrainAllowHosted) {
		t.Fatalf("refusal error should explain the opt-in and name %s; got: %v", envBrainAllowHosted, err)
	}
}

// TestPublishRefusesUnderNoEgress verifies the master local-only switch wins over
// the hosted opt-in, and still makes no HTTP request.
func TestPublishRefusesUnderNoEgress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("publish endpoint must not be called under no-egress; got %s", r.URL.Path)
	}))
	defer server.Close()

	repoDir := t.TempDir()
	dataDir := t.TempDir()
	writePublishBrainFixture(t, publishBrainDir(dataDir))

	t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
	t.Setenv("ENTIRE_API_URL", server.URL)
	t.Setenv("ENTIRE_API_TOKEN", testPublishToken)
	t.Setenv("ENTIRE_REPO_ID", testPublishRepoID)

	cmd := newPublishCmd(t, repoDir, dataDir, newPublishFixtureRunner())
	_, err := execute(t, cmd, "publish")
	if err == nil {
		t.Fatal("publish egressed under no-egress mode")
	}
	if !strings.Contains(err.Error(), "no_egress") {
		t.Fatalf("want no_egress refusal; got: %v", err)
	}
}

// TestPublishSendsBundleWhenOptedIn verifies the request path, auth header, and
// JSON body (kinds, refs, sha256: digests, base64 data) against a capturing
// httptest server.
func TestPublishSendsBundleWhenOptedIn(t *testing.T) {
	var captured capturedPublish
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.count++
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.auth = r.Header.Get("Authorization")
		captured.rawBodie = append(captured.rawBodie, body)

		var decoded wirePublishBody
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Errorf("server could not decode request body: %v", err)
		}
		stored := make([]map[string]string, 0, len(decoded.Artifacts))
		for _, a := range decoded.Artifacts {
			stored = append(stored, map[string]string{"kind": a.Kind, "ref": a.Ref})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "published", "stored": stored})
	}))
	defer server.Close()

	repoDir := t.TempDir()
	dataDir := t.TempDir()
	writePublishBrainFixture(t, publishBrainDir(dataDir))

	t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	t.Setenv("ENTIRE_API_URL", server.URL)
	t.Setenv("ENTIRE_API_TOKEN", testPublishToken)
	t.Setenv("ENTIRE_REPO_ID", testPublishRepoID)

	cmd := newPublishCmd(t, repoDir, dataDir, newPublishFixtureRunner())
	out, err := execute(t, cmd, "publish")
	if err != nil {
		t.Fatalf("publish failed: %v\n%s", err, out)
	}

	if captured.count != 1 {
		t.Fatalf("want exactly 1 request, got %d", captured.count)
	}
	if captured.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", captured.method)
	}
	wantPath := "/api/v1/repos/" + testPublishRepoID + "/brain/artifacts"
	if captured.path != wantPath {
		t.Fatalf("path = %q, want %q", captured.path, wantPath)
	}
	if captured.auth != "Bearer "+testPublishToken {
		t.Fatalf("Authorization = %q, want bearer token", captured.auth)
	}

	var decoded wirePublishBody
	if err := json.Unmarshal(captured.rawBodie[0], &decoded); err != nil {
		t.Fatalf("decode captured body: %v", err)
	}

	byKind := map[string][]wirePublishArtifact{}
	for _, a := range decoded.Artifacts {
		byKind[a.Kind] = append(byKind[a.Kind], a)
		// Every artifact carries a valid, matching sha256: digest over its bytes.
		if !strings.HasPrefix(a.Digest, "sha256:") {
			t.Fatalf("artifact %s/%s digest %q lacks sha256: prefix", a.Kind, a.Ref, a.Digest)
		}
		if got := expectedDigest(a.Data); got != a.Digest {
			t.Fatalf("artifact %s/%s digest = %q, want %q", a.Kind, a.Ref, a.Digest, got)
		}
	}

	// Manifest artifact: exactly one, addressed by HEAD, data is the wire
	// BrainArtifact declaring the repo_key, schema version, and provider.
	manifests := byKind[brainKindManifest]
	if len(manifests) != 1 {
		t.Fatalf("want 1 manifest artifact, got %d", len(manifests))
	}
	if manifests[0].Ref != testPublishHead {
		t.Fatalf("manifest ref = %q, want HEAD %q", manifests[0].Ref, testPublishHead)
	}
	var wire struct {
		Manifest struct {
			RepoKey            string `json:"repo_key"`
			BrainSchemaVersion string `json:"brain_schema_version"`
			Provider           string `json:"provider"`
			DefaultBranch      string `json:"default_branch"`
		} `json:"manifest"`
	}
	if err := json.Unmarshal(manifests[0].Data, &wire); err != nil {
		t.Fatalf("decode manifest artifact data: %v", err)
	}
	if wire.Manifest.RepoKey != "gh/example/repo" {
		t.Fatalf("manifest repo_key = %q, want gh/example/repo", wire.Manifest.RepoKey)
	}
	if wire.Manifest.BrainSchemaVersion != "1.0" {
		t.Fatalf("manifest brain_schema_version = %q, want 1.0", wire.Manifest.BrainSchemaVersion)
	}
	if wire.Manifest.Provider != "entire-sem" {
		t.Fatalf("manifest provider = %q, want entire-sem", wire.Manifest.Provider)
	}

	// Snapshot: keyed by commit, bytes preserved.
	if snaps := byKind[brainKindSnapshot]; len(snaps) != 1 || snaps[0].Ref != testPublishCommit {
		t.Fatalf("snapshot artifacts = %+v, want one ref %q", snaps, testPublishCommit)
	} else if !strings.Contains(string(snaps[0].Data), "\"symbol\":\"Foo\"") {
		t.Fatalf("snapshot data not preserved: %q", snaps[0].Data)
	}

	// Overlay: keyed by base..head; the all-branches report is not published.
	overlays := byKind[brainKindOverlay]
	wantOverlayRef := testPublishCommit + ".." + testPublishHead
	if len(overlays) != 1 || overlays[0].Ref != wantOverlayRef {
		t.Fatalf("overlay artifacts = %+v, want one ref %q (all-branches must be skipped)", overlays, wantOverlayRef)
	}

	// Facts: keyed by branch.
	if facts := byKind[brainKindFacts]; len(facts) != 1 || facts[0].Ref != "main" {
		t.Fatalf("facts artifacts = %+v, want one ref \"main\"", facts)
	}

	if !strings.Contains(out, "published") || !strings.Contains(out, "gh/example/repo") {
		t.Fatalf("success output missing summary: %q", out)
	}
}

// TestPublishIdempotent verifies that re-running publish over an unchanged brain
// produces a byte-identical request and no error.
func TestPublishIdempotent(t *testing.T) {
	var captured capturedPublish
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.count++
		captured.rawBodie = append(captured.rawBodie, body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "published", "stored": []any{}})
	}))
	defer server.Close()

	repoDir := t.TempDir()
	dataDir := t.TempDir()
	writePublishBrainFixture(t, publishBrainDir(dataDir))

	t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_API_URL", server.URL)
	t.Setenv("ENTIRE_API_TOKEN", testPublishToken)
	t.Setenv("ENTIRE_REPO_ID", testPublishRepoID)

	for i := 0; i < 2; i++ {
		cmd := newPublishCmd(t, repoDir, dataDir, newPublishFixtureRunner())
		if _, err := execute(t, cmd, "publish"); err != nil {
			t.Fatalf("publish run %d failed: %v", i+1, err)
		}
	}

	if captured.count != 2 {
		t.Fatalf("want 2 requests, got %d", captured.count)
	}
	if string(captured.rawBodie[0]) != string(captured.rawBodie[1]) {
		t.Fatalf("re-publish request differs; publish is not idempotent")
	}
}

// TestPublishConfigResolutionFailure verifies a missing token fails clearly with
// no HTTP request and no panic, even with the opt-in enabled.
func TestPublishConfigResolutionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request expected on config failure; got %s", r.URL.Path)
	}))
	defer server.Close()

	repoDir := t.TempDir()
	dataDir := t.TempDir()
	writePublishBrainFixture(t, publishBrainDir(dataDir))

	t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_API_URL", server.URL)
	t.Setenv("ENTIRE_API_TOKEN", "")
	t.Setenv("ENTIRE_REPO_ID", testPublishRepoID)

	cmd := newPublishCmd(t, repoDir, dataDir, newPublishFixtureRunner())
	_, err := execute(t, cmd, "publish")
	if err == nil {
		t.Fatal("publish succeeded with no token; want a clear resolution error")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Fatalf("error should name the missing token; got: %v", err)
	}
}

// TestPublishServerErrorsSurfaced verifies each notable status maps to a clear,
// distinct error.
func TestPublishServerErrorsSurfaced(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantSubstr string
	}{
		{"unauthorized", http.StatusUnauthorized, "publish_auth"},
		{"forbidden", http.StatusForbidden, "publish_auth"},
		{"validation", http.StatusUnprocessableEntity, "publish_rejected"},
		{"not-configured", http.StatusServiceUnavailable, "publish_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"title": "boom", "detail": "server said no"})
			}))
			defer server.Close()

			repoDir := t.TempDir()
			dataDir := t.TempDir()
			writePublishBrainFixture(t, publishBrainDir(dataDir))

			t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
			t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
			t.Setenv("ENTIRE_API_URL", server.URL)
			t.Setenv("ENTIRE_API_TOKEN", testPublishToken)
			t.Setenv("ENTIRE_REPO_ID", testPublishRepoID)

			cmd := newPublishCmd(t, repoDir, dataDir, newPublishFixtureRunner())
			_, err := execute(t, cmd, "publish")
			if err == nil {
				t.Fatalf("want error for status %d", tc.status)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("status %d error = %v, want substring %q", tc.status, err, tc.wantSubstr)
			}
		})
	}
}
