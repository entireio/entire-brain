package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// hooksDirRunner mimics the HARDENED runner: rev-parse answers normally, and
// the core.hooksPath values always end with the command-line pin, optionally
// preceded by a file-sourced value like the one husky writes.
type hooksDirRunner struct {
	gitDir        string
	fileHooksPath string
}

func (r hooksDirRunner) Run(_ context.Context, _, _ string, args ...string) ([]byte, []byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "--git-common-dir"):
		return []byte(r.gitDir + "\n"), nil, nil
	case strings.Contains(joined, "core.hooksPath"):
		out := ""
		if r.fileHooksPath != "" {
			out += r.fileHooksPath + "\n"
		}
		out += gitHooksDisabledPath + "\n"
		return []byte(out), nil, nil
	default:
		return nil, nil, nil
	}
}

const testHostHook = "#!/bin/sh\n# Entire session tracking hook\nentire hooks git pre-push \"$1\"\n"

func pushHookHarness(t *testing.T) (Options, string, string) {
	t.Helper()
	repoDir := t.TempDir()
	gitDir := filepath.Join(repoDir, ".git")
	hooksDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{Version: "test", Runner: hooksDirRunner{gitDir: gitDir}}
	return opts, repoDir, hooksDir
}

// The hardened runner pins core.hooksPath to the disabled sentinel on every
// git call, so the resolver must never trust `--git-path hooks` — this is the
// regression Bugbot caught: install resolved the sentinel and silently
// no-opped everywhere.
func TestGitHooksDirIgnoresTheHardenPin(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	dir, inRepo, err := gitHooksDir(context.Background(), opts, repoDir)
	if err != nil || !inRepo {
		t.Fatalf("inRepo=%v err=%v", inRepo, err)
	}
	if dir != hooksDir {
		t.Fatalf("pinned-only origins must resolve to the default hooks dir, got %q", dir)
	}
}

func TestGitHooksDirHonorsFileConfiguredHooksPath(t *testing.T) {
	repoDir := t.TempDir()
	gitDir := filepath.Join(repoDir, ".git")
	opts := Options{Version: "test", Runner: hooksDirRunner{gitDir: gitDir, fileHooksPath: ".husky"}}
	dir, inRepo, err := gitHooksDir(context.Background(), opts, repoDir)
	if err != nil || !inRepo {
		t.Fatalf("inRepo=%v err=%v", inRepo, err)
	}
	if dir != filepath.Join(repoDir, ".husky") {
		t.Fatalf("relative core.hooksPath must resolve against the worktree, got %q", dir)
	}
}

// Real git, real ExecRunner, real hardening: the one test that cannot be
// blinded by a stubbed runner.
func TestInstallPushHookAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repoDir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repoDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	opts := Options{Version: "test", Runner: ExecRunner{}}

	state, path, err := installPushHook(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if state != pushHookInstalled {
		t.Fatalf("state=%v", state)
	}
	resolvedRepo, _ := filepath.EvalSymlinks(repoDir)
	resolvedPath, _ := filepath.EvalSymlinks(filepath.Dir(path))
	if resolvedPath != filepath.Join(resolvedRepo, ".git", "hooks") {
		t.Fatalf("hook must land in the real hooks dir, got %q", path)
	}
	if !strings.Contains(readHook(t, path), pushHookMarker) {
		t.Fatal("hook content missing marker")
	}
}

func readHook(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestInstallPushHookIntoEmptySlot(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)

	state, path, err := installPushHook(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if state != pushHookInstalled || path != filepath.Join(hooksDir, "pre-push") {
		t.Fatalf("state=%v path=%v", state, path)
	}
	content := readHook(t, path)
	if !strings.Contains(content, pushHookMarker) || !strings.Contains(content, "--event push") {
		t.Fatalf("hook content missing marker or event: %q", content)
	}
	if !strings.Contains(content, "# Chain:") {
		t.Fatalf("chain line is unconditional ([ -x ]-guarded): %q", content)
	}
	if _, err := os.Stat(path + pushHookBackupSuffix); !os.IsNotExist(err) {
		t.Fatalf("empty slot must not create a backup: %v", err)
	}
	// Windows file modes carry no exec bit; git runs hooks there regardless.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0o100 == 0 {
			t.Fatalf("hook must be executable: %v %v", info, err)
		}
	}

	// Idempotent rerun.
	state, _, err = installPushHook(context.Background(), opts, repoDir)
	if err != nil || state != pushHookUnchanged {
		t.Fatalf("rerun state=%v err=%v", state, err)
	}
}

