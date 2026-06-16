package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Task candidates (Pattern Consolidation, Phase 2 redo).
//
// A task candidate is a RECURRING USER INTENT — not a command shape. Command
// n-grams were the wrong unit (they only ever surface generic, already-known
// operations); the thing worth turning into a skill is a task the user keeps
// doing in this repo, for which the brain holds non-obvious knowledge. Candidates
// are deterministic and cheap to compute; turning one into an actual SKILL.md is
// the agent-synthesis step (skill_synthesis.go), which is also where the
// "does this encode anything a model doesn't already know?" gate lives.

const patternsTasksPath = "patterns/tasks.ndjson"

const (
	taskMinSupport      = 3 // distinct episodes sharing the intent
	taskMinWithCommands = 2 // episodes that actually ran commands (drops chatter)
	taskTopCommands     = 6
	taskSampleIntents   = 3
)

// nonTaskLeadWords head conversational continuations/acknowledgements, not tasks.
// An intent signature starting with one of these clusters unrelated episodes that
// merely followed the same filler word ("yes", "keep going", "great now …"), so
// it is not a coherent task candidate.
var nonTaskLeadWords = map[string]bool{
	"yes": true, "no": true, "ok": true, "okay": true, "sure": true, "yep": true,
	"yeah": true, "great": true, "thanks": true, "thx": true, "keep": true,
	"now": true, "also": true, "hmm": true, "wait": true, "actually": true,
	"go": true, "nice": true, "perfect": true, "cool": true, "right": true,
	"done": true, "continue": true, "please": true, "good": true,
}

func nonTaskSignature(sig string) bool {
	head := sig
	if i := strings.IndexByte(sig, ':'); i >= 0 {
		head = sig[:i]
	}
	return nonTaskLeadWords[head]
}

type taskCandidate struct {
	ID              string              `json:"id"`
	RepoKey         string              `json:"repo_key,omitempty"`
	IntentSignature string              `json:"intent_signature"`
	Label           string              `json:"label"`
	Support         int                 `json:"support"`
	WithCommands    int                 `json:"with_commands"`
	Reinforcement   reinforcementCounts `json:"reinforcement"`
	Commands        []string            `json:"commands,omitempty"`
	SampleIntents   []string            `json:"sample_intents,omitempty"`
	Examples        []episodeAnchor     `json:"examples,omitempty"`
	Strength        float64             `json:"strength"`
	StrengthLabel   string              `json:"strength_label"`
}

func taskCandidateID(repoKey, signature string) string {
	sum := sha256.Sum256([]byte(repoKey + "\x00" + signature))
	return "task:" + hex.EncodeToString(sum[:])
}

// buildTaskCandidates clusters episodes by intent signature into recurring tasks.
func buildTaskCandidates(episodes []episodeRecord) []taskCandidate {
	type agg struct {
		signature    string
		support      int
		withCommands int
		reinf        reinforcementCounts
		cmdFreq      map[string]int
		intents      []string
		anchors      []episodeAnchor
		repoKey      string
	}
	aggs := map[string]*agg{}
	order := []string{}
	for i := range episodes {
		ep := &episodes[i]
		sig := ep.IntentSignature
		if sig == "" {
			continue
		}
		a := aggs[sig]
		if a == nil {
			a = &agg{signature: sig, cmdFreq: map[string]int{}, repoKey: ep.RepoKey}
			aggs[sig] = a
			order = append(order, sig)
		}
		a.support++
		if len(ep.CommandSequence) > 0 {
			a.withCommands++
		}
		switch ep.Reinforcement {
		case reinforcementSuccess:
			a.reinf.Success++
		case reinforcementCorrected:
			a.reinf.Corrected++
		default:
			a.reinf.Neutral++
		}
		for _, c := range ep.CommandSequence {
			a.cmdFreq[c]++
		}
		if ep.Intent != "" && len(a.intents) < 12 {
			a.intents = append(a.intents, ep.Intent)
		}
		if len(a.anchors) < 3 && ep.Source.Path != "" {
			a.anchors = append(a.anchors, ep.Source)
		}
	}

	var out []taskCandidate
	for _, sig := range order {
		a := aggs[sig]
		if a.support < taskMinSupport || a.withCommands < taskMinWithCommands || nonTaskSignature(sig) {
			continue
		}
		strength := taskStrength(a.support, a.withCommands, a.reinf)
		out = append(out, taskCandidate{
			ID:              taskCandidateID(a.repoKey, sig),
			RepoKey:         a.repoKey,
			IntentSignature: sig,
			Label:          representativeIntent(a.intents),
			Support:        a.support,
			WithCommands:   a.withCommands,
			Reinforcement:  a.reinf,
			Commands:       topCommands(a.cmdFreq, taskTopCommands),
			SampleIntents:  distinctSample(a.intents, taskSampleIntents),
			Examples:       a.anchors,
			Strength:       math.Round(strength*1000) / 1000,
			StrengthLabel:  strengthLabel(strength),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Strength != out[j].Strength {
			return out[i].Strength > out[j].Strength
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func taskStrength(support, withCommands int, r reinforcementCounts) float64 {
	recurrence := clamp01(math.Log2(1+float64(support)) / math.Log2(1+15))
	activity := 0.0
	if support > 0 {
		activity = clamp01(float64(withCommands) / float64(support))
	}
	reinf := 0.5
	if support > 0 {
		reinf = clamp01(0.5 + 0.5*float64(r.Success-r.Corrected)/float64(support))
	}
	return 0.45*recurrence + 0.30*activity + 0.25*reinf
}

// representativeIntent picks the longest sample intent (most descriptive),
// truncated — the human label for the cluster.
func representativeIntent(intents []string) string {
	best := ""
	for _, s := range intents {
		if len(s) > len(best) {
			best = s
		}
	}
	return truncateString(best, 160)
}

func distinctSample(intents []string, n int) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range intents {
		key := strings.ToLower(strings.TrimSpace(s))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, truncateString(s, 160))
		if len(out) >= n {
			break
		}
	}
	return out
}

func topCommands(freq map[string]int, n int) []string {
	type kv struct {
		cmd string
		n   int
	}
	var items []kv
	for c, k := range freq {
		items = append(items, kv{c, k})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].n != items[j].n {
			return items[i].n > items[j].n
		}
		return items[i].cmd < items[j].cmd
	})
	var out []string
	for _, it := range items {
		if len(out) >= n {
			break
		}
		out = append(out, it.cmd)
	}
	return out
}

func writeBrainTasksFile(outputDir string, tasks []taskCandidate) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, t := range tasks {
		if err := enc.Encode(t); err != nil {
			return fmt.Errorf("encode task: %w", err)
		}
	}
	if err := writeBrainRelativeFileAtomic(outputDir, patternsTasksPath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write tasks: %w", err)
	}
	return nil
}

func loadBrainTasks(brainDir string) ([]taskCandidate, error) {
	content, err := readBrainRelativeFile(brainDir, patternsTasksPath)
	if err != nil {
		return nil, nil
	}
	var tasks []taskCandidate
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var t taskCandidate
		if err := json.Unmarshal([]byte(line), &t); err != nil {
			return nil, fmt.Errorf("parse task line: %w", err)
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}
