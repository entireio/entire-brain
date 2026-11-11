package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/spf13/cobra"
)

func TestFinishMemoryOperationFailureEmitsReceiptOnlyInJSONMode(t *testing.T) {
	started := time.Date(2026, 8, 9, 20, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Second)
	cause := fmt.Errorf("%s: injected", brainstore.SourceStaleCode)

	for _, jsonOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "json"}[jsonOut], func(t *testing.T) {
			var output bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&output)
			receipt := newMemoryOperationReceipt("repair", started)
			receipt.Artifacts = []memoryReceiptArtifact{{Path: historystore.IndexPath, PriorState: "present", NewState: "present"}}

			err := finishMemoryOperationFailure(cmd, &receipt, jsonOut, finished, cause)
			if !errors.Is(err, cause) {
				t.Fatalf("returned error = %v, want original cause", err)
			}
			if receipt.FinishedAt != finished || receipt.ErrorCode != brainstore.SourceStaleCode {
				t.Fatalf("receipt = %+v", receipt)
			}
			if jsonOut {
				var decoded memoryOperationReceipt
				if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
					t.Fatalf("decode one JSON receipt: %v\n%s", err, output.String())
				}
				if decoded.OperationID != receipt.OperationID || decoded.FinishedAt != finished || decoded.ErrorCode != brainstore.SourceStaleCode {
					t.Fatalf("decoded receipt = %+v", decoded)
				}
				if strings.Count(output.String(), `"operation_id"`) != 1 {
					t.Fatalf("failure receipt emitted more than once: %s", output.String())
				}
				return
			}
			// Plain mode: the receipt is --json telemetry (COR-2205); the
			// returned error is the whole report and stdout stays silent.
			if output.Len() != 0 {
				t.Fatalf("plain failure wrote receipt telemetry = %q", output.String())
			}
		})
	}

}

func memoryAdminCommandFixture(t *testing.T) (Options, string) {
	t.Helper()
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := exportManifest{
		SchemaVersion: bmanifest.SchemaVersion,
		GeneratedAt:   time.Date(2026, 8, 9, 20, 0, 0, 0, time.UTC),
		RepoKey:       storage.Key,
		DefaultBranch: "main",
		Sources:       &brainSources{Sessions: &sessionSourceManifest{DefaultBranch: "main"}},
	}
	if err := writeBrainManifestAndReadme(storage.BrainDir, manifest); err != nil {
		t.Fatal(err)
	}
	// The commands built from these Options start a coordinator heartbeat that
	// samples this same clock from its own goroutine, so the counter is read and
	// written concurrently.
	var calls atomic.Int64
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time {
		return manifest.GeneratedAt.Add(time.Duration(calls.Add(1)) * time.Second)
	}}
	return opts, storage.BrainDir
}

func TestMemoryNotifyUsesResolvedRepoIdentityWhenKeyIsOmitted(t *testing.T) {
	opts, brainDir := memoryAdminCommandFixture(t)
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	oldLaunch := memoryWorkerLaunch
	memoryWorkerLaunch = func(string) error { return nil }
	t.Cleanup(func() { memoryWorkerLaunch = oldLaunch })

	out, err := execute(t, newMemoryNotifyCommand(opts), "--event", "session_start", "--session", "notify-session", "--json")
	if err != nil {
		t.Fatalf("notify without repo key: %v\n%s", err, out)
	}
	var payload struct {
		Recorded   string `json:"recorded"`
		SessionID  string `json:"session_id"`
		Generation int    `json:"generation"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil || payload.Recorded != "session_start" || payload.SessionID != "notify-session" || payload.Generation != 1 {
		t.Fatalf("notify payload=%+v err=%v\n%s", payload, err, out)
	}
	hints := loadMemoryHints(brainDir)
	if len(hints) != 1 || hints[0].RepoKey != manifest.RepoKey {
		t.Fatalf("resolved identity hint=%+v want repo=%q", hints, manifest.RepoKey)
	}

	mismatch := newMemoryNotifyCommand(opts)
	mismatch.SilenceErrors, mismatch.SilenceUsage = true, true
	out, err = execute(t, mismatch, "--event", "checkpoint", "--session", "notify-session", "--repo-key", "wrong/repository", "--json")
	if err == nil || !strings.Contains(err.Error(), "does not match the resolved repository") || out != "" {
		t.Fatalf("mismatched repo key: err=%v out=%q", err, out)
	}
	hints = loadMemoryHints(brainDir)
	if len(hints) != 1 || hints[0].Generation != 1 || hints[0].LastEvent != "session_start" {
		t.Fatalf("mismatched key mutated hints=%+v", hints)
	}
}

func addMemoryAdminConversationFixture(t *testing.T, brainDir string, now time.Time) string {
	t.Helper()
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	rel := "sessions/main/manual-receipt.jsonl"
	transcript := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"why did the parser reject the flag"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The strict parser rejected an unknown option."}]}}`,
	}, "\n") + "\n"
	path := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest.Sources = &brainSources{Sessions: &sessionSourceManifest{
		GeneratedAt: now, DefaultBranch: "main",
		Sessions: []exportSession{{SessionID: "manual-receipt", Branch: "main", Agent: "codex", TranscriptPath: rel, CreatedAt: now}},
	}}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBrainHistoryIndexAndSource(brainDir, now, nil); err != nil {
		t.Fatal(err)
	}
	return sessionViewForTest(t, brainDir).Ref
}

