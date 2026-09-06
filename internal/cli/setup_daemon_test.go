package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixedDaemonSpec is the spec both golden tests render, with paths that are
// stable across machines so the artifacts can be byte-compared.
func fixedDaemonSpec() daemonSpec {
	return daemonSpec{
		Name:       daemonDefaultName,
		Binary:     "/opt/entire/bin/entire-brain",
		Args:       []string{"workspace", "watch", "default", "--interval", "5m0s", "--distill", "--distill-every", "24h0m0s", "--effort", "low"},
		WorkingDir: "/opt/entire/bin",
		LogPath:    "/var/state/entire/logs/watch.log",
		Env: map[string]string{
			"ENTIRE_PLUGIN_DATA_DIR":  "/var/data/entire",
			"ENTIRE_PLUGIN_STATE_DIR": "/var/state/entire",
			"PATH":                    "/usr/bin:/bin",
		},
	}
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	if string(want) != got {
		t.Fatalf("golden %s mismatch\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// TestPlanBrainWatchDaemonDarwinGolden pins the launchd agent. The renderers
// take goos as an argument rather than reading runtime.GOOS, which is what lets
// both platforms' artifacts be verified from one machine.
func TestPlanBrainWatchDaemonDarwinGolden(t *testing.T) {
	t.Parallel()
	plan, err := planBrainWatchDaemon("darwin", "/Users/demo", "", "", fixedDaemonSpec())
	if err != nil {
		t.Fatalf("planBrainWatchDaemon: %v", err)
	}
	if plan.Manager != daemonManagerLaunchd {
		t.Fatalf("darwin must use launchd, got %s", plan.Manager)
	}
	if plan.Label != "io.entire.brain-watch.a2d3fd66" {
		t.Fatalf("unexpected launchd label %q", plan.Label)
	}
	if plan.UnitPath != "/Users/demo/Library/LaunchAgents/io.entire.brain-watch.a2d3fd66.plist" {
		t.Fatalf("unexpected plist path %q", plan.UnitPath)
	}
	assertGolden(t, "setup_daemon_launchd.plist.golden", plan.Contents)
}

func TestPlanBrainWatchDaemonLinuxGolden(t *testing.T) {
	t.Parallel()
	plan, err := planBrainWatchDaemon("linux", "/home/demo", "", "", fixedDaemonSpec())
	if err != nil {
		t.Fatalf("planBrainWatchDaemon: %v", err)
	}
	if plan.Manager != daemonManagerSystemd {
		t.Fatalf("linux must use a systemd user unit, got %s", plan.Manager)
	}
	if plan.Label != "entire-brain-watch.service" {
		t.Fatalf("unexpected unit name %q", plan.Label)
	}
	if plan.UnitPath != "/home/demo/.config/systemd/user/entire-brain-watch.service" {
		t.Fatalf("unexpected unit path %q", plan.UnitPath)
	}
	assertGolden(t, "setup_daemon_systemd.service.golden", plan.Contents)
}

func TestPlanBrainWatchDaemonHonorsXDGConfigHome(t *testing.T) {
	t.Parallel()
	plan, err := planBrainWatchDaemon("linux", "/home/demo", "/xdg/config", "", fixedDaemonSpec())
	if err != nil {
		t.Fatalf("planBrainWatchDaemon: %v", err)
	}
	if plan.UnitPath != "/xdg/config/systemd/user/entire-brain-watch.service" {
		t.Fatalf("XDG_CONFIG_HOME must win, got %q", plan.UnitPath)
	}
}

// TestPlannedUnitPathsBelongToTheTargetOS is the regression guard for the
// windows CI failure these goldens hit: the plan describes a launchd/systemd
// host, so every separator in it belongs to the TARGET OS, not to the machine
// doing the rendering. Building the path with filepath.Join made a darwin plan
// come out as "\Users\demo\Library\LaunchAgents\..." on a windows runner —
// correct for nothing, and invisible on any developer's mac.
func TestPlannedUnitPathsBelongToTheTargetOS(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ goos, home, configHome, want string }{
		{"darwin", "/Users/demo", "", "/Users/demo/Library/LaunchAgents/io.entire.brain-watch.a2d3fd66.plist"},
		{"linux", "/home/demo", "", "/home/demo/.config/systemd/user/entire-brain-watch.service"},
		{"linux", "/home/demo", "/xdg/config", "/xdg/config/systemd/user/entire-brain-watch.service"},
	} {
		plan, err := planBrainWatchDaemon(tc.goos, tc.home, tc.configHome, "", fixedDaemonSpec())
		if err != nil {
			t.Fatalf("%s plan: %v", tc.goos, err)
		}
		if plan.UnitPath != tc.want {
			t.Fatalf("%s: unit path must be a target-OS path, want %q got %q", tc.goos, tc.want, plan.UnitPath)
		}
		if strings.ContainsRune(plan.UnitPath, '\\') {
			t.Fatalf("%s: a POSIX unit path must never carry a host separator: %q", tc.goos, plan.UnitPath)
		}
	}
}

func TestPlanBrainWatchDaemonUnsupportedOS(t *testing.T) {
	t.Parallel()
	plan, err := planBrainWatchDaemon("plan9", "/home/demo", "", "", fixedDaemonSpec())
	if err != nil {
		t.Fatalf("planBrainWatchDaemon: %v", err)
	}
	if plan.supported() {
		t.Fatalf("plan9 has no service manager here; plan must report unsupported: %+v", plan)
	}
}

// TestPlanBrainWatchDaemonNamesStayInLockstep guards the one-identity rule: a
// custom name must produce a matching launchd label and systemd unit, so an
// install can never end up half-named.
func TestPlanBrainWatchDaemonNamesStayInLockstep(t *testing.T) {
	t.Parallel()
	spec := fixedDaemonSpec()
	spec.Name = "entire-brain-watch-smoke"
	darwin, err := planBrainWatchDaemon("darwin", "/Users/demo", "", "", spec)
	if err != nil {
		t.Fatalf("darwin plan: %v", err)
	}
	linux, err := planBrainWatchDaemon("linux", "/home/demo", "", "", spec)
	if err != nil {
		t.Fatalf("linux plan: %v", err)
	}
	if darwin.Label != "io.entire.brain-watch-smoke.8cee2df3" {
		t.Fatalf("unexpected label %q", darwin.Label)
	}
	if linux.Label != "entire-brain-watch-smoke.service" {
		t.Fatalf("unexpected unit %q", linux.Label)
	}
}

func TestPlanBrainWatchDaemonRejectsUnsafeNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"../escape", "with/slash", "", "-leading"} {
		spec := fixedDaemonSpec()
		spec.Name = name
		if name == "" {
			continue // empty falls back to the default by design
		}
		if _, err := planBrainWatchDaemon("darwin", "/Users/demo", "", "", spec); err == nil {
			t.Fatalf("name %q must be rejected", name)
		}
	}
}

