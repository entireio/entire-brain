package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// executeSplit runs a command with stdout and stderr captured separately, so
// tests can assert both a parseable stdout contract and the bounded stderr
// diagnostic at once (the shared execute helper merges the two streams).
func executeSplit(t *testing.T, cmd *cobra.Command, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errBuf.String(), err
}

func vitalityTestFact(text, branch string, now time.Time) factRecord {
	paths := normalizeFactPaths([]string{"architecture.data.flow"})
	return factRecord{
		ID:        factRecordID(text, paths),
		Paths:     paths,
		Text:      text,
		Branch:    branch,
		Origin:    factOriginAuthored,
		Status:    factStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func vitalityViewOrFatal(t *testing.T, brainDir, branch string) vitalityRollup {
	t.Helper()
	rollup, _, err := loadVitalityView(brainDir, branch)
	if err != nil {
		t.Fatalf("load vitality view: %v", err)
	}
	return rollup
}

func vitalitySidecarBytes(t *testing.T, brainDir, branch string) []byte {
	t.Helper()
	var all []byte
	for _, rel := range []string{factsVitalityLogRelPath(branch), factsVitalityRollupRelPath(branch)} {
		data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(rel)))
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read %s: %v", rel, err)
		}
		all = append(all, data...)
	}
	return all
}

func TestVitalityRecallRecordsServeReceipts(t *testing.T) {
	f := newVerifyFixture(t)
	fact := vitalityTestFact("The retrieval cache invalidates on manifest change.", "main", f.now)
	if err := writeFacts(f.brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}

	query := "retrieval cache invalidates"
	out, err := execute(t, NewRootCommand(f.opts), "recall", query, "--json")
	if err != nil {
		t.Fatalf("recall: %v\n%s", err, out)
	}
	if !strings.Contains(out, fact.ID) {
		t.Fatalf("recall did not serve the fact:\n%s", out)
	}

	vout, err := execute(t, NewRootCommand(f.opts), "facts", "vitality", "--json")
	if err != nil {
		t.Fatalf("facts vitality: %v\n%s", err, vout)
	}
	var report factsVitalityReport
	if err := json.Unmarshal([]byte(vout), &report); err != nil {
		t.Fatalf("parse vitality json: %v\n%s", err, vout)
	}
	if report.Branch != "main" || len(report.Facts) != 1 {
		t.Fatalf("unexpected vitality report: %+v", report)
	}
	entry := report.Facts[0]
	if entry.ID != fact.ID || entry.Served != 1 || entry.LastSurface != "recall" {
		t.Fatalf("entry = %+v", entry)
	}
	if !entry.LastServed.Equal(f.now) {
		t.Fatalf("last_served = %s, want %s", entry.LastServed, f.now)
	}
	if entry.LastHead != "abc123abc123abc123abc123abc123abc123abcd" {
		t.Fatalf("last_head = %q", entry.LastHead)
	}
	if entry.Missing || entry.Status != factStatusActive || entry.Text != fact.Text {
		t.Fatalf("store join incomplete: %+v", entry)
	}
	wantHash := vitalityTaskHash(query)
	if entry.LastTaskSHA != wantHash || !strings.HasPrefix(entry.LastTaskSHA, "sha256:") {
		t.Fatalf("task hash = %q, want %q", entry.LastTaskSHA, wantHash)
	}

	// Privacy: the sidecar files must carry the hash, never the query text.
	sidecar := string(vitalitySidecarBytes(t, f.brainDir, "main"))
	if strings.Contains(sidecar, query) {
		t.Fatalf("raw query text persisted in vitality sidecar:\n%s", sidecar)
	}
	if !strings.Contains(sidecar, wantHash) {
		t.Fatalf("task hash missing from vitality sidecar:\n%s", sidecar)
	}
}

