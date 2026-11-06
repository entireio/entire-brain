package hook

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/base"
)

// hookPromptMinScore is the relevance bar for matching a submitted prompt
// against the branch's memories. It is the highest bar of any hook verb, and it
// is MEASURED rather than chosen.
//
// This hook fires at the start of every turn, which is the noisiest position in
// the product: a memory that is merely topical trains an agent to skim past the
// channel, and that costs more than the silence it replaced.
//
// The full distribution of every nonzero score, real scorer over the 1,968
// active memories on this repo's main branch, across ten realistic prompts:
//
//	10 : 1789      40 :  243   <- the single-locus-hit floor
//	18 :  301      48 :   24
//	20 :  150      50 :   26
//	28 :   38      58 :    5
//	30 :   17      60 :    2
//	38 :    4      70 :    1
//	               118:    1
//
// Two things decide the bar. The spike at 40 is 243 of the 305 hits at or above
// it, and it is exactly where the two VAGUEST prompts top out ("update the
// readme" and "investigate why the daemon is at 113% cpu" both max at 40) — so
// a bar of 40, which is what post-failure uses, would fire on a prompt that
// says nothing. And 41..47 is empty across the whole distribution (width 7),
// which is where a threshold belongs.
//
// At 45 the two vague prompts go silent and the specific ones still fire, with
// roughly six candidates each before the token budget trims them. The band was
// read from the FULL histogram, not from the top-N per prompt: a convenience
// sample of the highest scores manufactures a gap in the middle, which is how
// the pre-edit stem gate was first set wrong.
const hookPromptMinScore = 45

// claudePromptEvent is the subset of Claude Code's UserPromptSubmit payload
// this hook needs. Every other field is ignored rather than rejected: the host
// owns the shape and adds to it.
//
// Captured, not inferred. The host's own hook table documents the event as
// "| UserPromptSubmit | - | When user submits |" — the dash is the matcher
// column, so this event takes NO tool matcher, unlike PreToolUse and
// PostToolUseFailure. It accepts additionalContext, and the Entire CLI already
// decodes `prompt` from it in production (agent/claudecode's
// userPromptSubmitRaw).
//
// UserPromptSubmit is the only event that hands over a task string at all.
// SessionStart has no task yet, and SubagentStart carries `subagent_type`, not
// the work to be done — so neither can feed a task-scoped retrieval however it
// is wired.
type claudePromptEvent struct {
	Prompt string `json:"prompt"`
}

// DecodeClaudePromptEvent reads one UserPromptSubmit event and returns the task
// text. Anything the hook must stay silent about — a nil reader, no bytes,
// unparseable JSON, an empty prompt — is ("", false), never an error.
func DecodeClaudePromptEvent(r io.Reader) (string, bool) {
	if r == nil {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(r, maxClaudeHookEventBytes))
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return "", false
	}
	var event claudePromptEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return "", false
	}
	prompt := strings.TrimSpace(event.Prompt)
	if prompt == "" {
		return "", false
	}
	return failureTail(prompt), true
}

// MatchPrompt ranks the branch's memories against a submitted prompt.
//
// Unlike MatchFailure this considers EVERY kind, not just gotcha and
// closed-negative: at the start of a turn a recorded preference or convention
// ("how this project wants things done") is as useful as a trap, where at a
// failure it is not. Precision comes from hookPromptMinScore instead, which is
// the measured bar above rather than a kind filter.
func MatchPrompt(facts []factmerge.Record, prompt string, score func(f factmerge.Record, query string, queryLocus []string) int) []factmerge.Record {
	promptLocus := base.FactLocus(prompt)
	type scored struct {
		rec factmerge.Record
		n   int
	}
	var hits []scored
	for _, f := range facts {
		if f.Status != factmerge.StatusActive {
			continue
		}
		if n := score(f, prompt, promptLocus); n >= hookPromptMinScore {
			hits = append(hits, scored{f, n})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].n != hits[j].n {
			return hits[i].n > hits[j].n
		}
		return hits[i].rec.UpdatedAt.After(hits[j].rec.UpdatedAt)
	})
	out := make([]factmerge.Record, len(hits))
	for i, h := range hits {
		out[i] = h.rec
	}
	return out
}
