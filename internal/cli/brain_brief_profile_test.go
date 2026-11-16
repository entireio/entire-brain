package cli

import (
	"encoding/json"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
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
	sessionDir := filepath.Join(storage.BrainDir, brainstore.SessionsDirName, "main")
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
		Origin:    factstore.OriginDistilled,
		Status:    factStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := factstore.Write(storage.BrainDir, []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	if err := updateFactSourceManifest(storage.BrainDir, now); err != nil {
		t.Fatalf("update fact source manifest: %v", err)
	}

	runner.responses[fakeCommandKey("git", "status", "--porcelain")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "status", "--porcelain", "--untracked-files=all")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff-index", "-M", "--shortstat", "HEAD")] = fakeCommandResponse{}
	runner.responses[fakeCommandKey("git", "diff", "--name-status", "-M", "-C", "HEAD")] = fakeCommandResponse{}
	return brainBriefProfileFixture{repoDir: repoDir, brainDir: storage.BrainDir, opts: opts}
}

func TestBrainBriefProfilePreservesPacketAndWritesCompletePrivateSidecar(t *testing.T) {
	fixture := newBrainBriefProfileFixture(t)
	profilePath := filepath.Join(t.TempDir(), "brief-profile.json")
	task := "ValidateToken PRIVATE_TASK_PAYLOAD PRIVATE_FACT_PAYLOAD ALPHA_ONE BETA_TWO GAMMA_THREE DELTA_FOUR EPSILON_FIVE ZETA_SIX ETA_SEVEN THETA_EIGHT IOTA_NINE"

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
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(data, &shape); err != nil {
		t.Fatalf("parse profile shape: %v", err)
	}
	for _, field := range []string{"schema_version", "duration_unit", "total_brief", "status_build_state", "semantic", "facts", "synthesis", "knowledge", "packet"} {
		if _, ok := shape[field]; !ok {
			t.Errorf("profile missing top-level field %q", field)
		}
	}
	if _, ok := shape["history"]; ok {
		t.Errorf("profile still has a history stage; the brief no longer searches history")
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	assertProfileObjectFields(t, object, "semantic", "context", "runtime_traces", "tests")
	assertProfileObjectFields(t, object, "facts", "load", "rank", "cache_flush")
	assertProfileObjectFields(t, object, "synthesis", "likely_files", "action_checklist")
	assertProfileObjectFields(t, object, "knowledge", "patterns", "consolidations", "themes")
	assertProfileObjectFields(t, object, "packet", "format", "serialization", "byte_count", "counts")
	assertProfileObjectFields(t, object, "status_build_state", "invoked", "duration_ns", "input_count", "output_count", "error_count")
	semanticObject := object["semantic"].(map[string]any)
	assertProfileObjectFields(t, semanticObject, "context", "invoked", "duration_ns", "input_count", "output_count", "error_count")
	factsObject := object["facts"].(map[string]any)
	assertProfileObjectFields(t, factsObject, "load", "invoked", "duration_ns", "input_count", "output_count", "error_count")
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
