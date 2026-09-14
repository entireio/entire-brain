package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// setup_daemon.go is the install glue behind `setup`: it turns one
// `entire-brain workspace watch <ws>` invocation into a supervised, machine-wide
// background service so the brain stays fresh without anyone remembering to run
// anything.
//
// Everything that decides WHAT gets installed is a pure function of
// (goos, home, spec) — planBrainWatchDaemon and the two renderers below take
// the target OS as an argument rather than reading runtime.GOOS. That is what
// makes the darwin and linux artifacts golden-testable from any machine, and it
// keeps the one genuinely platform-bound part (running launchctl/systemctl)
// behind the injectable CommandRunner.
const (
	// daemonDefaultName is the daemon's identity. It is deliberately a single
	// string: the launchd label and the systemd unit name are both derived from
	// it, so an installation can never end up half-named. Overridable so tests
	// and smoke runs never collide with a real user daemon.
	daemonDefaultName = "entire-brain-watch"

	daemonManagerLaunchd     = "launchd"
	daemonManagerSystemd     = "systemd"
	daemonManagerUnsupported = "unsupported"

	// envDaemonUnitDir redirects the unit FILE (launchd plist / systemd user
	// unit) somewhere other than the real service-manager directory. Tests and
	// smoke runs set it so a run with a REAL $HOME never writes into the
	// developer's ~/Library/LaunchAgents or ~/.config/systemd/user. Redirecting
	// $HOME alone is not enough: a sandboxed process that still sees the real
	// home would write a plist into it.
	//
	// It redirects the FILE AND NOTHING ELSE. `launchctl load -w <path>` and
	// `systemctl --user enable --now` act on the caller's live session whatever
	// directory the unit was read from, so a run that sets only this variable
	// still registers a real, KeepAlive, restart-forever job on the developer's
	// machine. Anything that must not touch the session -- CI, a smoke test,
	// the sandboxed trial harness -- has to set envDaemonNoRegister as well.
	envDaemonUnitDir = "ENTIRE_BRAIN_DAEMON_DIR"

	// envDaemonNoRegister makes the install a DRY RUN against the service
	// manager: the unit file is still written (so its bytes, path and
	// idempotence stay observable) but launchctl/systemctl are never invoked,
	// so nothing is loaded, enabled or started in the caller's session. This is
	// the half envDaemonUnitDir cannot provide, and the reason a CI job that set
	// only the directory still ended up with a live agent.
	envDaemonNoRegister = "ENTIRE_BRAIN_DAEMON_NO_REGISTER"
)

// daemonRegistrationDisabled reports whether this process may talk to the
// service manager at all. Read once, at plan time, so the decision travels with
// the plan and every consumer (install, uninstall, the running-probe) agrees.
func daemonRegistrationDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envDaemonNoRegister))) {
	case "", "0", "false", "no":
		return false
	default:
		return true
	}
}

var daemonNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// daemonPathJoin joins path segments for the TARGET operating system, which is
// always a POSIX one here: launchd (darwin) and systemd (linux) are the only
// service managers this file plans for. It is deliberately path.Join — always
// "/" — and never filepath.Join, whose separator is a property of the HOST.
//
// filepath.Join on a Windows host rendered a darwin plan as
// `\Users\demo\Library\LaunchAgents\io.entire...plist`: a path no mac would
// accept, and the reason the darwin/linux golden tests failed on windows CI
// while passing everywhere else. Taking goos as an argument only makes the plan
// portable if every path decision downstream of it is keyed to that argument
// too.
//
// A host-shaped root (a Windows t.TempDir(), or the ENTIRE_BRAIN_DAEMON_DIR
// sandbox) keeps whatever separators it arrived with; only the segments this
// function appends use "/", which every Windows file API accepts, so writing
// the unit still works when a test plans a darwin daemon on a windows box.
func daemonPathJoin(elem ...string) string { return path.Join(elem...) }

