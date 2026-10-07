package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
)

// hostedFake is an in-memory fact-set head speaking the brainstore wire:
// GET …/brain/facts (with the additive authors map), POST …/facts/advance
// (CAS: stale oldRef → 412), proposal routes → 404 (v1 cuts them).
type hostedFake struct {
	mu       sync.Mutex
	ref      string
	data     []byte
	authors  map[string]string
	requests int
	seq      int
}

func (f *hostedFake) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *hostedFake) head() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.data...)
}

func newHostedFakeServer(t *testing.T, fake *hostedFake) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.requests++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/brain/facts/proposals"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/brain/facts"):
			resp := map[string]any{"found": fake.ref != "", "ref": fake.ref, "version": 1, "data": fake.data}
			if fake.authors != nil {
				resp["authors"] = fake.authors
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/brain/facts/advance"):
			var body struct {
				Branch string `json:"branch"`
				OldRef string `json:"oldRef"`
				Data   []byte `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Data) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if body.OldRef != fake.ref {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			fake.seq++
			fake.ref = fmt.Sprintf("facts-%d", fake.seq)
			fake.data = body.Data
			_ = json.NewEncoder(w).Encode(map[string]any{"newRef": fake.ref, "version": fake.seq, "changed": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hostedSyncTestFact(text string, now time.Time) factRecord {
	paths := normalizeFactPaths([]string{"architecture.sync"})
	return factRecord{
		ID:         factRecordID(text, paths),
		Paths:      paths,
		Text:       text,
		Branch:     "main",
		Origin:     factOriginAuthored,
		Status:     factStatusActive,
		Provenance: []factmerge.Anchor{{SessionID: "s"}},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func encodeFactsNDJSON(t *testing.T, records []factRecord) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := factmerge.WriteNDJSON(&buf, records); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// hostedSyncHarness wires a temp brain dir, a binding to the fake server, and
// a runner that mints a fake token and fails git calls. The last return is the
// fake server's base URL (the bound target).
func hostedSyncHarness(t *testing.T, fake *hostedFake, bind bool) (Options, repoStorage, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	brainDir := t.TempDir()
	storage := repoStorage{Key: "test/repo", BrainDir: brainDir, HeadPath: filepath.Join(t.TempDir(), "HEAD")}
	srv := newHostedFakeServer(t, fake)
	if bind {
		if err := writeHostedRepoBinding(brainDir, hostedRepoBinding{RepoID: "repo1", BaseURL: srv.URL, Jurisdiction: "us"}); err != nil {
			t.Fatal(err)
		}
	}
	runner := commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		if name == "entire" && len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return []byte(fakeJWT + "\n"), nil, nil
		}
		return nil, nil, errors.New("not available")
	})
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: EntireEnv{RepoRoot: repoDir}, Runner: runner, Now: func() time.Time { return now }}
	return opts, storage, repoDir, srv.URL
}

func TestHostedSyncGates(t *testing.T) {
	t.Run("NO_EGRESS wins over a present binding", func(t *testing.T) {
		fake := &hostedFake{}
		opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
		t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")
		var errOut bytes.Buffer
		if err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
			t.Fatal(err)
		}
		if fake.requestCount() != 0 {
			t.Fatalf("NO_EGRESS must make zero requests, got %d", fake.requestCount())
		}
	})
	t.Run("no binding means silent no-op", func(t *testing.T) {
		fake := &hostedFake{}
		opts, storage, repoDir, _ := hostedSyncHarness(t, fake, false)
		var errOut bytes.Buffer
		if err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
			t.Fatal(err)
		}
		if fake.requestCount() != 0 {
			t.Fatalf("unconnected repo must make zero requests, got %d", fake.requestCount())
		}
		if errOut.Len() != 0 {
			t.Fatalf("unconnected repo must be silent, got %q", errOut.String())
		}
	})
}

func TestHostedSyncTokenMintFailureIsNonFatal(t *testing.T) {
	fake := &hostedFake{}
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	opts.Runner = commandRunnerFunc(func(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
		return nil, nil, errors.New("not logged in")
	})
	var errOut bytes.Buffer
	if err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatalf("mint failure must be non-fatal, got %v", err)
	}
	if fake.requestCount() != 0 {
		t.Fatalf("mint failure must make zero requests, got %d", fake.requestCount())
	}
	if lines := strings.Count(strings.TrimRight(errOut.String(), "\n"), "\n") + 1; errOut.Len() == 0 || lines != 1 {
		t.Fatalf("expected exactly one stderr line, got %q", errOut.String())
	}
}

func TestHostedSyncPublishesAndPulls(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	localFact := hostedSyncTestFact("local fact A", now)
	remoteFact := hostedSyncTestFact("remote fact B", now)
	fake := &hostedFake{
		ref:     "facts-remote",
		data:    nil,
		authors: map[string]string{remoteFact.ID: "teammate"},
		seq:     0,
	}
	opts, storage, repoDir, baseURL := hostedSyncHarness(t, fake, true)
	fake.mu.Lock()
	fake.data = encodeFactsNDJSON(t, []factRecord{remoteFact})
	fake.mu.Unlock()
	if err := writeFacts(storage.BrainDir, "main", []factRecord{localFact}); err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}

	remote, err := factmerge.ParseNDJSON(bytes.NewReader(fake.head()))
	if err != nil {
		t.Fatal(err)
	}
	remoteIDs := map[string]bool{}
	for _, rec := range remote {
		remoteIDs[rec.ID] = true
	}
	if !remoteIDs[localFact.ID] || !remoteIDs[remoteFact.ID] {
		t.Fatalf("remote head must hold A∪B, got %v", remoteIDs)
	}

	local, err := loadFacts(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	var pulled *factRecord
	for i := range local {
		if local[i].ID == remoteFact.ID {
			pulled = &local[i]
		}
	}
	if pulled == nil {
		t.Fatalf("remote fact must be pulled into the local store, got %d records", len(local))
	}
	if pulled.Author != "teammate" {
		t.Fatalf("pulled fact must carry the server-stamped author, got %q", pulled.Author)
	}
	if err := checkHostedFactsBinding(storage.BrainDir, "main", "repo1", baseURL); err != nil {
		t.Fatalf("per-branch hosted binding must be recorded: %v", err)
	}
	if !strings.Contains(errOut.String(), "hosted sync: branch main") {
		t.Fatalf("expected a summary line, got %q", errOut.String())
	}
}

func TestPullPreservesLocalStatus(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	retracted := hostedSyncTestFact("contested fact", now)
	retracted.Status = factStatusRetracted
	remoteActive := hostedSyncTestFact("contested fact", now)

	fake := &hostedFake{ref: "facts-remote"}
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	fake.mu.Lock()
	fake.data = encodeFactsNDJSON(t, []factRecord{remoteActive})
	fake.mu.Unlock()
	if err := writeFacts(storage.BrainDir, "main", []factRecord{retracted}); err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatal(err)
	}
	local, err := loadFacts(storage.BrainDir, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range local {
		if rec.ID == retracted.ID && rec.Status != factStatusRetracted {
			t.Fatalf("local retraction must survive the pull, got status %q", rec.Status)
		}
	}
}

func TestPullMergeFailsWithoutWrite(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	localFact := hostedSyncTestFact("local fact A", now)
	forged := hostedSyncTestFact("remote fact B", now)
	forged.ID = "fact:forged-id-that-does-not-match-content"

	fake := &hostedFake{ref: "facts-remote"}
	opts, storage, repoDir, _ := hostedSyncHarness(t, fake, true)
	fake.mu.Lock()
	fake.data = encodeFactsNDJSON(t, []factRecord{forged})
	fake.mu.Unlock()
	if err := writeFacts(storage.BrainDir, "main", []factRecord{localFact}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(storage.BrainDir, filepath.FromSlash(factsFileRelPath("main"))))
	if err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	if err := hostedFactsSyncAndPull(context.Background(), &errOut, opts, repoDir, storage, "main"); err != nil {
		t.Fatalf("sync failure must be non-fatal, got %v", err)
	}
	after, err := os.ReadFile(filepath.Join(storage.BrainDir, filepath.FromSlash(factsFileRelPath("main"))))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a failed sync must leave the local store byte-identical")
	}
	if errOut.Len() == 0 {
		t.Fatal("a failed sync must leave a stderr note")
	}
}
