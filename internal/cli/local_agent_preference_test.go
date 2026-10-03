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
func TestLocalModelIsPreferredOverACloudAgent(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		present []string
		want    string
	}{
		"ollama alone is selected":               {[]string{"ollama"}, "ollama"},
		"ollama wins over codex":                 {[]string{"ollama", "codex"}, "ollama"},
		"ollama wins over every cloud agent":     {[]string{"ollama", "codex", "claude"}, "ollama"},
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