// daemonSpec is the full description of the watcher service: which binary to
// run, with which arguments and environment. The environment matters more than
// it looks — a launchd/systemd job inherits almost nothing, so the ENTIRE_PLUGIN_*
// dirs (and PATH, for the `git`/`entire` the refresh shells out to) must be
// carried explicitly or the daemon would silently target a different brain store
// than the CLI that installed it.
type daemonSpec struct {
	Name       string
	Binary     string
	Args       []string
	WorkingDir string
	LogPath    string
	Env        map[string]string
}

// daemonPlan is the resolved, OS-specific install artifact: exactly one file to
// write and the identity to hand the service manager.
type daemonPlan struct {
	OS       string
	Manager  string
	Label    string
	UnitPath string
	Contents string
	Spec     daemonSpec
	// NoRegister carries the envDaemonNoRegister decision. It deliberately does
	// not affect Contents or UnitPath: what would be installed stays exactly
	// what a real run installs, so a sandboxed trial verifies the real artifact
	// and only the service-manager call is withheld.
	NoRegister bool
}

func (p daemonPlan) supported() bool { return p.Manager != daemonManagerUnsupported }

func validateDaemonName(name string) error {
	if !daemonNamePattern.MatchString(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("daemon name must be a plain identifier (letters, digits, dot, dash, underscore): %q", name)
	}
	return nil
}

// launchdLabel maps the daemon name into Apple's reverse-DNS convention. The
// readable stem alone is NOT injective — stripping the "entire-" prefix maps
// both "entire-watch" and "watch" onto io.entire.watch, so two differently named
// daemons would fight over one launchd job and one plist path. The short digest
// of the FULL name restores injectivity while keeping the label recognisable.
func launchdLabel(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "io.entire." + strings.TrimPrefix(name, "entire-") + "." + hex.EncodeToString(sum[:4])
}

// systemdUnitName is the daemon name with the unit suffix; systemd unit names
// are already plain identifiers, so no transformation is needed.
func systemdUnitName(name string) string { return name + ".service" }

// planBrainWatchDaemon resolves the spec against a target OS and home directory.
// goos/home/configHome/unitDir are parameters, not ambient state, so both
// platforms' artifacts are testable from one machine. A non-empty unitDir (the
// ENTIRE_BRAIN_DAEMON_DIR knob) replaces the service manager's directory
// entirely, which is what keeps a sandboxed run out of a real user's LaunchAgents.
func planBrainWatchDaemon(goos, home, configHome, unitDir string, spec daemonSpec) (daemonPlan, error) {
	if strings.TrimSpace(spec.Name) == "" {
		spec.Name = daemonDefaultName
	}
	if err := validateDaemonName(spec.Name); err != nil {
		return daemonPlan{}, err
	}
	if strings.TrimSpace(spec.Binary) == "" {
		return daemonPlan{}, fmt.Errorf("daemon binary path is required")
	}
	// A newline in any rendered value would close the line and let the rest be
	// read as further unit directives. No escaping expresses it; refuse instead.
	values := append([]string{spec.Binary, spec.WorkingDir, spec.LogPath}, spec.Args...)
	for key, value := range spec.Env {
		values = append(values, key, value)
	}
	for _, value := range values {
		if !unitValueSafe(value) {
			return daemonPlan{}, fmt.Errorf("daemon service values must not contain newlines: %q", value)
		}
	}
	unitDir = strings.TrimSpace(unitDir)
	plan := daemonPlan{OS: goos, Spec: spec}
	switch goos {
	case "darwin":
		if home == "" && unitDir == "" {
			return daemonPlan{}, fmt.Errorf("cannot resolve home directory for the launchd agent")
		}
		plan.Manager = daemonManagerLaunchd
		plan.Label = launchdLabel(spec.Name)
		root := unitDir
		if root == "" {
			root = daemonPathJoin(home, "Library", "LaunchAgents")
		}
		plan.UnitPath = daemonPathJoin(root, plan.Label+".plist")
		plan.Contents = renderLaunchdPlist(plan.Label, spec)
	case "linux":
		root := unitDir
		if root == "" {
			root = configHome
			if root == "" {
				if home == "" {
					return daemonPlan{}, fmt.Errorf("cannot resolve config directory for the systemd user unit")
				}
				root = daemonPathJoin(home, ".config")
			}
			root = daemonPathJoin(root, "systemd", "user")
		}
		plan.Manager = daemonManagerSystemd
		plan.Label = systemdUnitName(spec.Name)
		plan.UnitPath = daemonPathJoin(root, plan.Label)
		plan.Contents = renderSystemdUnit(spec)
	default:
		plan.Manager = daemonManagerUnsupported
		plan.Label = spec.Name
	}
	return plan, nil
}

