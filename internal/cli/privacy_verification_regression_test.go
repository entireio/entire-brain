package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func privacyRegressionCommandFixture(t *testing.T, now time.Time) (Options, string) {
	t.Helper()
	repoDir := t.TempDir()
	runner := &fakeCommandRunner{responses: map[string]fakeCommandResponse{
		fakeCommandKey("git", "rev-parse", "--show-toplevel"): {stdout: repoDir + "\n"},
		fakeCommandKey("git", "remote", "get-url", "origin"):  {stdout: "git@github.com:acme/privacy-regression.git\n"},
		fakeCommandKey("git", "branch", "--show-current"):     {stdout: "main\n"},
	}}
	opts := Options{
		Version: "test",
		Env: EntireEnv{
			RepoRoot:       repoDir,
			PluginDataDir:  t.TempDir(),
			PluginCacheDir: t.TempDir(),
		},
		Runner: runner,
		Now:    func() time.Time { return now },
	}
	storage, err := repoStoragePaths(context.Background(), runner, opts.Env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	fixture := writePrivacyFixture(t)
	if err := os.MkdirAll(filepath.Dir(storage.BrainDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture, storage.BrainDir); err != nil {
		t.Fatal(err)
	}
	return opts, storage.BrainDir
}

func writePrivacyVectorFixture(t *testing.T, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vectors.sqlite")
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("execute %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrivacyVectorSnapshotInspectsRealSQLiteMembership(t *testing.T) {
	path := writePrivacyVectorFixture(t,
		`CREATE TABLE history_ids(rowid INTEGER PRIMARY KEY, record_id TEXT UNIQUE NOT NULL)`,
		`INSERT INTO history_ids(record_id) VALUES ('retained'), ('excluded')`,
	)
	findings, err := inspectPrivacyVectorIDsSnapshot(path, "history/embed/vectors.sqlite", map[string]struct{}{"retained": {}}, "secret-sess")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].SessionID != "secret-sess" || findings[0].Artifact != "vector_store" || !strings.Contains(findings[0].Detail, "excluded") {
		t.Fatalf("findings = %+v, want the excluded real SQLite record only", findings)
	}

	findings, err = inspectPrivacyVectorIDsSnapshot(path, "history/embed/vectors.sqlite", map[string]struct{}{"retained": {}, "excluded": {}}, "secret-sess")
	if err != nil || len(findings) != 0 {
		t.Fatalf("all retained: findings=%+v err=%v", findings, err)
	}
}

func TestPrivacyVectorSnapshotFailsClosedOnMalformedStores(t *testing.T) {
	t.Run("missing table", func(t *testing.T) {
		path := writePrivacyVectorFixture(t, `CREATE TABLE unrelated(id TEXT)`)
		if _, err := inspectPrivacyVectorIDsSnapshot(path, "missing.sqlite", nil, "secret-sess"); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) || !strings.Contains(err.Error(), "history_ids") {
			t.Fatalf("error = %v, want corrupt missing-table failure", err)
		}
	})
	t.Run("malformed database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "malformed.sqlite")
		if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := inspectPrivacyVectorIDsSnapshot(path, "malformed.sqlite", nil, "secret-sess"); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) {
			t.Fatalf("error = %v, want corrupt-store failure", err)
		}
	})
	t.Run("null record id", func(t *testing.T) {
		path := writePrivacyVectorFixture(t,
			`CREATE TABLE history_ids(rowid INTEGER PRIMARY KEY, record_id TEXT)`,
			`INSERT INTO history_ids(record_id) VALUES (NULL)`,
		)
		if _, err := inspectPrivacyVectorIDsSnapshot(path, "null-id.sqlite", nil, "secret-sess"); err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) || !strings.Contains(err.Error(), "inspect vector ID") {
			t.Fatalf("error = %v, want corrupt row-scan failure", err)
		}
	})
}

func TestPrivacyVectorSnapshotFailsClosedDuringIteration(t *testing.T) {
	// quick_check succeeds; the first row is readable, then SQLite raises a
	// runtime error. A partially scanned store must never be reported clean.
	path := writePrivacyVectorFixture(t, `CREATE VIEW history_ids AS
		SELECT CASE n WHEN 1 THEN 'retained' ELSE CAST(abs(-9223372036854775808) AS TEXT) END AS record_id
		FROM (SELECT 1 AS n UNION ALL SELECT 2)`)
	findings, err := inspectPrivacyVectorIDsSnapshot(path, "late-error.sqlite", map[string]struct{}{"retained": {}}, "secret-sess")
	if err == nil || !strings.Contains(err.Error(), memoryErrStateCorrupt) || !strings.Contains(err.Error(), "integer overflow") || findings != nil {
		t.Fatalf("partially readable store must fail closed: findings=%+v err=%v", findings, err)
	}
}

