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

	// envDaemonUnitDir redirects the unit file (launchd plist / systemd user
	// unit) somewhere other than the real service-manager directory. Tests and
	// smoke runs set it so a run with a REAL $HOME can never write into the
	// developer's ~/Library/LaunchAgents or ~/.config/systemd/user. Redirecting
	// $HOME alone is not enough: a sandboxed process that still sees the real
	// home would install a live agent.
	envDaemonUnitDir = "ENTIRE_BRAIN_DAEMON_DIR"
)

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
		state.Current = string(data) == plan.Contents
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
	if err := writeFileAtomic(plan.UnitPath, []byte(plan.Contents), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", plan.UnitPath, err)
	}
	if runner == nil {
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
	if runner != nil {
		switch plan.Manager {
		case daemonManagerLaunchd:
			_, _, _ = runner.Run(ctx, "", "launchctl", "unload", "-w", plan.UnitPath)
		case daemonManagerSystemd:
			_, _, _ = runner.Run(ctx, "", "systemctl", "--user", "disable", "--now", plan.Label)
		}
	}
	if err := os.Remove(plan.UnitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", plan.UnitPath, err)
	}
	if runner != nil && plan.Manager == daemonManagerSystemd {
		_, _, _ = runner.Run(ctx, "", "systemctl", "--user", "daemon-reload")
	}
	return nil
}
