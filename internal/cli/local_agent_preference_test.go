package cli

import (
	"context"
	"strings"
	"testing"
)

// availabilityRunner reports a fixed set of commands as present on PATH.
type availabilityRunner struct{ present map[string]bool }

// commandLooksAvailable probes with Run(ctx, dir, <command>, "--version"), so
// the command is the NAME argument, not one of args.
func (a *availabilityRunner) Run(ctx context.Context, dir, name string, args ...string) ([]byte, []byte, error) {
	if a.present[name] {
		return []byte(name + " 1.0.0\n"), nil, nil
	}
	return nil, nil, context.Canceled
}

// Issue #328: a user who installed a local model to keep transcripts off a
// third party got either no distillation at all, or -- if a cloud CLI happened
// to be installed too -- their sessions sent to that provider instead.
//
// AMENDED. The original fix preferred ollama whenever the binary was on PATH,
// and that premise does not hold: AUTO selection cannot supply the --model
// that execOllamaDistillAgent requires (the distill path reads it from the
// flag alone; ENTIRE_BRAIN_OLLAMA_MODEL belongs to the embedding path). So
// "auto -> ollama" did not keep transcripts local -- it returned
// "distill: --agent ollama requires --model" and nobody got distillation at
// all. Installing ollama was enough to break distill, refresh,
// watch --distill, remember without --path and recall --expand.
//
// Auto therefore selects an agent that can actually run, and the concern
// behind #328 -- sessions going to a cloud provider SILENTLY -- is covered by
// ollamaModelMissingWarning, which names the fallback and how to get the local
// path back. Explicit `--agent ollama --model <name>` is unchanged.
//
// A model is not auto-picked: `ollama list` routinely offers an embedding
// model such as nomic-embed-text, which would return nonsense for
// distillation. Choosing one is a product decision, not a default.
func TestLocalModelIsPreferredOverACloudAgent(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		present []string
		want    string
	}{
		// Ollama alone cannot distill automatically, so auto reports none
		// rather than selecting an agent that will error.
		"ollama alone cannot be auto-selected":   {[]string{"ollama"}, "none"},
		"codex runs when ollama has no model":    {[]string{"ollama", "codex"}, "codex"},
		"claude runs when ollama has no model":   {[]string{"ollama", "claude"}, "claude-code"},
		"codex still used when ollama is absent": {[]string{"codex"}, "codex"},
		"claude still used as the last resort":   {[]string{"claude"}, "claude-code"},
		"nothing installed means none":           {nil, "none"},
	} {
		t.Run(name, func(t *testing.T) {
			present := map[string]bool{}
			for _, c := range tc.present {
				present[c] = true
			}
			got := defaultRefreshAgent(context.Background(), &availabilityRunner{present: present}, t.TempDir())
			if got != tc.want {
				t.Fatalf("with %v installed, chose %q, want %q", tc.present, got, tc.want)
			}
		})
	}
}

// The watcher runs unattended on a timer, so a hardcoded cloud agent there
// sends transcripts off the machine without the user ever choosing it. The
// sentinel must be the one runDistill actually resolves: "auto", not "".
func TestWatchDefaultsToDetectionNotAHardcodedCloudAgent(t *testing.T) {
	t.Parallel()

	got := defaultWatchOptions().distillAgent
	if got == "codex" || got == "claude-code" {
		t.Fatalf("the watcher hardcodes the cloud agent %q (#328)", got)
	}
	if got != "auto" {
		t.Fatalf("watch distill agent is %q; only \"auto\" is resolved through defaultRefreshAgent, "+
			"anything else is passed through as an agent name", got)
	}
	// Non-vacuity: prove "auto" is really the sentinel the distill path reads.
	src := readSourceForTest(t, "distill_cmd.go")
	if !strings.Contains(src, `distillOpts.agent == "auto"`) {
		t.Fatal("distill no longer resolves the \"auto\" sentinel; the watcher default would pass through unresolved")
	}
}
