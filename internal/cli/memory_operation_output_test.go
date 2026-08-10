package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func executeMemoryCommandWithFailOnce(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	writer := &failOnceMemoryReceiptWriter{}
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(writer)
	cmd.SetErr(&strings.Builder{})
	cmd.SetArgs(args)
	err := cmd.Execute()
	return writer.output.String(), err
}

func TestMemoryMaintenancePlainSuccessIncludesVersionedReceipt(t *testing.T) {
	assertReceipt := func(t *testing.T, out, operation, legacyResult string) {
		t.Helper()
		if strings.Contains(out, "{") || !strings.Contains(out, operation+" completed ") ||
			!strings.Contains(out, "schema_version=1") || !strings.Contains(out, "operation_id=op:") ||
			!strings.Contains(out, "finished_at=") || !strings.Contains(out, legacyResult) {
			t.Fatalf("%s plain success receipt = %q", operation, out)
		}
	}

	t.Run("repair", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 8, 0, 0, 0, time.UTC))
		out, err := execute(t, newMemoryRepairCommand(opts))
		if err != nil {
			t.Fatalf("repair: %v\n%s", err, out)
		}
		assertReceipt(t, out, "repair", "repair:")
	})

	t.Run("rebuild", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 8, 1, 0, 0, time.UTC))
		out, err := execute(t, newMemoryRebuildCommand(opts), "--all")
		if err != nil {
			t.Fatalf("rebuild: %v\n%s", err, out)
		}
		assertReceipt(t, out, "rebuild", "disposable projection artifacts from canonical sessions")
	})

	t.Run("migrate", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 8, 2, 0, 0, time.UTC))
		out, err := execute(t, newMemoryMigrateCommand(opts))
		if err != nil {
			t.Fatalf("migrate: %v\n%s", err, out)
		}
		assertReceipt(t, out, "migrate", "migrate: every derived schema is current")
	})
}

