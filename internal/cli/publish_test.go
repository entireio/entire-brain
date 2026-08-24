package cli

import (
	"context"
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
				Provider:        "entire-graph",
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
	// Candidate extraction results are local performance state, never a hosted
	// support/publish artifact. Keep a canary here so the bundle contract stays
	// explicit if facts-root collection changes in the future.
	writePublishFile(t,
		filepath.Join(brainDir, filepath.FromSlash(distillCandidateResultCacheV2Path)),
		[]byte("{\"type\":\"header\",\"version\":2}\n{\"private\":\"CANDIDATE_CACHE_CANARY\"}\n"))
	// Application receipts are disposable local state under facts/distill-v2,
	// never a hosted fact artifact.
	writePublishFile(t,
		filepath.Join(brainDir, filepath.FromSlash(distillApplicationReceiptsV2Path)),
		[]byte("APPLICATION_RECEIPT_CANARY\n"))
	// Local-only relationship observations are disposable privacy state, never
	// a hosted fact artifact. Keep a canary so facts-root collection cannot
	// accidentally bundle the adjacent distill-v2 store.
	writePublishFile(t,
		filepath.Join(brainDir, filepath.FromSlash(distillRelationshipStoreV2Path)),
		[]byte("RELATIONSHIP_STORE_CANARY\n"))
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

// TestPublishRefusesDirtyPrivacyState proves the hosted path applies the same
// fail-closed derived-state gate as local pattern reads, before it constructs
// or sends a bundle.
func TestPublishRefusesDirtyPrivacyState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("publish endpoint must not be called with dirty privacy state; got %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	repoDir := t.TempDir()
	dataDir := t.TempDir()
	brainDir := publishBrainDir(dataDir)
	writePublishBrainFixture(t, brainDir)
	// The publish fixture's disposable-state canaries are intentionally opaque
	// to the strict privacy verifier; remove them so this test isolates the
	// dirty derived-store decision.
	for _, rel := range []string{distillCandidateResultCacheV2Path, distillApplicationReceiptsV2Path, distillRelationshipStoreV2Path} {
		if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	staleStore := filepath.Join(brainDir, filepath.FromSlash(patternsTasksPath))
	writePublishFile(t, staleStore, []byte("{\"task\":\"old\"}\n"))
	stones := emptySessionTombstones()
	stones.Excluded["secret-sess"] = sessionTombstone{At: time.Now().UTC(), Reason: "test"}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	// Make the ordering unambiguous even on filesystems with coarse mtime
	// resolution: a stale derived store must predate the tombstone.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(staleStore, old, old); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	t.Setenv("ENTIRE_API_URL", server.URL)
	t.Setenv("ENTIRE_API_TOKEN", testPublishToken)
	t.Setenv("ENTIRE_REPO_ID", testPublishRepoID)

	cmd := newPublishCmd(t, repoDir, dataDir, newPublishFixtureRunner())
	_, err := execute(t, cmd, "publish")
	if err == nil || !strings.Contains(err.Error(), memoryErrPrivacyDirty) {
		t.Fatalf("dirty privacy publish error = %v, want %s", err, memoryErrPrivacyDirty)
	}
}

// TestPublishHoldsPrivacyAndWriteLocksThroughEgress proves neither a privacy
// tombstone cleanup nor an ordinary fact/brain writer can interleave while the
// hosted request is in flight. This makes the preflight check and first egress
// byte a single privacy and state-consistency linearization boundary.
func TestPublishHoldsPrivacyAndWriteLocksThroughEgress(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"published","stored":[]}`)
	}))
	defer server.Close()

	repoDir := t.TempDir()
	dataDir := t.TempDir()
	brainDir := publishBrainDir(dataDir)
	writePublishBrainFixture(t, brainDir)
	t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	t.Setenv("ENTIRE_API_URL", server.URL)
	t.Setenv("ENTIRE_API_TOKEN", testPublishToken)
	t.Setenv("ENTIRE_REPO_ID", testPublishRepoID)

	cmd := newPublishCmd(t, repoDir, dataDir, newPublishFixtureRunner())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"publish"})
	publishDone := make(chan error, 1)
	go func() { publishDone <- cmd.Execute() }()
	<-requestStarted

	tombstoneAttempted := make(chan struct{})
	tombstoneDone := make(chan error, 1)
	go func() {
		close(tombstoneAttempted)
		tombstoneDone <- withBrainPrivacySideEffectLock(brainDir, func() error {
			return withBrainWriteLock(brainDir, func() error {
				stones := emptySessionTombstones()
				stones.Excluded["secret-sess"] = sessionTombstone{At: time.Now().UTC(), Reason: "race"}
				return saveSessionTombstones(brainDir, stones)
			})
		})
	}()
	<-tombstoneAttempted

	writerAttempted := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		close(writerAttempted)
		writerDone <- withBrainWriteLock(brainDir, func() error {
			return os.WriteFile(filepath.Join(brainDir, "facts", "publish-race-canary"), []byte("writer"), 0o600)
		})
	}()
	<-writerAttempted

	assertBlocked := func(name string, done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			t.Fatalf("%s interleaved during hosted egress: %v", name, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	assertBlocked("tombstone cleanup", tombstoneDone)
	assertBlocked("Brain writer", writerDone)

	close(releaseRequest)
	if err := <-publishDone; err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	if err := <-tombstoneDone; err != nil {
		t.Fatalf("tombstone after publish failed: %v", err)
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("Brain writer after publish failed: %v", err)
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
	if strings.Contains(string(captured.rawBodie[0]), "APPLICATION_RECEIPT_CANARY") || strings.Contains(string(captured.rawBodie[0]), "CANDIDATE_CACHE_CANARY") || strings.Contains(string(captured.rawBodie[0]), "RELATIONSHIP_STORE_CANARY") {
		t.Fatal("publish bundle included disposable distill-v2 state")
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
	if wire.Manifest.Provider != "entire-graph" {
		t.Fatalf("manifest provider = %q, want entire-graph", wire.Manifest.Provider)
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
	if strings.Contains(string(captured.rawBodie[0]), "CANDIDATE_CACHE_CANARY") {
		t.Fatal("publish bundle included local candidate-result cache")
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
		{"too-large", http.StatusRequestEntityTooLarge, "publish_too_large"},
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

// TestCanonicalWireRepoKey verifies the wire repo_key is rendered in the exact
// "gh/owner/repo" form entire-api's brainSlugResolver emits — including the
// server's normalization (lowercase, .github -> github) — for github.com, and that
// any non-github.com remote (including GitHub Enterprise and look-alike "github.*"
// hosts the github.com-sourced server cannot resolve) fails fast client-side.
func TestCanonicalWireRepoKey(t *testing.T) {
	cases := []struct {
		name    string
		remote  string
		want    string
		wantErr string
	}{
		{name: "github.com", remote: "https://github.com/example/repo", want: "gh/example/repo"},
		{name: "github.com normalizes .github", remote: "https://github.com/Acme/.github.git", want: "gh/acme/github"},
		{name: "github.com scp", remote: "git@github.com:Example/Repo.git", want: "gh/example/repo"},
		{name: "github enterprise rejected", remote: "git@github.acme-corp.com:Team/Repo.git", wantErr: "github.com-hosted"},
		{name: "github lookalike rejected", remote: "https://github.evil.com/example/repo", wantErr: "github.com-hosted"},
		{name: "gitlab rejected", remote: "https://gitlab.com/example/repo", wantErr: "github.com-hosted"},
		{name: "self-hosted git rejected", remote: "git@git.example.com:team/repo.git", wantErr: "github.com-hosted"},
		{name: "no remote rejected", remote: "", wantErr: "github.com origin remote"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
				fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: tc.remote + "\n"},
			}}
			got, err := canonicalWireRepoKey(context.Background(), Options{Runner: runner}, ".")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q; got key=%q err=%v", tc.wantErr, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("wire repo_key = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPublishFailsFastForNonGitHubRemote verifies a non-GitHub origin remote is
// refused with a clear client error and NO HTTP request — the server's brain store
// is GitHub-sourced, so such a bundle would only 422.
func TestPublishFailsFastForNonGitHubRemote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("publish must not call the server for a non-GitHub remote; got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	repoDir := t.TempDir()
	dataDir := t.TempDir()
	// gitlab.com stores under the "gl" slug; lay the brain down there so the command
	// reaches wire-repo_key derivation rather than failing at "no brain found".
	brainDir := filepath.Join(dataDir, repoStoreDirName, "gl", "example", "repo")
	writePublishBrainFixture(t, brainDir)

	t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	t.Setenv("ENTIRE_API_URL", server.URL)
	t.Setenv("ENTIRE_API_TOKEN", testPublishToken)
	t.Setenv("ENTIRE_REPO_ID", testPublishRepoID)

	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "remote", "get-url", "origin"): {stdout: "https://gitlab.com/example/repo\n"},
		fakeCommandKey("git", "rev-parse", "HEAD"):           {stdout: testPublishHead + "\n"},
	}}
	cmd := newPublishCmd(t, repoDir, dataDir, runner)
	_, err := execute(t, cmd, "publish")
	if err == nil {
		t.Fatal("publish succeeded for a non-GitHub remote; want a fast client error")
	}
	if !strings.Contains(err.Error(), "github.com-hosted") {
		t.Fatalf("want a github.com-only client error; got: %v", err)
	}
}

// TestPublishFailsFastWhenBundleTooLarge verifies the total-size preflight refuses
// an over-cap bundle before any HTTP send. The ceiling is shrunk so the small
// fixture trips it without a hundreds-of-MiB brain.
func TestPublishFailsFastWhenBundleTooLarge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("publish must not send an over-limit bundle; got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()

	repoDir := t.TempDir()
	dataDir := t.TempDir()
	writePublishBrainFixture(t, publishBrainDir(dataDir))

	orig := maxPublishBodyBytes
	maxPublishBodyBytes = 16
	defer func() { maxPublishBodyBytes = orig }()

	t.Setenv("ENTIRE_BRAIN_ALLOW_HOSTED", "1")
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	t.Setenv("ENTIRE_API_URL", server.URL)
	t.Setenv("ENTIRE_API_TOKEN", testPublishToken)
	t.Setenv("ENTIRE_REPO_ID", testPublishRepoID)

	cmd := newPublishCmd(t, repoDir, dataDir, newPublishFixtureRunner())
	_, err := execute(t, cmd, "publish")
	if err == nil {
		t.Fatal("publish sent an over-limit bundle; want a preflight error")
	}
	if !strings.Contains(err.Error(), "publish_too_large") {
		t.Fatalf("want a publish_too_large preflight error; got: %v", err)
	}
}

// TestProjectedPublishBodyBytesUpperBounds verifies the projection never
// undershoots the real marshaled body — the invariant that makes the preflight a
// sound backstop against the server body cap.
func TestProjectedPublishBodyBytesUpperBounds(t *testing.T) {
	body := publishRequestBody{Artifacts: []publishArtifact{
		{Kind: brainKindManifest, Ref: testPublishHead, Digest: "sha256:deadbeef", Data: []byte(`{"manifest":{}}`)},
		{Kind: brainKindSnapshot, Ref: testPublishCommit, Digest: "sha256:cafe", Data: make([]byte, 5000)},
		{Kind: brainKindFacts, Ref: "main", Data: []byte(`{"branch":"main"}`)},
	}}
	projected := projectedPublishBodyBytes(body)
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	if projected < int64(len(payload)) {
		t.Fatalf("projection %d under-estimates real marshaled body %d", projected, len(payload))
	}
}
