package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
)

// The shape Claude Code delivers for UserPromptSubmit. The host's own hook
// table documents the event as "| UserPromptSubmit | - | When user submits |",
// and the Entire CLI already decodes `prompt` from it in production.
const realPromptEvent = `{"session_id":"20373dcb","transcript_path":"/x/y.jsonl",
  "cwd":"/repo","hook_event_name":"UserPromptSubmit",
  "prompt":"why does entities migrate refuse the whole range"}`

func TestPromptEventCarriesTheTaskText(t *testing.T) {
	t.Parallel()

	got, ok := DecodeClaudePromptEvent(strings.NewReader(realPromptEvent))
	if !ok {
		t.Fatal("the real payload must decode")
	}
	if got != "why does entities migrate refuse the whole range" {
		t.Errorf("prompt = %q", got)
	}
}

func TestPromptEventSilentCases(t *testing.T) {
	t.Parallel()

	for name, in := range map[string]string{
		"not json":      "this is not json at all",
		"truncated":     `{"prompt":"hi"`,
		"no prompt":     `{"hook_event_name":"UserPromptSubmit"}`,
		"blank prompt":  `{"prompt":"   \n\t "}`,
		"wrong type":    `{"prompt":123}`,
		"empty object":  `{}`,
		"empty":         "",
		"only spaces":   "   \n ",
		"null document": "null",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, ok := DecodeClaudePromptEvent(strings.NewReader(in)); ok {
				t.Fatalf("%s must be silent, got %q", name, got)
			}
		})
	}
	if _, ok := DecodeClaudePromptEvent(nil); ok {
		t.Fatal("a nil reader must be silent")
	}
}

// THE BAR IS THE WHOLE DESIGN of this verb, because it fires on every turn.
// It is 45, placed in the empty 41..47 band of the measured full distribution,
// specifically to clear the 243-strong spike at 40 -- which is where the two
// vaguest real prompts topped out. A boundary test, not a vibe test.
func TestPromptBarExcludesTheFortySpikeAndAdmitsAbove(t *testing.T) {
	t.Parallel()

	fact := func(id string) factmerge.Record {
		return factmerge.Record{
			ID: id, Paths: []string{"architecture.retrieval.decision"},
			Text: "a memory " + id, Branch: "main", Origin: "authored",
			Status: factmerge.StatusActive, UpdatedAt: time.Unix(0, 0),
		}
	}
	for _, tc := range []struct {
		score int
		want  bool
	}{
		{10, false}, // the 1789-strong floor
		{40, false}, // THE SPIKE: one locus hit, reached by "update the readme"
		{44, false}, // top of the empty band
		{45, true},  // the bar itself
		{48, true},  // first real cluster above it
		{50, true},
		{118, true},
	} {
		facts := []factmerge.Record{fact("f1")}
		got := MatchPrompt(facts, "a task", func(factmerge.Record, string, []string) int { return tc.score })
		if (len(got) == 1) != tc.want {
			t.Errorf("score %d: admitted=%v want %v", tc.score, len(got) == 1, tc.want)
		}
	}
}

// MatchPrompt deliberately diverges from MatchFailure: every kind is
// considered, not just gotcha and closed-negative. At the start of a turn a
// recorded preference or convention is as useful as a trap; at a failure it is
// not. Precision comes from the bar instead of a kind filter.
func TestPromptConsidersEveryKindUnlikeFailure(t *testing.T) {
	t.Parallel()

	kinds := []string{"decision", "invariant", "gotcha", "preference", "convention", "closed-negative"}
	var facts []factmerge.Record
	for i, kind := range kinds {
		facts = append(facts, factmerge.Record{
			ID: "f" + kind, Paths: []string{"workflow.x.convention"}, Kind: kind,
			Text: "memory about " + kind, Branch: "main", Origin: "authored",
			Status: factmerge.StatusActive, UpdatedAt: time.Unix(int64(i), 0),
		})
	}
	always := func(factmerge.Record, string, []string) int { return 100 }

	if got := MatchPrompt(facts, "a task", always); len(got) != len(kinds) {
		t.Errorf("MatchPrompt admitted %d of %d kinds; a preference or convention at turn start is useful", len(got), len(kinds))
	}
	// The contrast that justifies the divergence.
	if got := MatchFailure(facts, "a task", always); len(got) != 2 {
		t.Errorf("MatchFailure admitted %d kinds, want only gotcha and closed-negative", len(got))
	}
}