func TestInstallPushHookChainsExistingHook(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	if err := os.WriteFile(primary, []byte(testHostHook), 0o755); err != nil {
		t.Fatal(err)
	}

	state, _, err := installPushHook(context.Background(), opts, repoDir)
	if err != nil || state != pushHookInstalled {
		t.Fatalf("state=%v err=%v", state, err)
	}
	content := readHook(t, primary)
	if !strings.Contains(content, pushHookMarker) || !strings.Contains(content, "# Chain:") {
		t.Fatalf("primary must be ours with a chain: %q", content)
	}
	if got := readHook(t, primary+pushHookBackupSuffix); got != testHostHook {
		t.Fatalf("backup must hold the original hook verbatim, got %q", got)
	}
	if !strings.Contains(content, `exec "$backup" "$@"`) {
		t.Fatalf("chain must exec the backup with original args: %q", content)
	}
}

func TestInstallPushHookLeavesHostRewrapAlone(t *testing.T) {
	// The host CLI re-claimed the primary slot and moved our hook to ITS
	// backup name; our line still runs via its chain. Install must not touch
	// the primary — re-claiming would ping-pong with the host's reconcile.
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	if err := os.WriteFile(primary, []byte(testHostHook), 0o755); err != nil {
		t.Fatal(err)
	}
	moved := primary + ".pre-entire"
	ours := pushHookContent("/managed/entire-brain")
	if err := os.WriteFile(moved, []byte(ours), 0o755); err != nil {
		t.Fatal(err)
	}

	state, path, err := installPushHook(context.Background(), opts, repoDir)
	if err != nil || state != pushHookChainedIn || path != moved {
		t.Fatalf("state=%v path=%v err=%v", state, path, err)
	}
	if got := readHook(t, primary); got != testHostHook {
		t.Fatalf("primary must stay the host's: %q", got)
	}
}

func TestUninstallPushHookRestoresBackup(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	original := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(primary, []byte(original), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}

	if err := uninstallPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, primary); got != original {
		t.Fatalf("uninstall must restore the original hook, got %q", got)
	}
	if _, err := os.Stat(primary + pushHookBackupSuffix); !os.IsNotExist(err) {
		t.Fatalf("backup must be gone after restore: %v", err)
	}

	// Idempotent on a clean slate too.
	if err := uninstallPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshPushHookLeavesUninstalledRepoUntouched(t *testing.T) {
	// A bare setup after a disconnect (or on a never-connected repo) must not
	// resurrect or freshly install the hook — install belongs to connect.
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	if err := os.WriteFile(primary, []byte(testHostHook), 0o755); err != nil {
		t.Fatal(err)
	}

	state, _, err := refreshPushHook(context.Background(), opts, repoDir)
	if err != nil || state != pushHookAbsent {
		t.Fatalf("state=%v err=%v", state, err)
	}
	if got := readHook(t, primary); got != testHostHook {
		t.Fatalf("refresh must not touch a repo without our hook: %q", got)
	}
}

func TestRefreshPushHookUpdatesInstalledHook(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	stale := "#!/bin/sh\n" + pushHookMarker + "\n'/old/path' memory notify --event push &\n"
	if err := os.WriteFile(primary, []byte(stale), 0o755); err != nil {
		t.Fatal(err)
	}

	state, _, err := refreshPushHook(context.Background(), opts, repoDir)
	if err != nil || state != pushHookInstalled {
		t.Fatalf("state=%v err=%v", state, err)
	}
	if got := readHook(t, primary); strings.Contains(got, "/old/path") {
		t.Fatalf("refresh must rewrite the stale plugin path: %q", got)
	}
}