// sortedEnvKeys keeps rendered artifacts byte-stable across runs (Go map order
// is randomized), which is what lets idempotence be decided by comparing the
// file on disk against the freshly rendered plan.
func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func xmlEscape(value string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(value))
	return buf.String()
}

func renderLaunchdPlist(label string, spec daemonSpec) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	fmt.Fprintf(&b, "  <key>Label</key>\n  <string>%s</string>\n", xmlEscape(label))
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range append([]string{spec.Binary}, spec.Args...) {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(arg))
	}
	b.WriteString("  </array>\n")
	if len(spec.Env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, key := range sortedEnvKeys(spec.Env) {
			fmt.Fprintf(&b, "    <key>%s</key>\n    <string>%s</string>\n", xmlEscape(key), xmlEscape(spec.Env[key]))
		}
		b.WriteString("  </dict>\n")
	}
	if spec.WorkingDir != "" {
		fmt.Fprintf(&b, "  <key>WorkingDirectory</key>\n  <string>%s</string>\n", xmlEscape(spec.WorkingDir))
	}
	if spec.LogPath != "" {
		fmt.Fprintf(&b, "  <key>StandardOutPath</key>\n  <string>%s</string>\n", xmlEscape(spec.LogPath))
		fmt.Fprintf(&b, "  <key>StandardErrorPath</key>\n  <string>%s</string>\n", xmlEscape(spec.LogPath))
	}
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	// A crash-looping watcher must not spin: 60s between relaunches is far
	// below the 5m tick, so a healthy daemon never notices the throttle.
	b.WriteString("  <key>ThrottleInterval</key>\n  <integer>60</integer>\n")
	b.WriteString("  <key>ProcessType</key>\n  <string>Background</string>\n")
	b.WriteString("  <key>LowPriorityIO</key>\n  <true/>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// systemdQuote renders a value for systemd's quoted argument syntax. Use it
// ONLY for settings systemd unquotes — the ones parsed with extract_first_word
// (EXTRACT_UNQUOTE): ExecStart= and Environment=.
func systemdQuote(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + systemdEscapeSpecifiers(replacer.Replace(value)) + `"`
}

// systemdSingleValue renders a value for a setting that takes ONE value and is
// NOT unquoted by systemd: WorkingDirectory=, StandardOutput=, StandardError=.
//
// Those are parsed by config_parse_working_directory / config_parse_exec_output,
// which run unit_path_printf on the raw right-hand side and never call
// extract_first_word — so a quoted path arrives with its quote characters still
// in it and is then rejected by path_simplify_and_warn's absolute-path check.
// Wrapping these in quotes does not harden them, it breaks them. Spaces need no
// treatment at all here: the whole rest of the line is the value.
//
// What DOES need handling is the one transformation systemd applies to them,
// specifier expansion: a literal % must be written %% or it is silently read as
// a specifier (e.g. %h expands to the home directory). Newlines cannot be
// represented in a unit value in any form and are rejected by the caller.
func systemdSingleValue(value string) string {
	return systemdEscapeSpecifiers(value)
}

func systemdEscapeSpecifiers(value string) string {
	return strings.ReplaceAll(value, "%", "%%")
}

// unitValueSafe rejects the values no unit-file escaping can express. A newline
// would end the line and turn the remainder into a forged directive.
func unitValueSafe(value string) bool {
	return !strings.ContainsAny(value, "\n\r")
}

func renderSystemdUnit(spec daemonSpec) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Entire brain watcher (deterministic refresh is free; agent steps are gated)\n")
	b.WriteString("Documentation=https://github.com/entireio/entire-brain\n")
	b.WriteString("After=default.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	exec := make([]string, 0, len(spec.Args)+1)
	for _, arg := range append([]string{spec.Binary}, spec.Args...) {
		exec = append(exec, systemdQuote(arg))
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(exec, " "))
	if spec.WorkingDir != "" {
		fmt.Fprintf(&b, "WorkingDirectory=%s\n", systemdSingleValue(spec.WorkingDir))
	}
	for _, key := range sortedEnvKeys(spec.Env) {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(key+"="+spec.Env[key]))
	}
	if spec.LogPath != "" {
		fmt.Fprintf(&b, "StandardOutput=append:%s\n", systemdSingleValue(spec.LogPath))
		fmt.Fprintf(&b, "StandardError=append:%s\n", systemdSingleValue(spec.LogPath))
	}
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=60\n")
	b.WriteString("Nice=10\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// daemonPathPlaceholder stands in for the inherited search path while two
// rendered units are compared. It is never written to disk.
const daemonPathPlaceholder = "\x00entire-brain-daemon-path\x00"

// daemonUnitMaterial reduces a rendered unit to the part that decides what the
// daemon DOES, so `Current` answers "would this build install a different
// service" instead of "was this shell's environment byte-identical to the one
// that installed it".
//
// The only ambient value in the unit is PATH. daemonEnv copies it from the
// installing process because a launchd/systemd job inherits almost nothing and
// the deterministic refresh shells out to `git` and `entire` — but PATH is the
// most volatile variable on a developer machine. Installing a tool, opening a
// different terminal, a shell rc that prepends a directory twice: any of those
// made the on-disk unit differ from the freshly rendered one by a single line.
//
// Byte equality therefore reported drift on a perfectly good service, and
// `setup` "repairs" drift by unloading and reloading it — tearing down the ONE
// machine-wide watcher, mid-refresh, on a re-run that changed nothing. `status`
// meanwhile told the user their running watcher was stale and to re-run the
// command that would do it again. This is the same failure the log path caused
// before it was made machine-level (see brainWatchDaemonPlan); PATH is the
// remaining instance of it, and the more volatile one.
//
// Masking is deliberately narrow: only the PATH VALUE is neutralized. A unit
// that gains or loses PATH entirely, or whose binary, arguments, working
// directory, log path or ENTIRE_PLUGIN_* dirs changed, still reads as drift and
// is still repaired — and a real reinstall always writes the current PATH.
func daemonUnitMaterial(manager, contents string) string {
	lines := strings.Split(contents, "\n")
	switch manager {
	case daemonManagerLaunchd:
		for i, line := range lines {
			if strings.TrimSpace(line) != "<key>PATH</key>" || i+1 >= len(lines) {
				continue
			}
			next := strings.TrimSpace(lines[i+1])
			if strings.HasPrefix(next, "<string>") && strings.HasSuffix(next, "</string>") {
				lines[i+1] = daemonPathPlaceholder
			}
		}
	case daemonManagerSystemd:
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), `Environment="PATH=`) {
				lines[i] = daemonPathPlaceholder
			}
		}
	default:
		return contents
	}
	return strings.Join(lines, "\n")
}

