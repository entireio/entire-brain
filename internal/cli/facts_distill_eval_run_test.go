package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeDistillEvalExec intercepts the shell-out seam, records every call, and
// simulates the two child commands the harness spawns.
type fakeEvalCall struct {
	dir  string
	env  []string
	args []string
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func writeEvalBrainFixture(t *testing.T, brainDir string, sizes map[string]int, now time.Time) {
	t.Helper()
	var sessions []exportSession
	ids := make([]string, 0, len(sizes))
	for id := range sizes {
		ids = append(ids, id)
	}
	// Deterministic order for the manifest.
	for _, id := range ids {
		sessions = append(sessions, exportSession{
			SessionID: id, Branch: "main", LatestCheckpoint: "cp-" + id,
			TranscriptPath: "sessions/main/" + id + ".jsonl", CreatedAt: now,
		})
	}
	for _, s := range sessions {
		path := filepath.Join(brainDir, filepath.FromSlash(s.TranscriptPath))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), sizes[s.SessionID]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := exportManifest{
		SchemaVersion: brainManifestSchemaVersion,
		GeneratedAt:   now,
		DefaultBranch: "main",
		Sources: &brainSources{
			Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions},
		},
	}
	if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
		t.Fatal(err)
	}
}

func runDistillEvalCommand(t *testing.T, repoRoot string, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := NewRootCommand(Options{
		Version: "test",
		Env: EntireEnv{
			RepoRoot:        repoRoot,
			PluginConfigDir: t.TempDir(),
			PluginDataDir:   t.TempDir(),
			PluginStateDir:  t.TempDir(),
			PluginCacheDir:  t.TempDir(),
		},
		Now: time.Now,
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"facts", "distill-eval"}, args...))
	err := cmd.Execute()
	return stdout.String() + stderr.String(), err
}

func TestDistillEvalCorpusSelectsSmallestSessions(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	sizes := map[string]int{"s-small": 10, "s-mid": 20, "s-big": 30}

	var calls []fakeEvalCall
	restore := distillEvalExec
	distillEvalExec = func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		calls = append(calls, fakeEvalCall{dir: dir, env: env, args: args})
		if len(args) >= 2 && args[0] == "refresh" && args[1] == "sessions" {
			// First pass (no -o) writes metadata only, like the real export;
			// the -o pass materializes transcripts into the named bundle dir.
			brainDir := filepath.Join(envValue(env, "XDG_DATA_HOME"), "brainrepo")
			if out := argValue(args, "-o"); out != "" {
				brainDir = out
				writeEvalBrainFixture(t, brainDir, sizes, now)
			} else {
				writeEvalBrainFixture(t, brainDir, map[string]int{}, now)
				var sessions []exportSession
				for id := range sizes {
					sessions = append(sessions, exportSession{SessionID: id, Branch: "main", TranscriptPath: "sessions/main/" + id + ".jsonl", CreatedAt: now})
				}
				manifest := exportManifest{
					SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now, DefaultBranch: "main",
					Sources: &brainSources{Sessions: &sessionSourceManifest{GeneratedAt: now, DefaultBranch: "main", Sessions: sessions}},
				}
				if err := writeBrainManifestAndReadme(brainDir, manifest); err != nil {
					t.Fatal(err)
				}
			}
			return nil, nil
		}
		t.Fatalf("unexpected exec args: %v", args)
		return nil, nil
	}
	defer func() { distillEvalExec = restore }()

	outDir := t.TempDir()
	if _, err := runDistillEvalCommand(t, t.TempDir(), "corpus", "--out", outDir, "--sessions", "2", "--golden", "1"); err != nil {
		t.Fatalf("corpus: %v", err)
	}
	materialized := false
	for _, c := range calls {
		if c.args[0] == "refresh" && argValue(c.args, "-o") != "" {
			materialized = true
		}
	}
	if !materialized {
		t.Fatal("corpus must run a second `refresh sessions -o <brain-dir>` pass: without -o the export is metadata-only and no transcripts exist on disk")
	}

	data, err := os.ReadFile(filepath.Join(outDir, "corpus.json"))
	if err != nil {
		t.Fatalf("read corpus.json: %v", err)
	}
	var corpus distillEvalCorpus
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	if len(corpus.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(corpus.Sessions), corpus)
	}
	byID := map[string]distillEvalCorpusEntry{}
	for _, s := range corpus.Sessions {
		byID[s.SessionID] = s
	}
	if _, ok := byID["s-big"]; ok {
		t.Error("largest session must not be selected")
	}
	if !byID["s-small"].Golden || byID["s-mid"].Golden {
		t.Errorf("golden must mark only the smallest: %+v", corpus.Sessions)
	}
	if byID["s-small"].RawBytes != 10 || byID["s-small"].TranscriptPath == "" {
		t.Errorf("entry fields wrong: %+v", byID["s-small"])
	}
	sum := sha256.Sum256([]byte("s-mid\ns-small"))
	if corpus.CorpusSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("corpus_sha256 = %q, want sha256 of sorted ids", corpus.CorpusSHA256)
	}
}