func TestUninstallAfterHostRewrapDeletesBothOurCopies(t *testing.T) {
	// State after the host CLI re-wraps us: host's fresh hook in the primary
	// slot (chains to .pre-entire = our moved copy), and our old backup still
	// holding the ORIGINAL host hook. Uninstall must delete our moved copy AND
	// that stale backup — renaming the stale copy into the live chain would
	// run the host's pre-push twice. The host's chain call is [ -x ]-guarded,
	// so the removed chain target is a clean no-op.
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	if err := os.WriteFile(primary, []byte(testHostHook), 0o755); err != nil {
		t.Fatal(err)
	}
	moved := primary + ".pre-entire"
	if err := os.WriteFile(moved, []byte(pushHookContent("/managed/entire-brain")), 0o755); err != nil {
		t.Fatal(err)
	}
	staleBackup := primary + pushHookBackupSuffix
	if err := os.WriteFile(staleBackup, []byte(testHostHook), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := uninstallPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, primary); got != testHostHook {
		t.Fatalf("primary must stay the host's: %q", got)
	}
	for _, gone := range []string{moved, staleBackup} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s must be deleted, stat err=%v", gone, err)
		}
	}
}

func TestResolveHooksPathValueExpandsTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	got, err := resolveHooksPathValue("~/.githooks", "/repo")
	if err != nil || got != filepath.Join(home, ".githooks") {
		t.Fatalf("got %q err=%v", got, err)
	}
	if _, err := resolveHooksPathValue("~someone/.githooks", "/repo"); err == nil {
		t.Fatal("~user expansion must be refused, not misresolved")
	}
	if got, err := resolveHooksPathValue(".husky", "/repo"); err != nil || got != filepath.Join("/repo", ".husky") {
		t.Fatalf("relative: got %q err=%v", got, err)
	}
}

func TestInstallPushHookRefusesSharedHooksDir(t *testing.T) {
	// A global core.hooksPath (~/.githooks style) serves every repo at once: a
	// hook written there fires for repos that never connected, and uninstall
	// on one repo would strip it for all. Both directions must refuse.
	repoDir := t.TempDir()
	shared := filepath.Join(t.TempDir(), "githooks")
	gitDir := filepath.Join(repoDir, ".git")
	opts := Options{Version: "test", Runner: hooksDirRunner{gitDir: gitDir, fileHooksPath: shared}}

	state, path, err := installPushHook(context.Background(), opts, repoDir)
	if err != nil || state != pushHookSharedDir {
		t.Fatalf("state=%v path=%v err=%v", state, path, err)
	}
	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Fatalf("refusal must not create the shared dir: %v", err)
	}
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(shared, "pre-push")
	if err := os.WriteFile(hook, []byte(testHostHook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := uninstallPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, hook); got != testHostHook {
		t.Fatalf("uninstall must not touch a shared dir: %q", got)
	}
	if state, _, err := refreshPushHook(context.Background(), opts, repoDir); err != nil || state != pushHookSharedDir {
		t.Fatalf("refresh state=%v err=%v", state, err)
	}
}

func TestInstallPushHookAllowsWorktreeLocalHooksPath(t *testing.T) {
	repoDir := t.TempDir()
	gitDir := filepath.Join(repoDir, ".git")
	opts := Options{Version: "test", Runner: hooksDirRunner{gitDir: gitDir, fileHooksPath: ".husky"}}

	state, path, err := installPushHook(context.Background(), opts, repoDir)
	if err != nil || state != pushHookInstalled {
		t.Fatalf("state=%v err=%v", state, err)
	}
	if path != filepath.Join(repoDir, ".husky", "pre-push") {
		t.Fatalf("hook must land in the worktree-local hooks dir, got %q", path)
	}
}

// testHostChainHook is the shape of the host CLI's pre-push after it backed up
// a hook it found: its marker, its own line, then the chain call that runs
// pre-push.pre-entire by that literal name.
const testHostChainHook = "#!/bin/sh\n# Entire CLI hooks\nentire hooks git pre-push \"$1\"\n" +
	"# Chain: run pre-existing hook\n_entire_hook_dir=\"$(dirname \"$0\")\"\n" +
	"if [ -x \"$_entire_hook_dir/pre-push.pre-entire\" ]; then\n" +
	"    \"$_entire_hook_dir/pre-push.pre-entire\" \"$@\"\nfi\n"

// testHuskyStub is what husky 9 writes to .husky/_/<hook> on every install.
const testHuskyStub = "#!/usr/bin/env sh\n. \"${0%/*}/h\""

func writeHook(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func requireNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s must not exist, lstat err=%v", path, err)
	}
}

