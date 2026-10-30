package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/entireio/entire-brain/internal/cli/command/dash"
	"github.com/entireio/entire-brain/internal/cli/command/viz"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"github.com/spf13/cobra"
)

func writeSessionTombstoneBytes(t *testing.T, brainDir string, data []byte) {
	t.Helper()
	path := filepath.Join(brainDir, filepath.FromSlash(brainstore.SessionTombstonesPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireSessionTombstoneError(t *testing.T, err error, code string, state sessionTombstoneState) {
	t.Helper()
	var loadErr *sessionTombstoneLoadError
	if !errors.As(err, &loadErr) {
		t.Fatalf("error = %v, want *sessionTombstoneLoadError", err)
	}
	if loadErr.Code != code || loadErr.State != state || cmdutil.CommandErrorCode(err) != code {
		t.Fatalf("tombstone error = %+v (%v), want code=%s state=%s", loadErr, err, code, state)
	}
}

func TestSessionTombstonesInvalidStatesAreTyped(t *testing.T) {
	for _, test := range []struct {
		name    string
		data    string
		code    string
		state   sessionTombstoneState
		version int
	}{
		{name: "malformed", data: "{broken", code: brainstore.StateCorruptCode, state: brainstore.SessionTombstoneCorrupt},
		{name: "nil map", data: `{"version":1,"excluded":null}`, code: brainstore.StateCorruptCode, state: brainstore.SessionTombstoneCorrupt, version: 1},
		{name: "zero version", data: `{"version":0,"excluded":{}}`, code: brainstore.StateCorruptCode, state: brainstore.SessionTombstoneCorrupt},
		{name: "negative version", data: `{"version":-1,"excluded":{}}`, code: brainstore.StateCorruptCode, state: brainstore.SessionTombstoneCorrupt, version: -1},
		{name: "newer version", data: `{"version":2,"excluded":{}}`, code: brainstore.UnsupportedVersionCode, state: brainstore.SessionTombstoneUnsupported, version: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			brainDir := t.TempDir()
			writeSessionTombstoneBytes(t, brainDir, []byte(test.data))
			_, fileState, err := brainstore.LoadSessionTombstonesChecked(brainDir)
			requireSessionTombstoneError(t, err, test.code, test.state)
			if fileState.State != test.state || fileState.Version != test.version || fileState.Identity == "" {
				t.Fatalf("file state = %+v, want state=%s version=%d with identity", fileState, test.state, test.version)
			}
		})
	}
}

func TestCorruptSessionTombstonesFailClosedAcrossFreshRetrievalLoads(t *testing.T) {
	brainDir, _, _ := historyProjectionFixture(t)
	stones := brainstore.LoadSessionTombstones(brainDir)
	stones.Excluded["session-1"] = sessionTombstone{At: time.Now().UTC(), Reason: "private"}
	if err := brainstore.SaveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	// Simulate damage discovered after a process restart: no in-memory guard is
	// retained, so each request independently reloads the durable policy.
	writeSessionTombstoneBytes(t, brainDir, []byte("{broken"))
	for request := 1; request <= 2; request++ {
		results, err := retrieveConversation(brainDir, "projection publication", 5, retrieval.ModeLexical, retrievalOptions{})
		if len(results) != 0 {
			t.Fatalf("request %d served excluded results: %+v", request, results)
		}
		requireSessionTombstoneError(t, err, brainstore.StateCorruptCode, brainstore.SessionTombstoneCorrupt)
	}
}

func TestCorruptSessionTombstonesBlockConsolidationAndPreserveActiveGeneration(t *testing.T) {
	brainDir, _, now := historyProjectionFixture(t)
	previous, previousIndex := activeHistoryProjection(t, brainDir)
	writeSessionTombstoneBytes(t, brainDir, []byte("{broken"))

	prepared, err := prepareBrainHistoryProjection(brainDir, now.Add(time.Minute), nil)
	if prepared != nil {
		t.Fatalf("prepared corrupt-tombstone projection: %+v", prepared)
	}
	requireSessionTombstoneError(t, err, brainstore.StateCorruptCode, brainstore.SessionTombstoneCorrupt)
	current, currentIndex := activeHistoryProjection(t, brainDir)
	if current.IndexPath != previous.IndexPath || current.ProjectionStatePath != previous.ProjectionStatePath {
		t.Fatalf("failed consolidation switched generation: before=%+v after=%+v", previous, current)
	}
	if len(currentIndex.Records) != len(previousIndex.Records) {
		t.Fatalf("active records changed: before=%d after=%d", len(previousIndex.Records), len(currentIndex.Records))
	}
}

func TestCorruptSessionTombstonesBlockDeltaAndPreserveOverlay(t *testing.T) {
	brainDir, _, _ := shortTermFixture(t)
	buildShortTerm(t, brainDir)
	overlayPath := filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath))
	before, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatal(err)
	}
	writeSessionTombstoneBytes(t, brainDir, []byte("{broken"))

	err = brainstore.WithBrainWriteLock(brainDir, func() error {
		_, buildErr := buildHistoryShortTermLocked(brainDir, time.Now().UTC())
		return buildErr
	})
	requireSessionTombstoneError(t, err, brainstore.StateCorruptCode, brainstore.SessionTombstoneCorrupt)
	after, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed delta overwrote the previously readable overlay")
	}
}