func TestVitalityRetrieveGetAndStatusSurfaces(t *testing.T) {
	f := newVerifyFixture(t)
	fact := vitalityTestFact("Unified retrieval merges facts history docs with RRF.", "main", f.now)
	if err := writeFacts(f.brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}

	if out, err := execute(t, NewRootCommand(f.opts), "search", "unified retrieval merges", "--json"); err != nil {
		t.Fatalf("search: %v\n%s", err, out)
	}
	if out, err := execute(t, NewRootCommand(f.opts), "get", fact.ID, "--json"); err != nil {
		t.Fatalf("get: %v\n%s", err, out)
	}
	if out, err := execute(t, NewRootCommand(f.opts), "multi-get", fact.ID, "fact:absent", "--json"); err != nil {
		t.Fatalf("multi-get: %v\n%s", err, out)
	}

	rollup := vitalityViewOrFatal(t, f.brainDir, "main")
	entry := rollup.Facts[fact.ID]
	if entry == nil {
		t.Fatalf("no receipts for %s: %+v", fact.ID, rollup.Facts)
	}
	if entry.Served != 3 {
		t.Fatalf("served = %d, want 3", entry.Served)
	}
	for surface, want := range map[string]int{"search": 1, "get": 1, "multi-get": 1} {
		if entry.Surfaces[surface] != want {
			t.Fatalf("surfaces = %+v, want %s=%d", entry.Surfaces, surface, want)
		}
	}
	// Gets are id-addressed: no task text, so no hash.
	if entry.LastTaskSHA != "" {
		t.Fatalf("get should not record a task hash, got %q", entry.LastTaskSHA)
	}

	out, err := execute(t, NewRootCommand(f.opts), "facts", "status", "--json")
	if err != nil {
		t.Fatalf("facts status: %v\n%s", err, out)
	}
	var status factsStatusReport
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("parse status json: %v\n%s", err, out)
	}
	if status.Vitality == nil {
		t.Fatalf("status missing vitality block:\n%s", out)
	}
	if status.Vitality.ServedFacts != 1 || status.Vitality.ServedTotal != 3 {
		t.Fatalf("status vitality = %+v", status.Vitality)
	}
	if status.Vitality.LastServedFact != fact.ID || status.Vitality.LastServedAt == nil {
		t.Fatalf("status last-served evidence = %+v", status.Vitality)
	}
}

func TestVitalityBriefRecordsServeReceipts(t *testing.T) {
	f := newVerifyFixture(t)
	fact := vitalityTestFact("Verification requires the media playback capstone suite.", "main", f.now)
	if err := writeFacts(f.brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	// brief consults facts only when the manifest declares the source.
	if err := updateFactSourceManifest(f.brainDir, f.now); err != nil {
		t.Fatalf("update manifest: %v", err)
	}

	out, err := execute(t, NewRootCommand(f.opts), "brief", "media playback verification capstone", "--json")
	if err != nil {
		t.Fatalf("brief: %v\n%s", err, out)
	}
	var report brainBriefReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse brief json: %v\n%s", err, out)
	}
	if len(report.Facts) != 1 || report.Facts[0].ID != fact.ID {
		t.Fatalf("brief did not serve the fact: %+v", report.Facts)
	}

	rollup := vitalityViewOrFatal(t, f.brainDir, "main")
	entry := rollup.Facts[fact.ID]
	if entry == nil || entry.Served != 1 || entry.LastSurface != "brief" {
		t.Fatalf("brief receipt = %+v", entry)
	}
	if entry.LastTaskSHA != vitalityTaskHash("media playback verification capstone") {
		t.Fatalf("brief task hash = %q", entry.LastTaskSHA)
	}
}

func TestVitalityHookSurfacesRecordReceipts(t *testing.T) {
	f := newVerifyFixture(t)
	fact := vitalityTestFact("internal/cli/distill_cmd.go owns the distill flag surface.", "main", f.now)
	if err := writeFacts(f.brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}

	out, err := execute(t, NewRootCommand(f.opts), "hook", "pre-edit", "--file", "internal/cli/distill_cmd.go")
	if err != nil {
		t.Fatalf("hook pre-edit: %v\n%s", err, out)
	}
	if !strings.Contains(out, "distill flag surface") {
		t.Fatalf("hook pre-edit did not emit the fact:\n%s", out)
	}
	rollup := vitalityViewOrFatal(t, f.brainDir, "main")
	entry := rollup.Facts[fact.ID]
	if entry == nil || entry.Served != 1 || entry.LastSurface != "hook-pre-edit" {
		t.Fatalf("hook receipt = %+v", entry)
	}
	// The trigger (a file path) is hashed like any other task text.
	if entry.LastTaskSHA != vitalityTaskHash("internal/cli/distill_cmd.go") {
		t.Fatalf("hook task hash = %q", entry.LastTaskSHA)
	}

	// A silent emission (no matching facts) leaves no receipt.
	out, err = execute(t, NewRootCommand(f.opts), "hook", "pre-edit", "--file", "webui/unrelated_component.tsx")
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("silent hook should stay silent, got %q (err %v)", out, err)
	}
	rollup = vitalityViewOrFatal(t, f.brainDir, "main")
	if rollup.Facts[fact.ID].Served != 1 {
		t.Fatalf("silent hook must not add receipts: %+v", rollup.Facts[fact.ID])
	}
}