func TestPrivacyVectorSnapshotEnforcesScanCeiling(t *testing.T) {
	// The database is a single small view, not a multi-million-row fixture.
	// Repeated retained IDs keep the scan's memory use bounded independently of
	// its row count, while exercising the actual verification ceiling.
	path := writePrivacyVectorFixture(t, fmt.Sprintf(`CREATE VIEW history_ids AS
		WITH RECURSIVE sequence(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM sequence WHERE n < %d)
		SELECT 'retained' AS record_id FROM sequence`, privacyVectorVerifyMaxRows+1))
	findings, err := inspectPrivacyVectorIDsSnapshot(path, "oversized-view.sqlite", map[string]struct{}{"retained": {}}, "secret-sess")
	if err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) || !strings.Contains(err.Error(), "exceeds verification ceiling") || findings != nil {
		t.Fatalf("unbounded scan must fail closed: findings=%+v err=%v", findings, err)
	}
}

type privacyRegressionErrorWriter struct{}

func (privacyRegressionErrorWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("privacy regression output failure")
}

func TestPrivacyVerifyRootCommandTextJSONAndViolationError(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	opts, brainDir := privacyRegressionCommandFixture(t, now)
	out, err := execute(t, NewRootCommand(opts), "privacy", "verify")
	if err != nil || !strings.Contains(out, "verified 0 excluded/purged sessions") || !strings.Contains(out, "clean: no excluded content") {
		t.Fatalf("clean text verify: err=%v output=%q", err, out)
	}

	out, _, err = executeSplit(t, NewRootCommand(opts), "privacy", "verify", "--json")
	if err != nil {
		t.Fatalf("clean JSON verify: %v\n%s", err, out)
	}
	var clean privacyVerifyReport
	if json.Unmarshal([]byte(out), &clean) != nil || !clean.Clean || clean.Findings == nil {
		t.Fatalf("clean JSON report = %#v, output=%q", clean, out)
	}

	stones := loadSessionTombstones(brainDir)
	stones.Excluded["secret-sess"] = sessionTombstone{At: now, Reason: "regression test"}
	if err := saveSessionTombstones(brainDir, stones); err != nil {
		t.Fatal(err)
	}
	out, err = execute(t, NewRootCommand(opts), "privacy", "verify")
	if err == nil || !strings.Contains(err.Error(), "privacy verify found") || !strings.Contains(out, "VIOLATION secret-sess") || strings.Contains(out, "clean: no excluded content") {
		t.Fatalf("dirty text verify: err=%v output=%q", err, out)
	}
	out, _, err = executeSplit(t, NewRootCommand(opts), "privacy", "verify", "--json")
	if err == nil || !strings.Contains(err.Error(), "privacy verify found") {
		t.Fatalf("dirty JSON verify must return a violation error: err=%v output=%q", err, out)
	}
	var dirty privacyVerifyReport
	decodeErr := json.Unmarshal([]byte(out), &dirty)
	if decodeErr != nil || dirty.Clean || len(dirty.Findings) == 0 {
		t.Fatalf("dirty JSON report=%+v decodeErr=%v output=%q", dirty, decodeErr, out)
	}
}

