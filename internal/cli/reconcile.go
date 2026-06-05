package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const factsProposalsFileName = "proposals.ndjson"

func factsProposalsRelPath(branch string) string {
	return filepath.ToSlash(filepath.Join(factsBranchRelDir(branch), factsProposalsFileName))
}

// loadFactProposals reads a branch's pending proposals; a missing file yields
// none. A malformed line is a hard error so a corrupt queue is surfaced.
func loadFactProposals(brainDir, branch string) ([]factProposal, error) {
	path := filepath.Join(brainDir, filepath.FromSlash(factsProposalsRelPath(branch)))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var proposals []factProposal
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var p factProposal
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("parse %s line %d: %w", factsProposalsRelPath(branch), i+1, err)
		}
		proposals = append(proposals, p)
	}
	return proposals, nil
}

// writeFactProposals persists a branch's pending proposals, deduplicated by
// (action, candidate, target) and sorted for a stable file. An empty set
// removes the file so a fully-resolved branch leaves no stale queue.
func writeFactProposals(brainDir, branch string, proposals []factProposal) error {
	deduped := dedupeProposals(proposals)
	path := filepath.Join(brainDir, filepath.FromSlash(factsProposalsRelPath(branch)))
	if len(deduped) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	var buf strings.Builder
	for _, p := range deduped {
		data, err := json.Marshal(p)
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	dir := filepath.Join(brainDir, filepath.FromSlash(factsBranchRelDir(branch)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(buf.String()), 0o600)
}

func dedupeProposals(proposals []factProposal) []factProposal {
	seen := make(map[string]struct{}, len(proposals))
	out := make([]factProposal, 0, len(proposals))
	for _, p := range proposals {
		key := p.Action + "\x00" + p.CandidateID + "\x00" + p.TargetID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TargetID != out[j].TargetID {
			return out[i].TargetID < out[j].TargetID
		}
		return out[i].CandidateID < out[j].CandidateID
	})
	return out
}

const (
	// maxReconcileExisting bounds how many existing active facts at a
	// candidate's paths are shown to the agent, keeping the reconcile prompt
	// bounded even on busy paths. When more exist, the most recently updated
	// are shown and the cap is reported as a warning (no silent truncation).
	maxReconcileExisting = 50
)

// reconcilePrompt is the static system prompt for the reconcile pass. The
// dynamic candidate/existing lists are supplied on stdin (see reconcileInput).
// Like the distill prompt it must not begin with a dash — the agent runners
// pass it as a positional CLI argument.
func reconcilePrompt() string {
	return `You are deduplicating durable repository facts. On stdin you receive two
numbered lists: CANDIDATES (new facts just extracted from a session) and
EXISTING (facts already stored at the same taxonomy paths). For each candidate,
decide how it relates to the existing facts.

For every candidate output exactly one line, in candidate-number order:

    <candidate#> <decision> <existing#-or-dash> <confidence>

- <candidate#>: the candidate's number.
- <decision>: one of
    new        — not represented by any existing fact.
    merge       — the SAME meaning as an existing fact (a restatement); consolidate.
    supersede   — CONTRADICTS or replaces an existing fact (the candidate is newer/correct).
- <existing#-or-dash>: for merge/supersede, the EXISTING fact number it refers
  to; for new, a dash "-".
- <confidence>: 0.00 to 1.00, your confidence in a merge/supersede decision
  (use 1.0 for new).

Rules:
- A restatement ("use tabs") and a reversal ("use spaces") look similar but are
  NOT the same: the first is merge, the second is supersede. Read the meaning.
- When unsure between merge/supersede and new, prefer new with lower confidence
  rather than wrongly collapsing distinct facts.
- Output only these lines. No prose, no headers, no blank lines.

Example:
1 new - 1.0
2 merge 3 0.88
3 supersede 1 0.93`
}

// reconcileInput renders the candidate and existing fact lists for stdin. Both
// are numbered 1-based; the agent refers to them by number, and the parser maps
// those numbers back to records (existing numbers resolve to fact ids).
func reconcileInput(candidates, existing []factRecord) []byte {
	var b strings.Builder
	b.WriteString("CANDIDATES\n")
	for i, c := range candidates {
		fmt.Fprintf(&b, "%d [%s] %s\n", i+1, strings.Join(c.Paths, ","), c.Text)
	}
	b.WriteString("\nEXISTING\n")
	for i, e := range existing {
		fmt.Fprintf(&b, "%d [%s] %s\n", i+1, strings.Join(e.Paths, ","), e.Text)
	}
	return []byte(b.String())
}

// parseReconcileActions turns the agent's decision lines into factActions. Each
// candidate gets exactly one action: a line that is missing, malformed, or
// references an out-of-range existing fact degrades that candidate to `new`
// (with a warning) so a parser hiccup never drops a fact or misapplies a
// merge/supersede.
func parseReconcileActions(output string, candidates, existing []factRecord) ([]factAction, []string) {
	decided := make(map[int]factAction, len(candidates))
	var warnings []string

	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			warnings = append(warnings, "reconcile: malformed line: "+truncateString(line, 80))
			continue
		}
		ci, err := strconv.Atoi(fields[0])
		if err != nil || ci < 1 || ci > len(candidates) {
			warnings = append(warnings, "reconcile: bad candidate number: "+truncateString(line, 80))
			continue
		}
		candidate := candidates[ci-1]
		kind := strings.ToLower(fields[1])
		confidence := parseConfidence(fields[3])
		switch kind {
		case factActionNew:
			decided[ci] = factAction{Kind: factActionNew, Confidence: confidence, Candidate: candidate}
		case factActionMerge, factActionSupersede:
			ei, err := strconv.Atoi(fields[2])
			if err != nil || ei < 1 || ei > len(existing) {
				warnings = append(warnings, fmt.Sprintf("reconcile: %s with bad existing number, treating candidate %d as new", kind, ci))
				decided[ci] = factAction{Kind: factActionNew, Confidence: 1.0, Candidate: candidate}
				continue
			}
			decided[ci] = factAction{Kind: kind, TargetID: existing[ei-1].ID, Confidence: confidence, Candidate: candidate}
		default:
			warnings = append(warnings, "reconcile: unknown decision: "+truncateString(line, 80))
		}
	}

	actions := make([]factAction, 0, len(candidates))
	for i, candidate := range candidates {
		if action, ok := decided[i+1]; ok {
			actions = append(actions, action)
			continue
		}
		warnings = append(warnings, fmt.Sprintf("reconcile: no decision for candidate %d, treating as new", i+1))
		actions = append(actions, factAction{Kind: factActionNew, Confidence: 1.0, Candidate: candidate})
	}
	return actions, warnings
}