func TestVitalityMCPSurfacesRecordReceipts(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	opts := Options{Version: "test-version", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths((&cobra.Command{}).Context(), runner, env, repoDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	branch := "feature/mcp"
	fact := vitalityTestFact("qmd retrieval alpha contract", branch, now)
	if err := writeFacts(storage.BrainDir, branch, []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}

	input := frameMCP(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_search","arguments":{"query":"qmd retrieval alpha","limit":1,"branch":%q}}}`, branch)) +
		frameMCP(fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"brain_get","arguments":{"id":%q,"branch":%q}}}`, fact.ID, branch))
	var out bytes.Buffer
	if err := runMCP((&cobra.Command{}).Context(), strings.NewReader(input), &out, opts); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	for i, response := range readMCPResponses(t, out.String()) {
		if response["error"] != nil {
			t.Fatalf("response %d returned error: %+v", i+1, response)
		}
	}

	rollup := vitalityViewOrFatal(t, storage.BrainDir, branch)
	entry := rollup.Facts[fact.ID]
	if entry == nil || entry.Served != 2 {
		t.Fatalf("mcp receipts = %+v", entry)
	}
	if entry.Surfaces["mcp:brain_search"] != 1 || entry.Surfaces["mcp:brain_get"] != 1 {
		t.Fatalf("mcp surfaces = %+v", entry.Surfaces)
	}
	if entry.LastHead != "aaa111" {
		t.Fatalf("mcp head = %q", entry.LastHead)
	}
	sidecar := string(vitalitySidecarBytes(t, storage.BrainDir, branch))
	if strings.Contains(sidecar, "qmd retrieval alpha\"") || strings.Contains(sidecar, `"task":"`) {
		t.Fatalf("raw query text persisted:\n%s", sidecar)
	}
}

func TestVitalityConcurrentAppendsAreLossless(t *testing.T) {
	brainDir := t.TempDir()
	branch := "main"
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	const workers = 32

	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			events := []vitalityEvent{
				{Type: vitalityEventServed, FactID: "fact:shared", At: now.Add(time.Duration(i) * time.Second), Surface: "recall", Branch: branch},
				{Type: vitalityEventServed, FactID: fmt.Sprintf("fact:unique-%02d", i), At: now, Surface: "query", Branch: branch},
			}
			errs <- appendVitalityEvents(brainDir, branch, events)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent append: %v", err)
		}
	}

	rollup, stats, err := compactVitality(brainDir, branch)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if stats.Malformed != 0 {
		t.Fatalf("interleaved appends corrupted the log: %+v", stats)
	}
	if got := rollup.Facts["fact:shared"]; got == nil || got.Served != workers {
		t.Fatalf("shared fact receipts = %+v, want %d", got, workers)
	}
	total := 0
	for _, entry := range rollup.Facts {
		total += entry.Served
	}
	if total != workers*2 || len(rollup.Facts) != workers+1 {
		t.Fatalf("lost receipts: total=%d facts=%d", total, len(rollup.Facts))
	}
	if got := rollup.Facts["fact:shared"].LastServed; !got.Equal(now.Add(time.Duration(workers-1) * time.Second)) {
		t.Fatalf("last_served = %s", got)
	}
}