// rotatedHookLayout is the hooks dir after a husky reinstall in a repo the
// host CLI and Brain both hook (#645): husky rewrote the primary with its
// stub, and the host's reconcile backed the stub up to pre-push.pre-entire,
// rotating our hook, which had been there, to a timestamped copy that
// nothing runs.
func rotatedHookLayout(t *testing.T, hooksDir string) (primary, slot, rotated string) {
	t.Helper()
	primary = filepath.Join(hooksDir, "pre-push")
	slot = primary + ".pre-entire"
	rotated = slot + ".20261115T101500Z"
	writeHook(t, primary, testHostChainHook)
	writeHook(t, slot, testHuskyStub)
	writeHook(t, rotated, pushHookContent("/managed/entire-brain"))
	return primary, slot, rotated
}

// A rotated copy is not chained: connect must put our hook back where the
// host's chain runs it (#645), behind the host's hook rather than in front of
// it, so the host's next reconcile has nothing to re-wrap.
func TestInstallPushHookRepairsCopyTheHostRotated(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary, slot, rotated := rotatedHookLayout(t, hooksDir)
	// An earlier repair left the previous stub behind our chain; husky's
	// stubs are identical, so it is replaced rather than rotated.
	writeHook(t, slot+pushHookBackupSuffix, testHuskyStub)

	state, path, err := installPushHook(context.Background(), opts, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if state != pushHookInstalled || path != slot {
		t.Fatalf("a rotated copy must not read as chained: state=%v path=%v", state, path)
	}
	if got := readHook(t, slot); !strings.Contains(got, pushHookMarker) {
		t.Fatalf("our hook must be back in the host's chain slot: %q", got)
	}
	if got := readHook(t, slot+pushHookBackupSuffix); got != testHuskyStub {
		t.Fatalf("our chain must run the husky stub: %q", got)
	}
	if got := readHook(t, primary); got != testHostChainHook {
		t.Fatalf("primary must stay the host's: %q", got)
	}
	requireNoFile(t, primary+pushHookBackupSuffix)
	requireNoFile(t, slot+pushHookBackupSuffix+".20261115T101500Z")
	if got := readHook(t, rotated); !strings.Contains(got, pushHookMarker) {
		t.Fatalf("install leaves the rotated copy for uninstall: %q", got)
	}

	// The repaired layout reads as chained from then on.
	state, path, err = installPushHook(context.Background(), opts, repoDir)
	if err != nil || state != pushHookChainedIn || path != slot {
		t.Fatalf("rerun state=%v path=%v err=%v", state, path, err)
	}
}

// Setup refreshes and never installs, so a rotated copy is reported as not
// running, with the path, instead of as chained.
func TestRefreshPushHookReportsCopyTheHostRotated(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	_, slot, rotated := rotatedHookLayout(t, hooksDir)

	state, path, err := refreshPushHook(context.Background(), opts, repoDir)
	if err != nil || state != pushHookDormant || path != rotated {
		t.Fatalf("state=%v path=%v err=%v", state, path, err)
	}
	if got := readHook(t, slot); got != testHuskyStub {
		t.Fatalf("refresh must not install: %q", got)
	}
}

// With the host's hook in the primary and a hook in the slot its chain runs,
// we join that chain instead of moving the host's hook behind ours. Moving it
// made the host's next reconcile back ours up into the slot, which rotated
// the hook there to a copy nothing runs.
func TestInstallPushHookJoinsTheHostChain(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	slot := primary + ".pre-entire"
	userHook := "#!/bin/sh\necho user hook\n"
	writeHook(t, primary, testHostChainHook)
	writeHook(t, slot, userHook)

	state, path, err := installPushHook(context.Background(), opts, repoDir)
	if err != nil || state != pushHookInstalled || path != slot {
		t.Fatalf("state=%v path=%v err=%v", state, path, err)
	}
	if got := readHook(t, primary); got != testHostChainHook {
		t.Fatalf("primary must stay the host's: %q", got)
	}
	if got := readHook(t, slot); !strings.Contains(got, pushHookMarker) {
		t.Fatalf("slot must hold our hook: %q", got)
	}
	if got := readHook(t, slot+pushHookBackupSuffix); got != userHook {
		t.Fatalf("our chain must run the user's hook: %q", got)
	}
	requireNoFile(t, primary+pushHookBackupSuffix)
}

// The host's chain is joined only where it runs: a host hook that chains to
// nothing, an empty slot, and pre-commit's wrapper in the slot all keep the
// old install, in front of the primary.
func TestInstallPushHookWrapsThePrimaryOutsideTheHostChain(t *testing.T) {
	const hostNoChain = "#!/bin/sh\n# Entire CLI hooks\nentire hooks git pre-push \"$1\"\n"
	const preCommit = "#!/usr/bin/env bash\n# File generated by pre-commit: https://pre-commit.com\nexec pre-commit hook-impl --hook-type=pre-push\n"
	for name, tc := range map[string]struct{ primary, slot string }{
		"host hook without a chain": {primary: hostNoChain, slot: "#!/bin/sh\nexit 0\n"},
		"no slot":                   {primary: testHostChainHook},
		"pre-commit wrapper":        {primary: testHostChainHook, slot: preCommit},
	} {
		t.Run(name, func(t *testing.T) { requireWrapsThePrimary(t, tc.primary, tc.slot) })
	}
}

// requireWrapsThePrimary installs with hostHook in the primary and slotHook
// (none when empty) in the host's slot, and checks that ours went in front of
// the primary and left the slot alone.
func requireWrapsThePrimary(t *testing.T, hostHook, slotHook string) {
	t.Helper()
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	slot := primary + ".pre-entire"
	writeHook(t, primary, hostHook)
	if slotHook != "" {
		writeHook(t, slot, slotHook)
	}

	state, path, err := installPushHook(context.Background(), opts, repoDir)
	if err != nil || state != pushHookInstalled || path != primary {
		t.Fatalf("state=%v path=%v err=%v", state, path, err)
	}
	if got := readHook(t, primary+pushHookBackupSuffix); got != hostHook {
		t.Fatalf("host hook must move behind ours: %q", got)
	}
	if slotHook == "" {
		requireNoFile(t, slot)
	} else if got := readHook(t, slot); got != slotHook {
		t.Fatalf("slot must stay as it was: %q", got)
	}
	requireNoFile(t, slot+pushHookBackupSuffix)
}

// Uninstall puts back the hook our copy in the host's chain was running, so
// the host's chain keeps running it. Deleting our copy alone stranded it
// where nothing runs it.
func TestUninstallPushHookRestoresTheHookBehindUsInTheHostChain(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	slot := primary + ".pre-entire"
	writeHook(t, primary, testHostChainHook)
	writeHook(t, slot, pushHookContent("/managed/entire-brain"))
	writeHook(t, slot+pushHookBackupSuffix, testHuskyStub)

	if err := uninstallPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, slot); got != testHuskyStub {
		t.Fatalf("the hook we chained to must be back in the host's chain: %q", got)
	}
	requireNoFile(t, slot+pushHookBackupSuffix)
	if got := readHook(t, primary); got != testHostChainHook {
		t.Fatalf("primary must stay the host's: %q", got)
	}
}