func sessionViewForTest(t *testing.T, brainDir string) conversationSessionView {
	t.Helper()
	manifest, err := bmanifest.Load(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := loadFreshHistory(brainDir, manifest.Sources.History)
	if err != nil {
		t.Fatal(err)
	}
	views := buildConversationSessionViews(fresh, manifest.RepoKey, manifest)
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	for _, view := range views {
		return view
	}
	panic("unreachable")
}

func TestMemoryAdminCommandsEmitFailureReceiptAfterCommittedMutation(t *testing.T) {
	oldRebuild := rebuildMemoryProjection
	oldMark := markMemoryProjectionRefreshStates
	oldDetect := detectMemoryMigrationsForAdmin
	oldSave := saveMemoryMigrationProgressForAdmin
	t.Cleanup(func() {
		rebuildMemoryProjection = oldRebuild
		markMemoryProjectionRefreshStates = oldMark
		detectMemoryMigrationsForAdmin = oldDetect
		saveMemoryMigrationProgressForAdmin = oldSave
	})

	t.Run("rebuild plain", func(t *testing.T) {
		opts, _ := memoryAdminCommandFixture(t)
		committed := 0
		rebuildMemoryProjection = func(string, time.Time) error { committed++; return nil }
		markMemoryProjectionRefreshStates = func(string, *[]memoryReceiptArtifact) error {
			return fmt.Errorf("%s: injected post-commit receipt inspection failure", brainstore.StateUnsafeCode)
		}
		cmd := newMemoryRebuildCommand(opts)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		out, err := execute(t, cmd, "--all")
		if err == nil || !strings.Contains(err.Error(), brainstore.StateUnsafeCode) || committed != 1 {
			t.Fatalf("err=%v committed=%d out=%s", err, committed, out)
		}
		if out != "" {
			t.Fatalf("plain failure wrote receipt telemetry = %q", out)
		}
	})

	t.Run("repair json", func(t *testing.T) {
		opts, _ := memoryAdminCommandFixture(t)
		committed := 0
		rebuildMemoryProjection = func(string, time.Time) error { committed++; return nil }
		markMemoryProjectionRefreshStates = func(string, *[]memoryReceiptArtifact) error {
			return fmt.Errorf("%s: injected post-commit receipt inspection failure", brainstore.StateCorruptCode)
		}
		cmd := newMemoryRepairCommand(opts)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		out, err := execute(t, cmd, "--json")
		if err == nil || committed != 1 {
			t.Fatalf("err=%v committed=%d out=%s", err, committed, out)
		}
		var receipt memoryOperationReceipt
		if decodeErr := json.Unmarshal([]byte(out), &receipt); decodeErr != nil {
			t.Fatalf("decode one failure receipt: %v\n%s", decodeErr, out)
		}
		if receipt.Operation != "repair" || receipt.FinishedAt.IsZero() || receipt.ErrorCode != brainstore.StateCorruptCode || strings.Count(out, `"operation_id"`) != 1 {
			t.Fatalf("receipt=%+v out=%s", receipt, out)
		}
	})

	t.Run("migrate progress json", func(t *testing.T) {
		opts, _ := memoryAdminCommandFixture(t)
		committed := 0
		rebuildMemoryProjection = func(string, time.Time) error { committed++; return nil }
		markMemoryProjectionRefreshStates = oldMark
		detectMemoryMigrationsForAdmin = func(string, *historySourceManifest) []memoryMigrationFinding {
			return []memoryMigrationFinding{{Path: projectionStateRel, State: brainstore.MigrationRequiredCode, Action: "rebuild"}}
		}
		saveMemoryMigrationProgressForAdmin = func(string, memoryMigrationProgress) error {
			return errors.New("injected progress failure")
		}
		cmd := newMemoryMigrateCommand(opts)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		out, err := execute(t, cmd, "--json")
		if err == nil || committed != 1 {
			t.Fatalf("err=%v committed=%d out=%s", err, committed, out)
		}
		var receipt memoryOperationReceipt
		if decodeErr := json.Unmarshal([]byte(out), &receipt); decodeErr != nil {
			t.Fatalf("decode one failure receipt: %v\n%s", decodeErr, out)
		}
		if receipt.Operation != "migrate" || receipt.FinishedAt.IsZero() || receipt.ErrorCode != brainstore.StateCorruptCode || len(receipt.Artifacts) != 1 || strings.Count(out, `"operation_id"`) != 1 {
			t.Fatalf("receipt=%+v out=%s", receipt, out)
		}
	})
}

func TestMemoryInstallHealthUsesTypedReadOnlyEvidence(t *testing.T) {
	brainDir := t.TempDir()
	work := inspectMemoryInstallDirectory(brainDir, memoryWorkDirRel)
	if work.State != "creatable_unproven" || work.Exists || work.WritabilityProven || work.Evidence == "" {
		t.Fatalf("absent work directory health = %+v", work)
	}
	if _, err := os.Stat(filepath.Join(brainDir, filepath.FromSlash(memoryWorkDirRel))); !os.IsNotExist(err) {
		t.Fatalf("health inspection mutated absent work directory: %v", err)
	}

	historyPath := filepath.Join(brainDir, brainstore.HistoryDirName)
	if err := os.MkdirAll(historyPath, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(historyPath, 0o700) })
	history := inspectMemoryInstallDirectory(brainDir, brainstore.HistoryDirName)
	if history.State != "present_unproven" || !history.Exists || history.WritabilityProven {
		t.Fatalf("read-only mode health = %+v", history)
	}
	// The mode-derived hint is a POSIX signal. Windows ignores mode bits on
	// directories (ACLs govern access), so a 0500 mkdir still reports 0777 and
	// the hint stays true. The states asserted above are the portable contract;
	// only the hint is skipped.
	if runtime.GOOS != "windows" && history.ModeWriteHint {
		t.Fatalf("read-only mode hint = %+v", history)
	}

	outside := t.TempDir()
	workPath := filepath.Join(historyPath, "work")
	if err := os.Symlink(outside, workPath); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	unsafe := inspectMemoryInstallDirectory(brainDir, memoryWorkDirRel)
	if unsafe.State != "unsafe" || !unsafe.Exists || unsafe.WritabilityProven {
		t.Fatalf("symlink health = %+v", unsafe)
	}

	nonDirectoryBrain := t.TempDir()
	nonDirectoryWork := filepath.Join(nonDirectoryBrain, brainstore.HistoryDirName, "work")
	if err := os.MkdirAll(filepath.Dir(nonDirectoryWork), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nonDirectoryWork, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	unsafe = inspectMemoryInstallDirectory(nonDirectoryBrain, memoryWorkDirRel)
	if unsafe.State != "unsafe" || !unsafe.Exists || unsafe.WritabilityProven {
		t.Fatalf("non-directory health = %+v", unsafe)
	}
}

func TestMemoryReadOnlyHealthTreatsAbsentBrainDirectoriesAsUnproven(t *testing.T) {
	brainDir := filepath.Join(t.TempDir(), "not-built-yet")
	snapshot := memoryReadOnlyHealth(brainDir, time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC))
	if snapshot.ManifestHealth.State != "absent" {
		t.Fatalf("manifest health = %+v", snapshot.ManifestHealth)
	}
	install, ok := snapshot.Payload["install"].(map[string]any)
	if !ok {
		t.Fatalf("install health = %#v", snapshot.Payload["install"])
	}
	directories, ok := install["directories"].(map[string]memoryInstallDirectoryHealth)
	if !ok {
		t.Fatalf("directory health = %#v", install["directories"])
	}
	for _, name := range []string{"brain", "work", "history"} {
		if got := directories[name]; got.State != "creatable_unproven" || got.WritabilityProven {
			t.Fatalf("%s directory = %+v", name, got)
		}
	}
	for _, issue := range snapshot.Issues {
		if strings.HasPrefix(issue.Kind, "installation_directory_") {
			t.Fatalf("absent derived directory was mislabeled as an issue: %+v", issue)
		}
	}
	if _, err := os.Lstat(brainDir); !os.IsNotExist(err) {
		t.Fatalf("read-only health created the brain directory: %v", err)
	}
}

