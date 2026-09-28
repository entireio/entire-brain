package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryAdminCommandsRejectMalformedManifestWithoutWriting(t *testing.T) {
	for _, name := range []string{"repair", "migrate"} {
		t.Run(name, func(t *testing.T) {
			opts, brainDir := memoryAdminCommandFixture(t)
			addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 14, 0, 0, 0, time.UTC))
			manifestPath := filepath.Join(brainDir, exportManifestFileName)
			if err := os.WriteFile(manifestPath, []byte("{malformed manifest"), 0o600); err != nil {
				t.Fatal(err)
			}
			var (
				out string
				err error
			)
			if name == "repair" {
				out, err = execute(t, newMemoryRepairCommand(opts), "--json")
			} else {
				out, err = execute(t, newMemoryMigrateCommand(opts), "--json")
			}
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "manifest") {
				t.Fatalf("%s accepted malformed manifest: err=%v out=%q", name, err, out)
			}
			if got, readErr := os.ReadFile(manifestPath); readErr != nil || string(got) != "{malformed manifest" {
				t.Fatalf("%s rewrote malformed manifest: %q err=%v", name, got, readErr)
			}
		})
	}
}

func TestMemoryRepairScopedMissingSessionPreservesManifest(t *testing.T) {
	opts, brainDir := memoryAdminCommandFixture(t)
	addMemoryAdminConversationFixture(t, brainDir, time.Date(2026, 8, 10, 15, 0, 0, 0, time.UTC))
	manifestPath := filepath.Join(brainDir, exportManifestFileName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := newMemoryRepairCommand(opts)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	out, err := execute(t, cmd, "--session-ref", "conversation-session:missing", "--json")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("repair accepted an unknown session: err=%v out=%q", err, out)
	}
	var receipt memoryOperationReceipt
	if decodeErr := json.Unmarshal([]byte(out), &receipt); decodeErr != nil {
		t.Fatalf("decode repair failure receipt: %v\n%s", decodeErr, out)
	}
	if receipt.Operation != "repair" || receipt.SessionRef != "conversation-session:missing" || receipt.FinishedAt.IsZero() || receipt.ErrorCode == "" {
		t.Fatalf("unexpected repair failure receipt: %+v", receipt)
	}
	after, readErr := os.ReadFile(manifestPath)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("scoped repair changed manifest: read=%v", readErr)
	}
}

func TestMemoryMigrateRejectsUnsupportedSchemaWithoutWriting(t *testing.T) {
	opts, brainDir := memoryAdminCommandFixture(t)
	oldDetect := detectMemoryMigrationsForAdmin
	detectMemoryMigrationsForAdmin = func(string, *historySourceManifest) []memoryMigrationFinding {
		return []memoryMigrationFinding{{Path: historyShortTermPath, State: memoryErrUnsupportedVersion, Action: "read-only"}}
	}
	t.Cleanup(func() { detectMemoryMigrationsForAdmin = oldDetect })
	manifestPath := filepath.Join(brainDir, exportManifestFileName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := newMemoryMigrateCommand(opts)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	out, err := execute(t, cmd, "--json")
	if err == nil || !strings.Contains(err.Error(), memoryErrUnsupportedVersion) {
		t.Fatalf("migrate accepted an unsupported schema: err=%v out=%q", err, out)
	}
	var receipt memoryOperationReceipt
	if decodeErr := json.Unmarshal([]byte(out), &receipt); decodeErr != nil {
		t.Fatalf("decode migrate failure receipt: %v\n%s", decodeErr, out)
	}
	if receipt.Operation != "migrate" || receipt.FinishedAt.IsZero() || receipt.ErrorCode != memoryErrUnsupportedVersion {
		t.Fatalf("unexpected migrate failure receipt: %+v", receipt)
	}
	after, readErr := os.ReadFile(manifestPath)
	if readErr != nil || string(after) != string(before) {
		t.Fatalf("unsupported migration changed manifest: read=%v", readErr)
	}
	if _, present, progressErr := loadMemoryMigrationProgress(brainDir); progressErr != nil || present {
		t.Fatalf("unsupported migration wrote progress: present=%v err=%v", present, progressErr)
	}
}