func TestRenderedArtifactsAreStableAcrossRuns(t *testing.T) {
	t.Parallel()
	// Go randomizes map iteration; a plan whose bytes shift between runs would
	// make the "already current" idempotence check fire at random.
	spec := fixedDaemonSpec()
	first := renderLaunchdPlist("io.entire.brain-watch.a2d3fd66", spec)
	unitFirst := renderSystemdUnit(spec)
	for i := 0; i < 20; i++ {
		if renderLaunchdPlist("io.entire.brain-watch.a2d3fd66", spec) != first {
			t.Fatal("launchd plist rendering is not deterministic")
		}
		if renderSystemdUnit(spec) != unitFirst {
			t.Fatal("systemd unit rendering is not deterministic")
		}
	}
}

// recordingDaemonRunner captures the service-manager commands an install or
// uninstall issues.
type recordingDaemonRunner struct {
	calls   []string
	listErr error
}

func (r *recordingDaemonRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	if name == "launchctl" && len(args) > 0 && args[0] == "list" {
		return nil, nil, r.listErr
	}
	if name == "systemctl" && len(args) > 1 && args[1] == "is-active" {
		if r.listErr != nil {
			return []byte("inactive\n"), nil, r.listErr
		}
		return []byte("active\n"), nil, nil
	}
	return nil, nil, nil
}