// setupDaemonLockName serializes the daemon REGISTRATION sequence across
// processes. It lives beside the machine-level watch plan, in the plugin state
// dir, for the same reason the plan does: exactly one service exists per
// machine, so the thing that guards it must be machine-level too.
const setupDaemonLockName = "daemon-install.lock"

// withDaemonRegistrationLock runs fn holding the machine-level daemon lock.
//
// `setup` decides what to do about the daemon with inspect() and then acts with
// install()/uninstall(), and install() on darwin is not one call but two
// (`launchctl unload` then `launchctl load -w`) with the service DOWN in
// between. None of that was serialized, while the watch plan written moments
// earlier — the far less dangerous file — already was. Two repos onboarding at
// once is the ordinary case this whole file was rewritten for, and interleaving
// two unload/load pairs against one label means one run's `load` can land
// between the other's `unload` and `load`: the second then fails with "service
// already loaded" and `setup` reports "daemon install failed" for a watcher
// that is in fact running. Every process now observes the service, changes it
// and re-observes it as one atomic step.
//
// It degrades rather than fails: a state dir that cannot be resolved or locked
// runs fn unlocked, because a machine that cannot take the lock must still be
// able to install its watcher.
func withDaemonRegistrationLock(env EntireEnv, fn func() error) error {
	planPath, err := setupWatchPlanPath(env)
	if err != nil {
		return fn()
	}
	stateDir := filepath.Dir(planPath)
	if err := rejectExistingSymlinkPathComponents(stateDir, brainLockDirName); err != nil {
		return fn()
	}
	lock, err := acquireFileLock(filepath.Join(stateDir, brainLockDirName, setupDaemonLockName), "daemon_install_locked", brainWriteLockTimeout)
	if err != nil {
		return fn()
	}
	defer func() { _ = lock.Close() }()
	return fn()
}

