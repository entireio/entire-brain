//go:build unix

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// A name the filesystem refuses must fail the rotation, not loop over the
// numbered alternatives, which it refuses the same way. A 240-byte file name
// puts every rotated name over the 255-byte limit.
func TestRotateAsideFailsOnARefusedName(t *testing.T) {
	path := filepath.Join(t.TempDir(), strings.Repeat("h", 240))
	writeHook(t, path, testLeftBehindHook)

	done := make(chan error, 1)
	go func() {
		_, err := rotateAside(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("rotating to a name over the length limit must fail")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("rotateAside kept trying names the filesystem refuses")
	}
	if got := readHook(t, path); got != testLeftBehindHook {
		t.Fatalf("the file must stay where it was: %q", got)
	}
}

// A host slot that is a symlink to our backup of the primary is the hook
// being joined: the backup stays, the hook behind ours still resolves, and
// disconnect keeps the file the restored link points at.
func TestInstallPushHookKeepsTheBackupAHostSlotLinksTo(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	slot := primary + ".pre-entire"
	userHook := "#!/bin/sh\necho user hook\n"
	writeHook(t, primary, testHostChainHook)
	writeHook(t, primary+pushHookBackupSuffix, userHook)
	if err := os.Symlink("pre-push"+pushHookBackupSuffix, slot); err != nil {
		t.Fatal(err)
	}

	if _, _, err := installPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, slot); !strings.Contains(got, pushHookMarker) {
		t.Fatalf("our hook must join the host's slot: %q", got)
	}
	if got := readHook(t, slot+pushHookBackupSuffix); got != userHook {
		t.Fatalf("the hook behind ours must still resolve: %q", got)
	}

	if err := uninstallPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, slot); got != userHook {
		t.Fatalf("the host's slot must resolve to the user's hook again: %q", got)
	}
	if got := readHook(t, primary); got != testHostChainHook {
		t.Fatalf("the host's hook must stay in the primary: %q", got)
	}
}

// With the primary gone, our copy in the host's slot moves back into it. When
// the hook behind that copy is a symlink to our backup of the primary, the
// backup is that hook: it stays, and disconnect restores it.
func TestInstallPushHookKeepsTheBackupTheHookBehindItsSlotCopyLinksTo(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	slot := primary + ".pre-entire"
	userHook := "#!/bin/sh\necho user hook\n"
	writeHook(t, slot, pushHookContent("/managed/entire-brain"))
	writeHook(t, primary+pushHookBackupSuffix, userHook)
	if err := os.Symlink("pre-push"+pushHookBackupSuffix, slot+pushHookBackupSuffix); err != nil {
		t.Fatal(err)
	}

	if _, _, err := installPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, primary); !strings.Contains(got, pushHookMarker) {
		t.Fatalf("our copy must move back into the primary: %q", got)
	}
	if got := readHook(t, primary+pushHookBackupSuffix); got != userHook {
		t.Fatalf("the hook behind ours must stay where the chain line runs it: %q", got)
	}

	if err := uninstallPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, primary); got != userHook {
		t.Fatalf("disconnect must put the user's hook back in the primary: %q", got)
	}
}

// A file connect would set aside stays when the primary is a symlink to it:
// then it is the hook in the primary, and ours chains to it. For the fallback
// file our hook wraps it; for our old backup the host's hook stays in the
// primary and ours joins the host's chain. Disconnect puts the primary back.
func TestInstallPushHookKeepsAFileThePrimaryLinksTo(t *testing.T) {
	const userHook = "#!/bin/sh\necho user hook\n"
	for name, tc := range map[string]struct {
		linkTo     string // the primary links here, relative to the hooks dir
		linked     string // the linked file's content
		slot       string // the host's slot, if set
		wantBehind string // where the linked file must still resolve after connect
	}{
		"fallback file": {linkTo: "pre-push.pre-entire.pre-entire-brain", linked: userHook, wantBehind: "pre-push.pre-entire-brain"},
		"old backup":    {linkTo: "pre-push.pre-entire-brain", linked: testHostChainHook, slot: userHook, wantBehind: "pre-push"},
	} {
		t.Run(name, func(t *testing.T) {
			opts, repoDir, hooksDir := pushHookHarness(t)
			primary := filepath.Join(hooksDir, "pre-push")
			writeHook(t, filepath.Join(hooksDir, tc.linkTo), tc.linked)
			writeHookIfSet(t, primary+hostBackupSuffix, tc.slot)
			if err := os.Symlink(tc.linkTo, primary); err != nil {
				t.Fatal(err)
			}

			if _, _, err := installPushHook(context.Background(), opts, repoDir); err != nil {
				t.Fatal(err)
			}
			if got := readHook(t, filepath.Join(hooksDir, tc.wantBehind)); got != tc.linked {
				t.Fatalf("%s must still resolve to the linked hook: %q", tc.wantBehind, got)
			}
			if rotated := rotatedCopiesOf(t, filepath.Join(hooksDir, tc.linkTo)); len(rotated) != 0 {
				t.Fatalf("the linked hook must not be set aside: %v", rotated)
			}

			if err := uninstallPushHook(context.Background(), opts, repoDir); err != nil {
				t.Fatal(err)
			}
			if got := readHook(t, primary); got != tc.linked {
				t.Fatalf("disconnect must leave the primary resolving to the linked hook: %q", got)
			}
		})
	}
}