func TestMemoryStatusReportsTombstoneStateWithoutWriting(t *testing.T) {
	for _, test := range []struct {
		name  string
		data  string
		state sessionTombstoneState
		code  string
	}{
		{name: "corrupt", data: "{broken", state: brainstore.SessionTombstoneCorrupt, code: brainstore.StateCorruptCode},
		{name: "unsupported", data: `{"version":2,"excluded":{}}`, state: brainstore.SessionTombstoneUnsupported, code: brainstore.UnsupportedVersionCode},
	} {
		t.Run(test.name, func(t *testing.T) {
			brainDir, _, _ := historyProjectionFixture(t)
			writeSessionTombstoneBytes(t, brainDir, []byte(test.data))
			path := filepath.Join(brainDir, filepath.FromSlash(brainstore.SessionTombstonesPath))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			health := memoryAggregateHealth(brainDir, nil)
			privacy, ok := health["tombstones"].(map[string]any)
			if !ok {
				t.Fatalf("tombstone health missing: %+v", health)
			}
			if privacy["state"] != test.state || privacy["error_code"] != test.code {
				t.Fatalf("tombstone health = %+v, want state=%s code=%s", privacy, test.state, test.code)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("read-only status rewrote tombstone state")
			}
		})
	}
}

func TestCorruptTombstonesRefuseHandoffDashAndVizStartup(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: bmanifest.SchemaVersion, GeneratedAt: now, RepoKey: storage.Key,
		Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, Sessions: []exportSession{{
			SessionID: "private", Branch: "feature", Summary: &checkpointSummary{Intent: "PRIVATE-STARTUP-CANARY"},
		}}}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	writeSessionTombstoneBytes(t, storage.BrainDir, []byte("{broken"))

	cmd := &cobra.Command{Use: "privacy-startup-test"}
	for name, run := range map[string]func() error{
		"handoff": func() error { return runBrainHandoff(context.Background(), cmd, opts, 5, true) },
		"dash": func() error {
			_, err := dash.AssembleBrainSnapshot(context.Background(), dashConfig(opts), repoDir, repoDir, storage.BrainDir, "feature", 10)
			return err
		},
		"viz": func() error {
			return viz.Run(context.Background(), cmd, vizConfig(opts), viz.Flags{NoOpen: true}, repoDir)
		},
	} {
		t.Run(name, func(t *testing.T) {
			requireSessionTombstoneError(t, run(), brainstore.StateCorruptCode, brainstore.SessionTombstoneCorrupt)
		})
	}
}

func TestCorruptTombstonesReturnTypedCLIRetrievalErrors(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Version: "test", Env: env, Runner: runner, Now: time.Now}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	writeSessionTombstoneBytes(t, storage.BrainDir, []byte("{broken"))

	out, cliErr := execute(t, NewRootCommand(opts), "search", "private canary", "--format", "json")
	requireSessionTombstoneError(t, cliErr, brainstore.StateCorruptCode, brainstore.SessionTombstoneCorrupt)
	var envelope commandJSONError
	if err := json.Unmarshal([]byte(out), &envelope); err != nil || envelope.Code != brainstore.StateCorruptCode {
		t.Fatalf("CLI error envelope = %+v decode=%v output=%s", envelope, err, out)
	}
}
