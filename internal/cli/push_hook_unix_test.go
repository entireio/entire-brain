//go:build unix

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO at a hook path must fail the read, not block it: os.ReadFile on a
// pipe with no writer hangs before the first byte.
func TestInstallPushHookRefusesFIFOHook(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	if err := syscall.Mkfifo(primary, 0o755); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := installPushHook(context.Background(), opts, repoDir)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("install must refuse a FIFO in the hook slot")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("install blocked on the FIFO")
	}
}

// The generated hook's chain line, run for real: it runs its own backup, falls
// back to the hook it ran from the host's slot once the host's uninstall moved
// it back into the primary, and runs nothing from a copy the host moved aside.
func TestPushHookChainRunsTheRightHook(t *testing.T) {
	for name, tc := range map[string]struct {
		hookAt string // where our hook is, relative to the hooks dir
		others []string
		want   string // which of the others runs; "" for none
	}{
		"own backup":                   {hookAt: "pre-push", others: []string{"pre-push.pre-entire-brain", "pre-push.pre-entire.pre-entire-brain"}, want: "pre-push.pre-entire-brain"},
		"in the host's slot":           {hookAt: "pre-push.pre-entire", others: []string{"pre-push.pre-entire.pre-entire-brain", "pre-push.pre-entire-brain"}, want: "pre-push.pre-entire.pre-entire-brain"},
		"moved back by host uninstall": {hookAt: "pre-push", others: []string{"pre-push.pre-entire.pre-entire-brain"}, want: "pre-push.pre-entire.pre-entire-brain"},
		"host slot still there":        {hookAt: "pre-push", others: []string{"pre-push.pre-entire", "pre-push.pre-entire.pre-entire-brain"}},
		"moved aside by the host":      {hookAt: "pre-push.pre-entire", others: []string{"pre-push.pre-entire-brain"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "ran")
			for _, other := range tc.others {
				script := "#!/bin/sh\necho " + other + " >> '" + log + "'\n"
				if err := os.WriteFile(filepath.Join(dir, other), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			hook := filepath.Join(dir, tc.hookAt)
			if err := os.WriteFile(hook, []byte(pushHookContent(filepath.Join(dir, "no-such-plugin"))), 0o755); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(hook, "origin", "url").CombinedOutput(); err != nil {
				t.Fatalf("hook failed: %v %s", err, out)
			}
			got, err := os.ReadFile(log)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if want := tc.want; want != "" {
				want += "\n"
				if string(got) != want {
					t.Fatalf("ran %q, want %q", got, want)
				}
			} else if len(got) != 0 {
				t.Fatalf("ran %q, want nothing", got)
			}
		})
	}
}