func TestVitalityCompactionIsIdempotentAndCrashSafe(t *testing.T) {
	brainDir := t.TempDir()
	branch := "main"
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	events := []vitalityEvent{
		{Type: vitalityEventServed, FactID: "fact:a", At: now, Surface: "recall", Branch: branch},
		{Type: vitalityEventServed, FactID: "fact:a", At: now.Add(time.Minute), Surface: "query", Branch: branch},
		{Type: vitalityEventServed, FactID: "fact:b", At: now, Surface: "brief", Branch: branch},
	}
	if err := appendVitalityEvents(brainDir, branch, events); err != nil {
		t.Fatalf("append: %v", err)
	}
	logPath := filepath.Join(brainDir, filepath.FromSlash(factsVitalityLogRelPath(branch)))
	logBefore, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	if _, _, err := compactVitality(brainDir, branch); err != nil {
		t.Fatalf("compact: %v", err)
	}
	rollupPath := filepath.Join(brainDir, filepath.FromSlash(factsVitalityRollupRelPath(branch)))
	first, err := os.ReadFile(rollupPath)
	if err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	if info, err := os.Stat(logPath); err != nil || info.Size() != 0 {
		t.Fatalf("log not truncated after compaction: %v (size %d)", err, info.Size())
	}

	// Idempotence: recompacting with nothing new is a byte-identical no-op.
	if _, _, err := compactVitality(brainDir, branch); err != nil {
		t.Fatalf("recompact: %v", err)
	}
	second, err := os.ReadFile(rollupPath)
	if err != nil {
		t.Fatalf("reread rollup: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("recompaction changed the rollup:\n%s\nvs\n%s", first, second)
	}
	var afterFirst vitalityRollup
	if err := json.Unmarshal(first, &afterFirst); err != nil {
		t.Fatalf("parse rollup: %v", err)
	}
	if afterFirst.Facts["fact:a"].Served != 2 || afterFirst.Facts["fact:b"].Served != 1 {
		t.Fatalf("rollup counts = %+v", afterFirst.Facts)
	}
	if afterFirst.Facts["fact:a"].LastSurface != "query" {
		t.Fatalf("last surface = %q", afterFirst.Facts["fact:a"].LastSurface)
	}

	// Crash safety: reconstruct the state after phase 1 of compaction (rollup
	// written with the absorbed-log marker, truncation never happened) and
	// verify recompaction does not double-count the log.
	crashed := afterFirst
	crashed.Log = vitalityLogMarker{AbsorbedBytes: int64(len(logBefore)), AbsorbedSHA256: vitalityContentSHA(logBefore)}
	if err := writeVitalityRollup(brainDir, branch, crashed); err != nil {
		t.Fatalf("write crashed rollup: %v", err)
	}
	if err := appendBrainRelativeFile(brainDir, factsVitalityLogRelPath(branch), logBefore); err != nil {
		t.Fatalf("restore log: %v", err)
	}
	recovered, stats, err := compactVitality(brainDir, branch)
	if err != nil {
		t.Fatalf("recover compact: %v", err)
	}
	if stats.NewEvents != 0 {
		t.Fatalf("already-absorbed events re-counted: %+v", stats)
	}
	if recovered.Facts["fact:a"].Served != 2 || recovered.Facts["fact:b"].Served != 1 {
		t.Fatalf("double-counted after crash recovery: %+v", recovered.Facts)
	}
	if info, err := os.Stat(logPath); err != nil || info.Size() != 0 {
		t.Fatalf("log not truncated after recovery: %v", err)
	}
	final, err := os.ReadFile(rollupPath)
	if err != nil {
		t.Fatalf("read recovered rollup: %v", err)
	}
	if !bytes.Equal(first, final) {
		t.Fatalf("crash recovery changed the rollup:\n%s\nvs\n%s", first, final)
	}
}

func TestVitalityCorruptLogAndRollupDegrade(t *testing.T) {
	f := newVerifyFixture(t)
	fact := vitalityTestFact("The corrupt sidecar must not break recall.", "main", f.now)
	if err := writeFacts(f.brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}

	// Corrupt log line + corrupt rollup: reads keep working, receipts keep
	// appending, and the inspection surface reports the damage.
	if err := appendBrainRelativeFile(f.brainDir, factsVitalityLogRelPath("main"), []byte("{not json\n")); err != nil {
		t.Fatalf("seed corrupt log: %v", err)
	}
	if err := writeBrainRelativeFileAtomic(f.brainDir, factsVitalityRollupRelPath("main"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatalf("seed corrupt rollup: %v", err)
	}

	stdout, stderr, err := executeSplit(t, NewRootCommand(f.opts), "recall", "corrupt sidecar recall", "--json")
	if err != nil {
		t.Fatalf("recall with corrupt sidecar: %v\n%s", err, stdout)
	}
	var recallOut struct {
		Facts []factRecord `json:"facts"`
	}
	if err := json.Unmarshal([]byte(stdout), &recallOut); err != nil {
		t.Fatalf("recall stdout not clean JSON: %v\n%s", err, stdout)
	}
	if len(recallOut.Facts) != 1 || recallOut.Facts[0].ID != fact.ID {
		t.Fatalf("recall result changed by corrupt sidecar: %+v", recallOut.Facts)
	}
	if strings.Contains(stderr, "vitality") {
		// A corrupt log/rollup does not block appends, so no diagnostic fires.
		t.Fatalf("append path should tolerate corrupt sidecar silently, stderr=%q", stderr)
	}

	out, err := execute(t, NewRootCommand(f.opts), "facts", "vitality", "--json")
	if err != nil {
		t.Fatalf("facts vitality: %v\n%s", err, out)
	}
	var report factsVitalityReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse vitality json: %v\n%s", err, out)
	}
	if report.Log.MalformedLines != 1 || report.Log.PendingEvents != 1 {
		t.Fatalf("log state = %+v", report.Log)
	}
	if len(report.Facts) != 1 || report.Facts[0].ID != fact.ID || report.Facts[0].Served != 1 {
		t.Fatalf("valid receipt lost next to corruption: %+v", report.Facts)
	}
	if joined := strings.Join(report.Warnings, "\n"); !strings.Contains(joined, "rollup is corrupt") {
		t.Fatalf("warnings missing corrupt-rollup notice: %+v", report.Warnings)
	}

	// --compact self-heals: the rollup is rebuilt from the surviving log.
	out, err = execute(t, NewRootCommand(f.opts), "facts", "vitality", "--compact", "--json")
	if err != nil {
		t.Fatalf("facts vitality --compact: %v\n%s", err, out)
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse compacted vitality json: %v\n%s", err, out)
	}
	if joined := strings.Join(report.Warnings, "\n"); !strings.Contains(joined, "rebuilt") {
		t.Fatalf("compaction should report the rebuild: %+v", report.Warnings)
	}
	if len(report.Facts) != 1 || report.Facts[0].Served != 1 || report.Log.PendingEvents != 0 {
		t.Fatalf("rebuild lost the surviving receipt: %+v", report)
	}
	rollup, stats, err := loadVitalityView(f.brainDir, "main")
	if err != nil || stats.RollupCorrupt {
		t.Fatalf("rollup not healed: %+v (err %v)", stats, err)
	}
	if rollup.Facts[fact.ID].Served != 1 {
		t.Fatalf("healed rollup = %+v", rollup.Facts)
	}
}

func TestVitalityUnwritableSidecarPreservesReads(t *testing.T) {
	f := newVerifyFixture(t)
	fact := vitalityTestFact("Reads survive an unwritable vitality sidecar.", "main", f.now)
	if err := writeFacts(f.brainDir, "main", []factRecord{fact}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	// A directory where the log should be makes every append fail.
	logPath := filepath.Join(f.brainDir, filepath.FromSlash(factsVitalityLogRelPath("main")))
	if err := os.MkdirAll(logPath, 0o700); err != nil {
		t.Fatalf("block log path: %v", err)
	}

	stdout, stderr, err := executeSplit(t, NewRootCommand(f.opts), "recall", "unwritable vitality sidecar", "--json")
	if err != nil {
		t.Fatalf("recall must not fail when receipts cannot be written: %v\n%s", err, stdout)
	}
	var recallOut struct {
		Facts []factRecord `json:"facts"`
	}
	if err := json.Unmarshal([]byte(stdout), &recallOut); err != nil {
		t.Fatalf("recall stdout not clean JSON: %v\n%s", err, stdout)
	}
	if len(recallOut.Facts) != 1 || recallOut.Facts[0].ID != fact.ID {
		t.Fatalf("recall result changed: %+v", recallOut.Facts)
	}
	if got := strings.Count(stderr, "facts vitality recording degraded"); got != 1 {
		t.Fatalf("want exactly one bounded diagnostic, got %d:\n%s", got, stderr)
	}
}

func TestVitalityMissingFactReceiptsAreFlagged(t *testing.T) {
	f := newVerifyFixture(t)
	kept := vitalityTestFact("This fact still exists.", "main", f.now)
	if err := writeFacts(f.brainDir, "main", []factRecord{kept}); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	events := []vitalityEvent{
		{Type: vitalityEventServed, FactID: kept.ID, At: f.now, Surface: "recall", Branch: "main"},
		{Type: vitalityEventServed, FactID: "fact:departed", At: f.now.Add(time.Minute), Surface: "query", Branch: "main"},
	}
	if err := appendVitalityEvents(f.brainDir, "main", events); err != nil {
		t.Fatalf("append: %v", err)
	}

	out, err := execute(t, NewRootCommand(f.opts), "facts", "vitality", "--json")
	if err != nil {
		t.Fatalf("facts vitality: %v\n%s", err, out)
	}
	var report factsVitalityReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse vitality json: %v\n%s", err, out)
	}
	if len(report.Facts) != 2 {
		t.Fatalf("facts = %+v", report.Facts)
	}
	byID := map[string]factsVitalityEntry{}
	for _, entry := range report.Facts {
		byID[entry.ID] = entry
	}
	if entry := byID["fact:departed"]; !entry.Missing || entry.Text != "" {
		t.Fatalf("departed fact not flagged: %+v", entry)
	}
	if entry := byID[kept.ID]; entry.Missing || entry.Text != kept.Text {
		t.Fatalf("present fact misflagged: %+v", entry)
	}

	// A fact with no receipts reports cleanly instead of erroring.
	out, err = execute(t, NewRootCommand(f.opts), "facts", "vitality", "--fact", "fact:never-served")
	if err != nil {
		t.Fatalf("facts vitality --fact: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no serve receipts for fact:never-served") {
		t.Fatalf("missing-fact message absent:\n%s", out)
	}
}

func TestVitalityRollupEvictionIsDeterministic(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	facts := map[string]*vitalityFactRollup{
		"fact:a": {Served: 1, LastServed: now.Add(1 * time.Minute)},
		"fact:b": {Served: 1, LastServed: now.Add(2 * time.Minute)},
		"fact:c": {Served: 1, LastServed: now},                      // oldest: evicted first
		"fact:d": {Served: 1, LastServed: now.Add(1 * time.Minute)}, // ties with a: id breaks the tie
		"fact:e": {Served: 1, LastServed: now.Add(3 * time.Minute)},
	}
	evictVitalityOverflow(facts, 3)
	if len(facts) != 3 {
		t.Fatalf("evicted to %d entries", len(facts))
	}
	for _, want := range []string{"fact:b", "fact:d", "fact:e"} {
		if facts[want] == nil {
			t.Fatalf("wrongly evicted %s: %+v", want, facts)
		}
	}
}

func TestVitalityAppendCompactsWhenLogExceedsCap(t *testing.T) {
	brainDir := t.TempDir()
	branch := "main"
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		event := vitalityEvent{Type: vitalityEventServed, FactID: "fact:capped", At: now.Add(time.Duration(i) * time.Second), Surface: "recall", Branch: branch}
		if err := appendVitalityEventsBounded(brainDir, branch, []vitalityEvent{event}, 1); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		info, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(factsVitalityLogRelPath(branch))))
		if err != nil || info.Size() != 0 {
			t.Fatalf("append %d left the log above the cap: %v (size %d)", i, err, info.Size())
		}
	}
	rollup := vitalityViewOrFatal(t, brainDir, branch)
	if got := rollup.Facts["fact:capped"]; got == nil || got.Served != 3 {
		t.Fatalf("inline compaction lost receipts: %+v", got)
	}
}

