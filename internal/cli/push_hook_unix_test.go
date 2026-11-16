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

// A file connect would set aside or delete is a hook that runs when a live
// hook links to it: it moves into the link's place, so the chain keeps running
// it and nothing is left at a name of ours to shadow the hook behind ours. The
// user's hook runs after connect, after the host's uninstall where ours joined
// the host's chain, and after disconnect.
func TestInstallPushHookMovesALinkedHookIntoItsLink(t *testing.T) {
	ours := pushHookContent("/managed/entire-brain")
	for name, tc := range map[string]struct {
		files         map[string]string // file name in the hooks dir -> content
		links         map[string]string // symlink name in the hooks dir -> its target
		hostUninstall bool              // the host's uninstall runs after connect
	}{
		"host slot links to our old backup": {
			files: map[string]string{"pre-push": testRunnableHostHook, "pre-push.pre-entire-brain": testUserHook},
			links: map[string]string{"pre-push.pre-entire": "pre-push.pre-entire-brain"},
		},
		"host slot links to our old backup, then the host uninstalls": {
			files: map[string]string{"pre-push": testRunnableHostHook, "pre-push.pre-entire-brain": testUserHook},
			links: map[string]string{"pre-push.pre-entire": "pre-push.pre-entire-brain"}, hostUninstall: true,
		},
		"the hook behind our slot copy links to our old backup": {
			files: map[string]string{"pre-push.pre-entire": ours, "pre-push.pre-entire-brain": testUserHook},
			links: map[string]string{"pre-push.pre-entire.pre-entire-brain": "pre-push.pre-entire-brain"},
		},
		"primary links to the fallback file": {
			files: map[string]string{"pre-push.pre-entire.pre-entire-brain": testUserHook},
			links: map[string]string{"pre-push": "pre-push.pre-entire.pre-entire-brain"},
		},
		"primary with the host's hook links to our old backup": {
			files: map[string]string{"pre-push.pre-entire-brain": testRunnableHostHook, "pre-push.pre-entire": testUserHook},
			links: map[string]string{"pre-push": "pre-push.pre-entire-brain"},
		},
		"primary with the host's hook links to our old backup, then the host uninstalls": {
			files: map[string]string{"pre-push.pre-entire-brain": testRunnableHostHook, "pre-push.pre-entire": testUserHook},
			links: map[string]string{"pre-push": "pre-push.pre-entire-brain"}, hostUninstall: true,
		},
		"host slot links to the name it moves to": {
			files: map[string]string{"pre-push": testRunnableHostHook, "pre-push.pre-entire.pre-entire-brain": testUserHook},
			links: map[string]string{"pre-push.pre-entire": "pre-push.pre-entire.pre-entire-brain"},
		},
		"primary links to the name it moves to": {
			files: map[string]string{"pre-push.pre-entire-brain": testUserHook},
			links: map[string]string{"pre-push": "pre-push.pre-entire-brain"},
		},
		"primary and the fallback file both link to the user's hook": {
			files: map[string]string{"user-hook": testUserHook},
			links: map[string]string{"pre-push": "user-hook", "pre-push.pre-entire.pre-entire-brain": "pre-push"},
		},
		"primary links to the user's hook and our backup name links back to it": {
			files: map[string]string{"user-hook": testUserHook},
			links: map[string]string{"pre-push": "user-hook", "pre-push.pre-entire-brain": "pre-push"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts, repoDir, hooksDir := pushHookHarness(t)
			for file, content := range tc.files {
				writeHook(t, filepath.Join(hooksDir, file), content)
			}
			for link, to := range tc.links {
				if err := os.Symlink(to, filepath.Join(hooksDir, link)); err != nil {
					t.Fatal(err)
				}
			}
			primary := filepath.Join(hooksDir, "pre-push")

			if _, _, err := installPushHook(context.Background(), opts, repoDir); err != nil {
				t.Fatal(err)
			}
			if rotated := rotatedCopiesOf(t, filepath.Join(hooksDir, "pre-push")); len(rotated) != 0 {
				t.Fatalf("a linked hook must not be set aside: %v", rotated)
			}
			if tc.hostUninstall {
				// The host's uninstall removes its hook and moves its slot back.
				if err := os.Remove(primary); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(primary+hostBackupSuffix, primary); err != nil {
					t.Fatal(err)
				}
			}
			requireRunsTheUserHook(t, hooksDir)

			if err := uninstallPushHook(context.Background(), opts, repoDir); err != nil {
				t.Fatal(err)
			}
			requireRunsTheUserHook(t, hooksDir)
			requireNoHookOfOurs(t, hooksDir)
		})
	}
}

// testUserHook prints a line the chain tests look for.
const testUserHook = "#!/bin/sh\necho user hook\n"

// testRunnableHostHook is the host's hook without its call to the entire
// binary, so a test can run it.
var testRunnableHostHook = strings.Replace(testHostChainHook, "entire hooks git pre-push \"$1\"\n", "", 1)

// requireRunsTheUserHook runs the primary hook the way git does and fails
// unless the user's hook runs. Our copies are rewritten first to point at a
// missing plugin: install bakes in the running binary, here the test binary.
func requireRunsTheUserHook(t *testing.T, hooksDir string) {
	t.Helper()
	for _, sibling := range append(pushHookSiblings(hooksDir), filepath.Join(hooksDir, "pre-push")) {
		if data, err := readHookFile(sibling); err == nil && strings.Contains(string(data), pushHookMarker) {
			writeHook(t, sibling, pushHookContent(filepath.Join(hooksDir, "no-such-plugin")))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(hooksDir, "pre-push"), "origin", "url").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "user hook") {
		t.Fatalf("the user's hook must run: err=%v output=%q", err, out)
	}
}

// requireNoHookOfOurs fails if a copy of our hook is left in the hooks dir.
func requireNoHookOfOurs(t *testing.T, hooksDir string) {
	t.Helper()
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if data, err := readHookFile(filepath.Join(hooksDir, entry.Name())); err == nil && strings.Contains(string(data), pushHookMarker) {
			t.Fatalf("%s still holds our hook", entry.Name())
		}
	}
}

// Relinking a hook goes through a temporary symlink beside it. A file that
// already has that name stays as it was, and the next free name is used.
func TestRelinkKeepsAFileAtItsTemporaryName(t *testing.T) {
	dir := t.TempDir()
	hook := filepath.Join(dir, "pre-push")
	path := filepath.Join(dir, "pre-push"+pushHookBackupSuffix)
	taken := filepath.Join(dir, ".entire-brain-relink-pre-push")
	writeHook(t, filepath.Join(dir, "user-hook"), testUserHook)
	writeHook(t, taken, testLeftBehindHook)
	if err := os.Symlink("user-hook", hook); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pre-push", path); err != nil {
		t.Fatal(err)
	}

	if err := relinkToFileOf(hook, path); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, taken); got != testLeftBehindHook {
		t.Fatalf("the file at the temporary name must stay: %q", got)
	}
	if got := readHook(t, hook); got != testUserHook {
		t.Fatalf("the hook must resolve to the user's hook: %q", got)
	}
	requireNoFile(t, path)
	requireNoFile(t, taken+"-1")
}