// parseConfidence reads a confidence token, clamped to [0,1]; unparseable
// values default to 0 so an unreadable confidence is treated as low (and thus
// queued for review rather than auto-applied).
func parseConfidence(token string) float64 {
	value, err := strconv.ParseFloat(token, 64)
	if err != nil {
		return 0
	}
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

// activeFactsAtPaths returns the active facts whose paths intersect the given
// path set, most-recently-updated first, capped at maxReconcileExisting. The
// boolean reports whether the cap dropped any facts.
func activeFactsAtPaths(records []factRecord, paths []string) ([]factRecord, bool) {
	want := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		want[p] = struct{}{}
	}
	var matched []factRecord
	for _, record := range records {
		if record.Status != factStatusActive {
			continue
		}
		for _, p := range record.Paths {
			if _, ok := want[p]; ok {
				matched = append(matched, record)
				break
			}
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].UpdatedAt.Equal(matched[j].UpdatedAt) {
			return matched[i].UpdatedAt.After(matched[j].UpdatedAt)
		}
		return matched[i].ID < matched[j].ID
	})
	if len(matched) > maxReconcileExisting {
		return matched[:maxReconcileExisting], true
	}
	return matched, false
}

// candidatePathSet returns the sorted union of paths across the candidates.
func candidatePathSet(candidates []factRecord) []string {
	seen := map[string]struct{}{}
	for _, c := range candidates {
		for _, p := range c.Paths {
			seen[p] = struct{}{}
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// reconcileChunkCandidates resolves a chunk's candidates against the current
// active set: when no existing facts share the candidates' paths, every
// candidate is trivially `new` (no agent call); otherwise the agent decides.
// An agent error falls back to treating all candidates as `new` so facts are
// never lost. It returns the actions to apply plus any warnings.
func reconcileChunkCandidates(ctx context.Context, run distillAgentRunner, agentArgs []string, candidates, active []factRecord, repoDir string, timeout time.Duration) ([]factAction, []string) {
	existing, capped := activeFactsAtPaths(active, candidatePathSet(candidates))
	if len(existing) == 0 {
		actions := make([]factAction, len(candidates))
		for i, c := range candidates {
			actions[i] = factAction{Kind: factActionNew, Confidence: 1.0, Candidate: c}
		}
		return actions, nil
	}
	var warnings []string
	if capped {
		warnings = append(warnings, fmt.Sprintf("reconcile: more than %d existing facts at these paths; comparing against the %d most recent", maxReconcileExisting, maxReconcileExisting))
	}
	out, err := run(ctx, repoDir, agentArgs, reconcileInput(candidates, existing), timeout)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("reconcile agent failed, treating %d candidates as new: %v", len(candidates), err))
		actions := make([]factAction, len(candidates))
		for i, c := range candidates {
			actions[i] = factAction{Kind: factActionNew, Confidence: 1.0, Candidate: c}
		}
		return actions, warnings
	}
	actions, parseWarnings := parseReconcileActions(out, candidates, existing)
	return actions, append(warnings, parseWarnings...)
}
