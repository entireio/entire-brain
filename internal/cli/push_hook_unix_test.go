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
// A dangling symlink is absent to the shell's [ -e ], as it is to disconnect.
func TestPushHookChainRunsTheRightHook(t *testing.T) {
	for name, tc := range map[string]struct {
		hookAt   string // where our hook is, relative to the hooks dir
		others   []string
		dangling []string // dangling symlinks
		want     string   // which of the others runs; "" for none
	}{
		"own backup":                   {hookAt: "pre-push", others: []string{"pre-push.pre-entire-brain", "pre-push.pre-entire.pre-entire-brain"}, want: "pre-push.pre-entire-brain"},
		"in the host's slot":           {hookAt: "pre-push.pre-entire", others: []string{"pre-push.pre-entire.pre-entire-brain", "pre-push.pre-entire-brain"}, want: "pre-push.pre-entire.pre-entire-brain"},
		"moved back by host uninstall": {hookAt: "pre-push", others: []string{"pre-push.pre-entire.pre-entire-brain"}, want: "pre-push.pre-entire.pre-entire-brain"},
		"dangling host slot":           {hookAt: "pre-push", others: []string{"pre-push.pre-entire.pre-entire-brain"}, dangling: []string{"pre-push.pre-entire"}, want: "pre-push.pre-entire.pre-entire-brain"},
		"host slot still there":        {hookAt: "pre-push", others: []string{"pre-push.pre-entire", "pre-push.pre-entire.pre-entire-brain"}},
		"moved aside by the host":      {hookAt: "pre-push.pre-entire", others: []string{"pre-push.pre-entire-brain"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := runChainHook(t, tc.hookAt, tc.others, tc.dangling)
			want := tc.want
			if want != "" {
				want += "\n"
			}
			if got != want {
				t.Fatalf("ran %q, want %q", got, want)
			}
		})
	}
}

// runChainHook writes our hook at hookAt in a fresh directory, a logging hook
// at each of others and a dangling symlink at each of dangling, runs ours, and
// returns the log of the hooks it ran.
func runChainHook(t *testing.T, hookAt string, others, dangling []string) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "ran")
	for _, other := range others {
		script := "#!/bin/sh\necho " + other + " >> '" + log + "'\n"
		if err := os.WriteFile(filepath.Join(dir, other), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, link := range dangling {
		if err := os.Symlink(filepath.Join(dir, "no-such-hook"), filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	hook := filepath.Join(dir, hookAt)
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
	return string(got)
}

// Disconnect decides whether the host's slot is there the way the hook's
// [ -e ] does: through a dangling symlink at the slot the hook runs the file
// behind it, so that is the file disconnect restores.
func TestUninstallPushHookTreatsADanglingSlotAsAbsent(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	slot := primary + ".pre-entire"
	stranded := slot + pushHookBackupSuffix
	userHook := "#!/bin/sh\necho user hook\n"
	writeHook(t, primary, pushHookContent("/managed/entire-brain"))
	writeHook(t, stranded, userHook)
	if err := os.Symlink(filepath.Join(hooksDir, "no-such-hook"), slot); err != nil {
		t.Fatal(err)
	}

	if err := uninstallPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, primary); got != userHook {
		t.Fatalf("the hook the chain line ran must be back in the primary: %q", got)
	}
	requireNoFile(t, stranded)
}
