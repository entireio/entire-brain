//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// plantWorkingDirectoryShims fills dir with entire-brain/entire .cmd and .bat
// scripts, each of which records that it ran by writing a file into markers.
// Those are exactly the names the pre-edit hook and agent CLIs run bare, so
// any of them running means a bare name resolved from the working directory.
func plantWorkingDirectoryShims(t *testing.T, dir, markers string) {
	t.Helper()
	for _, name := range []string{"entire-brain.cmd", "entire-brain.bat", "entire.cmd", "entire.bat"} {
		script := "@echo ran> \"" + filepath.Join(markers, name+".ran") + "\"\r\n@exit /b 0\r\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func shimMarkers(t *testing.T, markers string) []string {
	t.Helper()
	entries, err := os.ReadDir(markers)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func clearShimMarkers(t *testing.T, markers string) {
	t.Helper()
	for _, name := range shimMarkers(t, markers) {
		if err := os.Remove(filepath.Join(markers, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func systemCmdExe(t *testing.T) string {
	t.Helper()
	root := os.Getenv("SystemRoot")
	if root == "" {
		t.Skip("SystemRoot is not set")
	}
	return filepath.Join(root, "System32", "cmd.exe")
}

// withoutExeLookupSetting returns env minus any NoDefaultCurrentDirectoryInExePath
// entry, so a test proves the setting comes from the code under test and not
// from the machine.
func withoutExeLookupSetting(env []string) []string {
	var out []string
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(key, base.EnvNoDefaultCurrentDirectoryInExePath) {
			out = append(out, entry)
		}
	}
	return out
}

// isolateExeLookupEnv points PATH at System32 only (cmd.exe stays reachable;
// no real entire-brain or entire is) and removes the lookup setting from this
// process's environment for the duration of the test.
func isolateExeLookupEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", filepath.Join(os.Getenv("SystemRoot"), "System32"))
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	t.Setenv(base.EnvNoDefaultCurrentDirectoryInExePath, "")
	if err := os.Unsetenv(base.EnvNoDefaultCurrentDirectoryInExePath); err != nil {
		t.Fatal(err)
	}
}

// runHookLine runs a settings.json hook command line verbatim in dir, the way
// a host hands it to CreateProcess, and returns cmd.exe's exit code.
func runHookLine(t *testing.T, dir, line string) int {
	t.Helper()
	cmd := exec.Command(systemCmdExe(t))
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}
	cmd.Dir = dir
	cmd.Env = os.Environ()
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.ExitCode()
	default:
		t.Fatalf("run %s: %v", line, err)
		return -1
	}
}

// TestPreEditHookResolvesFromPathNotTheWorkingDirectory runs the wired hook
// line in a repository that carries entire-brain/entire scripts at its root.
// None of them may run, and with neither binary on PATH the hook exits 0.
func TestPreEditHookResolvesFromPathNotTheWorkingDirectory(t *testing.T) {
	isolateExeLookupEnv(t)
	repo, markers := t.TempDir(), t.TempDir()
	plantWorkingDirectoryShims(t, repo, markers)

	// Control: the earlier line, without the setting, does run a planted
	// script. Without this the assertion below could pass on a machine that
	// never searched the working directory in the first place.
	runHookLine(t, repo, legacyWindowsPreEditHookCommand)
	if len(shimMarkers(t, markers)) == 0 {
		t.Fatal("control: the legacy hook line ran no working-directory script, so this test proves nothing here")
	}
	clearShimMarkers(t, markers)

	command := preEditHookCommandFor("windows")
	if code := runHookLine(t, repo, command); code != 0 {
		t.Fatalf("hook exited %d with neither binary on PATH, want 0", code)
	}
	if ran := shimMarkers(t, markers); len(ran) != 0 {
		t.Fatalf("the hook ran working-directory scripts %v: %s", ran, command)
	}
}

// TestDistillAgentChildResolvesFromPathNotTheWorkingDirectory starts an agent
// child the way distill does, in a repository with planted scripts, and has it
// run the bare name entire-brain. It must not resolve to the planted script,
// whether or not distillChildEnvKey supplies the child's environment.
func TestDistillAgentChildResolvesFromPathNotTheWorkingDirectory(t *testing.T) {
	isolateExeLookupEnv(t)
	repo, markers := t.TempDir(), t.TempDir()
	plantWorkingDirectoryShims(t, repo, markers)
	cmdExe := systemCmdExe(t)

	// Control: the same child without the setting runs the planted script.
	control := exec.Command(cmdExe, "/c", "entire-brain")
	control.Dir = repo
	control.Env = withoutExeLookupSetting(os.Environ())
	_ = control.Run()
	if len(shimMarkers(t, markers)) == 0 {
		t.Fatal("control: a child without the setting ran no working-directory script, so this test proves nothing here")
	}
	clearShimMarkers(t, markers)

	for name, ctx := range map[string]context.Context{
		"inherited environment": context.Background(),
		"distillChildEnvKey":    context.WithValue(context.Background(), distillChildEnvKey{}, withoutExeLookupSetting(os.Environ())),
	} {
		// The agent fails (entire-brain is not on PATH); only whether it ran
		// the planted script matters here.
		_, _ = execCustomDistillAgent(ctx, repo, []string{cmdExe, "/c", "entire-brain"}, nil, 30*time.Second)
		if ran := shimMarkers(t, markers); len(ran) != 0 {
			t.Fatalf("%s: the agent child ran working-directory scripts %v", name, ran)
		}
	}
}