// daemonState is what `setup` and `status` report about the installed watcher.
type daemonState struct {
	Manager   string `json:"manager"`
	Label     string `json:"label,omitempty"`
	UnitPath  string `json:"unit_path,omitempty"`
	Installed bool   `json:"installed"`
	// Current is true when the file on disk matches what this build would
	// write. A stale unit is "installed" but must be rewritten — that is the
	// difference between idempotent re-run and silent drift.
	Current bool   `json:"current"`
	Running bool   `json:"running"`
	Detail  string `json:"detail,omitempty"`
}

// inspectDaemon reports the installed/current/running triple without changing
// anything. Detection is deliberately cheap: one file read plus one service
// manager query.
func inspectDaemon(ctx context.Context, runner CommandRunner, plan daemonPlan) daemonState {
	state := daemonState{Manager: plan.Manager, Label: plan.Label, UnitPath: plan.UnitPath}
	if !plan.supported() {
		// Same wording setup's skip line uses, so `status` and `setup` describe
		// one platform fact the same way instead of two apparent conditions.
		state.Detail = "not supported on " + plan.OS + " yet (no launchd or systemd)"
		return state
	}
	data, err := os.ReadFile(plan.UnitPath)
	switch {
	case err == nil:
		state.Installed = true
		state.Current = daemonUnitMaterial(plan.Manager, string(data)) == daemonUnitMaterial(plan.Manager, plan.Contents)
	case os.IsNotExist(err):
		// Nothing installed under this label, so nothing can be running under
		// it either. Skipping the probe keeps `status` free of a subprocess on
		// the overwhelmingly common not-set-up path.
		return state
	default:
		state.Detail = "read unit: " + err.Error()
		return state
	}
	state.Running, state.Detail = daemonRunning(ctx, runner, plan)
	return state
}

func daemonRunning(ctx context.Context, runner CommandRunner, plan daemonPlan) (bool, string) {
	if plan.NoRegister {
		// Never probe the live session in a no-register run: `launchctl list`
		// against a real label would report a job this process did not install
		// and must not claim.
		return false, "not registered (" + envDaemonNoRegister + ")"
	}
	if runner == nil {
		return false, "no command runner"
	}
	switch plan.Manager {
	case daemonManagerLaunchd:
		if _, _, err := runner.Run(ctx, "", "launchctl", "list", plan.Label); err != nil {
			return false, ""
		}
		return true, ""
	case daemonManagerSystemd:
		stdout, _, err := runner.Run(ctx, "", "systemctl", "--user", "is-active", plan.Label)
		// systemctl exits nonzero for every inactive state, so the stdout word
		// is the signal, not the exit code.
		if strings.TrimSpace(string(stdout)) == "active" {
			return true, ""
		}
		if err != nil {
			return false, ""
		}
		return false, ""
	}
	return false, ""
}