// Disconnect removes every copy of our hook, including ones an installer
// rotated aside; they no longer run, and nothing else would remove them.
func TestUninstallPushHookRemovesRotatedCopies(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary, slot, rotated := rotatedHookLayout(t, hooksDir)
	ownRotated := primary + pushHookBackupSuffix + ".20261114T090000Z"
	writeHook(t, ownRotated, pushHookContent("/old/entire-brain"))
	userRotated := slot + ".20261113T080000Z"
	writeHook(t, userRotated, testHuskyStub)

	if err := uninstallPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
	requireNoFile(t, rotated)
	requireNoFile(t, ownRotated)
	if got := readHook(t, userRotated); got != testHuskyStub {
		t.Fatalf("a rotated copy of someone else's hook stays: %q", got)
	}
	if got := readHook(t, slot); got != testHuskyStub {
		t.Fatalf("the live chain stays as it was: %q", got)
	}
	if got := readHook(t, primary); got != testHostChainHook {
		t.Fatalf("primary must stay the host's: %q", got)
	}
}

func TestIsRotatedHookCopy(t *testing.T) {
	for name, want := range map[string]bool{
		"pre-push.pre-entire.20261115T101500Z":                  true,
		"pre-push.pre-entire.20261115T101500Z-2":                true,
		"pre-push.pre-entire-brain.20261115T101500Z":            true,
		"pre-push.pre-entire.pre-entire-brain.20261115T101500Z": true,
		"pre-push.pre-entire":                                   false,
		"pre-push.pre-entire.pre-entire-brain":                  false,
		"pre-push.legacy":                                       false,
		"pre-push.old":                                          false,
		"pre-push.pre-entire.20261315T101500Z":                  false,
		"pre-push.pre-entire.20261115T101500Z-":                 false,
		"pre-push.pre-entire.20261115T101500Z-x":                false,
	} {
		if got := isRotatedHookCopy(filepath.Join("/hooks", name)); got != want {
			t.Errorf("isRotatedHookCopy(%q) = %v, want %v", name, got, want)
		}
	}
}

