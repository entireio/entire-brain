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

// taskProcedure is a command shape (n-gram) that co-occurs within a task's
// episodes, with its corpus specificity — the "non-obvious operational evidence"
// that distinguishes a real task from a generic one.
type taskProcedure struct {
	Commands    []string `json:"commands"`
	Count       int      `json:"count"`
	Specificity float64  `json:"specificity"`
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
	Procedures      []taskProcedure     `json:"procedures,omitempty"`     // co-occurring procedure evidence
	MatchingFacts   []string            `json:"matching_facts,omitempty"` // repo-specific durable facts
	SampleIntents   []string            `json:"sample_intents,omitempty"`
	Examples        []episodeAnchor     `json:"examples,omitempty"`
	Workspace       string              `json:"workspace,omitempty"`      // workspace scope
	Repos           int                 `json:"repos,omitempty"`          // workspace scope
	RepoBreakdown   []patternRepoStat   `json:"repo_breakdown,omitempty"` // workspace scope
	Strength        float64             `json:"strength"`
	StrengthLabel   string              `json:"strength_label"`
}

func taskCandidateID(repoKey, signature string) string {
	sum := sha256.Sum256([]byte(repoKey + "\x00" + signature))
	return "task:" + hex.EncodeToString(sum[:])
}

const (
	taskMaxProcedures = 5
	taskMaxFacts      = 5
)