// refreshDaemonUnitBytes rewrites the unit file when it differs from what this
// build would install, WITHOUT going near the service manager.
//
// It is the other half of daemonUnitMaterial. Masking PATH out of the
// "is it current" decision correctly stops `setup` from tearing the live
// watcher down over a shell variable — but on its own it would also freeze the
// daemon's PATH at whatever the very first install happened to inherit, with
// nothing ever refreshing it. Writing the file here and reloading nothing gets
// both: the running service is left strictly alone, and it picks up the current
// PATH at its next natural restart (login, crash, reboot).
//
// A failure is deliberately not an error to the caller: the watcher is running
// and correct, and refusing a re-run of `setup` over a cosmetic rewrite would
// be worse than skipping it.
func refreshDaemonUnitBytes(plan daemonPlan) error {
	if !plan.supported() {
		return nil
	}
	data, err := os.ReadFile(plan.UnitPath)
	if err != nil || string(data) == plan.Contents {
		return err
	}
	return writeFileAtomic(plan.UnitPath, []byte(plan.Contents), 0o600)
}

// installDaemon writes the unit and (re)loads it. It is idempotent by
// construction: the file write is unconditional-but-identical when nothing
// changed, and the unload-then-load pair leaves exactly one job registered no
// matter how many times setup runs.
func installDaemon(ctx context.Context, runner CommandRunner, plan daemonPlan) error {
	if !plan.supported() {
		return fmt.Errorf("no supported service manager for %s", plan.OS)
	}
	if err := os.MkdirAll(filepath.Dir(plan.UnitPath), 0o755); err != nil {
		return fmt.Errorf("create service directory: %w", err)
	}
	if plan.Spec.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(plan.Spec.LogPath), 0o700); err != nil {
			return fmt.Errorf("create log directory: %w", err)
		}
	}
	if err := writeFileAtomic(plan.UnitPath, []byte(plan.Contents), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", plan.UnitPath, err)
	}
	if runner == nil || plan.NoRegister {
		// No-register: the unit file is on disk and byte-identical to what a
		// real install would write, and the caller's launchd/systemd session is
		// untouched.
		return nil
	}
	switch plan.Manager {
	case daemonManagerLaunchd:
		// Unload first so a re-run replaces a stale job instead of failing with
		// "service already loaded" — this is the idempotence hinge on darwin.
		_, _, _ = runner.Run(ctx, "", "launchctl", "unload", plan.UnitPath)
		if _, stderr, err := runner.Run(ctx, "", "launchctl", "load", "-w", plan.UnitPath); err != nil {
			return fmt.Errorf("launchctl load: %w: %s", err, strings.TrimSpace(string(stderr)))
		}
	case daemonManagerSystemd:
		if _, stderr, err := runner.Run(ctx, "", "systemctl", "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(stderr)))
		}
		if _, stderr, err := runner.Run(ctx, "", "systemctl", "--user", "enable", "--now", plan.Label); err != nil {
			return fmt.Errorf("systemctl enable --now: %w: %s", err, strings.TrimSpace(string(stderr)))
		}
	}
	return nil
}

