package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ashtom/entire-brain/internal/tui"
)

func setupRenderFixtureReport() setupReport {
	return setupReport{
		SchemaVersion: 1,
		GeneratedAt:   setupTestNow,
		Repo:          "/repo",
		RepoKey:       "gh/demo-org/brain-demo",
		BrainPath:     "/brain",
		Instant: setupPhase{
			State:   "degraded",
			Seconds: 0.7,
			Components: []setupComponent{
				{Name: brainComponentSessions, State: "failed", Detail: "checkpoint_scope_incomplete", Hint: "add a git remote"},
				{Name: brainComponentSeed, State: "ok"},
				{Name: brainComponentDocs, State: "ok"},
				{Name: brainComponentSemantic, State: "ok"},
			},
		},
		Backfill:  setupPhase{State: "skipped", Detail: "--no-backfill"},
		Workspace: setupWorkspaceState{Name: "default", Registered: true},
		Daemon:    daemonState{Manager: daemonManagerUnsupported, Detail: "skipped: --no-daemon"},
		Facts:     factsBackfillStatus{Sessions: 618, Distilled: 300},
	}
}

func renderSetupSummaryPlain(t *testing.T, report setupReport, timings *setupTimings) string {
	t.Helper()
	out := &bytes.Buffer{}
	theme, _ := tui.ThemeByName("default")
	renderSetupSummary(out, tui.NewRendererWith(theme, tui.PlainCaps()), report, timings, setupWatchPlan{}, setupBrainBinaryName)
	return out.String()
}

// TestSetupSummaryPlainGolden pins the closing block in plain mode. Timings are
// supplied explicitly so the golden is a function of the report, not of how
// fast the test machine is.
func TestSetupSummaryPlainGolden(t *testing.T) {
	t.Parallel()
	timings := &setupTimings{}
	timings.record("instant", 700*time.Millisecond)
	timings.record("backfill", 0)
	timings.record("workspace", 30*time.Millisecond)
	timings.record("daemon", 0)

	got := renderSetupSummaryPlain(t, setupRenderFixtureReport(), timings)
	const want = "\nBrain ready (degraded) in 730ms\n" +
		"  repo        /repo  (gh/demo-org/brain-demo)\n" +
		"  brain       /brain\n" +
		"  instant     x sessions  + seed  + docs  + semantic  (700ms)\n" +
		"              x session export: checkpoint_scope_incomplete\n" +
		"                hint: add a git remote\n" +
		"  facts       ######........ 300/618 sessions distilled\n" +
		"  backfill    - skipped (--no-backfill)  (0s)\n" +
		"  workspace   + default  (30ms)\n" +
		"  daemon      - skipped: --no-daemon  (0s)\n" +
		"\nNext\n" +
		"  entire-brain overview        what this project is\n" +
		"  entire-brain brief \"<task>\"  task-shaped context\n" +
		"  entire-brain status          backfill progress and daemon health\n"
	if got != want {
		t.Fatalf("setup summary\n got:\n%s\nwant:\n%s", got, want)
	}
}

// TestSetupSummaryPlainHasNoNonASCII is the "□" guard applied to the summary:
// nothing in the closing block may assume a UTF-8 locale.
func TestSetupSummaryPlainHasNoNonASCII(t *testing.T) {
	t.Parallel()
	got := renderSetupSummaryPlain(t, setupRenderFixtureReport(), nil)
	for _, r := range got {
		if r > 127 {
			t.Fatalf("non-ASCII %q in the plain summary:\n%s", r, got)
		}
	}
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("plain summary carries escape sequences:\n%s", got)
	}
}

// TestSetupSummaryColoursEveryPhaseDistinctly proves the phase colour-coding
// reaches the summary, not just the live lines.
func TestSetupSummaryColoursEveryPhaseDistinctly(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	theme, _ := tui.ThemeByName("default")
	render := tui.NewRendererWith(theme, tui.Caps{TTY: true, Color: true, Unicode: true, Width: 200})
	renderSetupSummary(out, render, setupRenderFixtureReport(), nil, setupWatchPlan{}, setupBrainBinaryName)
	got := out.String()
	for name, color := range map[string]string{
		"good":    string(render.PhaseColor(tui.PhaseDone)),
		"bad":     string(render.PhaseColor(tui.PhaseFailed)),
		"skipped": string(render.PhaseColor(tui.PhaseSkipped)),
		"daemon":  string(render.PhaseColor(tui.PhaseDaemon)),
	} {
		if !strings.Contains(got, "38;5;"+color+"m") {
			t.Errorf("the %s phase colour (%s) never appears in the summary:\n%s", name, color, got)
		}
	}
}

