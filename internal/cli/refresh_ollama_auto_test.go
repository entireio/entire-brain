package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestAutoOllamaFallbackIsDisclosedByDistill(t *testing.T) {
	opts, _, brainDir := statusTruthFixture(t)
	writeDistillFixtureAt(t, brainDir, opts.Now())
	runner := opts.Runner.(*fakeCommandRunner)
	for _, name := range []string{"ollama", "codex"} {
		runner.responses[fakeCommandKey(name, "--version")] = fakeCommandResponse{stdout: "available"}
	}
	var out, diagnostic bytes.Buffer
	cmd := NewRootCommand(opts)
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostic)
	cmd.SetArgs([]string{"distill", "--agent", "auto", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ollama", "--model", "codex"} {
		if !strings.Contains(diagnostic.String(), want) {
			t.Errorf("fallback diagnostic lacks %q: %s", want, diagnostic.String())
		}
	}
}

type fakeWhichRunner struct{ present map[string]bool }

func (f fakeWhichRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if len(args) > 0 && f.present[args[0]] {
		return []byte("/usr/local/bin/" + args[0] + "\n"), nil, nil
	}
	if f.present[name] {
		return []byte("ok\n"), nil, nil
	}
	return nil, nil, context.Canceled
}

// `auto` used to resolve to ollama whenever the binary was on PATH, but
// execOllamaDistillAgent hard-errors with "--agent ollama requires --model"
// and auto has no model to give: the distill path reads it from --model alone,
// and ENTIRE_BRAIN_OLLAMA_MODEL is the EMBEDDING path's variable, not this one.
//
// So merely installing ollama broke distill, refresh, watch --distill,
// remember without --path, and recall --expand. `ollama --version` succeeds
// with no server and no models pulled, so installation alone was enough.
func TestAutoNeverResolvesToOllama(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	all := fakeWhichRunner{present: map[string]bool{"ollama": true, "codex": true, "claude": true}}
	if got := defaultRefreshAgent(ctx, all, t.TempDir()); got == "ollama" {
		t.Error("auto must not choose ollama: it cannot supply the --model ollama requires")
	}

	// It still chooses a working agent rather than giving up.
	if got := defaultRefreshAgent(ctx, all, t.TempDir()); got != "codex" {
		t.Errorf("auto should fall through to the next usable agent, got %q", got)
	}
	onlyClaude := fakeWhichRunner{present: map[string]bool{"ollama": true, "claude": true}}
	if got := defaultRefreshAgent(ctx, onlyClaude, t.TempDir()); got != "claude-code" {
		t.Errorf("with only claude beside ollama, want claude-code, got %q", got)
	}
	// Nothing usable is still "none", not ollama.
	onlyOllama := fakeWhichRunner{present: map[string]bool{"ollama": true}}
	if got := defaultRefreshAgent(ctx, onlyOllama, t.TempDir()); got != "none" {
		t.Errorf("ollama alone cannot distill automatically, want none, got %q", got)
	}
}

// Falling through must not be silent: a user who installed ollama for privacy
// needs to know a cloud agent ran instead, and how to get the local one.
func TestFallingBackFromOllamaIsDisclosed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	all := fakeWhichRunner{present: map[string]bool{"ollama": true, "codex": true}}

	warn := ollamaModelMissingWarning(ctx, all, t.TempDir(), "codex")
	for _, want := range []string{"ollama", "--model", "codex"} {
		if !strings.Contains(warn, want) {
			t.Errorf("the warning must mention %q: %q", want, warn)
		}
	}

	// No warning when ollama was actually used, when nothing ran, or when
	// ollama is not installed at all -- otherwise it is noise.
	if got := ollamaModelMissingWarning(ctx, all, t.TempDir(), "ollama"); got != "" {
		t.Errorf("no warning when ollama was chosen: %q", got)
	}
	if got := ollamaModelMissingWarning(ctx, all, t.TempDir(), "none"); got != "" {
		t.Errorf("no warning when no agent ran: %q", got)
	}
	noOllama := fakeWhichRunner{present: map[string]bool{"codex": true}}
	if got := ollamaModelMissingWarning(ctx, noOllama, t.TempDir(), "codex"); got != "" {
		t.Errorf("no warning when ollama is not installed: %q", got)
	}
}