// buildTaskCandidates clusters episodes by intent signature into recurring tasks,
// attaches the co-occurring procedure evidence and matching durable facts, and
// REJECTS any candidate with no non-obvious evidence (no repo-specific command
// shape and no matching fact) before it ever reaches an agent. A candidate is
// therefore an intent + corroborating procedure evidence + outcome + repo facts,
// not just a shallow first-two-token signature.
func buildTaskCandidates(episodes []episodeRecord, facts []factRecord) []taskCandidate {
	idf, corpus := commandIDF(episodes)

	type agg struct {
		signature    string
		support      int
		withCommands int
		reinf        reinforcementCounts
		cmdFreq      map[string]int
		ngramCount   map[string]int
		ngramCmds    map[string][]string
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
			a = &agg{signature: sig, cmdFreq: map[string]int{}, ngramCount: map[string]int{}, ngramCmds: map[string][]string{}, repoKey: ep.RepoKey}
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
		// Co-occurring command shapes, counted once per episode.
		cmds := collapseRuns(ep.CommandSequence)
		seen := map[string]bool{}
		for n := procedureNGramMin; n <= procedureNGramMax; n++ {
			for _, w := range commandWindows(cmds, n) {
				key := strings.Join(w, "\x00")
				if seen[key] {
					continue
				}
				seen[key] = true
				a.ngramCount[key]++
				a.ngramCmds[key] = w
			}
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
		procs := taskProcedureEvidence(a.ngramCount, a.ngramCmds, idf, corpus)
		label := representativeIntent(a.intents)
		factHits := matchFactsToTask(facts, label, a.cmdFreq)
		if !hasNonObviousEvidence(procs, factHits) {
			continue // gate: nothing repo-specific to encode — never reaches an agent
		}
		strength := taskStrength(a.support, a.withCommands, a.reinf, procs, len(factHits))
		out = append(out, taskCandidate{
			ID:              taskCandidateID(a.repoKey, sig),
			RepoKey:         a.repoKey,
			IntentSignature: sig,
			Label:           label,
			Support:         a.support,
			WithCommands:    a.withCommands,
			Reinforcement:   a.reinf,
			Commands:        topCommands(a.cmdFreq, taskTopCommands),
			Procedures:      procs,
			MatchingFacts:   factHits,
			SampleIntents:   distinctSample(a.intents, taskSampleIntents),
			Examples:        a.anchors,
			Strength:        math.Round(strength*1000) / 1000,
			StrengthLabel:   strengthLabel(strength),
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

// taskProcedureEvidence ranks the cluster's command shapes by corpus specificity
// then frequency, capped — the operational evidence attached to a candidate.
func taskProcedureEvidence(count map[string]int, cmds map[string][]string, idf map[string]float64, corpus int) []taskProcedure {
	var out []taskProcedure
	for key, n := range count {
		w := cmds[key]
		out = append(out, taskProcedure{Commands: w, Count: n, Specificity: math.Round(specificityScore(w, idf, corpus)*1000) / 1000})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Specificity != out[j].Specificity {
			return out[i].Specificity > out[j].Specificity
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return strings.Join(out[i].Commands, " ") < strings.Join(out[j].Commands, " ")
	})
	if len(out) > taskMaxProcedures {
		out = out[:taskMaxProcedures]
	}
	return out
}

// hasNonObviousEvidence is the deterministic gate: a candidate must carry at
// least one RECURRING specific command shape (specific AND seen in ≥2 of the
// cluster's episodes) or a matching repo fact, or it is dropped before any agent
// call. The recurrence requirement rejects incoherent clusters whose episodes run
// unrelated one-off commands; the specificity requirement rejects generic loops
// (e.g. ubiquitous git commit/push) with no repo-specific evidence.
func hasNonObviousEvidence(procs []taskProcedure, factHits []string) bool {
	for _, p := range procs {
		if p.Specificity >= procedureSpecFloor && p.Count >= 2 {
			return true
		}
	}
	return len(factHits) > 0
}

// matchFactsToTask returns active durable facts whose text overlaps the task's
// label/command terms, ranked by overlap, capped.
func matchFactsToTask(facts []factRecord, label string, cmdFreq map[string]int) []string {
	if len(facts) == 0 {
		return nil
	}
	terms := map[string]bool{}
	for _, t := range brainBriefFileMatchTerms(label) {
		terms[t] = true
	}
	for c := range cmdFreq {
		for _, w := range strings.Fields(strings.ToLower(c)) {
			terms[w] = true
		}
	}
	if len(terms) == 0 {
		return nil
	}
	type scored struct {
		text    string
		overlap int
	}
	var matched []scored
	for _, f := range facts {
		if f.Status != "active" {
			continue
		}
		seen := map[string]bool{}
		overlap := 0
		for _, w := range brainBriefTaskWordPattern.FindAllString(strings.ToLower(f.Text), -1) {
			if terms[w] && !seen[w] {
				seen[w] = true
				overlap++
			}
		}
		if overlap > 0 {
			matched = append(matched, scored{truncateString(f.Text, 200), overlap})
		}
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].overlap > matched[j].overlap })
	var out []string
	for _, m := range matched {
		if len(out) >= taskMaxFacts {
			break
		}
		out = append(out, m.text)
	}
	return out
}

// loadAllActiveFacts flattens active facts across every branch, deduped by id.
func loadAllActiveFacts(brainDir string) []factRecord {
	byBranch, err := loadAllFactBranches(brainDir)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []factRecord
	for _, recs := range byBranch {
		for _, f := range recs {
			if f.Status == "active" && !seen[f.ID] {
				seen[f.ID] = true
				out = append(out, f)
			}
		}
	}
	return out
}

func taskStrength(support, withCommands int, r reinforcementCounts, procs []taskProcedure, factCount int) float64 {
	recurrence := clamp01(math.Log2(1+float64(support)) / math.Log2(1+15))
	activity := 0.0
	if support > 0 {
		activity = clamp01(float64(withCommands) / float64(support))
	}
	reinf := 0.5
	if support > 0 {
		reinf = clamp01(0.5 + 0.5*float64(r.Success-r.Corrected)/float64(support))
	}
	// Evidence dominates: best procedure specificity, lifted to 0.6 when repo
	// facts corroborate even if the commands themselves are common.
	evidence := 0.0
	for _, p := range procs {
		if p.Specificity > evidence {
			evidence = p.Specificity
		}
	}
	if factCount > 0 && evidence < 0.6 {
		evidence = 0.6
	}
	return 0.30*recurrence + 0.10*activity + 0.20*reinf + 0.40*clamp01(evidence)
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