// After the host's uninstall moved our hook from its slot back into the
// primary, disconnect restores the hook we ran from the slot to the primary.
func TestUninstallPushHookRestoresTheHookTheHostUninstallLeftBehind(t *testing.T) {
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	stranded := primary + ".pre-entire" + pushHookBackupSuffix
	userHook := "#!/bin/sh\necho user hook\n"
	writeHook(t, primary, pushHookContent("/managed/entire-brain"))
	writeHook(t, stranded, userHook)

	if err := uninstallPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, primary); got != userHook {
		t.Fatalf("the hook we chained to must be back in the primary: %q", got)
	}
	requireNoFile(t, stranded)
}

// A file at the name the chain line falls back to is set aside when our hook
// goes into a primary that was not ours: the join that put it there was undone
// by something other than the host's uninstall, and our new hook must not
// start running it. A copy of ours in the host's slot keeps it as its backup.
func TestInstallPushHookSetsAsideAFileTheFallbackWouldRun(t *testing.T) {
	for name, tc := range map[string]struct {
		primary, slot string // "" for no file
		keep          bool
	}{
		"empty primary":             {},
		"another hook in primary":   {primary: "#!/bin/sh\necho user hook\n"},
		"our copy in the host slot": {slot: pushHookContent("/managed/entire-brain"), keep: true},
	} {
		t.Run(name, func(t *testing.T) {
			requireSetsAsideTheFallbackFile(t, tc.primary, tc.slot, tc.keep)
		})
	}
}

const testLeftBehindHook = "#!/bin/sh\necho left behind\n"

// requireSetsAsideTheFallbackFile installs over primaryHook and slotHook
// ("" for none) with a file at the fallback's name, and checks that the file
// is rotated aside, or kept in place when keep is set.
func requireSetsAsideTheFallbackFile(t *testing.T, primaryHook, slotHook string, keep bool) {
	t.Helper()
	opts, repoDir, hooksDir := pushHookHarness(t)
	primary := filepath.Join(hooksDir, "pre-push")
	stranded := primary + ".pre-entire" + pushHookBackupSuffix
	writeHookIfSet(t, primary, primaryHook)
	writeHookIfSet(t, primary+".pre-entire", slotHook)
	writeHook(t, stranded, testLeftBehindHook)

	if _, _, err := installPushHook(context.Background(), opts, repoDir); err != nil {
		t.Fatal(err)
	}
	if got := readHook(t, primary); !strings.Contains(got, pushHookMarker) {
		t.Fatalf("our hook must be in the primary: %q", got)
	}
	if primaryHook != "" && readHook(t, primary+pushHookBackupSuffix) != primaryHook {
		t.Fatal("the primary's hook must be our backup")
	}
	rotated := rotatedCopiesOf(t, stranded)
	if keep {
		if got := readHook(t, stranded); got != testLeftBehindHook || len(rotated) != 0 {
			t.Fatalf("our slot copy's backup must stay: %q, rotated %v", got, rotated)
		}
		return
	}
	requireNoFile(t, stranded)
	if len(rotated) != 1 || readHook(t, rotated[0]) != testLeftBehindHook {
		t.Fatalf("the left-behind file must be kept under a rotated name: %v", rotated)
	}
}

func writeHookIfSet(t *testing.T, path, content string) {
	t.Helper()
	if content != "" {
		writeHook(t, path, content)
	}
}

// rotatedCopiesOf lists the rotated copies of path.
func rotatedCopiesOf(t *testing.T, path string) []string {
	t.Helper()
	matches, err := filepath.Glob(path + ".*")
	if err != nil {
		t.Fatal(err)
	}
	var rotated []string
	for _, m := range matches {
		if isRotatedHookCopy(m) {
			rotated = append(rotated, m)
		}
	}
	return rotated
}
