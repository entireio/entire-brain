package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/internal/tui"
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
		Agents: setupAgentsState{
			Installed:   true,
			Files:       []string{".entire/agent-guide.md", "AGENTS.md", "CLAUDE.md"},
			Changed:     []string{".entire/agent-guide.md", "AGENTS.md", "CLAUDE.md"},
			Hook:        "/repo/.claude/settings.json",
			HookChanged: true,
		},
		Facts: factsBackfillStatus{Sessions: 618, Distilled: 300},
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
		"missing": tui.MarkSkipped,
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
