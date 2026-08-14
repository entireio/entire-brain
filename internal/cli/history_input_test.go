package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHistorySessionInventoryDeterministic(t *testing.T) {
	brainDir := t.TempDir()
	mainDir := filepath.Join(brainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(mainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"20260809T100000Z_middle.jsonl",
		"20260809T120000Z_newest.json",
		"20260809T080000Z_oldest.txt",
		"ignored.bin",
	} {
		if err := os.WriteFile(filepath.Join(mainDir, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"sessions/main/20260809T120000Z_newest.json",
		"sessions/main/20260809T100000Z_middle.jsonl",
		"sessions/main/20260809T080000Z_oldest.txt",
	}
	for attempt := 0; attempt < 2; attempt++ {
		inventory, err := collectHistorySessionInventory(context.Background(), brainDir)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, file := range inventory.files {
			got = append(got, file.Rel)
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("inventory order = %v, want %v", got, want)
		}
	}
}

func TestHistorySessionInventoryCancellation(t *testing.T) {
	brainDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(brainDir, exportSessionsDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := collectHistorySessionInventory(ctx, brainDir); !errors.Is(err, context.Canceled) {
		t.Fatalf("inventory error = %v, want context cancellation", err)
	}
}

func TestHistoryProjectionRejectsExternalTranscriptSymlinkWithoutPublication(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires privileges on many Windows builders")
	}
	brainDir, _, now := historyProjectionFixture(t)
	manifestPath := filepath.Join(brainDir, exportManifestFileName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.jsonl")
	canary := "Decision: external symlink canary must never be indexed.\n"
	if err := os.WriteFile(external, []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(brainDir, exportSessionsDirectory, "main", "external.jsonl")
	if err := os.Symlink(external, alias); err != nil {
		t.Fatal(err)
	}
	_, err = writeBrainHistoryIndexAndSource(brainDir, now.Add(time.Minute), nil)
	if !errors.Is(err, errHistorySessionInventoryDegraded) || memoryErrorCode(err) != memoryErrSessionInventory {
		t.Fatalf("projection error = %v, code=%q", err, memoryErrorCode(err))
	}
	after, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("failed unsafe rebuild changed the published manifest")
	}
	_, index := activeHistoryProjection(t, brainDir)
	for _, record := range index.Records {
		if strings.Contains(record.Summary, "external symlink canary") {
			t.Fatalf("external canary leaked into active history: %+v", record)
		}
	}
}

func TestHistoryProjectionRejectsLeafSwapWithoutPublication(t *testing.T) {
	brainDir, rel, now := historyProjectionFixture(t)
	manifestPath := filepath.Join(brainDir, exportManifestFileName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(brainDir, filepath.FromSlash(rel))
	changedTime := now.Add(2 * time.Hour)
	if err := os.Chtimes(target, changedTime, changedTime); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	originalHook := beforeHistorySessionFileOpen
	swapped := false
	beforeHistorySessionFileOpen = func(openRel string) {
		if swapped || openRel != rel {
			return
		}
		swapped = true
		oldPath := target + ".old"
		if renameErr := os.Rename(target, oldPath); renameErr != nil {
			t.Fatalf("swap transcript: %v", renameErr)
		}
		if writeErr := os.WriteFile(target, body, 0o600); writeErr != nil {
			t.Fatalf("replace transcript: %v", writeErr)
		}
		if timeErr := os.Chtimes(target, oldInfo.ModTime(), oldInfo.ModTime()); timeErr != nil {
			t.Fatalf("restore replacement timestamp: %v", timeErr)
		}
	}
	t.Cleanup(func() { beforeHistorySessionFileOpen = originalHook })
	_, err = writeBrainHistoryIndexAndSource(brainDir, now.Add(time.Minute), nil)
	if !swapped || !errors.Is(err, errHistorySessionInventoryDegraded) {
		t.Fatalf("swapped=%v projection error=%v", swapped, err)
	}
	after, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("leaf-swap refusal changed the published manifest")
	}
}

func TestHistorySessionInventoryCeilingPreservesProjectionAndOverlay(t *testing.T) {
	t.Run("full projection", func(t *testing.T) {
		brainDir, _, now := historyProjectionFixture(t)
		manifestPath := filepath.Join(brainDir, exportManifestFileName)
		before, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		oldLimit := historySessionInventoryMaxEntries
		historySessionInventoryMaxEntries = 1
		t.Cleanup(func() { historySessionInventoryMaxEntries = oldLimit })
		_, err = writeBrainHistoryIndexAndSource(brainDir, now.Add(time.Minute), nil)
		if !errors.Is(err, errHistorySessionInventoryDegraded) || memoryErrorCode(err) != memoryErrSessionInventory {
			t.Fatalf("ceiling error = %v, code=%q", err, memoryErrorCode(err))
		}
		after, readErr := os.ReadFile(manifestPath)
		if readErr != nil || !bytes.Equal(after, before) {
			t.Fatalf("manifest changed after ceiling refusal: err=%v", readErr)
		}
	})

	t.Run("short-term overlay", func(t *testing.T) {
		brainDir, _, _ := shortTermFixture(t)
		buildShortTerm(t, brainDir)
		overlayPath := filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath))
		before, err := os.ReadFile(overlayPath)
		if err != nil {
			t.Fatal(err)
		}
		oldLimit := historySessionInventoryMaxEntries
		historySessionInventoryMaxEntries = 1
		t.Cleanup(func() { historySessionInventoryMaxEntries = oldLimit })
		err = withBrainWriteLock(brainDir, func() error {
			_, buildErr := buildHistoryShortTermLocked(brainDir, time.Now().UTC())
			return buildErr
		})
		if !errors.Is(err, errHistorySessionInventoryDegraded) {
			t.Fatalf("delta ceiling error = %v", err)
		}
		after, readErr := os.ReadFile(overlayPath)
		if readErr != nil || !bytes.Equal(after, before) {
			t.Fatalf("overlay changed after ceiling refusal: err=%v", readErr)
		}
	})
}

func TestShortTermDeltaMembershipRacePreservesOverlay(t *testing.T) {
	for _, racedName := range []string{"20260809T130000Z_raced.jsonl", "irrelevant.bin"} {
		t.Run(racedName, func(t *testing.T) {
			brainDir, _, _ := shortTermFixture(t)
			buildShortTerm(t, brainDir)
			overlayPath := filepath.Join(brainDir, filepath.FromSlash(historyShortTermPath))
			before, err := os.ReadFile(overlayPath)
			if err != nil {
				t.Fatal(err)
			}
			racedPath := filepath.Join(brainDir, exportSessionsDirectory, "main", racedName)
			originalHook := beforeHistoryShortTermInventoryRecheck
			landed := false
			beforeHistoryShortTermInventoryRecheck = func() {
				if landed {
					return
				}
				landed = true
				if writeErr := os.WriteFile(racedPath, []byte("{}\n"), 0o600); writeErr != nil {
					t.Fatalf("land raced transcript-tree leaf: %v", writeErr)
				}
			}
			t.Cleanup(func() { beforeHistoryShortTermInventoryRecheck = originalHook })
			err = withBrainWriteLock(brainDir, func() error {
				_, buildErr := buildHistoryShortTermLocked(brainDir, time.Now().UTC())
				return buildErr
			})
			if !landed || err == nil || !strings.Contains(err.Error(), memoryErrSourceStale) {
				t.Fatalf("landed=%v delta error=%v", landed, err)
			}
			after, readErr := os.ReadFile(overlayPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("membership-race refusal changed the published overlay")
			}
		})
	}
}

func TestConversationExpansionRejectsSourceSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires privileges on many Windows builders")
	}
	brainDir, rel, _ := historyProjectionFixture(t)
	_, index := activeHistoryProjection(t, brainDir)
	var exchange historyRecord
	for _, record := range index.Records {
		if record.Kind == conversationKind && record.Path == rel {
			exchange = record
			break
		}
	}
	if exchange.ID == "" {
		t.Fatal("fixture did not produce a conversation exchange")
	}
	target := filepath.Join(brainDir, filepath.FromSlash(rel))
	external := filepath.Join(t.TempDir(), "external.jsonl")
	if err := os.WriteFile(external, []byte("external expansion canary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, target); err != nil {
		t.Fatal(err)
	}
	if _, err := expandConversationExchange(brainDir, exchange); !errors.Is(err, errHistorySessionInventoryDegraded) {
		t.Fatalf("expansion error = %v, want safe inventory refusal", err)
	}
}