func TestMemoryMaintenanceOutputFailurePreservesSuccessfulMutationOutcome(t *testing.T) {
	run := func(t *testing.T, cmdArgs []string, command interface{ Execute() error }, setOutput func(*failOnceMemoryReceiptWriter)) (memoryOperationReceipt, error) {
		t.Helper()
		writer := &failOnceMemoryReceiptWriter{}
		setOutput(writer)
		err := command.Execute()
		var receipt memoryOperationReceipt
		if decodeErr := json.Unmarshal([]byte(writer.output.String()), &receipt); decodeErr != nil {
			t.Fatalf("decode fallback failure receipt: %v command_err=%v\n%s", decodeErr, err, writer.output.String())
		}
		if !errors.Is(err, errMemoryReceiptOutput) || receipt.ErrorCode != memoryErrStateCorrupt || receipt.FinishedAt.IsZero() {
			t.Fatalf("writer failure err=%v receipt=%+v args=%v", err, receipt, cmdArgs)
		}
		for _, artifact := range receipt.Artifacts {
			if artifact.NewState == "write_outcome_unknown" {
				t.Fatalf("successful mutation outcome was lost: %+v", receipt.Artifacts)
			}
		}
		return receipt, err
	}

	t.Run("repair", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 8, 3, 0, 0, time.UTC))
		manifest, err := loadBrainManifest(brainDir)
		if err != nil || manifest.Sources == nil || manifest.Sources.History == nil {
			t.Fatalf("load repair fixture: manifest=%+v err=%v", manifest, err)
		}
		if err := os.Remove(filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.History.IndexPath))); err != nil {
			t.Fatalf("make repair necessary: %v", err)
		}
		cmd := newMemoryRepairCommand(opts)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		cmd.SetArgs([]string{"--json"})
		receipt, _ := run(t, []string{"--json"}, cmd, func(writer *failOnceMemoryReceiptWriter) {
			cmd.SetOut(writer)
			cmd.SetErr(&strings.Builder{})
		})
		if receipt.Operation != "repair" || len(receipt.Artifacts) == 0 {
			t.Fatalf("repair receipt = %+v", receipt)
		}
		manifest, err = loadBrainManifest(brainDir)
		if err != nil || manifest.Sources == nil || manifest.Sources.History == nil || manifest.Sources.History.IndexDigest == "" {
			t.Fatalf("repair mutation was not durable: manifest=%+v err=%v", manifest, err)
		}
	})

	t.Run("rebuild", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 8, 4, 0, 0, time.UTC))
		cmd := newMemoryRebuildCommand(opts)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		cmd.SetArgs([]string{"--all", "--json"})
		receipt, _ := run(t, []string{"--all", "--json"}, cmd, func(writer *failOnceMemoryReceiptWriter) {
			cmd.SetOut(writer)
			cmd.SetErr(&strings.Builder{})
		})
		if receipt.Operation != "rebuild" || len(receipt.Artifacts) == 0 {
			t.Fatalf("rebuild receipt = %+v", receipt)
		}
		manifest, err := loadBrainManifest(brainDir)
		if err != nil || manifest.Sources == nil || manifest.Sources.History == nil || manifest.Sources.History.IndexDigest == "" {
			t.Fatalf("rebuild mutation was not durable: manifest=%+v err=%v", manifest, err)
		}
	})

	t.Run("migrate", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 8, 5, 0, 0, time.UTC))
		oldRebuild := rebuildMemoryProjection
		oldDetect := detectMemoryMigrationsForAdmin
		committed, scans := 0, 0
		rebuildMemoryProjection = func(string, time.Time) error { committed++; return nil }
		detectMemoryMigrationsForAdmin = func(_ string, source *historySourceManifest) []memoryMigrationFinding {
			scans++
			if scans == 1 {
				path := projectionStateRel
				if source != nil && strings.TrimSpace(source.ProjectionStatePath) != "" {
					path = source.ProjectionStatePath
				}
				return []memoryMigrationFinding{{Path: path, State: memoryErrMigrationRequired, Action: "rebuild"}}
			}
			return nil
		}
		t.Cleanup(func() {
			rebuildMemoryProjection = oldRebuild
			detectMemoryMigrationsForAdmin = oldDetect
		})

		cmd := newMemoryMigrateCommand(opts)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		cmd.SetArgs([]string{"--json"})
		receipt, _ := run(t, []string{"--json"}, cmd, func(writer *failOnceMemoryReceiptWriter) {
			cmd.SetOut(writer)
			cmd.SetErr(&strings.Builder{})
		})
		if receipt.Operation != "migrate" || committed != 1 || len(receipt.Artifacts) != 1 || receipt.Artifacts[0].NewState != "current" {
			t.Fatalf("migrate committed=%d receipt=%+v", committed, receipt)
		}
	})
}