// TestSetupJSONStdoutCarriesOnlyTheReport is the --json contract at the command
// level: every friendly line, spinner, bar and colour must stay off stdout so
// the report is parseable. This is the property the renderer work must not have
// broken, and it is checked against a REAL setup run rather than a renderer
// call, because the leak this guards against would come from a caller printing
// around the renderer.
func TestSetupJSONStdoutCarriesOnlyTheReport(t *testing.T) {
	f := newSetupTestFixture(t, "s1")
	rec := &recordedSetup{}
	opts := defaultSetupOptions()
	opts.noDaemon = true
	opts.noBackfill = true
	opts.json = true

	out := &bytes.Buffer{}
	cmd := setupTestCommand(t, out, opts)
	if err := runSetup(context.Background(), cmd, f.opts, opts, f.repoDir, rec.steps(f)); err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	var report setupReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("--json stdout is not a bare JSON document (%v):\n%s", err, out.String())
	}
	if report.RepoKey == "" {
		t.Fatalf("--json report is empty:\n%s", out.String())
	}
	if strings.ContainsRune(out.String(), 0x1b) || strings.Contains(out.String(), "\r") {
		t.Fatalf("--json stdout carries terminal control characters:\n%q", out.String())
	}
}

func TestSetupComponentMarks(t *testing.T) {
	t.Parallel()
	for state, want := range map[string]tui.Mark{
		"ok":      tui.MarkDone,
		"built":   tui.MarkDone,
		"failed":  tui.MarkFailed,
		"skipped": tui.MarkSkipped,
		"missing": tui.MarkPending,
		"":        tui.MarkPending,
	} {
		if got := setupComponentMark(state); got != want {
			t.Errorf("setupComponentMark(%q) = %v, want %v", state, got, want)
		}
	}
}

func TestSetupTimingsTotal(t *testing.T) {
	t.Parallel()
	timings := &setupTimings{}
	timings.record("instant", 700*time.Millisecond)
	timings.record("daemon", 300*time.Millisecond)
	if got := timings.total(); got != time.Second {
		t.Errorf("total = %v, want 1s", got)
	}
	if got, ok := timings.get("daemon"); !ok || got != 300*time.Millisecond {
		t.Errorf("get(daemon) = %v %t", got, ok)
	}
	if _, ok := timings.get("absent"); ok {
		t.Error("get on an unrecorded phase must report false")
	}
	// A nil timings set is the "not measured" case and must not panic.
	var nilTimings *setupTimings
	nilTimings.record("x", time.Second)
	if got := nilTimings.total(); got != 0 {
		t.Errorf("nil total = %v", got)
	}
}

// TestSetupComponentObserverTicksLive proves components are reported AS THEY
// LAND. Without it the live tick row would be a lie: every mark would appear at
// once when the phase returned.
func TestSetupComponentObserverTicksLive(t *testing.T) {
	t.Parallel()
	observer := &setupComponentObserver{}
	var seen []string
	observer.bind(func(component setupComponent) { seen = append(seen, component.Name) })
	observer.notify(setupComponent{Name: "seed", State: "ok"})
	observer.notify(setupComponent{Name: "docs", State: "ok"})
	observer.bind(nil)
	observer.notify(setupComponent{Name: "late", State: "ok"})
	if strings.Join(seen, ",") != "seed,docs" {
		t.Fatalf("observed %v; an unbound observer must drop notifications", seen)
	}
	// A nil observer is the injected-steps case and must be inert, not fatal.
	var nilObserver *setupComponentObserver
	nilObserver.bind(func(setupComponent) { t.Fatal("nil observer called back") })
	nilObserver.notify(setupComponent{Name: "x"})
}