func TestPromptSkipsInactiveMemories(t *testing.T) {
	t.Parallel()

	facts := []factmerge.Record{
		{ID: "a", Text: "superseded", Status: factmerge.StatusSuperseded, Branch: "main"},
		{ID: "b", Text: "retracted", Status: "retracted", Branch: "main"},
		{ID: "c", Text: "active", Status: factmerge.StatusActive, Branch: "main"},
	}
	got := MatchPrompt(facts, "a task", func(factmerge.Record, string, []string) int { return 100 })
	if len(got) != 1 || got[0].ID != "c" {
		t.Errorf("only the active memory may be served, got %+v", got)
	}
}

// Same bug class as post-failure: the envelope is keyed by event name and a
// host silently drops one whose key does not match the event it fired.
func TestPromptClaudeHookAnswersWithTheUserPromptSubmitEnvelope(t *testing.T) {
	repoDir := t.TempDir()
	brainDir := t.TempDir()

	facts := []factmerge.Record{{
		ID: "fact:0001", Paths: []string{"project.regression.gotcha"}, Kind: "gotcha",
		Text:  "entities migrate refuses a range on any warning, even one naming an untouched file",
		Locus: []string{"migrate", "warnings"}, Branch: "main",
		Origin: "authored", Status: "active",
	}}
	if err := factstore.Write(brainDir, "main", facts); err != nil {
		t.Fatalf("write facts: %v", err)
	}

	var servedSurface string
	cfg := Config{Deps: Deps{
		ResolveTarget: func(context.Context, base.Options, string) (string, string, string, error) {
			return repoDir, brainDir, "main", nil
		},
		FactLexicalScore: func(factmerge.Record, string, []string) int { return 500 },
		RecordServed: func(_ io.Writer, _ context.Context, _ base.Options, _, _, surface, _, _ string, served []factmerge.Record) {
			servedSurface = surface
			if len(served) == 0 {
				t.Error("an emission that carried a memory must leave a receipt for it")
			}
		},
	}}

	var out bytes.Buffer
	cmd := NewPrompt(cfg)
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader(realPromptEvent))
	cmd.SetArgs([]string{"--claude-hook"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("prompt --claude-hook: %v", err)
	}

	var envelope struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("the reply is not a hook envelope: %v\n%s", err, out.String())
	}
	if got := envelope.HookSpecificOutput.HookEventName; got != "UserPromptSubmit" {
		t.Errorf("hookEventName = %q, want UserPromptSubmit; the host drops a mismatched key silently", got)
	}
	if !strings.Contains(envelope.HookSpecificOutput.AdditionalContext, "untouched file") {
		t.Errorf("the envelope carries no memory:\n%s", envelope.HookSpecificOutput.AdditionalContext)
	}
	if servedSurface != "hook-prompt" {
		t.Errorf("receipt surface = %q, want hook-prompt (distinguishable in vitality)", servedSurface)
	}
}

// An event with no prompt must emit nothing and never open the brain: this
// fires on every turn, so an empty envelope is noise on every turn.
func TestPromptClaudeHookStaysSilentWithoutAPrompt(t *testing.T) {
	resolved := false
	cfg := Config{Deps: Deps{
		ResolveTarget: func(context.Context, base.Options, string) (string, string, string, error) {
			resolved = true
			return t.TempDir(), t.TempDir(), "main", nil
		},
		FactLexicalScore: func(factmerge.Record, string, []string) int { return 500 },
		RecordServed: func(io.Writer, context.Context, base.Options, string, string, string, string, string, []factmerge.Record) {
			t.Error("a promptless event must not leave a serve receipt")
		},
	}}

	var out bytes.Buffer
	cmd := NewPrompt(cfg)
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader(`{"hook_event_name":"UserPromptSubmit","prompt":"  "}`))
	cmd.SetArgs([]string{"--claude-hook"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("a promptless event must exit 0, got %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("a promptless event must emit nothing, got %q", out.String())
	}
	if resolved {
		t.Error("a promptless event must be decided before the brain is opened")
	}
}

// Hand-running without --claude-hook and without --task is misuse, and must say
// so rather than reading anything.
func TestPromptWithoutATaskIsMisuse(t *testing.T) {
	t.Parallel()

	cmd := NewPrompt(Config{})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(nil)
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--task") {
		t.Fatalf("want a --task misuse error, got %v", err)
	}
}