func TestOtherMemoryMutatorsClassifyOutputFailureAfterSuccessfulMutation(t *testing.T) {
	assertFailureReceipt := func(t *testing.T, out string, err error, operation, newState string) memoryOperationReceipt {
		t.Helper()
		if !errors.Is(err, errMemoryReceiptOutput) {
			t.Fatalf("%s writer error = %v", operation, err)
		}
		var receipt memoryOperationReceipt
		if decodeErr := json.Unmarshal([]byte(out), &receipt); decodeErr != nil || receipt.Operation != operation || receipt.ErrorCode != memoryErrStateCorrupt || receipt.FinishedAt.IsZero() {
			t.Fatalf("%s failure receipt=%+v decode=%v\n%s", operation, receipt, decodeErr, out)
		}
		found := newState == ""
		for _, artifact := range receipt.Artifacts {
			if artifact.NewState == "write_outcome_unknown" {
				t.Fatalf("%s lost successful artifact outcome: %+v", operation, receipt.Artifacts)
			}
			found = found || artifact.NewState == newState
		}
		if !found {
			t.Fatalf("%s receipt has no %q outcome: %+v", operation, newState, receipt.Artifacts)
		}
		return receipt
	}
	newJob := func(t *testing.T, brainDir, repoKey, state, suffix string, now time.Time) memoryJob {
		t.Helper()
		job := memoryJob{
			Kind: memoryJobKindProjection, RepoKey: repoKey, SessionID: suffix,
			SessionRef: "conversation-session:" + suffix, InputDigest: "sha256:" + suffix,
			State: state, Attempt: 1, CreatedAt: now, AvailableAt: now,
		}
		job.JobID = memoryJobID(job.RepoKey, job.SessionRef, job.InputDigest, job.Kind)
		if err := saveMemoryJob(brainDir, job); err != nil {
			t.Fatal(err)
		}
		return job
	}

	t.Run("retry", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		job := newJob(t, brainDir, manifest.RepoKey, memoryJobStateRetryable, "retry-output", manifest.GeneratedAt)
		out, err := executeMemoryCommandWithFailOnce(t, newMemoryRetryCommand(opts), job.JobID, "--json")
		assertFailureReceipt(t, out, err, "retry", memoryJobStatePending)
		stored, loadErr := memoryJobByID(brainDir, job.JobID)
		if loadErr != nil || stored.State != memoryJobStatePending {
			t.Fatalf("retry mutation stored=%+v err=%v", stored, loadErr)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			t.Fatal(err)
		}
		job := newJob(t, brainDir, manifest.RepoKey, memoryJobStatePending, "cancel-output", manifest.GeneratedAt)
		out, err := executeMemoryCommandWithFailOnce(t, newMemoryCancelCommand(opts), job.JobID, "--json")
		assertFailureReceipt(t, out, err, "cancel", memoryJobStateCancelled)
		stored, loadErr := memoryJobByID(brainDir, job.JobID)
		if loadErr != nil || stored.State != memoryJobStateCancelled {
			t.Fatalf("cancel mutation stored=%+v err=%v", stored, loadErr)
		}
	})

	t.Run("configure abstracts", func(t *testing.T) {
		t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
		t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
		opts, brainDir := memoryAdminCommandFixture(t)
		local := &privacyCountingAbstractor{}
		oldFactory := memoryAbstractorFactory
		memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return local, nil }
		t.Cleanup(func() { memoryAbstractorFactory = oldFactory })
		out, err := executeMemoryCommandWithFailOnce(t, newMemoryConfigureCommand(opts), "abstracts", "--enable", "--provider", "ollama", "--model", "local", "--json")
		assertFailureReceipt(t, out, err, "configure_abstracts", string(memoryConfigCurrent))
		config, state, loadErr := loadMemoryConfigChecked(brainDir)
		if loadErr != nil || state != memoryConfigCurrent || !config.Abstracts.Enabled {
			t.Fatalf("configure mutation config=%+v state=%s err=%v", config, state, loadErr)
		}
	})

	t.Run("manual abstract", func(t *testing.T) {
		t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
		t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
		opts, brainDir := memoryAdminCommandFixture(t)
		ref := addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC))
		local := &privacyCountingAbstractor{}
		oldFactory := memoryAbstractorFactory
		memoryAbstractorFactory = func(memoryAbstractsConfig) (ConversationAbstractor, error) { return local, nil }
		t.Cleanup(func() { memoryAbstractorFactory = oldFactory })
		out, err := executeMemoryCommandWithFailOnce(t, newMemoryAbstractCommand(opts), ref, "--provider", "ollama", "--model", "local", "--json")
		receipt := assertFailureReceipt(t, out, err, "abstract_generate", memoryJobStateComplete)
		if local.calls.Load() != 1 || len(receipt.JobIDs) != 1 {
			t.Fatalf("manual abstract provider_calls=%d receipt=%+v", local.calls.Load(), receipt)
		}
		stored, loadErr := memoryJobByID(brainDir, receipt.JobIDs[0])
		if loadErr != nil || stored.State != memoryJobStateComplete {
			t.Fatalf("manual abstract mutation stored=%+v err=%v", stored, loadErr)
		}
	})
}
