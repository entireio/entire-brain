package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

type brainBriefProfileFixture struct {
	repoDir  string
	brainDir string
	opts     Options
}

func newBrainBriefProfileFixture(t *testing.T) brainBriefProfileFixture {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticBoundaryFixtureSnapshot())
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}

	if err := runSemanticIndex((&cobra.Command{}).Context(), &cobra.Command{Use: "index"}, opts, semanticIndexOptions{semBinary: "entire"}, repoDir); err != nil {
		t.Fatalf("index: %v", err)
	}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	sessionDir := filepath.Join(storage.BrainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "private-session.jsonl"), []byte(`{"type":"agent_message","message":"PRIVATE_HISTORY_PAYLOAD unrelated ambient record"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if _, err := writeBrainHistoryIndexAndSource(storage.BrainDir, now, nil); err != nil {
		t.Fatalf("write history index: %v", err)
	}

	paths := normalizeFactPaths([]string{"private.profile.measurement"})
	fact := factRecord{
		ID:        factRecordID("PRIVATE_FACT_PAYLOAD", paths),
		Paths:     paths,
		Text:      "PRIVATE_FACT_PAYLOAD",
		Branch:    "feature",
		Origin:    factOriginDistilled,
		Status:    factStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := writeFacts(storage.BrainDir, "feature", []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	if err := updateFactSourceManifest(storage.BrainDir, now); err != nil {
		t.Fatalf("update fact source manifest: %v", err)
	}

	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}
	return brainBriefProfileFixture{repoDir: repoDir, brainDir: storage.BrainDir, opts: opts}
}

func TestBrainBriefProfilePreservesPacketAndWritesCompletePrivateSidecar(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	profilePath := filepath.Join(t.TempDir(), "brief-profile.json")
	task := "ValidateToken PRIVATE_TASK_PAYLOAD PRIVATE_FACT_PAYLOAD ALPHA_ONE BETA_TWO GAMMA_THREE DELTA_FOUR EPSILON_FIVE ZETA_SIX ETA_SEVEN THETA_EIGHT IOTA_NINE"
	if got := len(brainBriefRawHistoryQueries(task)); got != 8 {
		t.Fatalf("raw history query cap = %d, want 8", got)
	}

	withoutProfile, err := execute(t, NewRootCommand(fixture.opts), "brief", task, "--json")
	if err != nil {
		t.Fatalf("brief without profile: %v\n%s", err, withoutProfile)
	}
	if _, statErr := os.Stat(profilePath); !os.IsNotExist(statErr) {
		t.Fatalf("unprofiled brief created a sidecar: %v", statErr)
	}
	withProfile, err := execute(t, NewRootCommand(fixture.opts), "brief", task, "--json", "--profile-json", profilePath)
	if err != nil {
		t.Fatalf("brief with profile: %v\n%s", err, withProfile)
	}
	if withProfile != withoutProfile {
		t.Fatalf("profiling changed packet bytes\nwithout (%d):\n%s\nwith (%d):\n%s", len(withoutProfile), withoutProfile, len(withProfile), withProfile)
	}

	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	info, err := os.Stat(profilePath)
	if err != nil {
		t.Fatalf("stat profile: %v", err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("profile mode = %o, want 600", got)
	}
	for _, private := range []string{task, "PRIVATE_TASK_PAYLOAD", "ALPHA_ONE", "PRIVATE_HISTORY_PAYLOAD", "PRIVATE_FACT_PAYLOAD", fixture.repoDir, fixture.brainDir, "private-session.jsonl"} {
		if strings.Contains(string(data), private) {
			t.Fatalf("profile leaked private value %q:\n%s", private, data)
		}
	}

	var profile brainBriefProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatalf("parse profile: %v\n%s", err, data)
	}
	if profile.SchemaVersion != brainBriefProfileSchemaVersion || profile.DurationUnit != brainBriefProfileDurationUnit {
		t.Fatalf("unexpected profile contract: version=%d unit=%q", profile.SchemaVersion, profile.DurationUnit)
	}
	if profile.TotalBrief.DurationNS <= 0 || !profile.StatusBuildState.Invoked {
		t.Fatalf("missing total/status timing: %+v", profile)
	}
	if !profile.Semantic.Context.Invoked || !profile.Semantic.RuntimeTraces.Invoked || !profile.Semantic.Tests.Invoked {
		t.Fatalf("semantic stages incomplete: %+v", profile.Semantic)
	}
	if profile.History.IndexLoad.Invoked || !profile.History.IndexedRank.Invoked || !profile.History.RawFallback.Invoked {
		t.Fatalf("history stages incomplete: %+v", profile.History)
	}
	if !profile.Facts.Load.Invoked || !profile.Facts.Rank.Invoked {
		t.Fatalf("facts stages incomplete: %+v", profile.Facts)
	}
	if !profile.Synthesis.LikelyFiles.Invoked || !profile.Synthesis.ActionChecklist.Invoked {
		t.Fatalf("synthesis stages incomplete: %+v", profile.Synthesis)
	}
	if !profile.Knowledge.Patterns.Invoked || !profile.Knowledge.Consolidations.Invoked || !profile.Knowledge.Themes.Invoked {
		t.Fatalf("knowledge stages incomplete: %+v", profile.Knowledge)
	}
	if !profile.Packet.Serialization.Invoked || profile.Packet.Format != "json" || profile.Packet.ByteCount != len(withProfile) {
		t.Fatalf("packet metrics incomplete: %+v (output bytes %d)", profile.Packet, len(withProfile))
	}
	if profile.Packet.Counts.SemanticSymbols == 0 || profile.Packet.Counts.Facts == 0 {
		t.Fatalf("packet counts missing semantic/fact outputs: %+v", profile.Packet.Counts)
	}
	if profile.History.RawFallback.QueryCount != 8 || len(profile.History.RawFallback.Queries) != 8 {
		t.Fatalf("raw fallback did not record eight scans: %+v", profile.History.RawFallback)
	}
	assertRawHistoryProfileAggregates(t, profile.History.RawFallback)

	var shape map[string]json.RawMessage
	if err := json.Unmarshal(data, &shape); err != nil {
		t.Fatalf("parse profile shape: %v", err)
	}
	for _, field := range []string{"schema_version", "duration_unit", "total_brief", "status_build_state", "semantic", "history", "facts", "synthesis", "knowledge", "packet"} {
		if _, ok := shape[field]; !ok {
			t.Errorf("profile missing top-level field %q", field)
		}
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	assertProfileObjectFields(t, object, "semantic", "context", "runtime_traces", "tests")
	assertProfileObjectFields(t, object, "history", "index_load", "indexed_rank", "raw_fallback")
	assertProfileObjectFields(t, object, "facts", "load", "vector_cache_load", "embed", "rank", "cache_flush")
	assertProfileObjectFields(t, object, "synthesis", "likely_files", "action_checklist")
	assertProfileObjectFields(t, object, "knowledge", "patterns", "consolidations", "themes")
	assertProfileObjectFields(t, object, "packet", "format", "serialization", "byte_count", "counts")
	assertProfileObjectFields(t, object, "status_build_state", "invoked", "duration_ns", "input_count", "output_count", "error_count")
	semanticObject := object["semantic"].(map[string]any)
	assertProfileObjectFields(t, semanticObject, "context", "invoked", "duration_ns", "input_count", "output_count", "error_count")
	historyObject := object["history"].(map[string]any)
	assertProfileObjectFields(t, historyObject, "raw_fallback", "invoked", "duration_ns", "query_count", "scanned_file_count", "scanned_byte_count", "match_count", "truncation_count", "error_count", "queries")
	factsObject := object["facts"].(map[string]any)
	assertProfileObjectFields(t, factsObject, "vector_cache_load", "invoked", "duration_ns", "input_count", "output_count", "error_count")
	assertProfileObjectFields(t, factsObject, "embed", "invoked", "duration_ns", "query_call_count", "fact_call_count", "valid_vector_count", "invalid_vector_count")
}

func TestBrainBriefProfilePreservesTextPacket(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	task := "ValidateToken PRIVATE_TEXT_PACKET_TASK"
	withoutProfile, err := execute(t, NewRootCommand(fixture.opts), "brief", task)
	if err != nil {
		t.Fatalf("text brief without profile: %v\n%s", err, withoutProfile)
	}
	profilePath := filepath.Join(t.TempDir(), "text-profile.json")
	withProfile, err := execute(t, NewRootCommand(fixture.opts), "brief", task, "--profile-json", profilePath)
	if err != nil {
		t.Fatalf("text brief with profile: %v\n%s", err, withProfile)
	}
	if withProfile != withoutProfile {
		t.Fatalf("profiling changed text packet bytes\nwithout (%d):\n%s\nwith (%d):\n%s", len(withoutProfile), withoutProfile, len(withProfile), withProfile)
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	var profile brainBriefProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}
	if profile.Packet.Format != "text" || profile.Packet.ByteCount != len(withProfile) {
		t.Fatalf("text packet profile mismatch: %+v", profile.Packet)
	}
	if profile.Packet.Counts.SemanticRelations != 0 || profile.Packet.Counts.SemanticNeighbors != 0 || profile.Packet.Counts.TestRoots != 0 || profile.Packet.Counts.Patterns != 0 || profile.Packet.Counts.GuidanceItems != 0 {
		t.Fatalf("text profile counted JSON-only records: %+v", profile.Packet.Counts)
	}
	if strings.Contains(string(data), task) || strings.Contains(string(data), fixture.repoDir) {
		t.Fatalf("text packet profile leaked private content:\n%s", data)
	}
}

func TestBrainBriefRawHistoryProfileAggregatesAndCountsTruncation(t *testing.T) {
	brainDir := t.TempDir()
	sessionDir := filepath.Join(brainDir, "sessions", "main")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	task := "ALPHA_ONE BETA_TWO GAMMA_THREE DELTA_FOUR EPSILON_FIVE ZETA_SIX ETA_SEVEN THETA_EIGHT IOTA_NINE"
	queries := brainBriefRawHistoryQueries(task)
	if len(queries) != 8 {
		t.Fatalf("queries = %+v, want eight", queries)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "history.jsonl"), []byte("unrelated payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	brokenLink := filepath.Join(sessionDir, "broken.txt")
	brokenLinkCreated := os.Symlink(filepath.Join(sessionDir, "missing-target"), brokenLink) == nil

	profile := brainBriefProfileRawHistory{Queries: make([]brainBriefProfileRawHistoryQuery, 0)}
	matches, err := brainBriefRawHistoryMatchesObserved(brainDir, task, nil, 8, &profile)
	if err != nil {
		t.Fatalf("raw matches: %v", err)
	}
	if len(matches) != 0 || profile.QueryCount != 8 || len(profile.Queries) != 8 {
		t.Fatalf("unexpected raw aggregate: matches=%d profile=%+v", len(matches), profile)
	}
	if profile.ScannedFileCount < 8 || profile.ScannedByteCount <= 0 {
		t.Fatalf("raw scan counts missing: %+v", profile)
	}
	if brokenLinkCreated && profile.ErrorCount != 8 {
		t.Fatalf("broken input error count = %d, want one per query", profile.ErrorCount)
	}
	assertRawHistoryProfileAggregates(t, profile)

	if err := os.WriteFile(filepath.Join(sessionDir, "history.jsonl"), []byte(queries[0]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	truncated := brainBriefProfileRawHistory{Queries: make([]brainBriefProfileRawHistoryQuery, 0)}
	matches, err = brainBriefRawHistoryMatchesObserved(brainDir, task, nil, 1, &truncated)
	if err != nil {
		t.Fatalf("truncated raw matches: %v", err)
	}
	if len(matches) != 1 || truncated.QueryCount != 1 || truncated.TruncationCount != 1 || !truncated.Queries[0].Truncated {
		t.Fatalf("truncation not recorded: matches=%d profile=%+v", len(matches), truncated)
	}
}

func TestBrainBriefProfileWriteFailureIsAtomicAndFailClosed(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	parent := t.TempDir()
	profilePath := filepath.Join(parent, "profile-target")
	if err := os.Mkdir(profilePath, 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := execute(t, NewRootCommand(fixture.opts), "brief", "PRIVATE_FAIL_CLOSED_TASK", "--json", "--profile-json", profilePath)
	if err == nil {
		t.Fatalf("brief succeeded despite unwritable profile target:\n%s", out)
	}
	if strings.Contains(out, `"task": "PRIVATE_FAIL_CLOSED_TASK"`) {
		t.Fatalf("successful packet escaped before profile commit:\n%s", out)
	}
	if info, statErr := os.Stat(profilePath); statErr != nil || !info.IsDir() {
		t.Fatalf("failed atomic write replaced target directory: info=%v err=%v", info, statErr)
	}
	temps, globErr := filepath.Glob(filepath.Join(parent, ".profile-target.tmp-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(temps) != 0 {
		t.Fatalf("failed atomic write left temporary files: %+v", temps)
	}
}

func TestBrainBriefProfileRejectsHandoffWithoutWriting(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "handoff-profile.json")
	out, err := execute(t, NewRootCommand(Options{Version: "test"}), "brief", "--handoff", "--profile-json", profilePath)
	if err == nil || !strings.Contains(err.Error(), "only supported for task briefs") {
		t.Fatalf("handoff profiling error = %v\n%s", err, out)
	}
	if _, statErr := os.Stat(profilePath); !os.IsNotExist(statErr) {
		t.Fatalf("rejected handoff wrote profile: %v", statErr)
	}
}

func TestBrainBriefProfilingEmbedderRecordsCountsWithoutText(t *testing.T) {
	metrics := brainBriefProfileEmbedding{}
	embedder := &brainBriefProfilingEmbedder{Embedder: briefProfileTestEmbedder{}, profile: &metrics}
	embedder.Embed("PRIVATE_DOCUMENT_TEXT")
	embedder.EmbedQuery("PRIVATE_QUERY_TEXT")
	if !metrics.Invoked || metrics.FactCallCount != 1 || metrics.QueryCallCount != 1 || metrics.ValidVectorCount != 2 || metrics.InvalidCount != 0 {
		t.Fatalf("unexpected embed metrics: %+v", metrics)
	}
	data, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE") {
		t.Fatalf("embed profile retained input text: %s", data)
	}
}

type briefProfileTestEmbedder struct{}

func (briefProfileTestEmbedder) Embed(string) []float32 { return []float32{1, 0} }
func (briefProfileTestEmbedder) Dim() int               { return 2 }
func (briefProfileTestEmbedder) ID() string             { return "test" }

func assertRawHistoryProfileAggregates(t *testing.T, profile brainBriefProfileRawHistory) {
	t.Helper()
	var files, matches, truncations, errors int
	var bytes int64
	for i, query := range profile.Queries {
		if query.Ordinal != i+1 || query.DurationNS < 0 {
			t.Errorf("bad per-query ordinal/duration at %d: %+v", i, query)
		}
		files += query.ScannedFileCount
		bytes += query.ScannedByteCount
		matches += query.MatchCount
		if query.Truncated {
			truncations++
		}
		errors += query.ErrorCount
	}
	if profile.QueryCount != len(profile.Queries) || profile.ScannedFileCount != files || profile.ScannedByteCount != bytes || profile.MatchCount != matches || profile.TruncationCount != truncations || profile.ErrorCount != errors {
		t.Fatalf("raw aggregate does not equal per-query sums: aggregate=%+v sums={queries:%d files:%d bytes:%d matches:%d truncations:%d errors:%d}", profile, len(profile.Queries), files, bytes, matches, truncations, errors)
	}
	if strings.Contains(fmt.Sprintf("%+v", profile), "ALPHA_ONE") {
		t.Fatalf("raw profile retained query text: %+v", profile)
	}
}

func assertProfileObjectFields(t *testing.T, parent map[string]any, objectName string, fields ...string) {
	t.Helper()
	value, ok := parent[objectName]
	if !ok {
		t.Fatalf("profile object %q is missing", objectName)
	}
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("profile field %q is %T, want object", objectName, value)
	}
	for _, field := range fields {
		if _, ok := object[field]; !ok {
			t.Errorf("profile object %q missing field %q", objectName, field)
		}
	}
}
