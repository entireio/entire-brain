package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func semanticRegressionPathDigest(t *testing.T, repoDir, clean string, ignore brainIgnore) ([32]byte, int64) {
	t.Helper()
	h := sha256.New()
	var total int64
	if err := appendWorktreePathContent(h, &total, repoDir, clean, ignore); err != nil {
		t.Fatal(err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, total
}

func TestSemanticWorktreePathFingerprintIncludesFilesAndExcludesIgnoredAndSymlinks(t *testing.T) {
	repoDir := t.TempDir()
	for rel, body := range map[string]string{
		"untracked/kept.go":          "package kept\n",
		"untracked/ignored/key.pem":  "secret-one\n",
		"untracked/ignored-file.pem": "secret-one\n",
	} {
		path := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ignore := brainIgnore{patterns: []string{"untracked/ignored/**", "untracked/ignored-file.pem"}}
	base, baseBytes := semanticRegressionPathDigest(t, repoDir, "untracked", ignore)
	if baseBytes == 0 {
		t.Fatal("included worktree file contributed no fingerprint bytes")
	}
	if _, singleBytes := semanticRegressionPathDigest(t, repoDir, "untracked/kept.go", ignore); singleBytes == 0 {
		t.Fatal("top-level untracked file contributed no fingerprint bytes")
	}
	if err := os.WriteFile(filepath.Join(repoDir, "untracked", "kept.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	included, _ := semanticRegressionPathDigest(t, repoDir, "untracked", ignore)
	if included == base {
		t.Fatal("changing an included file did not change the worktree fingerprint")
	}
	if err := os.WriteFile(filepath.Join(repoDir, "untracked", "ignored", "key.pem"), []byte("secret-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ignored, _ := semanticRegressionPathDigest(t, repoDir, "untracked", ignore)
	if ignored != included {
		t.Fatal("changing an ignored file changed the worktree fingerprint")
	}
	if err := os.WriteFile(filepath.Join(repoDir, "untracked", "ignored-file.pem"), []byte("secret-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ignoredFile, _ := semanticRegressionPathDigest(t, repoDir, "untracked", ignore)
	if ignoredFile != ignored {
		t.Fatal("changing an ignored sibling file changed the worktree fingerprint")
	}
	missing, missingBytes := semanticRegressionPathDigest(t, repoDir, "missing", ignore)
	if missing != sha256.Sum256(nil) || missingBytes != 0 {
		t.Fatalf("missing path contributed to fingerprint: %x bytes=%d", missing, missingBytes)
	}
	t.Run("symlink paths", func(t *testing.T) {
		external := filepath.Join(t.TempDir(), "external.go")
		if err := os.WriteFile(external, []byte("external-one\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		symlink := filepath.Join(repoDir, "untracked", "linked.go")
		if err := os.Symlink(external, symlink); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if directLink, linkBytes := semanticRegressionPathDigest(t, repoDir, "untracked/linked.go", ignore); directLink != sha256.Sum256(nil) || linkBytes != 0 {
			t.Fatalf("top-level symlink contributed to fingerprint: %x bytes=%d", directLink, linkBytes)
		}
		if err := os.WriteFile(external, []byte("external-two\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		linked, _ := semanticRegressionPathDigest(t, repoDir, "untracked", ignore)
		if linked != ignoredFile {
			t.Fatal("changing a symlink target outside the repository changed the fingerprint")
		}
	})
}

func TestSemanticWorktreePathFingerprintRejectsOversizedFilesAndTotals(t *testing.T) {
	repoDir := t.TempDir()
	path := filepath.Join(repoDir, "oversized.go")
	if err := os.WriteFile(path, make([]byte, semanticWorktreeFingerprintMaxFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	var total int64
	if err := appendWorktreePathContent(h, &total, repoDir, "oversized.go", brainIgnore{}); err == nil {
		t.Fatal("oversized worktree path was accepted into fingerprint")
	}

	if err := os.WriteFile(filepath.Join(repoDir, "small.go"), []byte("package small\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	total = semanticWorktreeFingerprintMaxTotal
	if err := appendWorktreePathContent(h, &total, repoDir, "small.go", brainIgnore{}); err == nil {
		t.Fatal("fingerprint exceeding total size budget was accepted")
	}
}

func TestSemanticWorktreePathFingerprintPropagatesWalkErrors(t *testing.T) {
	repoDir := t.TempDir()
	blocked := filepath.Join(repoDir, "untracked", "blocked")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "hidden.go"), []byte("package hidden\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	h := sha256.New()
	var total int64
	err := appendWorktreePathContent(h, &total, repoDir, "untracked", brainIgnore{})
	if err == nil {
		t.Skip("test environment permits traversal of mode-000 directories")
	}
}

func TestSemanticFailedReplacementPreservesReadableActiveGeneration(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC) }}
	cmd := &cobra.Command{Use: "index"}
	if err := runSemanticIndex(context.Background(), cmd, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("initial index: %v", err)
	}
	brainDir, err := brainDirForKey(env, "gh/example/repo")
	if err != nil {
		t.Fatal(err)
	}
	before, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	oldStore := filepath.Join(brainDir, filepath.FromSlash(before.Sources.Semantic.StorePath))
	oldDB, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(oldStore))
	if err != nil {
		t.Fatal(err)
	}
	var oldID, oldName string
	if err := oldDB.QueryRow(`SELECT id, name FROM symbols LIMIT 1`).Scan(&oldID, &oldName); err != nil {
		_ = oldDB.Close()
		t.Fatal(err)
	}
	if err := oldDB.Close(); err != nil {
		t.Fatal(err)
	}
	key := fakeCommandKey("entire", "graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network")
	runner.responses[key] = fakeCommandResponse{stdout: `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"aaa111","tree":"tree111","warnings":[],"partial_failures":[]}` + "\n" + `{"record_type":"symbol"}` + "\n"}
	if err := runSemanticIndex(context.Background(), cmd, opts, semanticIndexOptions{graphBinary: "entire", force: true}, repoDir); err == nil {
		t.Fatal("malformed replacement generation unexpectedly succeeded")
	}
	after, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Sources.Semantic.StorePath != before.Sources.Semantic.StorePath || after.Sources.Semantic.GenerationPath != before.Sources.Semantic.GenerationPath {
		t.Fatalf("failed replacement changed active generation: before=%+v after=%+v", before.Sources.Semantic, after.Sources.Semantic)
	}
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(oldStore))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var gotID, gotName string
	if err := db.QueryRow(`SELECT id, name FROM symbols LIMIT 1`).Scan(&gotID, &gotName); err != nil || gotID != oldID || gotName != oldName {
		t.Fatalf("old generation identity changed after failed replacement: got=(%q,%q) want=(%q,%q) err=%v", gotID, gotName, oldID, oldName, err)
	}
}

func TestSemanticWarmReuseAndForcedFullIndexHaveSameSymbols(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Env: env, Runner: runner, Now: func() time.Time { return time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC) }}
	index := func(force bool) *semanticSourceManifest {
		t.Helper()
		cmd := &cobra.Command{Use: "index"}
		if err := runSemanticIndex(context.Background(), cmd, opts, semanticIndexOptions{graphBinary: "entire", force: force}, repoDir); err != nil {
			t.Fatalf("index force=%t: %v", force, err)
		}
		brainDir, err := brainDirForKey(env, "gh/example/repo")
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := loadBrainManifest(brainDir)
		if err != nil || manifest.Sources.Semantic == nil {
			t.Fatalf("load semantic source: manifest=%+v err=%v", manifest, err)
		}
		return manifest.Sources.Semantic
	}
	readSymbols := func(source *semanticSourceManifest) []string {
		t.Helper()
		brainDir, err := brainDirForKey(env, "gh/example/repo")
		if err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(filepath.Join(brainDir, filepath.FromSlash(source.StorePath))))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.Query(`SELECT id || ':' || name FROM symbols ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var symbols []string
		for rows.Next() {
			var symbol string
			if err := rows.Scan(&symbol); err != nil {
				t.Fatal(err)
			}
			symbols = append(symbols, symbol)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return symbols
	}

	initial := readSymbols(index(false))
	warmReuse := readSymbols(index(false))
	full := readSymbols(index(true))
	if len(initial) == 0 || !reflect.DeepEqual(warmReuse, initial) || !reflect.DeepEqual(full, initial) {
		t.Fatalf("index symbol parity failed: initial=%q warm-reuse=%q full=%q", initial, warmReuse, full)
	}
}

func TestSemanticChangedIndexMatchesFreshFullRebuildAndRemovesStaleSymbols(t *testing.T) {
	changedSnapshot := `{"schema_version":"1.0","provider":"entire-graph","provider_version":"0.1.0","repo_key":"gh/example/repo","commit":"bbb222","tree":"tree222","capabilities":["go"],"warnings":[],"partial_failures":[]}
{"record_type":"symbol","id":"gh/example/repo:go:internal/auth/token.go:function:auth.RefreshToken","kind":"function","name":"RefreshToken","qualified_name":"auth.RefreshToken","file_path":"internal/auth/token.go","start_line":10,"end_line":20,"signature":"func RefreshToken(token string) error","language":"Go","stable_id_version":"1"}
{"record_type":"summary","warnings":[],"partial_failures":[]}
`
	configureChangedFixture := func(repoDir string, runner *fakeCommandRunner) {
		runner.responses[fakeCommandKey("git", "rev-parse", "HEAD")] = fakeCommandResponse{stdout: "bbb222\n"}
		runner.responses[fakeCommandKey("git", "rev-parse", "HEAD^{tree}")] = fakeCommandResponse{stdout: "tree222\n"}
		runner.responses[fakeCommandKey("entire", "graph", "snapshot", "--repo", repoDir, "--format", "ndjson", "--no-network")] = fakeCommandResponse{stdout: changedSnapshot}
	}
	readSymbols := func(t *testing.T, env EntireEnv, source *semanticSourceManifest) []string {
		t.Helper()
		brainDir, err := brainDirForKey(env, "gh/example/repo")
		if err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(filepath.Join(brainDir, filepath.FromSlash(source.StorePath))))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.Query(`SELECT id || ':' || name FROM symbols ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var symbols []string
		for rows.Next() {
			var symbol string
			if err := rows.Scan(&symbol); err != nil {
				t.Fatal(err)
			}
			symbols = append(symbols, symbol)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return symbols
	}
	index := func(t *testing.T, repoDir string, env EntireEnv, runner *fakeCommandRunner) *semanticSourceManifest {
		t.Helper()
		if err := runSemanticIndex(context.Background(), &cobra.Command{Use: "index"}, Options{Env: env, Runner: runner, Now: time.Now}, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
			t.Fatal(err)
		}
		brainDir, err := brainDirForKey(env, "gh/example/repo")
		if err != nil {
			t.Fatal(err)
		}
		manifest, err := loadBrainManifest(brainDir)
		if err != nil || manifest.Sources.Semantic == nil {
			t.Fatalf("load changed index manifest=%+v err=%v", manifest, err)
		}
		return manifest.Sources.Semantic
	}

	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	initial := index(t, repoDir, env, runner)
	if symbols := readSymbols(t, env, initial); len(symbols) != 1 || symbols[0] != "gh/example/repo:go:internal/auth/token.go:function:auth.ValidateToken:ValidateToken" {
		t.Fatalf("initial symbols=%q", symbols)
	}
	configureChangedFixture(repoDir, runner)
	changed := index(t, repoDir, env, runner)
	changedSymbols := readSymbols(t, env, changed)

	freshRepo := t.TempDir()
	freshEnv := semanticTestEnv(t, freshRepo)
	freshRunner := semanticFixtureRunner(freshRepo, changedSnapshot)
	configureChangedFixture(freshRepo, freshRunner)
	freshSymbols := readSymbols(t, freshEnv, index(t, freshRepo, freshEnv, freshRunner))
	if !reflect.DeepEqual(changedSymbols, freshSymbols) || len(changedSymbols) != 1 || changedSymbols[0] != "gh/example/repo:go:internal/auth/token.go:function:auth.RefreshToken:RefreshToken" {
		t.Fatalf("changed/fresh symbol parity failed: changed=%q fresh=%q", changedSymbols, freshSymbols)
	}
}

func TestSemanticImpactSnapshotFallbackMatchesStoreBackedQuery(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	opts := Options{Env: env, Runner: runner, Now: time.Now}
	if err := runSemanticIndex(context.Background(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{graphBinary: "entire"}, repoDir); err != nil {
		t.Fatal(err)
	}
	brainDir, err := brainDirForKey(env, "gh/example/repo")
	if err != nil {
		t.Fatal(err)
	}
	source := *mustSemanticSource(t, env)
	storeRoots, storeSymbols, storeRelations, err := semanticImpactFacts(brainDir, &source, "ValidateToken", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(storeRoots) == 0 || len(storeSymbols) == 0 {
		t.Fatal("fixture produced no impact evidence")
	}
	source.GenerationPath, source.StorePath = "", ""
	snapshotRoots, snapshotSymbols, snapshotRelations, err := semanticImpactFacts(brainDir, &source, "ValidateToken", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshotRoots, storeRoots) || !reflect.DeepEqual(snapshotSymbols, storeSymbols) || !reflect.DeepEqual(snapshotRelations, storeRelations) {
		t.Fatalf("snapshot-backed impact differs from store query: snapshot=(%+v,%+v,%+v) store=(%+v,%+v,%+v)", snapshotRoots, snapshotSymbols, snapshotRelations, storeRoots, storeSymbols, storeRelations)
	}
}
