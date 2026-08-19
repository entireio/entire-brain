package cli

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
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
)

var daemonNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

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

// launchdLabel maps the daemon name into Apple's reverse-DNS convention while
// keeping the two names one-to-one: entire-brain-watch <-> io.entire.brain-watch.
func launchdLabel(name string) string {
	return "io.entire." + strings.TrimPrefix(name, "entire-")
}

// systemdUnitName is the daemon name with the unit suffix; systemd unit names
// are already plain identifiers, so no transformation is needed.
func systemdUnitName(name string) string { return name + ".service" }

// planBrainWatchDaemon resolves the spec against a target OS and home directory.
// goos/home/configHome are parameters, not ambient state, so both platforms'
// artifacts are testable from one machine.
func planBrainWatchDaemon(goos, home, configHome string, spec daemonSpec) (daemonPlan, error) {
	if strings.TrimSpace(spec.Name) == "" {
		spec.Name = daemonDefaultName
	}
	if err := validateDaemonName(spec.Name); err != nil {
		return daemonPlan{}, err
	}
	if strings.TrimSpace(spec.Binary) == "" {
		return daemonPlan{}, fmt.Errorf("daemon binary path is required")
	}
	plan := daemonPlan{OS: goos, Spec: spec}
	switch goos {
	case "darwin":
		if home == "" {
			return daemonPlan{}, fmt.Errorf("cannot resolve home directory for the launchd agent")
		}
		plan.Manager = daemonManagerLaunchd
		plan.Label = launchdLabel(spec.Name)
		plan.UnitPath = filepath.Join(home, "Library", "LaunchAgents", plan.Label+".plist")
		plan.Contents = renderLaunchdPlist(plan.Label, spec)
	case "linux":
		root := configHome
		if root == "" {
			if home == "" {
				return daemonPlan{}, fmt.Errorf("cannot resolve config directory for the systemd user unit")
			}
			root = filepath.Join(home, ".config")
		}
		plan.Manager = daemonManagerSystemd
		plan.Label = systemdUnitName(spec.Name)
		plan.UnitPath = filepath.Join(root, "systemd", "user", plan.Label)
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

// systemdQuote renders a value for systemd's quoted argument syntax.
func systemdQuote(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(value) + `"`
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
		fmt.Fprintf(&b, "WorkingDirectory=%s\n", spec.WorkingDir)
	}
	for _, key := range sortedEnvKeys(spec.Env) {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(key+"="+spec.Env[key]))
	}
	if spec.LogPath != "" {
		fmt.Fprintf(&b, "StandardOutput=append:%s\n", spec.LogPath)
		fmt.Fprintf(&b, "StandardError=append:%s\n", spec.LogPath)
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
		state.Detail = "no supported service manager for " + plan.OS
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