func TestDistillEvalRunProducesArtifactsInSandbox(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	corpusDir := t.TempDir()
	corpus := distillEvalCorpus{
		RepoKey: "test/repo", CreatedAt: now, CorpusSHA256: "abc123",
		Sessions: []distillEvalCorpusEntry{
			{SessionID: "s1", Branch: "main", RawBytes: 10, Chunks: 1, Golden: true, TranscriptPath: "sessions/main/s1.jsonl"},
			{SessionID: "s2", Branch: "main", RawBytes: 20, Chunks: 1, TranscriptPath: "sessions/main/s2.jsonl"},
		},
	}
	corpusPath := filepath.Join(corpusDir, "corpus.json")
	data, _ := json.Marshal(corpus)
	if err := os.WriteFile(corpusPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var calls []fakeEvalCall
	restore := distillEvalExec
	distillEvalExec = func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		calls = append(calls, fakeEvalCall{dir: dir, env: env, args: args})
		if args[0] == "refresh" {
			dir := argValue(args, "-o")
			if dir == "" {
				dir = filepath.Join(envValue(env, "XDG_DATA_HOME"), "brainrepo")
			}
			writeEvalBrainFixture(t, dir, map[string]int{"s1": 10, "s2": 20}, now)
			return nil, nil
		}
		if args[0] == "distill" {
			session := argValue(args, "--session")
			rawOut := argValue(args, "--raw-facts-out")
			trace := argValue(args, "--call-trace")
			if session == "" || rawOut == "" || trace == "" {
				t.Fatalf("distill call missing flags: %v", args)
			}
			chunk := `{"type":"chunk","session_id":"` + session + `","branch":"main","chunk_index":1,"chunk_count":1,"facts_emitted":1,"cap":2}` + "\n"
			fact := `{"type":"fact","session_id":"` + session + `","branch":"main","chunk_index":1,"fact_id":"f-` + session + `","kind":"gotcha","paths":["a.b.c"],"text":"T","checkpoint_id":"cp"}` + "\n"
			if err := os.WriteFile(rawOut, []byte(chunk+fact), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(trace, []byte(`{"tokens":5}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return nil, nil
		}
		t.Fatalf("unexpected exec args: %v", args)
		return nil, nil
	}
	defer func() { distillEvalExec = restore }()

	if _, err := runDistillEvalCommand(t, t.TempDir(), "run", "--corpus", corpusPath, "--name", "base", "--cap", "2"); err != nil {
		t.Fatalf("run: %v", err)
	}

	runDir := filepath.Join(corpusDir, "runs", "base")
	chunks, facts := readRawEvalLines(t, filepath.Join(runDir, "raw-facts.jsonl"))
	if len(chunks) != 2 || len(facts) != 2 {
		t.Fatalf("concatenated raw output wrong: %d chunks %d facts", len(chunks), len(facts))
	}
	traceData, err := os.ReadFile(filepath.Join(runDir, "call-trace.jsonl"))
	if err != nil || strings.Count(string(traceData), "\n") != 2 {
		t.Fatalf("call trace not concatenated: %v %q", err, traceData)
	}

	var meta distillEvalRunMeta
	metaData, err := os.ReadFile(filepath.Join(runDir, "run.json"))
	if err != nil {
		t.Fatalf("run.json: %v", err)
	}
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Variant.Name != "base" || meta.Variant.Agent != "claude-code" || meta.Variant.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("variant defaults wrong: %+v", meta.Variant)
	}
	if meta.Variant.CapPerChunk != 2 || meta.Variant.TemplateSHA256 == "" || meta.CorpusSHA256 != "abc123" {
		t.Errorf("variant metadata wrong: %+v", meta)
	}

	distillCalls := 0
	for _, c := range calls {
		if c.args[0] != "distill" {
			continue
		}
		distillCalls++
		if got := envValue(c.env, "XDG_DATA_HOME"); got != filepath.Join(runDir, "sandbox", "data") {
			t.Errorf("child XDG_DATA_HOME = %q", got)
		}
		if got := envValue(c.env, envFactsMaxPerChunk); got != "2" {
			t.Errorf("child cap env = %q, want 2", got)
		}
		if argValue(c.args, "--agent") != "claude-code" || argValue(c.args, "--model") != "claude-haiku-4-5-20251001" {
			t.Errorf("distill args missing agent/model pin: %v", c.args)
		}
	}
	if distillCalls != 2 {
		t.Errorf("distill called %d times, want 2", distillCalls)
	}
}

// run.json must hash the template the CHILD uses: the child env strips
// ENTIRE_BRAIN_DISTILL_TEMPLATE, so with no --template the hash is the
// embedded template's, regardless of the parent's env.
func TestDistillEvalTemplateSHAIgnoresEnvOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "alt.md")
	if err := os.WriteFile(override, []byte("OVERRIDE ${TAXONOMY_BLOCK}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envDistillTemplate, override)

	got, err := distillEvalTemplateSHA("")
	if err != nil {
		t.Fatal(err)
	}
	os.Unsetenv(envDistillTemplate)
	embedded, err := distillEvalTemplateSHA("")
	if err != nil {
		t.Fatal(err)
	}
	if got != embedded {
		t.Errorf("template sha honored the parent env override the child never sees: %s != %s", got, embedded)
	}
	fromOverride, err := distillEvalTemplateSHA(override)
	if err != nil {
		t.Fatal(err)
	}
	if got == fromOverride {
		t.Error("sha matches the override template; it must match the embedded one")
	}
}

func TestDistillEvalRunRefusesWithoutCorpus(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	_, err := runDistillEvalCommand(t, t.TempDir(), "run", "--corpus", missing, "--name", "x")
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("expected error naming %q, got %v", missing, err)
	}
}