func TestVitalityAppendRefusesWhenLogCannotCompact(t *testing.T) {
	brainDir := t.TempDir()
	branch := "main"
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	// A rollup path blocked by a directory makes every compaction fail, so an
	// oversized log must stop accepting appends instead of growing forever.
	if err := os.MkdirAll(filepath.Join(brainDir, filepath.FromSlash(factsVitalityRollupRelPath(branch))), 0o700); err != nil {
		t.Fatalf("block rollup path: %v", err)
	}
	event := vitalityEvent{Type: vitalityEventServed, FactID: "fact:x", At: now, Surface: "recall", Branch: branch}
	// First append succeeds, then fails to compact past the 1-byte cap.
	if err := appendVitalityEventsBounded(brainDir, branch, []vitalityEvent{event}, 1); err == nil {
		t.Fatalf("compaction with a blocked rollup should surface an error")
	}
	logPath := filepath.Join(brainDir, filepath.FromSlash(factsVitalityLogRelPath(branch)))
	before, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	// Beyond the 8x backstop the append itself is refused and the log stops growing.
	if err := appendVitalityEventsBounded(brainDir, branch, []vitalityEvent{event}, 1); err == nil ||
		!strings.Contains(err.Error(), "dropping receipts") {
		t.Fatalf("oversized uncompactable log must refuse appends, got %v", err)
	}
	after, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("log grew despite backstop: %d -> %d", before.Size(), after.Size())
	}
}
