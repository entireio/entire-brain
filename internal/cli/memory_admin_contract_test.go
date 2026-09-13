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

	"github.com/spf13/cobra"
)

func TestFinishMemoryOperationFailureEmitsOneCompleteReceiptInRequestedFormat(t *testing.T) {
	started := time.Date(2026, 8, 9, 20, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Second)
	cause := fmt.Errorf("%s: injected", memoryErrSourceStale)

	for _, jsonOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "json"}[jsonOut], func(t *testing.T) {
			var output bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&output)
			receipt := newMemoryOperationReceipt("repair", started)
			receipt.Artifacts = []memoryReceiptArtifact{{Path: historyIndexPath, PriorState: "present", NewState: "present"}}

			err := finishMemoryOperationFailure(cmd, &receipt, jsonOut, finished, cause)
			if !errors.Is(err, cause) {
				t.Fatalf("returned error = %v, want original cause", err)
			}
			if receipt.FinishedAt != finished || receipt.ErrorCode != memoryErrSourceStale {
				t.Fatalf("receipt = %+v", receipt)
			}
			if jsonOut {
				var decoded memoryOperationReceipt
				if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
					t.Fatalf("decode one JSON receipt: %v\n%s", err, output.String())
				}
				if decoded.OperationID != receipt.OperationID || decoded.FinishedAt != finished || decoded.ErrorCode != memoryErrSourceStale {
					t.Fatalf("decoded receipt = %+v", decoded)
				}
				if strings.Count(output.String(), `"operation_id"`) != 1 {
					t.Fatalf("failure receipt emitted more than once: %s", output.String())
				}
				return
			}
			if strings.Contains(output.String(), "{") || strings.Count(output.String(), "repair failed ") != 1 ||
				!strings.Contains(output.String(), "error_code="+memoryErrSourceStale) || !strings.Contains(output.String(), "finished_at="+finished.Format(time.RFC3339Nano)) {
				t.Fatalf("plain failure receipt = %q", output.String())
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
		SchemaVersion: brainManifestSchemaVersion,
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
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		t.Fatal(err)
	}
	oldLaunch := memoryWorkerLaunch
	memoryWorkerLaunch = func(string) error { return nil }
	t.Cleanup(func() { memoryWorkerLaunch = oldLaunch })

	out, err := execute(t, newMemoryNotifyCommand(opts), "--event", "session_end", "--session", "notify-session", "--json")
	if err != nil {
		t.Fatalf("notify without repo key: %v\n%s", err, out)
	}
	var payload struct {
		Recorded   string `json:"recorded"`
		SessionID  string `json:"session_id"`
		Generation int    `json:"generation"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil || payload.Recorded != "session_end" || payload.SessionID != "notify-session" || payload.Generation != 1 {
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
	if len(hints) != 1 || hints[0].Generation != 1 || hints[0].LastEvent != "session_end" {
		t.Fatalf("mismatched key mutated hints=%+v", hints)
	}
}

func addMemoryAdminConversationFixture(t *testing.T, brainDir string, now time.Time) string {
	t.Helper()
	manifest, err := loadBrainManifest(brainDir)
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

func TestManualAbstractMutationReceiptsAndDryRun(t *testing.T) {
	opts, brainDir := memoryAdminCommandFixture(t)
	ref := addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 9, 21, 0, 0, 0, time.UTC))
	provider := &privacyCountingAbstractor{}
	oldFactory := memoryAbstractorFactory
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return provider, nil }
	t.Cleanup(func() { memoryAbstractorFactory = oldFactory })

	out, err := execute(t, newMemoryAbstractCommand(opts), ref, "--provider", "ollama", "--model", "test", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("abstract dry-run: %v\n%s", err, out)
	}
	var payload struct {
		Receipt        memoryOperationReceipt `json:"receipt"`
		JobID          string                 `json:"job_id"`
		JobCreated     bool                   `json:"job_created"`
		JobWouldCreate bool                   `json:"job_would_create"`
		JobCompleted   bool                   `json:"job_completed"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil || !payload.Receipt.DryRun || payload.Receipt.FinishedAt.IsZero() || payload.JobID == "" || payload.JobCreated || !payload.JobWouldCreate || payload.JobCompleted || len(payload.Receipt.Artifacts) != 1 {
		t.Fatalf("abstract dry-run payload=%+v err=%v\n%s", payload, err, out)
	}
	if provider.calls.Load() != 0 || len(loadMemoryJobs(brainDir)) != 0 {
		t.Fatalf("abstract dry-run side effects: provider_calls=%d jobs=%+v", provider.calls.Load(), loadMemoryJobs(brainDir))
	}
	if _, ok := loadSessionAbstract(brainDir, sessionViewDigest(sessionViewForTest(t, brainDir))); ok {
		t.Fatal("abstract dry-run published an artifact")
	}

	out, err = execute(t, newMemoryAbstractCommand(opts), ref, "--provider", "ollama", "--model", "test", "--json")
	if err != nil {
		t.Fatalf("abstract generate: %v\n%s", err, out)
	}
	payload = struct {
		Receipt        memoryOperationReceipt `json:"receipt"`
		JobID          string                 `json:"job_id"`
		JobCreated     bool                   `json:"job_created"`
		JobWouldCreate bool                   `json:"job_would_create"`
		JobCompleted   bool                   `json:"job_completed"`
	}{}
	if err := json.Unmarshal([]byte(out), &payload); err != nil || payload.Receipt.Operation != "abstract_generate" || payload.Receipt.DryRun || payload.Receipt.FinishedAt.IsZero() || !payload.JobCreated || !payload.JobCompleted || len(payload.Receipt.JobIDs) != 1 || len(payload.Receipt.Artifacts) < 2 {
		t.Fatalf("abstract generation payload=%+v err=%v\n%s", payload, err, out)
	}
	if provider.calls.Load() != 1 || len(loadMemoryJobs(brainDir)) != 1 {
		t.Fatalf("abstract generation effects: provider_calls=%d jobs=%+v", provider.calls.Load(), loadMemoryJobs(brainDir))
	}

	out, err = execute(t, newMemoryAbstractCommand(opts), ref, "--provider", "ollama", "--model", "test")
	if err != nil || strings.Contains(out, "{") || !strings.Contains(out, "abstract_generate completed") || !strings.Contains(out, "operation_id=") {
		t.Fatalf("repeat abstract plain receipt: err=%v\n%s", err, out)
	}
	if provider.calls.Load() != 2 || len(loadMemoryJobs(brainDir)) != 1 {
		t.Fatalf("repeat generation duplicated identity: provider_calls=%d jobs=%+v", provider.calls.Load(), loadMemoryJobs(brainDir))
	}

	missing := newMemoryAbstractCommand(opts)
	missing.SilenceErrors, missing.SilenceUsage = true, true
	out, err = execute(t, missing, "conversation-session:not-found", "--generate", "--json")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing abstract error=%v out=%s", err, out)
	}
	var failure memoryOperationReceipt
	if decodeErr := json.Unmarshal([]byte(out), &failure); decodeErr != nil || failure.Operation != "abstract_generate" || failure.ErrorCode != memoryErrSourceStale || failure.FinishedAt.IsZero() {
		t.Fatalf("missing abstract receipt=%+v decode=%v\n%s", failure, decodeErr, out)
	}
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
			return fmt.Errorf("%s: injected post-commit receipt inspection failure", memoryErrStateUnsafe)
		}
		cmd := newMemoryRebuildCommand(opts)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		out, err := execute(t, cmd, "--all")
		if err == nil || !strings.Contains(err.Error(), memoryErrStateUnsafe) || committed != 1 {
			t.Fatalf("err=%v committed=%d out=%s", err, committed, out)
		}
		if strings.Contains(out, "{") || strings.Count(out, "rebuild failed ") != 1 || !strings.Contains(out, "finished_at=") {
			t.Fatalf("plain failure receipt = %q", out)
		}
	})

	t.Run("repair json", func(t *testing.T) {
		opts, _ := memoryAdminCommandFixture(t)
		committed := 0
		rebuildMemoryProjection = func(string, time.Time) error { committed++; return nil }
		markMemoryProjectionRefreshStates = func(string, *[]memoryReceiptArtifact) error {
			return fmt.Errorf("%s: injected post-commit receipt inspection failure", memoryErrStateCorrupt)
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
		if receipt.Operation != "repair" || receipt.FinishedAt.IsZero() || receipt.ErrorCode != memoryErrStateCorrupt || strings.Count(out, `"operation_id"`) != 1 {
			t.Fatalf("receipt=%+v out=%s", receipt, out)
		}
	})

	t.Run("migrate progress json", func(t *testing.T) {
		opts, _ := memoryAdminCommandFixture(t)
		committed := 0
		rebuildMemoryProjection = func(string, time.Time) error { committed++; return nil }
		markMemoryProjectionRefreshStates = oldMark
		detectMemoryMigrationsForAdmin = func(string, *historySourceManifest) []memoryMigrationFinding {
			return []memoryMigrationFinding{{Path: projectionStateRel, State: memoryErrMigrationRequired, Action: "rebuild"}}
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
		if receipt.Operation != "migrate" || receipt.FinishedAt.IsZero() || receipt.ErrorCode != memoryErrStateCorrupt || len(receipt.Artifacts) != 1 || strings.Count(out, `"operation_id"`) != 1 {
			t.Fatalf("receipt=%+v out=%s", receipt, out)
		}
	})
}

func TestConfigureAbstractsEmitsFinalizedReceiptAfterCommittedFailure(t *testing.T) {
	opts, brainDir := memoryAdminCommandFixture(t)
	jobsPath := filepath.Join(brainDir, filepath.FromSlash(memoryJobsDirRel))
	if err := os.MkdirAll(filepath.Dir(jobsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobsPath, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newMemoryConfigureCommand(opts)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	out, err := execute(t, cmd, "abstracts", "--disable", "--json")
	if err == nil {
		t.Fatalf("configure unexpectedly succeeded: %s", out)
	}
	var receipt memoryOperationReceipt
	if decodeErr := json.Unmarshal([]byte(out), &receipt); decodeErr != nil || receipt.Operation != "configure_abstracts" || receipt.FinishedAt.IsZero() || receipt.ErrorCode == "" || len(receipt.Artifacts) != 1 || receipt.Artifacts[0].Path != memoryConfigRel || receipt.Artifacts[0].NewState != "written" || strings.Count(out, `"operation_id"`) != 1 {
		t.Fatalf("configure failure receipt=%+v decode=%v\n%s", receipt, decodeErr, out)
	}
	stored, state, loadErr := loadMemoryConfigChecked(brainDir)
	if loadErr != nil || state != memoryConfigCurrent || stored.Abstracts.Enabled {
		t.Fatalf("committed config not reflected after later failure: stored=%+v state=%s err=%v", stored, state, loadErr)
	}
}

func TestConfigureAbstractsValidationFailureHonorsPlainReceiptMode(t *testing.T) {
	opts, brainDir := memoryAdminCommandFixture(t)
	oldFactory := memoryAbstractorFactory
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		return nil, errors.New("configured provider is unavailable")
	}
	t.Cleanup(func() { memoryAbstractorFactory = oldFactory })
	cmd := newMemoryConfigureCommand(opts)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	out, err := execute(t, cmd, "abstracts", "--enable", "--provider", "codex")
	if err == nil || !strings.Contains(err.Error(), memoryErrProviderUnavail) {
		t.Fatalf("configure validation error=%v out=%s", err, out)
	}
	if strings.Contains(out, "{") || strings.Count(out, "configure_abstracts failed ") != 1 || !strings.Contains(out, "schema_version=1") || !strings.Contains(out, "error_code="+memoryErrProviderUnavail) || !strings.Contains(out, "finished_at=") {
		t.Fatalf("configure plain failure receipt=%q", out)
	}
	if _, state, loadErr := loadMemoryConfigChecked(brainDir); loadErr != nil || state != memoryConfigAbsent {
		t.Fatalf("validation failure mutated config state=%s err=%v", state, loadErr)
	}
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

	historyPath := filepath.Join(brainDir, historyDirName)
	if err := os.MkdirAll(historyPath, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(historyPath, 0o700) })
	history := inspectMemoryInstallDirectory(brainDir, historyDirName)
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
	nonDirectoryWork := filepath.Join(nonDirectoryBrain, historyDirName, "work")
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
		{name: "unknown newer", state: "unsupported", code: memoryErrUnsupportedVersion, version: brainManifestSchemaVersion + 1, data: []byte("{\"schema_version\":4,\"future\":{\"preserve\":true}}\n")},
		{name: "corrupt", state: "corrupt", code: memoryErrStateCorrupt, data: []byte("{not-json\n")},
		{name: "unsafe symlink", state: "unsafe", code: memoryErrStateUnsafe, symlink: true, data: []byte("{\"schema_version\":3}\n")},
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
			manifestPath := filepath.Join(storage.BrainDir, exportManifestFileName)
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
			if report.Brain.ManifestState != tc.state || report.Brain.SupportedSchema != brainManifestSchemaVersion {
				t.Fatalf("brain health = %+v", report.Brain)
			}
			if tc.version != 0 && report.Brain.Schema != tc.version {
				t.Fatalf("observed schema = %d, want %d", report.Brain.Schema, tc.version)
			}
			for _, section := range []string{"install", "locks", "schemas", "coordinator", "worker_log"} {
				if _, ok := report.Memory[section]; !ok {
					t.Fatalf("partial status missing %q: %#v", section, report.Memory)
				}
			}
			found := false
			for _, issue := range report.Issues {
				if issue.Kind == "manifest" && issue.Code == tc.code && issue.Path == exportManifestFileName {
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
			var mcpOut bytes.Buffer
			input := frameMCP(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_status","arguments":{}}}`)
			if err := runMCP(context.Background(), strings.NewReader(input), &mcpOut, opts); err != nil {
				t.Fatalf("MCP brain_status: %v", err)
			}
			responses := readMCPResponses(t, mcpOut.String())
			if len(responses) != 1 {
				t.Fatalf("MCP responses = %d", len(responses))
			}
			payload := mcpTextJSONPayload(t, responses[0])
			brain, _ := payload["brain"].(map[string]any)
			memory, _ := payload["memory"].(map[string]any)
			if brain["manifest_state"] != tc.state || memory["install"] == nil || memory["issues"] == nil {
				t.Fatalf("partial MCP brain_status = %#v", payload)
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
	future := []byte("{\"schema_version\":4,\"future\":true}\n")
	if err := os.WriteFile(filepath.Join(storage.BrainDir, exportManifestFileName), future, 0o600); err != nil {
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
	for _, name := range []string{"memory_install", "memory_host_adapter", "memory_coordinator", "write_lock", "memory_provider_egress", "memory_schema_capabilities", "memory_schemas", "memory_migrations", "memory_worker_log", "manifest"} {
		if _, ok := checks[name]; !ok {
			t.Fatalf("doctor check %q missing: %+v", name, report.Checks)
		}
	}
	if checks["manifest"].State != "error" || !strings.Contains(checks["manifest"].Detail, memoryErrUnsupportedVersion) {
		t.Fatalf("manifest check = %+v", checks["manifest"])
	}
	// Not a warning: this process never observes the host adapter, so the
	// check could never pass. It still has to name the authority that can.
	if got := checks["memory_host_adapter"]; got.State != "ok" || !strings.Contains(got.Detail, "authority entire-cli") {
		t.Fatalf("host adapter check = %+v", got)
	}
	if got := checks["memory_schema_capabilities"]; got.State != "ok" || !strings.Contains(got.Detail, fmt.Sprintf("manifest %d", brainManifestSchemaVersion)) {
		t.Fatalf("compiled schema capabilities = %+v", got)
	}
	if got := checks["memory_schemas"]; got.State != "error" || !strings.Contains(got.Detail, memoryErrUnsupportedVersion) || !strings.Contains(got.Detail, "schema 4") {
		t.Fatalf("observed schema health = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(storage.BrainDir, memoryWorkDirRel)); !os.IsNotExist(err) {
		t.Fatalf("doctor created memory work state: %v", err)
	}
	if after, err := os.ReadFile(filepath.Join(storage.BrainDir, exportManifestFileName)); err != nil || !bytes.Equal(after, future) {
		t.Fatalf("doctor changed future manifest: err=%v got=%q", err, after)
	}
}

func TestMemoryReadOnlyHealthReportsStaleCoordinatorProviderSchemasLocksAndLog(t *testing.T) {
	brainDir := t.TempDir()
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	now := time.Date(2026, 8, 10, 3, 0, 0, 0, time.UTC)
	if err := writeBrainManifestAndReadme(brainDir, exportManifest{SchemaVersion: brainManifestSchemaVersion, GeneratedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := saveMemoryConfig(brainDir, memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{
		Enabled: true, Automatic: true, Provider: "codex", Model: "explicit-model", HostedEgressAllowed: true,
	}}); err != nil {
		t.Fatal(err)
	}
	oldFactory := memoryAbstractorFactory
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return &fakeAbstractor{hosted: true}, nil }
	t.Cleanup(func() { memoryAbstractorFactory = oldFactory })

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
	if err := writeBrainRelativeFileAtomic(brainDir, memoryCoordinatorStateRel, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeBrainRelativeFileAtomic(brainDir, memoryWorkerLogRel, []byte(now.Add(-time.Hour).Format(time.RFC3339)+" event=worker_started owner=content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lockRel := filepath.ToSlash(filepath.Join(brainLockDirName, brainWriteLockName))
	if err := writeBrainRelativeFileAtomic(brainDir, lockRel, nil, 0o600); err != nil {
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
	if got := checks["memory_provider_egress"]; got.State != "ok" || !strings.Contains(got.Detail, "hosted") || !strings.Contains(got.Detail, "global policy default") || !strings.Contains(got.Detail, "effective available_unverified") {
		t.Fatalf("provider/egress check = %+v", got)
	}
	if got := checks["memory_schema_capabilities"]; got.State != "ok" || !strings.Contains(got.Detail, fmt.Sprintf("jobs %d", memoryJobSchemaVersion)) {
		t.Fatalf("compiled schema check = %+v", got)
	}
	if got := checks["memory_schemas"]; got.State != "ok" || !strings.Contains(got.Detail, "observed state current") {
		t.Fatalf("schema check = %+v", got)
	}
	// The detail reports an OS-native absolute path, so compare against the
	// native spelling of the relative path rather than its slash form.
	if got := checks["memory_worker_log"]; got.State != "ok" || !strings.Contains(got.Detail, filepath.FromSlash(memoryWorkerLogRel)) || !strings.Contains(got.Detail, fmt.Sprint(memoryWorkerLogMaxBytes)) {
		t.Fatalf("worker log check = %+v", got)
	}
	install, _ := snapshot.Payload["install"].(map[string]any)
	provider, _ := install["provider_egress"].(map[string]any)
	if provider["provider"] != "codex" || provider["model"] != "explicit-model" || provider["egress_class"] != "hosted" || provider["hosted_egress_allowed"] != true {
		t.Fatalf("provider health = %#v", provider)
	}
	schemas, _ := snapshot.Payload["schemas"].(map[string]any)
	compiled, _ := schemas["compiled_capabilities"].(map[string]any)
	observed, _ := schemas["observed_health"].(map[string]any)
	manifestObserved, _ := observed["brain_manifest"].(map[string]any)
	if compiled["vector_progress"] != memoryVectorSchema || compiled["abstract_egress"] != abstractEgressSchemaVersion || compiled["job"] != memoryJobSchemaVersion || manifestObserved["state"] != "current" {
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
		{name: "current", data: []byte(fmt.Sprintf("{\"schema_version\":%d}\n", brainManifestSchemaVersion)), wantState: "ok", wantVer: brainManifestSchemaVersion},
		{name: "newer", data: []byte(fmt.Sprintf("{\"schema_version\":%d,\"future\":true}\n", brainManifestSchemaVersion+1)), wantState: "error", wantCode: memoryErrUnsupportedVersion, wantVer: brainManifestSchemaVersion + 1},
		{name: "corrupt", data: []byte("{broken\n"), wantState: "error", wantCode: memoryErrStateCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brainDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(brainDir, exportManifestFileName), tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			snapshot := memoryReadOnlyHealth(brainDir, now)
			checks := map[string]doctorCheckResult{}
			for _, check := range memoryDoctorChecks(snapshot) {
				checks[check.Name] = check
			}
			if got := checks["memory_schema_capabilities"]; got.State != "ok" || !strings.Contains(got.Detail, fmt.Sprintf("manifest %d", brainManifestSchemaVersion)) {
				t.Fatalf("compiled capabilities = %+v", got)
			}
			if got := checks["memory_schemas"]; got.State != tc.wantState || (tc.wantCode != "" && !strings.Contains(got.Detail, tc.wantCode)) {
				t.Fatalf("observed schema check = %+v, want state=%s code=%s", got, tc.wantState, tc.wantCode)
			}
			schemas, _ := snapshot.Payload["schemas"].(map[string]any)
			compiled, _ := schemas["compiled_capabilities"].(map[string]any)
			observed, _ := schemas["observed_health"].(map[string]any)
			manifestObserved, _ := observed["brain_manifest"].(map[string]any)
			if compiled["brain_manifest"] != brainManifestSchemaVersion || manifestObserved["state"] != snapshot.ManifestHealth.State || manifestObserved["observed"] != tc.wantVer {
				t.Fatalf("schema payload = %#v", schemas)
			}
		})
	}

	// The same separation applies to derived schemas. A supported Brain with a
	// newer config must not inherit the compiled-capability "ok" state.
	t.Run("newer abstract config", func(t *testing.T) {
		brainDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(brainDir, exportManifestFileName), []byte(fmt.Sprintf("{\"schema_version\":%d}\n", brainManifestSchemaVersion)), 0o600); err != nil {
			t.Fatal(err)
		}
		configPath := filepath.Join(brainDir, filepath.FromSlash(memoryConfigRel))
		if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, []byte(fmt.Sprintf("{\"schema_version\":%d,\"abstracts\":{},\"future\":true}\n", memoryConfigSchemaVersion+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot := memoryReadOnlyHealth(brainDir, now)
		checks := map[string]doctorCheckResult{}
		for _, check := range memoryDoctorChecks(snapshot) {
			checks[check.Name] = check
		}
		if got := checks["memory_schemas"]; got.State != "error" || !strings.Contains(got.Detail, "observed schema health error") {
			t.Fatalf("newer config schema check = %+v", got)
		}
		schemas, _ := snapshot.Payload["schemas"].(map[string]any)
		observed, _ := schemas["observed_health"].(map[string]any)
		configObserved, _ := observed["abstract_config"].(map[string]any)
		if configObserved["state"] != memoryConfigUnsupported || configObserved["schema_version"] != memoryConfigSchemaVersion+1 || configObserved["supported_schema_version"] != memoryConfigSchemaVersion || configObserved["error_code"] != memoryErrUnsupportedVersion {
			t.Fatalf("observed abstract config schema = %#v", configObserved)
		}
	})
}

func TestMemoryProviderEgressHealthReportsEffectivePolicy(t *testing.T) {
	brainDir := t.TempDir()
	oldFactory := memoryAbstractorFactory
	memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) {
		return &fakeAbstractor{hosted: true}, nil
	}
	t.Cleanup(func() { memoryAbstractorFactory = oldFactory })

	for _, tc := range []struct {
		name          string
		noEgress      string
		acknowledged  bool
		wantEffective string
		wantPolicy    string
		wantBlock     string
		wantDoctor    string
	}{
		{name: "available with explicit acknowledgement", acknowledged: true, wantEffective: "available_unverified", wantPolicy: "default", wantDoctor: "ok"},
		{name: "blocked by global no egress", noEgress: "1", acknowledged: true, wantEffective: "blocked_by_global_policy", wantPolicy: "local_only", wantBlock: "no_egress", wantDoctor: "warn"},
		{name: "blocked without acknowledgement", acknowledged: false, wantEffective: "blocked_by_configuration", wantPolicy: "default", wantBlock: "hosted_egress_unacknowledged", wantDoctor: "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ENTIRE_BRAIN_NO_EGRESS", tc.noEgress)
			t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
			if err := saveMemoryConfig(brainDir, memoryConfig{SchemaVersion: memoryConfigSchemaVersion, Abstracts: memoryAbstractsConfig{
				Enabled: true, Automatic: true, Provider: "codex", Model: "explicit", HostedEgressAllowed: tc.acknowledged,
			}}); err != nil {
				t.Fatal(err)
			}
			snapshot := memoryReadOnlyHealth(brainDir, time.Date(2026, 8, 10, 5, 0, 0, 0, time.UTC))
			install, _ := snapshot.Payload["install"].(map[string]any)
			health, _ := install["provider_egress"].(map[string]any)
			gotBlock, _ := health["block_code"].(string)
			if health["effective_state"] != tc.wantEffective || health["global_policy"] != tc.wantPolicy || gotBlock != tc.wantBlock {
				t.Fatalf("provider/egress health = %#v", health)
			}
			allowed, _ := health["effective_provider_allowed"].(bool)
			if allowed != (tc.wantEffective == "available_unverified") {
				t.Fatalf("effective_provider_allowed=%v health=%#v", allowed, health)
			}
			checks := map[string]doctorCheckResult{}
			for _, check := range memoryDoctorChecks(snapshot) {
				checks[check.Name] = check
			}
			if got := checks["memory_provider_egress"]; got.State != tc.wantDoctor || !strings.Contains(got.Detail, tc.wantEffective) {
				t.Fatalf("doctor provider check = %+v", got)
			}
			foundBlock := false
			for _, issue := range snapshot.Issues {
				if issue.Kind == "provider_policy" && issue.Code == tc.wantBlock {
					foundBlock = true
				}
			}
			if (tc.wantBlock != "") != foundBlock {
				t.Fatalf("provider policy issue presence=%v issues=%+v", foundBlock, snapshot.Issues)
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
	if err := os.WriteFile(filepath.Join(storage.BrainDir, exportManifestFileName), []byte(fmt.Sprintf("{\"schema_version\":%d,\"future\":true}\n", brainManifestSchemaVersion+1)), 0o600); err != nil {
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
			if err := os.WriteFile(filepath.Join(storage.BrainDir, exportManifestFileName), tc.data, 0o600); err != nil {
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
				if issue.Kind == "manifest" && issue.Code == memoryErrUnsupportedVersion {
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
			if _, _, err := readBrainManifest(storage.BrainDir, true); err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
				t.Fatalf("strict loader error = %v, want %s", err, memoryErrUnsupportedVersion)
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
	if err := os.WriteFile(filepath.Join(storage.BrainDir, exportManifestFileName), []byte("{\"schema_version\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, health, issue := inspectBrainManifestHealth(storage.BrainDir); issue != nil || health.State != "current" {
		t.Fatalf("v1 manifest must stay current: state=%q issue=%+v", health.State, issue)
	}
}