func TestMemoryInstallHealthReportsExternalAuthorityAndDoesNotExecuteEntire(t *testing.T) {
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "executed")
	if runtime.GOOS != "windows" {
		script := []byte("#!/bin/sh\nprintf executed > '" + marker + "'\n")
		if err := os.WriteFile(filepath.Join(binDir, "entire"), script, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	brainDir := t.TempDir()
	health := memoryInstallHealth(brainDir)
	binary, ok := health["entire_binary"].(memoryInstallBinaryHealth)
	wantBinaryState := "present_unproven"
	if runtime.GOOS == "windows" {
		wantBinaryState = "not_found"
	}
	if !ok || binary.State != wantBinaryState || binary.Executed || binary.VersionVerified {
		t.Fatalf("binary health = %#v", health["entire_binary"])
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Entire binary was executed during read-only health inspection: %v", err)
	}
	adapter, ok := health["host_adapter"].(memoryHostAdapterHealth)
	if !ok || adapter.Authority != "entire-cli" || adapter.State != "not_observable" || adapter.ImplementationState != "external_unverified" || adapter.Observability != "not_observable" {
		t.Fatalf("host adapter health = %#v", health["host_adapter"])
	}
	for _, target := range []string{"claude_code", "codex"} {
		got := adapter.Targets[target]
		if got.Presence != "unknown" || got.Enabled != "unknown" || got.Trust != "unknown" {
			t.Fatalf("%s adapter target = %#v", target, got)
		}
	}
	if _, old := health["work_dir_writable"]; old {
		t.Fatalf("misleading top-level writability boolean survived: %+v", health)
	}
	legacy, ok := health["legacy_mode_hints"].(map[string]any)
	if !ok || legacy["deprecated"] != true || !strings.Contains(legacy["semantics"].(string), "not a writability proof") {
		t.Fatalf("legacy hints = %#v", health["legacy_mode_hints"])
	}
}

func TestAvailableStatusPreservesUnsupportedCorruptAndUnsafeManifest(t *testing.T) {
	now := time.Date(2026, 8, 10, 1, 2, 3, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		state   string
		code    string
		version int
		data    []byte
		symlink bool
	}{
		{name: "unknown newer", state: "unsupported", code: brainstore.UnsupportedVersionCode, version: bmanifest.SchemaVersion + 1, data: []byte(fmt.Sprintf("{\"schema_version\":%d,\"future\":{\"preserve\":true}}\n", bmanifest.SchemaVersion+1))},
		{name: "corrupt", state: "corrupt", code: brainstore.StateCorruptCode, data: []byte("{not-json\n")},
		{name: "unsafe symlink", state: "unsafe", code: brainstore.StateUnsafeCode, symlink: true, data: []byte("{\"schema_version\":3}\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			env.RepoRoot = repoDir
			runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
			opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
			storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(storage.BrainDir, bmanifest.FileName)
			if tc.symlink {
				outside := filepath.Join(t.TempDir(), "future-manifest.json")
				if err := os.WriteFile(outside, tc.data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, manifestPath); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else if err := os.WriteFile(manifestPath, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}

			available, err := buildAvailableBrainStatusReport(context.Background(), opts, repoDir)
			if err != nil {
				t.Fatalf("build available status: %v", err)
			}
			if available.Manifest != nil {
				t.Fatal("unsupported manifest must not become an in-process source")
			}
			out, err := execute(t, NewRootCommand(opts), "status", "--json")
			if err != nil {
				t.Fatalf("available status failed: %v\n%s", err, out)
			}
			var report brainStatusReport
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatalf("decode status: %v\n%s", err, out)
			}
			if report.Brain.ManifestState != tc.state || report.Brain.SupportedSchema != bmanifest.SchemaVersion {
				t.Fatalf("brain health = %+v", report.Brain)
			}
			if tc.version != 0 && report.Brain.Schema != tc.version {
				t.Fatalf("observed schema = %d, want %d", report.Brain.Schema, tc.version)
			}
			if report.Memory != nil || report.Integrity != "not_checked" {
				t.Fatalf("status must leave deep memory health to doctor: %+v", report)
			}
			found := false
			for _, issue := range report.Issues {
				if issue.Kind == "manifest" && issue.Code == tc.code && issue.Path == bmanifest.FileName {
					found = true
				}
			}
			if !found {
				t.Fatalf("typed manifest issue missing: %+v", report.Issues)
			}
			if _, err := buildBrainStatusReport(context.Background(), opts, repoDir); err == nil || !strings.Contains(err.Error(), tc.code) {
				t.Fatalf("strict non-status loader error = %v, want %s", err, tc.code)
			}
			memoryOut, memoryErr := execute(t, newMemoryStatusCommand(opts), "--json")
			if memoryErr != nil {
				t.Fatalf("memory status: %v\n%s", memoryErr, memoryOut)
			}
			var memoryPayload map[string]any
			if err := json.Unmarshal([]byte(memoryOut), &memoryPayload); err != nil {
				t.Fatalf("decode memory status: %v\n%s", err, memoryOut)
			}
			manifestHealth, _ := memoryPayload["manifest_health"].(map[string]any)
			if manifestHealth["state"] != tc.state || memoryPayload["install"] == nil || memoryPayload["issues"] == nil {
				t.Fatalf("partial memory status = %#v", memoryPayload)
			}
			if tc.symlink {
				if info, err := os.Lstat(manifestPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("unsafe manifest link changed: info=%v err=%v", info, err)
				}
			} else if after, err := os.ReadFile(manifestPath); err != nil || !bytes.Equal(after, tc.data) {
				t.Fatalf("manifest bytes changed: err=%v got=%q want=%q", err, after, tc.data)
			}
		})
	}
}

func TestDoctorReportsC5HealthWithoutCreatingState(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = repoDir
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	now := time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC)
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	future := []byte(fmt.Sprintf("{\"schema_version\":%d,\"future\":true}\n", bmanifest.SchemaVersion+1))
	if err := os.WriteFile(filepath.Join(storage.BrainDir, bmanifest.FileName), future, 0o600); err != nil {
		t.Fatal(err)
	}
	// This fixture is a deliberately broken brain (a manifest from the future),
	// so doctor's default gate trips. The contract under test is the JSON
	// payload, which must be complete on stdout before it does.
	out, err := execute(t, NewRootCommand(opts), "doctor", "--json")
	if !errors.Is(err, errDoctorGate) {
		t.Fatalf("doctor on an unsupported manifest = %v, want the gate error\n%s", err, out)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode doctor: %v\n%s", err, out)
	}
	for _, section := range []string{"install", "locks", "schemas", "coordinator", "worker_log"} {
		if _, ok := report.Memory[section]; !ok {
			t.Fatalf("doctor memory health missing %q: %#v", section, report.Memory)
		}
	}
	checks := map[string]doctorCheckResult{}
	for _, check := range report.Checks {
		checks[check.Name] = check
	}
	for _, name := range []string{"memory_install", "memory_coordinator", "write_lock", "memory_schemas", "memory_migrations", "memory_worker_log", "manifest"} {
		if _, ok := checks[name]; !ok {
			t.Fatalf("doctor check %q missing: %+v", name, report.Checks)
		}
	}
	if checks["manifest"].State != "error" || !strings.Contains(checks["manifest"].Detail, brainstore.UnsupportedVersionCode) {
		t.Fatalf("manifest check = %+v", checks["manifest"])
	}
	// Not findings: both were string literals, so they are gone from the
	// report. The payload they were formatted from is still in the machine
	// contract, which is the half a caller can actually use.
	for _, gone := range []string{"memory_host_adapter", "memory_schema_capabilities"} {
		if got, ok := checks[gone]; ok {
			t.Fatalf("%s is back in the report: %+v", gone, got)
		}
	}
	install, _ := report.Memory["install"].(map[string]any)
	adapter, _ := install["host_adapter"].(map[string]any)
	if adapter["authority"] != "entire-cli" {
		t.Fatalf("host-adapter payload lost its authority: %#v", install["host_adapter"])
	}
	schemas, _ := report.Memory["schemas"].(map[string]any)
	compiled, _ := schemas["compiled_capabilities"].(map[string]any)
	if compiled["brain_manifest"] != float64(bmanifest.SchemaVersion) {
		t.Fatalf("compiled schema capabilities payload = %#v", compiled)
	}
	if got := checks["memory_schemas"]; got.State != "error" || !strings.Contains(got.Detail, brainstore.UnsupportedVersionCode) || !strings.Contains(got.Detail, fmt.Sprintf("schema %d", bmanifest.SchemaVersion+1)) {
		t.Fatalf("observed schema health = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(storage.BrainDir, memoryWorkDirRel)); !os.IsNotExist(err) {
		t.Fatalf("doctor created memory work state: %v", err)
	}
	if after, err := os.ReadFile(filepath.Join(storage.BrainDir, bmanifest.FileName)); err != nil || !bytes.Equal(after, future) {
		t.Fatalf("doctor changed future manifest: err=%v got=%q", err, after)
	}
}

func TestMemoryReadOnlyHealthReportsStaleCoordinatorProviderSchemasLocksAndLog(t *testing.T) {
	brainDir := t.TempDir()
	now := time.Date(2026, 8, 10, 3, 0, 0, 0, time.UTC)
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{SchemaVersion: bmanifest.SchemaVersion, GeneratedAt: now}); err != nil {
		t.Fatal(err)
	}
	coordinator := memoryCoordinatorState{
		SchemaVersion: memoryCoordinatorSchema,
		OwnerToken:    "content-free-owner",
		PID:           42,
		State:         "running",
		StartedAt:     now.Add(-2 * time.Hour),
		HeartbeatAt:   now.Add(-time.Hour),
		BinaryVersion: "test",
	}
	data, err := json.Marshal(coordinator)
	if err != nil {
		t.Fatal(err)
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, memoryCoordinatorStateRel, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, memoryWorkerLogRel, []byte(now.Add(-time.Hour).Format(time.RFC3339)+" event=worker_started owner=content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockRel := filepath.ToSlash(filepath.Join(brainstore.BrainLockDirName, brainstore.BrainWriteLockName))
	if err := brainstore.WriteBrainRelativeFileAtomic(brainDir, lockRel, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot := memoryReadOnlyHealth(brainDir, now)
	checks := map[string]doctorCheckResult{}
	for _, check := range memoryDoctorChecks(snapshot) {
		checks[check.Name] = check
	}
	if got := checks["memory_coordinator"]; got.State != "warn" || !strings.Contains(got.Detail, "stale heartbeat") {
		t.Fatalf("coordinator check = %+v", got)
	}
	// A persistent lock leaf is the normal steady state (it is never unlinked
	// on release), so it reports ok with the unproven state named. The stuck-
	// refresh signal is memory_coordinator's stale heartbeat, asserted above.
	if got := checks["write_lock"]; got.State != "ok" || !strings.Contains(got.Detail, "present_unproven") {
		t.Fatalf("write lock check = %+v", got)
	}
	if got, ok := checks["memory_schema_capabilities"]; ok {
		t.Fatalf("compiled-capability check is back in the report: %+v", got)
	}
	if got := checks["memory_schemas"]; got.State != "ok" || !strings.Contains(got.Detail, "observed state current") {
		t.Fatalf("schema check = %+v", got)
	}
	// The detail reports an OS-native absolute path, so compare against the
	// native spelling of the relative path rather than its slash form.
	if got := checks["memory_worker_log"]; got.State != "ok" || !strings.Contains(got.Detail, filepath.FromSlash(memoryWorkerLogRel)) || !strings.Contains(got.Detail, fmt.Sprint(memoryWorkerLogMaxBytes)) {
		t.Fatalf("worker log check = %+v", got)
	}
	schemas, _ := snapshot.Payload["schemas"].(map[string]any)
	compiled, _ := schemas["compiled_capabilities"].(map[string]any)
	observed, _ := schemas["observed_health"].(map[string]any)
	manifestObserved, _ := observed["brain_manifest"].(map[string]any)
	if compiled["vector_progress"] != memoryVectorSchema || compiled["job"] != memoryJobSchemaVersion || manifestObserved["state"] != "current" {
		t.Fatalf("schema health = %#v", schemas)
	}
}

func TestMemoryDoctorSchemaHealthSeparatesCompiledCapabilitiesFromObservedState(t *testing.T) {
	now := time.Date(2026, 8, 10, 4, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		data      []byte
		wantState string
		wantCode  string
		wantVer   int
	}{
		{name: "current", data: []byte(fmt.Sprintf("{\"schema_version\":%d}\n", bmanifest.SchemaVersion)), wantState: "ok", wantVer: bmanifest.SchemaVersion},
		{name: "newer", data: []byte(fmt.Sprintf("{\"schema_version\":%d,\"future\":true}\n", bmanifest.SchemaVersion+1)), wantState: "error", wantCode: brainstore.UnsupportedVersionCode, wantVer: bmanifest.SchemaVersion + 1},
		{name: "corrupt", data: []byte("{broken\n"), wantState: "error", wantCode: brainstore.StateCorruptCode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(brainDir, bmanifest.FileName), tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			snapshot := memoryReadOnlyHealth(brainDir, now)
			checks := map[string]doctorCheckResult{}
			for _, check := range memoryDoctorChecks(snapshot) {
				checks[check.Name] = check
			}
			if got, ok := checks["memory_schema_capabilities"]; ok {
				t.Fatalf("compiled-capability check is back in the report: %+v", got)
			}
			if got := checks["memory_schemas"]; got.State != tc.wantState || (tc.wantCode != "" && !strings.Contains(got.Detail, tc.wantCode)) {
				t.Fatalf("observed schema check = %+v, want state=%s code=%s", got, tc.wantState, tc.wantCode)
			}
			schemas, _ := snapshot.Payload["schemas"].(map[string]any)
			compiled, _ := schemas["compiled_capabilities"].(map[string]any)
			observed, _ := schemas["observed_health"].(map[string]any)
			manifestObserved, _ := observed["brain_manifest"].(map[string]any)
			if compiled["brain_manifest"] != bmanifest.SchemaVersion || manifestObserved["state"] != snapshot.ManifestHealth.State || manifestObserved["observed"] != tc.wantVer {
				t.Fatalf("schema payload = %#v", schemas)
			}
		})
	}

}

func TestDoctorSamplesMemoryReadOnlyHealthOnce(t *testing.T) {
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = repoDir
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	var nowCalls atomic.Int64
	opts := Options{
		Version: "test", Env: env, Runner: runner,
		Now: func() time.Time {
			return time.Date(2026, 8, 10, 6, 0, int(nowCalls.Add(1)), 0, time.UTC)
		},
	}
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage.BrainDir, bmanifest.FileName), []byte(fmt.Sprintf("{\"schema_version\":%d,\"future\":true}\n", bmanifest.SchemaVersion+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := execute(t, NewRootCommand(opts), "doctor", "--json")
	if !errors.Is(err, errDoctorGate) {
		t.Fatalf("doctor on an unsupported manifest = %v, want the gate error\n%s", err, out)
	}
	if sampled := nowCalls.Load(); sampled != 1 {
		t.Fatalf("doctor sampled the memory-health clock %d times, want exactly once", sampled)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || report.Memory["manifest_health"] == nil {
		t.Fatalf("doctor report decode=%v payload=%#v\n%s", err, report.Memory, out)
	}
}

// TestAManifestWithNoSchemaVersionIsNotCurrent covers the backward half of the
// version check. Forward skew was already handled well -- v4 is refused as
// `unsupported` and left untouched -- but the floor was `version < 0`, so the
// two values that mean "this file declares no version" fell through it: `{}`,
// where the field is absent and decodes to Go's zero, and an explicit 0. Both
// were normalised to 1 and reported as `manifest current`, and the normalised 1
// was then reported as the OBSERVED version, so the report hid its own input.
//
// 0 is not a pre-versioning document to adapt. The commit that introduced
// manifest.json already wrote schema_version 1 and the field carries no
// `omitempty`, so no build of this tool has ever emitted a manifest without one.
func TestAManifestWithNoSchemaVersionIsNotCurrent(t *testing.T) {
	now := time.Date(2026, 8, 10, 1, 2, 3, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		data     []byte
		observed int
	}{
		{name: "empty object", data: []byte("{}\n"), observed: 0},
		{name: "explicit zero", data: []byte("{\"schema_version\":0}\n"), observed: 0},
		{name: "negative", data: []byte("{\"schema_version\":-5}\n"), observed: -5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := t.TempDir()
			env := semanticTestEnv(t, repoDir)
			env.RepoRoot = repoDir
			runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
			opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time { return now }}
			storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(storage.BrainDir, bmanifest.FileName), tc.data, 0o600); err != nil {
				t.Fatal(err)
			}

			out, err := execute(t, NewRootCommand(opts), "status", "--json")
			if err != nil {
				t.Fatalf("status: %v\n%s", err, out)
			}
			var report brainStatusReport
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatalf("decode status: %v\n%s", err, out)
			}
			if report.Brain.ManifestState == "current" {
				t.Fatalf("a manifest declaring no schema version was reported current: %+v", report.Brain)
			}
			if report.Brain.ManifestState != "unsupported" {
				t.Fatalf("manifest state = %q, want unsupported", report.Brain.ManifestState)
			}
			if report.Brain.Schema != tc.observed {
				t.Fatalf("reported schema = %d, want the observed %d (the 0 -> 1 coercion must not reach the report)", report.Brain.Schema, tc.observed)
			}
			found := false
			for _, issue := range report.Issues {
				if issue.Kind == "manifest" && issue.Code == brainstore.UnsupportedVersionCode {
					found = true
				}
			}
			if !found {
				t.Fatalf("typed manifest issue missing: %+v", report.Issues)
			}

			// The remedy has to match the direction of the skew. There is no
			// build to upgrade INTO for a version older than the first one ever
			// written, so the way out is a rebuild.
			_, health, issue := inspectBrainManifestHealth(storage.BrainDir)
			if issue == nil {
				t.Fatal("no manifest issue raised")
			}
			if strings.Contains(health.RecommendedAction, "upgrade") {
				t.Fatalf("a backward-skewed manifest was answered with an upgrade: %q", health.RecommendedAction)
			}
			if !strings.Contains(health.RecommendedAction, "refresh") {
				t.Fatalf("the remedy does not name the command that rebuilds it: %q", health.RecommendedAction)
			}

			// The strict loader every writer gates on must refuse it too, or a
			// refresh would rewrite from a manifest it could not vouch for.
			if _, _, err := bmanifest.Read(storage.BrainDir, true); err == nil || !strings.Contains(err.Error(), brainstore.UnsupportedVersionCode) {
				t.Fatalf("strict loader error = %v, want %s", err, brainstore.UnsupportedVersionCode)
			}
		})
	}

	// The control: a manifest that does declare a supported version is current,
	// so the floor did not simply reject everything.
	repoDir := t.TempDir()
	env := semanticTestEnv(t, repoDir)
	env.RepoRoot = repoDir
	runner := semanticFixtureRunner(repoDir, semanticFixtureSnapshot("1.0"))
	storage, err := repoStoragePaths(context.Background(), runner, env, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(storage.BrainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage.BrainDir, bmanifest.FileName), []byte("{\"schema_version\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, health, issue := inspectBrainManifestHealth(storage.BrainDir); issue != nil || health.State != "current" {
		t.Fatalf("v1 manifest must stay current: state=%q issue=%+v", health.State, issue)
	}
}