func TestInstallDaemonWritesUnitAndLoadsItIdempotently(t *testing.T) {
	home := t.TempDir()
	spec := fixedDaemonSpec()
	spec.LogPath = filepath.Join(home, "logs", "watch.log")
	plan, err := planBrainWatchDaemon("darwin", home, "", "", spec)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	runner := &recordingDaemonRunner{}
	for i := 0; i < 2; i++ {
		if err := installDaemon(context.Background(), runner, plan); err != nil {
			t.Fatalf("installDaemon (pass %d): %v", i, err)
		}
	}
	data, err := os.ReadFile(plan.UnitPath)
	if err != nil {
		t.Fatalf("read installed plist: %v", err)
	}
	if string(data) != plan.Contents {
		t.Fatal("installed plist does not match the plan")
	}
	info, err := os.Stat(plan.UnitPath)
	if err != nil {
		t.Fatalf("stat installed plist: %v", err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("installed unit exposes local paths/env to other users: mode %o", got)
	}
	// Unload-before-load is the idempotence hinge: without it a second install
	// fails with "service already loaded" and leaves a stale job registered.
	if got := strings.Join(runner.calls, "\n"); strings.Count(got, "launchctl unload") != 2 || strings.Count(got, "launchctl load -w") != 2 {
		t.Fatalf("each install must unload then load exactly once:\n%s", got)
	}
}

func TestInspectDaemonReportsInstalledCurrentAndRunning(t *testing.T) {
	home := t.TempDir()
	spec := fixedDaemonSpec()
	spec.LogPath = filepath.Join(home, "logs", "watch.log")
	plan, err := planBrainWatchDaemon("darwin", home, "", "", spec)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	runner := &recordingDaemonRunner{}

	if state := inspectDaemon(context.Background(), runner, plan); state.Installed || state.Running {
		t.Fatalf("nothing is installed yet: %+v", state)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("an absent unit must not cost a subprocess, got %v", runner.calls)
	}

	if err := installDaemon(context.Background(), runner, plan); err != nil {
		t.Fatalf("installDaemon: %v", err)
	}
	state := inspectDaemon(context.Background(), runner, plan)
	if !state.Installed || !state.Current || !state.Running {
		t.Fatalf("installed daemon should read installed+current+running: %+v", state)
	}

	// A unit written by an older build is installed but NOT current: that is
	// the difference between an idempotent re-run and silent drift.
	if err := os.WriteFile(plan.UnitPath, []byte("stale contents\n"), 0o644); err != nil {
		t.Fatalf("write stale unit: %v", err)
	}
	if state := inspectDaemon(context.Background(), runner, plan); !state.Installed || state.Current {
		t.Fatalf("a drifted unit must report installed but not current: %+v", state)
	}
}

func TestUninstallDaemonRemovesUnitAndIsSafeTwice(t *testing.T) {
	home := t.TempDir()
	spec := fixedDaemonSpec()
	spec.LogPath = filepath.Join(home, "logs", "watch.log")
	plan, err := planBrainWatchDaemon("darwin", home, "", "", spec)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	runner := &recordingDaemonRunner{}
	if err := installDaemon(context.Background(), runner, plan); err != nil {
		t.Fatalf("installDaemon: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := uninstallDaemon(context.Background(), runner, plan); err != nil {
			t.Fatalf("uninstallDaemon (pass %d): %v", i, err)
		}
	}
	if _, err := os.Stat(plan.UnitPath); !os.IsNotExist(err) {
		t.Fatalf("unit should be gone, stat err = %v", err)
	}
}

func TestSystemdRunningStateReadsStdoutNotExitCode(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	plan, err := planBrainWatchDaemon("linux", home, "", "", fixedDaemonSpec())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(plan.UnitPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(plan.UnitPath, []byte(plan.Contents), 0o644); err != nil {
		t.Fatalf("write unit: %v", err)
	}
	if state := inspectDaemon(context.Background(), &recordingDaemonRunner{}, plan); !state.Running {
		t.Fatalf("systemctl printing \"active\" means running: %+v", state)
	}
}

// TestSystemdSingleValueSettingsAreNotQuoted encodes a systemd parsing fact that
// is easy to get backwards. WorkingDirectory=, StandardOutput= and StandardError=
// take ONE value and are parsed by config_parse_working_directory /
// config_parse_exec_output, which never call extract_first_word(EXTRACT_UNQUOTE)
// — so quotes are NOT stripped; they land inside the path and the setting is
// then rejected by path_simplify_and_warn's absolute-path check. Spaces need no
// treatment at all there: the whole rest of the line is the value. Quoting
// belongs only on the settings systemd does unquote, ExecStart= and Environment=.
func TestSystemdSingleValueSettingsAreNotQuoted(t *testing.T) {
	t.Parallel()
	spec := fixedDaemonSpec()
	spec.WorkingDir = "/opt/entire tools/bin"
	spec.LogPath = "/var/state/entire tools/watch.log"
	unit := renderSystemdUnit(spec)

	for _, want := range []string{
		"WorkingDirectory=/opt/entire tools/bin\n",
		"StandardOutput=append:/var/state/entire tools/watch.log\n",
		"StandardError=append:/var/state/entire tools/watch.log\n",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("single-value settings must be written verbatim, missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, `WorkingDirectory="`) || strings.Contains(unit, `append:"`) {
		t.Fatalf("quoting these settings breaks them (the quote becomes part of the path):\n%s", unit)
	}
	// The list settings systemd DOES unquote stay quoted.
	if !strings.Contains(unit, `ExecStart="/opt/entire/bin/entire-brain"`) {
		t.Fatalf("ExecStart arguments must stay quoted:\n%s", unit)
	}
}

// TestSystemdEscapesSpecifiers: a literal % is a systemd specifier introducer
// (%h expands to the home directory), so an unescaped one silently rewrites the
// path. This is the escaping these settings DO need.
func TestSystemdEscapesSpecifiers(t *testing.T) {
	t.Parallel()
	spec := fixedDaemonSpec()
	spec.WorkingDir = "/opt/100%good/bin"
	spec.LogPath = "/var/log/50%h.log"
	spec.Env = map[string]string{"PATH": "/usr/bin:/opt/50%h"}
	unit := renderSystemdUnit(spec)

	for _, want := range []string{
		"WorkingDirectory=/opt/100%%good/bin\n",
		"StandardOutput=append:/var/log/50%%h.log\n",
		`Environment="PATH=/usr/bin:/opt/50%%h"`,
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("a literal %% must be escaped as %%%%, missing %q:\n%s", want, unit)
		}
	}
}

// TestPlanBrainWatchDaemonRejectsNewlineValues: no unit-file escaping can express
// a newline, and an unescaped one would turn the rest of the value into forged
// directives.
func TestPlanBrainWatchDaemonRejectsNewlineValues(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*daemonSpec){
		"working dir": func(s *daemonSpec) { s.WorkingDir = "/opt\nExecStart=/bin/sh" },
		"log path":    func(s *daemonSpec) { s.LogPath = "/var/log\nRestart=no" },
		"arg":         func(s *daemonSpec) { s.Args = append(s.Args, "--model\nExecStopPost=/bin/sh") },
		"env value":   func(s *daemonSpec) { s.Env = map[string]string{"PATH": "/usr/bin\nUser=root"} },
	} {
		spec := fixedDaemonSpec()
		mutate(&spec)
		if _, err := planBrainWatchDaemon("linux", "/home/demo", "", "", spec); err == nil {
			t.Fatalf("%s: a newline in a rendered value must be rejected", name)
		}
	}
}

// TestLaunchdLabelIsInjective guards the label collision the review found:
// trimming the "entire-" prefix alone maps two distinct daemon names onto one
// label, so two differently named daemons would share one launchd job and one
// plist path — installing the second silently replaces the first.
func TestLaunchdLabelIsInjective(t *testing.T) {
	t.Parallel()
	names := []string{"entire-brain-watch", "brain-watch", "entire-watch", "watch", "entire-brain-watch-smoke"}
	seen := map[string]string{}
	for _, name := range names {
		label := launchdLabel(name)
		if previous, clash := seen[label]; clash {
			t.Fatalf("daemon names %q and %q collide on label %q", previous, name, label)
		}
		seen[label] = name
		if !strings.HasPrefix(label, "io.entire.") {
			t.Fatalf("label must stay reverse-DNS under io.entire: %s", label)
		}
	}
	if launchdLabel("entire-brain-watch") != launchdLabel("entire-brain-watch") {
		t.Fatal("the label must be stable for one name, or every setup would reinstall")
	}
}
