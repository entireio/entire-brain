package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func receiptHasProspectiveTransition(receipt memoryOperationReceipt) bool {
	for _, artifact := range receipt.Artifacts {
		if strings.HasPrefix(artifact.NewState, "would_") && artifact.NewState != artifact.PriorState {
			return true
		}
	}
	return false
}

func TestMemoryMaintenanceDryRunsReportProspectiveArtifactTransitions(t *testing.T) {
	t.Run("repair distinguishes needed rebuild from clean no-op", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC))

		cleanOut, err := execute(t, newMemoryRepairCommand(opts), "--dry-run", "--json")
		if err != nil {
			t.Fatalf("clean repair dry-run: %v\n%s", err, cleanOut)
		}
		var clean struct {
			Receipt         memoryOperationReceipt `json:"receipt"`
			RebuildRequired bool                   `json:"rebuild_required"`
			WouldRebuild    bool                   `json:"would_rebuild"`
			Rebuilt         bool                   `json:"rebuilt"`
		}
		if err := json.Unmarshal([]byte(cleanOut), &clean); err != nil || clean.RebuildRequired || clean.WouldRebuild || clean.Rebuilt || receiptHasProspectiveTransition(clean.Receipt) {
			t.Fatalf("clean dry-run payload=%+v err=%v\n%s", clean, err, cleanOut)
		}

		manifest, err := loadBrainManifest(brainDir)
		if err != nil || manifest.Sources == nil || manifest.Sources.History == nil {
			t.Fatalf("load repair fixture: manifest=%+v err=%v", manifest, err)
		}
		indexPath := filepath.Join(brainDir, filepath.FromSlash(manifest.Sources.History.IndexPath))
		if err := os.Remove(indexPath); err != nil {
			t.Fatalf("make repair necessary: %v", err)
		}
		dirtyOut, err := execute(t, newMemoryRepairCommand(opts), "--dry-run", "--json")
		if err != nil {
			t.Fatalf("dirty repair dry-run: %v\n%s", err, dirtyOut)
		}
		var dirty struct {
			Receipt         memoryOperationReceipt `json:"receipt"`
			RebuildRequired bool                   `json:"rebuild_required"`
			WouldRebuild    bool                   `json:"would_rebuild"`
			Rebuilt         bool                   `json:"rebuilt"`
		}
		if err := json.Unmarshal([]byte(dirtyOut), &dirty); err != nil || !dirty.Receipt.DryRun || !dirty.RebuildRequired || !dirty.WouldRebuild || dirty.Rebuilt || !receiptHasProspectiveTransition(dirty.Receipt) {
			t.Fatalf("dirty dry-run payload=%+v err=%v\n%s", dirty, err, dirtyOut)
		}
		if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
			t.Fatalf("repair dry-run changed the deliberately missing index: %v", err)
		}
	})

	t.Run("rebuild plans replacement without staging", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 10, 1, 0, 0, time.UTC))
		manifestBefore, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
		if err != nil {
			t.Fatal(err)
		}
		stagingPath := filepath.Join(brainDir, filepath.FromSlash(historyStagingDir))
		stagingBefore, err := os.ReadDir(stagingPath)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out, err := execute(t, newMemoryRebuildCommand(opts), "--all", "--dry-run", "--json")
		if err != nil {
			t.Fatalf("rebuild dry-run: %v\n%s", err, out)
		}
		var receipt memoryOperationReceipt
		if err := json.Unmarshal([]byte(out), &receipt); err != nil || !receipt.DryRun || !receiptHasProspectiveTransition(receipt) {
			t.Fatalf("rebuild dry-run receipt=%+v err=%v\n%s", receipt, err, out)
		}
		manifestAfter, err := os.ReadFile(filepath.Join(brainDir, exportManifestFileName))
		if err != nil || string(manifestAfter) != string(manifestBefore) {
			t.Fatalf("rebuild dry-run changed manifest: err=%v", err)
		}
		stagingAfter, err := os.ReadDir(stagingPath)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if len(stagingAfter) != len(stagingBefore) {
			t.Fatalf("rebuild dry-run changed staging entries: before=%d after=%d", len(stagingBefore), len(stagingAfter))
		}
		for i := range stagingBefore {
			if stagingBefore[i].Name() != stagingAfter[i].Name() {
				t.Fatalf("rebuild dry-run changed staging entries: before=%q after=%q", stagingBefore[i].Name(), stagingAfter[i].Name())
			}
		}
	})

	t.Run("migrate names every planned schema transition", func(t *testing.T) {
		opts, brainDir := memoryAdminCommandFixture(t)
		oldDetect := detectMemoryMigrationsForAdmin
		oldRebuild := rebuildMemoryProjection
		rebuildCalls := 0
		detectMemoryMigrationsForAdmin = func(string, *historySourceManifest) []memoryMigrationFinding {
			return []memoryMigrationFinding{
				{Path: projectionStateRel, State: memoryErrMigrationRequired, Action: "rebuild"},
				{Path: memoryJobsDirRel + "/legacy.json", State: memoryErrMigrationRequired, Action: "rewrite"},
			}
		}
		rebuildMemoryProjection = func(string, time.Time) error { rebuildCalls++; return nil }
		t.Cleanup(func() {
			detectMemoryMigrationsForAdmin = oldDetect
			rebuildMemoryProjection = oldRebuild
		})

		out, err := execute(t, newMemoryMigrateCommand(opts), "--dry-run", "--json")
		if err != nil {
			t.Fatalf("migrate dry-run: %v\n%s", err, out)
		}
		var payload struct {
			Receipt  memoryOperationReceipt `json:"receipt"`
			Migrated int                    `json:"migrated"`
		}
		if err := json.Unmarshal([]byte(out), &payload); err != nil || !payload.Receipt.DryRun || payload.Migrated != 0 || len(payload.Receipt.Artifacts) != 2 {
			t.Fatalf("migrate dry-run payload=%+v err=%v\n%s", payload, err, out)
		}
		for _, artifact := range payload.Receipt.Artifacts {
			if artifact.PriorState != memoryErrMigrationRequired || artifact.NewState != "would_be_current" {
				t.Fatalf("untruthful planned migration artifact: %+v", artifact)
			}
		}
		if rebuildCalls != 0 {
			t.Fatalf("migrate dry-run rebuilt projection %d times", rebuildCalls)
		}
		if _, present, err := loadMemoryMigrationProgress(brainDir); err != nil || present {
			t.Fatalf("migrate dry-run wrote progress: present=%v err=%v", present, err)
		}
	})
}