func TestPrivacyVerifyCommandPropagatesOutputErrors(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	opts, _ := privacyRegressionCommandFixture(t, now)
	for _, args := range [][]string{nil, {"--json"}} {
		cmd := newPrivacyVerifyCommand(opts)
		cmd.SetOut(privacyRegressionErrorWriter{})
		cmd.SetErr(privacyRegressionErrorWriter{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "privacy regression output failure") {
			t.Fatalf("args=%v error=%v, want output failure", args, err)
		}
	}
}

func TestPrivacyVerifyCommandPropagatesStorageResolutionError(t *testing.T) {
	cmd := newPrivacyVerifyCommand(Options{
		Env:    EntireEnv{RepoRoot: filepath.Join("relative", "repo")},
		Runner: &fakeCommandRunner{},
		Now:    time.Now,
	})
	if err := cmd.Execute(); err == nil {
		t.Fatal("verify unexpectedly accepted an unresolved relative repository root")
	}
}

func privacyTreeDigest(t *testing.T, root string) [32]byte {
	t.Helper()
	h := sha256.New()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(data))
		_, _ = h.Write(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func TestPrivacyRetentionRootDryRunBoundariesAndBytePreservation(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	opts, brainDir := privacyRegressionCommandFixture(t, now)
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Sources.Sessions.Sessions[0].CreatedAt = now.Add(-2 * time.Hour)
	manifest.Sources.Sessions.Sessions[0].Branch = "release"
	manifest.Sources.Sessions.Sessions[1].CreatedAt = now.Add(-time.Hour) // exactly at cutoff: excluded from selection
	manifest.Sources.Sessions.Sessions[1].Branch = "release"
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	before := privacyTreeDigest(t, brainDir)
	out, err := execute(t, NewRootCommand(opts), "privacy", "retention", "--max-age", "1h", "--branch", "release", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("retention dry run: %v\n%s", err, out)
	}
	var payload struct {
		DryRun   bool                 `json:"dry_run"`
		Sessions []retentionPlanEntry `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decode dry run: %v\n%s", err, out)
	}
	if !payload.DryRun || len(payload.Sessions) != 1 || payload.Sessions[0].SessionID != "clean-sess" || payload.Sessions[0].Action != "exclude" {
		t.Fatalf("dry-run payload = %+v", payload)
	}
	if after := privacyTreeDigest(t, brainDir); after != before {
		t.Fatal("dry run changed brain bytes")
	}
	out, err = execute(t, NewRootCommand(opts), "privacy", "retention", "--max-age", "1h", "--branch", "release", "--dry-run")
	if err != nil || !strings.Contains(out, "dry-run: would apply retention") || !strings.Contains(out, "exclude clean-sess") {
		t.Fatalf("text dry run: err=%v output=%q", err, out)
	}
	if after := privacyTreeDigest(t, brainDir); after != before {
		t.Fatal("text dry run changed brain bytes")
	}

	out, err = execute(t, NewRootCommand(opts), "privacy", "retention", "--max-age", "1h", "--branch", "missing", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("empty retention plan: %v\n%s", err, out)
	}
	payload.Sessions = nil
	if err := json.Unmarshal([]byte(out), &payload); err != nil || payload.Sessions == nil || len(payload.Sessions) != 0 {
		t.Fatalf("empty plan must encode as []: payload=%+v err=%v output=%q", payload, err, out)
	}
}

func TestPrivacyRetentionRootAppliesPurgeAndPreservesUnselectedSession(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	opts, brainDir := privacyRegressionCommandFixture(t, now)
	cleanPath := filepath.Join(brainDir, "sessions", "main", "20260801T000000Z_clean.jsonl")
	cleanBefore, err := os.ReadFile(cleanPath)
	if err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(brainDir, "sessions", "main", "20260802T000000Z_secret.jsonl")
	secretBefore, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, NewRootCommand(opts), "privacy", "retention", "--max-age", "25h", "--purge")
	if err != nil {
		t.Fatalf("apply purge retention: %v\n%s", err, out)
	}
	if !strings.Contains(out, "applied retention") || !strings.Contains(out, "purge clean-sess") || strings.Contains(out, "purge secret-sess") {
		t.Fatalf("unexpected apply output: %q", out)
	}
	if _, err := os.Stat(cleanPath); !os.IsNotExist(err) {
		t.Fatalf("selected transcript still exists: %v", err)
	}
	secretAfter, err := os.ReadFile(secretPath)
	if err != nil || string(secretAfter) != string(secretBefore) {
		t.Fatalf("unselected transcript bytes changed: err=%v before=%q after=%q", err, secretBefore, secretAfter)
	}
	if len(cleanBefore) == 0 {
		t.Fatal("selected fixture transcript unexpectedly empty")
	}
	stones := loadSessionTombstones(brainDir)
	if stone, ok := stones.Excluded["clean-sess"]; !ok || stone.Reason != "purged" {
		t.Fatalf("purge tombstone = %+v, present=%t", stone, ok)
	}
	report, err := verifySessionPrivacy(brainDir)
	if err != nil || !report.Clean {
		t.Fatalf("post-retention verify: clean=%t findings=%+v err=%v", report.Clean, report.Findings, err)
	}
}

func TestPrivacyRetentionRootFailsClosedOnMalformedState(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	opts, brainDir := privacyRegressionCommandFixture(t, now)
	if err := os.WriteFile(filepath.Join(brainDir, exportManifestFileName), []byte(`{"schema_version":`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, NewRootCommand(opts), "privacy", "retention", "--max-age", "24h", "--dry-run")
	if err == nil {
		t.Fatalf("malformed manifest retention unexpectedly succeeded: %q", out)
	}
}