// uninstallDaemon stops the service and removes the unit. Missing state is not
// an error: uninstalling twice must succeed.
func uninstallDaemon(ctx context.Context, runner CommandRunner, plan daemonPlan) error {
	if !plan.supported() {
		return nil
	}
	if _, err := os.Stat(plan.UnitPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if runner != nil && !plan.NoRegister {
		switch plan.Manager {
		case daemonManagerLaunchd:
			if _, stderr, err := runner.Run(ctx, "", "launchctl", "unload", "-w", plan.UnitPath); err != nil {
				return fmt.Errorf("launchctl unload: %w: %s", err, strings.TrimSpace(string(stderr)))
			}
		case daemonManagerSystemd:
			if _, stderr, err := runner.Run(ctx, "", "systemctl", "--user", "disable", "--now", plan.Label); err != nil {
				return fmt.Errorf("systemctl disable --now: %w: %s", err, strings.TrimSpace(string(stderr)))
			}
		}
	}
	if err := os.Remove(plan.UnitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", plan.UnitPath, err)
	}
	if runner != nil && !plan.NoRegister && plan.Manager == daemonManagerSystemd {
		_, _, _ = runner.Run(ctx, "", "systemctl", "--user", "daemon-reload")
	}
	return nil
}

const (
	// daemonLogMaxBytes caps the installed watcher's log before it is rotated.
	// The watcher is a KeepAlive service meant to run for months and it prints
	// several lines PER TICK PER REPO — plus, on a brain whose manifest cannot
	// be parsed, three error lines every tick forever. Nothing rotated that
	// file: not launchd (StandardOutPath is a plain append), not systemd
	// (append: likewise), and not this program, which caps the memory worker's
	// log at 1 MiB and the facts vitality log at 1 MiB but left the one log
	// that grows fastest and lives longest unbounded.
	//
	// 4 MiB plus one archive keeps roughly a week of healthy five-minute ticks
	// readable while bounding the whole thing at 8 MiB.
	daemonLogMaxBytes = 4 << 20
	// daemonLogArchiveSuffix names the single retained previous generation.
	daemonLogArchiveSuffix = ".1"
)

// daemonLogPath resolves the file the installed unit points stdout and stderr
// at. It is derived from the plugin state dir exactly as brainWatchDaemonPlan
// derives it, so the running watcher can find its own log without the unit
// having to tell it — which means no unit bytes change and no installed daemon
// is restarted to gain rotation.
func daemonLogPath(env EntireEnv) (string, error) {
	dirs, err := resolvePluginDirs(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(dirs.State, "logs", setupDaemonLogFile), nil
}

// rotateDaemonLogIfLarge copies the log aside and truncates it IN PLACE.
//
// Truncation rather than rename is the whole point: launchd and systemd opened
// this file with O_APPEND before the process started and hold that descriptor
// for the life of the job. Renaming it would leave the service writing to the
// renamed inode forever and the fresh file permanently empty, so the log would
// appear to stop. An O_APPEND write always seeks to the end first, so after
// truncation the very next line lands at offset 0.
//
// Anything that is not a plain regular file is left alone, so a watcher run in
// a terminal, or one whose log path has been replaced with a symlink, is never
// touched.
func rotateDaemonLogIfLarge(path string, maxBytes int64) error {
	if maxBytes <= 0 {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= maxBytes {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path+daemonLogArchiveSuffix, data, 0o600); err != nil {
		return err
	}
	return os.Truncate(path, 0)
}

// daemonLogMaintainer returns the per-pass upkeep for a watcher that IS the
// installed service, and nil for every other caller.
//
// The test is identity, not configuration: the returned closure exists only
// when this process's stdout is the very file the unit names. A `workspace
// watch` run by hand in a terminal, a test with a buffer, or a second watcher
// redirected somewhere else therefore cannot truncate the daemon's log — only
// the process actually writing to it can.
func daemonLogMaintainer(env EntireEnv) func() {
	path, err := daemonLogPath(env)
	if err != nil {
		return nil
	}
	if !stdoutIsFile(path) {
		return nil
	}
	return func() { _ = rotateDaemonLogIfLarge(path, daemonLogMaxBytes) }
}

func stdoutIsFile(path string) bool {
	target, err := os.Stat(path)
	if err != nil {
		return false
	}
	mine, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return os.SameFile(mine, target)
}
