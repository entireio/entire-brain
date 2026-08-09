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
	calls := 0
	opts := Options{Version: "test", Env: env, Runner: runner, Now: func() time.Time {
		calls++
		return manifest.GeneratedAt.Add(time.Duration(calls) * time.Second)
	}}
	return opts, storage.BrainDir
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
	if history.State != "present_unproven" || !history.Exists || history.ModeWriteHint || history.WritabilityProven {
		t.Fatalf("read-only mode health = %+v", history)
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
	wantBinaryState := "present_unverified"
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
	if !ok || adapter.Authority != "entire-cli" || adapter.State != "external_not_implemented" || adapter.ImplementationState != "external_not_implemented" || adapter.Observability != "not_observable" {
		t.Fatalf("host adapter health = %#v", health["host_adapter"])
	}
	if _, old := health["work_dir_writable"]; old {
		t.Fatalf("misleading top-level writability boolean survived: %+v", health)
	}
	legacy, ok := health["legacy_mode_hints"].(map[string]any)
	if !ok || legacy["deprecated"] != true || !strings.Contains(legacy["semantics"].(string), "not a writability proof") {
		t.Fatalf("legacy hints = %#v", health["legacy_mode_hints"])
	}
}
