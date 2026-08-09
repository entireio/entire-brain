//go:build !windows

package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestHistorySessionInventoryRejectsFIFOAndSocketWithoutBlocking(t *testing.T) {
	for _, kind := range []string{"fifo", "socket"} {
		t.Run(kind, func(t *testing.T) {
			brainDir, err := os.MkdirTemp("/tmp", "eb-history-input-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(brainDir) })
			dir := filepath.Join(brainDir, exportSessionsDirectory, "main")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			leaf := filepath.Join(dir, "hostile.jsonl")
			var listener net.Listener
			switch kind {
			case "fifo":
				if err := unix.Mkfifo(leaf, 0o600); err != nil {
					t.Fatal(err)
				}
			case "socket":
				listener, err = net.Listen("unix", leaf)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
			}

			done := make(chan error, 1)
			go func() {
				inventory, err := collectHistorySessionInventory(context.Background(), brainDir)
				if inventory != nil {
					_ = inventory.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, errHistorySessionInventoryDegraded) || memoryErrorCode(err) != memoryErrSessionInventory {
					t.Fatalf("inventory error = %v, code=%q", err, memoryErrorCode(err))
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("inventory blocked on %s", kind)
			}
		})
	}
}

func TestHistorySessionOpenDoesNotBlockOnRegularToFIFOSwap(t *testing.T) {
	brainDir, err := os.MkdirTemp("/tmp", "eb-history-swap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(brainDir) })
	dir := filepath.Join(brainDir, exportSessionsDirectory, "main")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inventory, err := collectHistorySessionInventory(context.Background(), brainDir)
	if err != nil {
		t.Fatal(err)
	}
	defer inventory.Close()
	if len(inventory.files) != 1 {
		t.Fatalf("inventory files = %d", len(inventory.files))
	}
	originalHook := beforeHistorySessionDescriptorOpen
	swapped := false
	var swapErr error
	beforeHistorySessionDescriptorOpen = func(rel string) {
		if swapped || rel != inventory.files[0].Rel {
			return
		}
		swapped = true
		if swapErr = os.Rename(target, target+".old"); swapErr != nil {
			return
		}
		swapErr = unix.Mkfifo(target, 0o600)
	}
	t.Cleanup(func() { beforeHistorySessionDescriptorOpen = originalHook })

	done := make(chan error, 1)
	go func() {
		f, openErr := inventory.open(context.Background(), inventory.files[0])
		if f != nil {
			_ = f.Close()
		}
		done <- openErr
	}()
	select {
	case openErr := <-done:
		if swapErr != nil {
			t.Fatalf("swap transcript with FIFO: %v", swapErr)
		}
		if !swapped || !errors.Is(openErr, errHistorySessionInventoryDegraded) {
			t.Fatalf("swapped=%v open error=%v", swapped, openErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("descriptor open blocked on raced FIFO")
	}
}

func TestConversationExpansionRejectsBrainRootSwapAfterRead(t *testing.T) {
	parent := t.TempDir()
	brainDir := filepath.Join(parent, "brain")
	rel := "sessions/main/20260809T000000Z_root-swap.jsonl"
	body := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"private request"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"private response"}]}}`,
		"",
	}, "\n")
	full := filepath.Join(brainDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	record := historyRecord{Path: rel, Line: 1, EndLine: 2, SourceDigest: conversationDigest([]byte(body))}

	originalHook := beforeCanonicalHistoryTranscriptFinish
	beforeCanonicalHistoryTranscriptFinish = func() {
		detached := filepath.Join(parent, "detached-brain")
		if err := os.Rename(brainDir, detached); err != nil {
			t.Fatalf("detach Brain root: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(brainDir, "sessions", "main"), 0o700); err != nil {
			t.Fatalf("replace Brain root: %v", err)
		}
	}
	t.Cleanup(func() { beforeCanonicalHistoryTranscriptFinish = originalHook })

	expansion, err := expandConversationExchange(brainDir, record)
	if err == nil || !errors.Is(err, errHistorySessionInventoryDegraded) {
		t.Fatalf("Brain root swap must fail closed: expansion=%+v err=%v", expansion, err)
	}
	if expansion.Request != "" || expansion.Response != "" {
		t.Fatalf("Brain root swap exposed transcript bytes: %+v", expansion)
	}
}
