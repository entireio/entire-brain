package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
)

const (
	distillCacheFileName    = "distill-cache.json"
	distillCachePath        = factsDirName + "/" + distillCacheFileName
	distillCacheVersion     = 1
	defaultDistillChunkSize = 48 * 1024
	defaultDistillTimeout   = 10 * time.Minute
	distillDefaultBranch    = "main"
	// maxDistillWarnings bounds the warning list so a systemic failure (e.g.
	// every agent call failing) cannot balloon the manifest with one warning
	// per chunk.
	maxDistillWarnings = 50

	// distillAgentAbortThreshold aborts a run once this many agent calls have all
	// failed with nothing distilled yet. A misconfiguration (an invalid --model, a
	// missing agent, an auth error) fails every call identically; surfacing it
	// after a few attempts beats churning silently through every session reporting
	// "0 facts found".
	distillAgentAbortThreshold = 5
)

// capWarnings truncates a warning list to max entries, replacing the overflow
// with a single summary line so a systemic failure stays legible.
func capWarnings(warnings []string, max int) []string {
	if len(warnings) <= max {
		return warnings
	}
	extra := len(warnings) - max
	out := append(warnings[:max:max], fmt.Sprintf("... and %d more warnings (suppressed)", extra))
	return out
}

// distillAgentRunner executes the seed agent with the distillation prompt for
// one transcript chunk and returns the agent's raw stdout. It is injected so
// the orchestration can be tested with a fake agent (the real implementation,
// execDistillAgent, shells out).
type distillAgentRunner func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error)

type distillCommandOptions struct {
	branch              string
	force               bool
	json                bool
	agent               string
	agentCommand        []string
	timeout             time.Duration
	maxChunkBytes       int
	confidenceThreshold float64
	model               string
	effort              string
	dryRun              bool
	jobs                int
	run                 distillAgentRunner
	// progress, when set, is called as each session is processed so the command
	// can render a spinner/progress line. It is nil in tests and for callers
	// that do not want progress output.
	progress func(distillProgress)
}

// distillProgress reports how far the distillation loop has advanced. Distill
// runs one extraction call per uncached transcript chunk, plus an optional
// reconcile call, so this drives a refresh-style progress line.
type distillProgress struct {
	SessionsDone  int
	SessionsTotal int
	Branch        string
	Facts         int // candidate facts the agent has produced so far
}

// transcriptChunk is a line-numbered slice of one session transcript handed to
// the agent. StartLine is the 1-based line offset of the chunk's first line in
// the original transcript, used as the chunk's provenance anchor in Phase A.
type transcriptChunk struct {
	StartLine int
	EndLine   int
	Text      string
}

// distillCache memoizes the fingerprint of each session that has been
// distilled, so an incremental run skips sessions whose transcript and
// checkpoint are unchanged.
type distillCache struct {
	Version  int               `json:"version"`
	Sessions map[string]string `json:"sessions"`
}

type distillPlan struct {
	Manifest           *exportManifest
	PrevCache          distillCache
	Sessions           []distillSessionPlan
	Work               []distillChunkWork
	BranchOrder        []string
	TotalSessions      int
	CachedSessions     int
	SessionsToDistill  int
	MissingTranscripts int
	RawBytes           int64
	PreprocessedBytes  int64
	Chunks             int
	Warnings           []string
	CacheSalt          string
}

type distillSessionPlan struct {
	Session           exportSession
	Branch            string
	Fingerprint       string
	Cached            bool
	ReadFailed        bool
	RawBytes          int
	PreprocessedBytes int
	ChunkIndexes      []int
}

type distillChunkWork struct {
	SessionIndex int
	ChunkIndex   int
	Branch       string
	Session      exportSession
	Chunk        transcriptChunk
	Anchor       factAnchor
}

type distillChunkResult struct {
	Output    string
	Err       error
	Completed bool
}

type distillRunPreparation struct {
	Plan                distillPlan
	Taxonomy            factTaxonomy
	TaxonomyFingerprint string
	Args                []string
	ReconcileArgs       []string
	Threshold           float64
}

type distillDryRunReport struct {
	GeneratedAt                   time.Time              `json:"generated_at"`
	BrainPath                     string                 `json:"brain_path"`
	Branch                        string                 `json:"branch,omitempty"`
	Force                         bool                   `json:"force"`
	MaxChunkBytes                 int                    `json:"max_chunk_bytes"`
	Sessions                      int                    `json:"sessions"`
	CachedSessions                int                    `json:"cached_sessions"`
	SessionsToDistill             int                    `json:"sessions_to_distill"`
	MissingTranscripts            int                    `json:"missing_transcripts"`
	RawBytes                      int64                  `json:"raw_bytes"`
	PreprocessedBytes             int64                  `json:"preprocessed_bytes"`
	Chunks                        int                    `json:"chunks"`
	ExtractionAgentCalls          int                    `json:"extraction_agent_calls"`
	ReconcileAgentCallsUpperBound int                    `json:"reconcile_agent_calls_upper_bound"`
	EstimatedAgentCallsUpperBound int                    `json:"estimated_agent_calls_upper_bound"`
	Branches                      []distillDryRunBranch  `json:"branches"`
	LargestSessions               []distillDryRunSession `json:"largest_sessions"`
	Warnings                      []string               `json:"warnings,omitempty"`
}

type distillDryRunBranch struct {
	Branch            string `json:"branch"`
	Sessions          int    `json:"sessions"`
	CachedSessions    int    `json:"cached_sessions"`
	SessionsToDistill int    `json:"sessions_to_distill"`
	Chunks            int    `json:"chunks"`
	PreprocessedBytes int64  `json:"preprocessed_bytes"`
}

type distillDryRunSession struct {
	SessionID         string `json:"session_id"`
	Branch            string `json:"branch"`
	Transcript        string `json:"transcript"`
	Cached            bool   `json:"cached"`
	Chunks            int    `json:"chunks"`
	RawBytes          int    `json:"raw_bytes"`
	PreprocessedBytes int    `json:"preprocessed_bytes"`
}

func newDistillCommand(opts Options) *cobra.Command {
	distillOpts := distillCommandOptions{agent: "auto", timeout: defaultDistillTimeout, maxChunkBytes: defaultDistillChunkSize, confidenceThreshold: defaultFactConfidenceThreshold}
	cmd := &cobra.Command{
		Use:   "distill [path]",
		Short: "Distill captured sessions into durable facts (agent-required)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runDistill(cmd.Context(), cmd, opts, distillOpts, target)
		},
	}
	cmd.Flags().StringVar(&distillOpts.branch, "branch", "", "Limit distillation to a single branch (default: all exported branches)")
	cmd.Flags().BoolVar(&distillOpts.force, "force", false, "Recompute all distilled facts from scratch instead of skipping unchanged sessions")
	cmd.Flags().BoolVar(&distillOpts.json, "json", false, "Emit the fact source summary as JSON")
	cmd.Flags().StringVar(&distillOpts.agent, "agent", "auto", "Distillation agent: auto, codex, claude-code, ollama, or command")
	cmd.Flags().StringArrayVar(&distillOpts.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().Float64Var(&distillOpts.confidenceThreshold, "confidence", defaultFactConfidenceThreshold, "Minimum agent confidence to auto-apply a merge/supersede; below this it is queued for review")
	cmd.Flags().StringVar(&distillOpts.model, "model", "", "Override the agent model for codex/claude-code, or select the local Ollama model")
	cmd.Flags().StringVar(&distillOpts.effort, "effort", "", "Override the reasoning effort for codex/claude-code (e.g. low)")
	cmd.Flags().BoolVar(&distillOpts.dryRun, "dry-run", false, "Estimate distill work without calling an agent or writing facts")
	cmd.Flags().IntVar(&distillOpts.jobs, "jobs", 1, "Parallel extraction jobs; reconciliation and writes remain deterministic")
	return cmd
}

func runDistill(ctx context.Context, cmd *cobra.Command, opts Options, distillOpts distillCommandOptions, target string) error {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("distill requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	if distillOpts.agent == "auto" {
		distillOpts.agent = defaultRefreshAgent(ctx, opts.Runner, repoDir)
	}
	if distillOpts.dryRun {
		report, err := buildDistillDryRunReport(storage.BrainDir, distillOpts, opts.Now().UTC())
		if err != nil {
			return err
		}
		if distillOpts.json {
			return writeJSON(cmd, report)
		}
		printDistillDryRunReport(cmd, report)
		return nil
	}
	if distillOpts.agent == "none" {
		return errors.New("distillation requires an agent (codex or claude-code); none found on PATH")
	}
	if distillOpts.jobs <= 0 {
		return fmt.Errorf("--jobs must be greater than 0")
	}
	if distillOpts.run == nil {
		distillOpts.run = defaultDistillAgentRunner(distillOpts.agent)
	}
	// Progress goes to stderr so it never corrupts the --json summary on stdout.
	progress := newProgress(cmd.ErrOrStderr(), "distill")
	task := progress.Begin("distill sessions")
	distillOpts.progress = func(p distillProgress) {
		task.Update(distillProgressLabel(p))
	}
	source, err := runDistillForBrain(ctx, repoDir, storage.BrainDir, distillOpts, opts.Now().UTC())
	task.Finish(err)
	if err != nil {
		return err
	}
	if distillOpts.json {
		data, err := json.MarshalIndent(source, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "distilled %d facts (%d distilled, %d authored, %d superseded) across %d branch(es) from %d chunks; %d proposals queued for review\n",
		source.Facts, source.Distilled, source.Authored, source.Superseded, len(source.Branches), source.ChunksScanned, source.Proposals)
	return nil
}

// distillProgressLabel renders a distillProgress as a refresh-style line, e.g.
// "distill sessions 12/55 done (main), 87 facts found". The count is deliberately
// NOT preceded by ": " so the label does not match progressCountPattern: distill
// reports once per session (coarse granularity), and matching that pattern would
// let the non-TTY throttle collapse same-fact-count sessions into a single status
// and suppress per-session lines. (Guarded by TestDistillProgressLabelNotThrottled.)
func distillProgressLabel(p distillProgress) string {
	label := "distill sessions"
	if p.SessionsTotal <= 0 {
		return label
	}
	label += fmt.Sprintf(" %d/%d done", p.SessionsDone, p.SessionsTotal)
	if p.Branch != "" {
		label += " (" + p.Branch + ")"
	}
	return label + fmt.Sprintf(", %s found", pluralCount(p.Facts, "fact"))
}

// runDistillForBrain is the agent-driven distillation engine, operating on an
// already-exported brain directory. It enumerates sessions chronologically,
// chunks each transcript, runs the agent per chunk, merges the resulting facts
// into per-branch stores, and records the fact source on the brain manifest.
// repoDir is the working directory the agent runs in (sandboxed read-only).
func runDistillForBrain(ctx context.Context, repoDir, brainDir string, distillOpts distillCommandOptions, now time.Time) (*factSourceManifest, error) {
	prep, err := prepareDistillRun(brainDir, distillOpts, now)
	if err != nil {
		return nil, err
	}
	extractionStarted := time.Now()
	results := runDistillExtraction(ctx, repoDir, prep.Args, prep.Plan.Work, distillOpts)
	extractionSeconds := time.Since(extractionStarted).Seconds()
	return commitDistillResults(ctx, repoDir, brainDir, distillOpts, now, prep, results, extractionSeconds)
}

func prepareDistillRun(brainDir string, distillOpts distillCommandOptions, now time.Time) (distillRunPreparation, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return distillRunPreparation{}, err
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil || len(manifest.Sources.Sessions.Sessions) == 0 {
		return distillRunPreparation{}, errors.New("no exported sessions; run `entire brain refresh` first")
	}
	if distillOpts.run == nil {
		return distillRunPreparation{}, errors.New("distill: no agent runner configured")
	}
	if distillOpts.maxChunkBytes <= 0 {
		distillOpts.maxChunkBytes = defaultDistillChunkSize
	}

	taxonomy, err := loadFactTaxonomy(brainDir, now)
	if err != nil {
		return distillRunPreparation{}, err
	}
	prompt, err := renderDistillPrompt(taxonomy)
	if err != nil {
		return distillRunPreparation{}, err
	}
	args, err := distillAgentCommandArgs(distillOpts.agent, distillOpts.agentCommand, prompt)
	if err != nil {
		return distillRunPreparation{}, err
	}
	reconcilePromptText := reconcilePrompt()
	reconcileArgs, err := distillAgentCommandArgs(distillOpts.agent, distillOpts.agentCommand, reconcilePromptText)
	if err != nil {
		return distillRunPreparation{}, err
	}
	// Pin the agent model + effort when set, so distill AND reconcile run on the same
	// (cheap) model. Both are no-ops when empty.
	args = injectAgentEffort(injectAgentModel(args, distillOpts.agent, distillOpts.model), distillOpts.agent, distillOpts.effort)
	reconcileArgs = injectAgentEffort(injectAgentModel(reconcileArgs, distillOpts.agent, distillOpts.model), distillOpts.agent, distillOpts.effort)
	threshold := distillOpts.confidenceThreshold
	if threshold <= 0 {
		threshold = defaultFactConfidenceThreshold
	}

	cacheSalt := distillCacheSalt(prompt, reconcilePromptText, threshold, distillOpts)
	plan, err := buildDistillPlan(brainDir, manifest, distillOpts, cacheSalt)
	if err != nil {
		return distillRunPreparation{}, err
	}
	return distillRunPreparation{
		Plan:                plan,
		Taxonomy:            taxonomy,
		TaxonomyFingerprint: factTaxonomyFingerprint(taxonomy),
		Args:                args,
		ReconcileArgs:       reconcileArgs,
		Threshold:           threshold,
	}, nil
}

func commitDistillResults(ctx context.Context, repoDir, brainDir string, distillOpts distillCommandOptions, now time.Time, prep distillRunPreparation, results []distillChunkResult, extractionSeconds float64) (*factSourceManifest, error) {
	var source *factSourceManifest
	err := withBrainWriteLock(brainDir, func() error {
		committed, commitErr := commitDistillResultsLocked(ctx, repoDir, brainDir, distillOpts, now, prep, results, extractionSeconds)
		source = committed
		return commitErr
	})
	return source, err
}

func commitDistillResultsLocked(ctx context.Context, repoDir, brainDir string, distillOpts distillCommandOptions, now time.Time, prep distillRunPreparation, results []distillChunkResult, extractionSeconds float64) (*factSourceManifest, error) {
	plan := prep.Plan
	if err := ensureDistillPlanSourceFresh(brainDir, plan); err != nil {
		return nil, err
	}
	currentTaxonomy, err := loadFactTaxonomy(brainDir, now)
	if err != nil {
		return nil, err
	}
	if got := factTaxonomyFingerprint(currentTaxonomy); got != prep.TaxonomyFingerprint {
		return nil, errors.New("distill taxonomy changed before commit; rerun distill")
	}
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return nil, err
	}
	currentCache := loadDistillCache(brainDir)
	newCache := distillCache{Version: distillCacheVersion, Sessions: make(map[string]string, len(currentCache.Sessions))}
	for sessionID, fingerprint := range currentCache.Sessions {
		newCache.Sessions[sessionID] = fingerprint
	}
	for _, sessionPlan := range plan.Sessions {
		delete(newCache.Sessions, distillSessionCacheKey(sessionPlan.Branch, sessionPlan.Session.SessionID))
		delete(newCache.Sessions, sessionPlan.Session.SessionID) // legacy pre-branch-scoped key
	}

	byBranch := map[string][]factRecord{}
	proposalsByBranch := map[string][]factProposal{}
	loaded := map[string]bool{}
	warnings := append([]string(nil), plan.Warnings...)
	chunksScanned, chunksDistilled := len(plan.Work), 0
	failedChunks := 0
	sessionsDone, factsFound := 0, 0
	// Track agent-call outcomes so a misconfiguration that fails every call (e.g.
	// an invalid --model) aborts fast with the agent's own error, instead of
	// silently churning through every session reporting "0 facts found".
	agentFailures, anyAgentSuccess := 0, false
	var lastAgentErr error

	// ensureBranch lazily loads a branch's existing facts. On --force the
	// previously distilled facts are dropped so they are rebuilt from scratch;
	// authored facts are always preserved.
	ensureBranch := func(branch string) {
		if loaded[branch] {
			return
		}
		existing, loadErr := loadFacts(brainDir, branch)
		if loadErr != nil {
			warnings = append(warnings, fmt.Sprintf("load facts for %s: %v", branch, loadErr))
		}
		if distillOpts.force {
			kept := existing[:0]
			for _, record := range existing {
				if record.Origin != factOriginDistilled {
					kept = append(kept, record)
				}
			}
			existing = kept
		}
		byBranch[branch] = existing
		loaded[branch] = true
	}

	for _, branch := range plan.BranchOrder {
		ensureBranch(branch)
	}

	anyExtractionSuccess := false
	for _, result := range results {
		if result.Completed && result.Err == nil {
			anyExtractionSuccess = true
			break
		}
	}

	reconcileStarted := time.Now()
	sessionFailed := map[int]bool{}
	for sessionIndex, sessionPlan := range plan.Sessions {
		branch := sessionPlan.Branch

		sessionsDone++
		// Report once per session, at every exit path, so the running fact count
		// includes the session just processed (an emit at session start would lag
		// by one session and always show 0 for a single-session run).
		reportProgress := func() {
			if distillOpts.progress != nil {
				distillOpts.progress(distillProgress{SessionsDone: sessionsDone, SessionsTotal: plan.TotalSessions, Branch: branch, Facts: factsFound})
			}
		}

		if sessionPlan.ReadFailed {
			reportProgress()
			continue
		}
		if sessionPlan.Cached {
			newCache.Sessions[distillSessionCacheKey(sessionPlan.Branch, sessionPlan.Session.SessionID)] = sessionPlan.Fingerprint
			reportProgress()
			continue
		}

		for _, workIndex := range sessionPlan.ChunkIndexes {
			work := plan.Work[workIndex]
			result := results[workIndex]
			if !result.Completed {
				warnings = append(warnings, fmt.Sprintf("agent did not run on %s:%d", work.Session.SessionID, work.Chunk.StartLine))
				sessionFailed[sessionIndex] = true
				failedChunks++
				continue
			}
			if result.Err != nil {
				warnings = append(warnings, fmt.Sprintf("agent failed on %s:%d: %v", work.Session.SessionID, work.Chunk.StartLine, result.Err))
				sessionFailed[sessionIndex] = true
				failedChunks++
				agentFailures++
				// Fail fast on a misconfiguration: nothing has distilled yet and the
				// agent keeps failing, so every call is almost certainly erroring the
				// same way (bad --model, missing agent, auth). Abort with the agent's
				// own error instead of churning through every remaining session.
				lastAgentErr = result.Err
				if !anyExtractionSuccess && !anyAgentSuccess && agentFailures >= distillAgentAbortThreshold {
					return nil, fmt.Errorf("distill aborted after %d agent failures with no facts distilled — check --agent and --model. Last error: %v", agentFailures, result.Err)
				}
				continue
			}
			anyAgentSuccess = true
			records, chunkWarnings := distilledFactsFromOutput(result.Output, prep.Taxonomy, work.Anchor, branch, now)
			warnings = append(warnings, chunkWarnings...)
			if len(records) == 0 {
				continue
			}
			factsFound += len(records)
			chunksDistilled++
			// Reconcile the chunk's candidates against the branch's active facts
			// so near-duplicates merge and contradictions supersede instead of
			// piling up. Low-confidence decisions are queued for review.
			actions, recWarnings := reconcileChunkCandidates(ctx, distillOpts.run, prep.ReconcileArgs, records, byBranch[branch], repoDir, distillOpts.timeout)
			warnings = append(warnings, recWarnings...)
			var chunkProposals []factProposal
			byBranch[branch], chunkProposals = applyFactActions(byBranch[branch], actions, prep.Threshold, now)
			proposalsByBranch[branch] = append(proposalsByBranch[branch], chunkProposals...)
		}
		// Only cache a session as distilled when every chunk succeeded, so a
		// session whose agent calls failed is retried on the next run rather
		// than being silently treated as done.
		if !sessionFailed[sessionIndex] {
			newCache.Sessions[distillSessionCacheKey(sessionPlan.Branch, sessionPlan.Session.SessionID)] = sessionPlan.Fingerprint
		}
		reportProgress()
	}
	reconcileSeconds := time.Since(reconcileStarted).Seconds()
	if chunksScanned > 0 && agentFailures > 0 && !anyAgentSuccess {
		return nil, fmt.Errorf("distill failed: all %d agent calls failed with no successful extraction — check --agent and --model. Last error: %v", agentFailures, lastAgentErr)
	}

	warnings = capWarnings(warnings, maxDistillWarnings)

	writeStarted := time.Now()
	for branch, records := range byBranch {
		if err := writeFacts(brainDir, branch, records); err != nil {
			return nil, err
		}
		// Union this run's proposals with any already queued (a --force rebuild
		// starts fresh since it rebuilds the distilled facts), then persist.
		var prior []factProposal
		if !distillOpts.force {
			if loadedProposals, loadErr := loadFactProposals(brainDir, branch); loadErr != nil {
				warnings = append(warnings, fmt.Sprintf("load proposals for %s: %v", branch, loadErr))
			} else {
				prior = loadedProposals
			}
		}
		merged := dedupeProposals(append(prior, proposalsByBranch[branch]...))
		if err := writeFactProposals(brainDir, branch, merged); err != nil {
			return nil, err
		}
	}
	if err := writeFactTaxonomy(brainDir, prep.Taxonomy); err != nil {
		return nil, err
	}
	saveDistillCache(brainDir, newCache)

	// Summarize the manifest from the whole on-disk store, not just the branches
	// touched this run. A branch-limited (`--branch`) or incremental run only
	// loads a subset into byBranch; folding that subset into the manifest would
	// drop counts/branches/proposals for facts that still exist on other
	// branches. Re-reading the store after writing keeps sources.facts whole.
	allBranches, err := loadAllFactBranches(brainDir)
	if err != nil {
		return nil, err
	}
	branchNames := make([]string, 0, len(allBranches))
	for branch := range allBranches {
		branchNames = append(branchNames, branch)
	}
	totalProposals := countFactProposals(brainDir, branchNames)

	source := summarizeFactSource(now, allBranches, chunksScanned, chunksDistilled, totalProposals, warnings)
	source.CacheHits = plan.CachedSessions
	source.FailedChunks = failedChunks
	source.PreprocessedBytes = plan.PreprocessedBytes
	source.ExtractionSeconds = extractionSeconds
	source.ReconcileSeconds = reconcileSeconds
	source.WriteSeconds = time.Since(writeStarted).Seconds()
	if manifest.Sources == nil {
		manifest.Sources = &brainSources{}
	}
	manifest.Sources.Facts = source
	if manifest.GeneratedAt.IsZero() {
		manifest.GeneratedAt = now
	}
	if err := writeBrainManifestAndReadme(brainDir, *manifest); err != nil {
		return nil, err
	}
	return source, nil
}

func buildDistillDryRunReport(brainDir string, distillOpts distillCommandOptions, now time.Time) (distillDryRunReport, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return distillDryRunReport{}, err
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil || len(manifest.Sources.Sessions.Sessions) == 0 {
		return distillDryRunReport{}, errors.New("no exported sessions; run `entire brain refresh` first")
	}
	cacheSalt, err := distillCacheSaltForBrain(brainDir, distillOpts, now)
	if err != nil {
		return distillDryRunReport{}, err
	}
	plan, err := buildDistillPlan(brainDir, manifest, distillOpts, cacheSalt)
	if err != nil {
		return distillDryRunReport{}, err
	}
	report := distillDryRunReport{
		GeneratedAt:                   now,
		BrainPath:                     brainDir,
		Branch:                        strings.TrimSpace(distillOpts.branch),
		Force:                         distillOpts.force,
		MaxChunkBytes:                 distillOpts.maxChunkBytes,
		Sessions:                      plan.TotalSessions,
		CachedSessions:                plan.CachedSessions,
		SessionsToDistill:             plan.SessionsToDistill,
		MissingTranscripts:            plan.MissingTranscripts,
		RawBytes:                      plan.RawBytes,
		PreprocessedBytes:             plan.PreprocessedBytes,
		Chunks:                        plan.Chunks,
		ExtractionAgentCalls:          plan.Chunks,
		ReconcileAgentCallsUpperBound: plan.Chunks,
		EstimatedAgentCallsUpperBound: plan.Chunks * 2,
		Warnings:                      capWarnings(plan.Warnings, maxDistillWarnings),
	}
	if report.MaxChunkBytes <= 0 {
		report.MaxChunkBytes = defaultDistillChunkSize
	}
	branchStats := map[string]*distillDryRunBranch{}
	for _, branch := range plan.BranchOrder {
		branchStats[branch] = &distillDryRunBranch{Branch: branch}
	}
	for _, session := range plan.Sessions {
		stat := branchStats[session.Branch]
		stat.Sessions++
		stat.PreprocessedBytes += int64(session.PreprocessedBytes)
		if session.Cached {
			stat.CachedSessions++
		} else if !session.ReadFailed {
			stat.SessionsToDistill++
			stat.Chunks += len(session.ChunkIndexes)
		}
		report.LargestSessions = append(report.LargestSessions, distillDryRunSession{
			SessionID:         session.Session.SessionID,
			Branch:            session.Branch,
			Transcript:        filepath.ToSlash(session.Session.TranscriptPath),
			Cached:            session.Cached,
			Chunks:            len(session.ChunkIndexes),
			RawBytes:          session.RawBytes,
			PreprocessedBytes: session.PreprocessedBytes,
		})
	}
	for _, branch := range plan.BranchOrder {
		report.Branches = append(report.Branches, *branchStats[branch])
	}
	sort.Slice(report.LargestSessions, func(i, j int) bool {
		if report.LargestSessions[i].Chunks != report.LargestSessions[j].Chunks {
			return report.LargestSessions[i].Chunks > report.LargestSessions[j].Chunks
		}
		return report.LargestSessions[i].PreprocessedBytes > report.LargestSessions[j].PreprocessedBytes
	})
	if len(report.LargestSessions) > 10 {
		report.LargestSessions = report.LargestSessions[:10]
	}
	if report.Branches == nil {
		report.Branches = []distillDryRunBranch{}
	}
	if report.LargestSessions == nil {
		report.LargestSessions = []distillDryRunSession{}
	}
	return report, nil
}

func printDistillDryRunReport(cmd *cobra.Command, report distillDryRunReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "distill dry-run: %d sessions, %d cached, %d to distill, %d chunks\n",
		report.Sessions, report.CachedSessions, report.SessionsToDistill, report.Chunks)
	fmt.Fprintf(out, "estimated agent calls: %d extraction + up to %d reconcile = up to %d total\n",
		report.ExtractionAgentCalls, report.ReconcileAgentCallsUpperBound, report.EstimatedAgentCallsUpperBound)
	fmt.Fprintf(out, "bytes: %d raw, %d preprocessed\n", report.RawBytes, report.PreprocessedBytes)
	for _, branch := range report.Branches {
		fmt.Fprintf(out, "branch %s: %d sessions, %d cached, %d chunks\n",
			branch.Branch, branch.Sessions, branch.CachedSessions, branch.Chunks)
	}
	if len(report.Warnings) > 0 {
		for _, warning := range report.Warnings {
			fmt.Fprintf(out, "warning: %s\n", warning)
		}
	}
}

func buildDistillPlan(brainDir string, manifest *exportManifest, distillOpts distillCommandOptions, cacheSalt string) (distillPlan, error) {
	if distillOpts.maxChunkBytes <= 0 {
		distillOpts.maxChunkBytes = defaultDistillChunkSize
	}
	sessions := append([]exportSession(nil), manifest.Sources.Sessions.Sessions...)
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].CreatedAt.Before(sessions[j].CreatedAt) })
	prevCache := loadDistillCache(brainDir)
	branchSeen := map[string]struct{}{}
	plan := distillPlan{Manifest: manifest, PrevCache: prevCache, CacheSalt: cacheSalt}
	for _, session := range sessions {
		branch := resolveDistillBranch(manifest, session)
		if distillOpts.branch != "" && branch != distillOpts.branch {
			continue
		}
		if _, ok := branchSeen[branch]; !ok {
			branchSeen[branch] = struct{}{}
			plan.BranchOrder = append(plan.BranchOrder, branch)
		}
		sessionPlan := distillSessionPlan{Session: session, Branch: branch}
		plan.TotalSessions++

		content, readErr := readBrainRelativeFile(brainDir, session.TranscriptPath)
		if readErr != nil {
			sessionPlan.ReadFailed = true
			plan.MissingTranscripts++
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("read transcript %s: %v", session.TranscriptPath, readErr))
			plan.Sessions = append(plan.Sessions, sessionPlan)
			continue
		}
		distillInput := preprocessTranscriptForDistill(content)
		sessionPlan.RawBytes = len(content)
		sessionPlan.PreprocessedBytes = len(distillInput)
		plan.RawBytes += int64(sessionPlan.RawBytes)
		plan.PreprocessedBytes += int64(sessionPlan.PreprocessedBytes)
		sessionPlan.Fingerprint = distillSessionFingerprint(session, branch, distillInput, plan.CacheSalt)
		if !distillOpts.force {
			cacheKey := distillSessionCacheKey(branch, session.SessionID)
			prev, ok := prevCache.Sessions[cacheKey]
			if !ok {
				prev, ok = prevCache.Sessions[session.SessionID] // legacy pre-branch-scoped cache key
			}
			if ok && prev == sessionPlan.Fingerprint {
				sessionPlan.Cached = true
				plan.CachedSessions++
				plan.Sessions = append(plan.Sessions, sessionPlan)
				continue
			}
		}
		chunks := chunkTranscript(distillInput, distillOpts.maxChunkBytes)
		plan.SessionsToDistill++
		for chunkIndex, chunk := range chunks {
			workIndex := len(plan.Work)
			sessionPlan.ChunkIndexes = append(sessionPlan.ChunkIndexes, workIndex)
			plan.Work = append(plan.Work, distillChunkWork{
				SessionIndex: len(plan.Sessions),
				ChunkIndex:   chunkIndex,
				Branch:       branch,
				Session:      session,
				Chunk:        chunk,
				Anchor: factAnchor{
					SessionID:    session.SessionID,
					CheckpointID: session.LatestCheckpoint,
					Transcript:   filepath.ToSlash(session.TranscriptPath),
					Line:         chunk.StartLine,
				},
			})
		}
		plan.Chunks += len(chunks)
		plan.Sessions = append(plan.Sessions, sessionPlan)
	}
	sort.Strings(plan.BranchOrder)
	return plan, nil
}

func ensureDistillPlanSourceFresh(brainDir string, plan distillPlan) error {
	for _, sessionPlan := range plan.Sessions {
		if sessionPlan.ReadFailed {
			continue
		}
		content, err := readBrainRelativeFile(brainDir, sessionPlan.Session.TranscriptPath)
		if err != nil {
			return fmt.Errorf("distill source changed before commit: read transcript %s: %w", sessionPlan.Session.TranscriptPath, err)
		}
		fingerprint := distillSessionFingerprint(sessionPlan.Session, sessionPlan.Branch, preprocessTranscriptForDistill(content), plan.CacheSalt)
		if fingerprint != sessionPlan.Fingerprint {
			return fmt.Errorf("distill source changed before commit for session %s; rerun distill", sessionPlan.Session.SessionID)
		}
	}
	return nil
}

func resolveDistillBranch(manifest *exportManifest, session exportSession) string {
	branch := session.Branch
	if branch == "" && manifest != nil {
		branch = strings.TrimSpace(manifest.DefaultBranch)
	}
	if branch == "" {
		branch = distillDefaultBranch
	}
	return branch
}

func runDistillExtraction(ctx context.Context, repoDir string, args []string, work []distillChunkWork, distillOpts distillCommandOptions) []distillChunkResult {
	results := make([]distillChunkResult, len(work))
	if len(work) == 0 {
		return results
	}
	jobs := distillOpts.jobs
	if jobs <= 0 {
		jobs = 1
	}
	if jobs > len(work) {
		jobs = len(work)
	}
	if jobs == 1 {
		agentFailures, anyAgentSuccess := 0, false
		for i, item := range work {
			out, err := distillOpts.run(ctx, repoDir, args, []byte(item.Chunk.Text), distillOpts.timeout)
			results[i] = distillChunkResult{Output: out, Err: err, Completed: true}
			if err != nil {
				agentFailures++
				if !anyAgentSuccess && agentFailures >= distillAgentAbortThreshold {
					break
				}
				continue
			}
			anyAgentSuccess = true
		}
		return results
	}
	var wg sync.WaitGroup
	indexes := make(chan int)
	var agentFailures atomic.Int32
	var anyAgentSuccess atomic.Bool
	var stop atomic.Bool
	for range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indexes {
				item := work[i]
				out, err := distillOpts.run(ctx, repoDir, args, []byte(item.Chunk.Text), distillOpts.timeout)
				results[i] = distillChunkResult{Output: out, Err: err, Completed: true}
				if err != nil {
					if !anyAgentSuccess.Load() && agentFailures.Add(1) >= distillAgentAbortThreshold {
						stop.Store(true)
					}
					continue
				}
				anyAgentSuccess.Store(true)
			}
		}()
	}
	// Dispatch a small window past the serial abort threshold before honoring a
	// parallel stop signal. That keeps ordered mixed runs from dropping a nearby
	// success while still preventing a systemic agent failure from traversing a
	// large repository.
	minDispatchBeforeAbort := distillAgentAbortThreshold + jobs
	for i := range work {
		if i >= minDispatchBeforeAbort && stop.Load() {
			break
		}
		indexes <- i
	}
	close(indexes)
	wg.Wait()
	return results
}

// preprocessTranscriptForDistill strips tool calls and tool outputs (plus
// session/meta records) from a JSONL transcript before chunking, leaving only the
// human and assistant conversation text that the quality gate actually mines for
// facts. Each input line maps to exactly one output line: a dropped record
// becomes a blank line, which chunkTranscript skips while still incrementing the
// line counter, so a distilled fact's provenance anchor keeps pointing at the
// correct line in the original transcript. Lines that are not JSON objects (for
// example an already-rendered/compact transcript) are passed through unchanged so
// the preprocessor is safe for any transcript format.
func preprocessTranscriptForDistill(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[i] = ""
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue // not JSONL; leave the line as-is
		}
		// Collapse the kept text onto a single line so one record stays one line
		// and the line numbering (provenance) is preserved.
		lines[i] = strings.Join(strings.Fields(distillConversationText(obj)), " ")
	}
	return strings.Join(lines, "\n")
}

// distillConversationText returns the human/assistant text worth distilling from
// one transcript record, or "" for tool calls, tool outputs, reasoning, and
// session/meta records. It mirrors the record routing in
// extractHistoryJSONFragments (codex `event_msg`/`response_item` and Claude
// `assistant`/`user` shapes) but deliberately keeps user turns — where standing
// rules and preferences live — and discards everything tool-related.
func distillConversationText(obj map[string]any) string {
	switch jsonString(obj["type"]) {
	case "agent_message":
		return jsonString(obj["message"])
	case "event_msg":
		payload := jsonMap(obj["payload"])
		switch jsonString(payload["type"]) {
		case "agent_message", "user_message":
			return jsonString(payload["message"])
		case "task_complete":
			return jsonString(payload["last_agent_message"])
		}
		return "" // task_started, exec_command_end, patch_apply_end, ...
	case "response_item":
		payload := jsonMap(obj["payload"])
		if jsonString(payload["type"]) != "message" {
			return "" // function_call, custom_tool_call, function_call_output, reasoning
		}
		if role := jsonString(payload["role"]); role != "assistant" && role != "user" {
			return ""
		}
		return distillTextBlocks(payload["content"])
	case "assistant", "user":
		return distillTextBlocks(jsonMap(obj["message"])["content"])
	case "session_meta", "turn_context", "permission-mode", "progress":
		return ""
	default:
		return jsonString(obj["message"])
	}
}

// distillTextBlocks pulls plain text out of a message "content" field, keeping
// text blocks and dropping tool_use / tool_result blocks. content may be a plain
// string (a bare user/assistant message) or an array of typed blocks.
func distillTextBlocks(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var parts []string
		for _, item := range value {
			block := jsonMap(item)
			if len(block) == 0 {
				continue
			}
			switch jsonString(block["type"]) {
			case "tool_use", "tool_result":
				continue // tool call / tool output
			default:
				if text := firstNonEmptyString(block["text"], block["input_text"], block["output_text"], block["content"]); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

// chunkTranscript splits a transcript into line-numbered chunks no larger than
// maxBytes (measured on the rendered, line-numbered text). A single line that
// exceeds maxBytes still becomes its own chunk rather than being dropped, so no
// content is silently lost. Blank input yields no chunks.
func chunkTranscript(content string, maxBytes int) []transcriptChunk {
	return chunkLines(content, maxBytes, true)
}

// chunkLines is the shared chunker behind chunkTranscript. numberLines controls
// the per-line rendering: distill wants the "<lineNo>\t" prefix so the agent can
// cite exact source lines, but the doc index wants clean text — the prefix would
// otherwise pollute the indexed text/embeddings and stop firstHeading from seeing
// markdown headings (a chunk would start "12\t## Title", not "## Title").
// Chunk StartLine/EndLine metadata is preserved either way.
func chunkLines(content string, maxBytes int, numberLines bool) []transcriptChunk {
	if maxBytes <= 0 {
		maxBytes = defaultDistillChunkSize
	}
	lines := strings.Split(content, "\n")
	var chunks []transcriptChunk
	var buf strings.Builder
	startLine := 0
	lineNo := 0
	flush := func(endLine int) {
		if buf.Len() == 0 {
			return
		}
		chunks = append(chunks, transcriptChunk{StartLine: startLine, EndLine: endLine, Text: buf.String()})
		buf.Reset()
		startLine = 0
	}
	for _, line := range lines {
		lineNo++
		if strings.TrimSpace(line) == "" {
			// Transcripts drop blank lines (noise) but keep counting for accurate
			// line numbers. Docs preserve *internal* blank lines so markdown
			// paragraph/code-block structure survives into the indexed text and
			// embeddings; leading blanks (empty buffer) are skipped and trailing
			// ones are trimmed by the caller (loadDocRecordsFromSeed). The blank is
			// subject to the same size cap, so a run of blanks can't grow a chunk
			// past maxBytes — it flushes at the boundary instead.
			if !numberLines && buf.Len() > 0 {
				if buf.Len()+1 > maxBytes {
					flush(lineNo - 1)
				} else {
					buf.WriteString("\n")
				}
			}
			continue
		}
		rendered := line + "\n"
		if numberLines {
			rendered = fmt.Sprintf("%d\t%s\n", lineNo, line)
		}
		if buf.Len() > 0 && buf.Len()+len(rendered) > maxBytes {
			flush(lineNo - 1)
		}
		if buf.Len() == 0 {
			startLine = lineNo
		}
		buf.WriteString(rendered)
	}
	flush(lineNo)
	return chunks
}

// distillSessionFingerprint is the incremental-skip signal for one session. It
// hashes the session identity (id, latest checkpoint, transcript path, resolved
// branch) together with a digest of the *preprocessed* distill input (tool I/O
// and meta records already stripped), so any change to what the agent actually
// distills — even one that keeps the same checkpoint, such as a compact↔raw
// re-export or an exporter fix — invalidates the cache and forces a re-distill,
// while churn confined to stripped tool I/O does not. The branch is the
// *resolved* one the facts are written under (session.Branch may be empty and
// fall back to the manifest default), so a default-branch change re-keys the
// session instead of letting a stale cache skip it.
func distillSessionFingerprint(session exportSession, branch, content, cacheSalt string) string {
	contentSum := sha256.Sum256([]byte(content))
	sum := sha256.Sum256([]byte(strings.Join([]string{
		session.SessionID,
		session.LatestCheckpoint,
		filepath.ToSlash(session.TranscriptPath),
		branch,
		cacheSalt,
		hex.EncodeToString(contentSum[:]),
	}, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func distillCacheSaltForBrain(brainDir string, distillOpts distillCommandOptions, now time.Time) (string, error) {
	taxonomy, err := loadFactTaxonomy(brainDir, now)
	if err != nil {
		return "", err
	}
	prompt, err := renderDistillPrompt(taxonomy)
	if err != nil {
		return "", err
	}
	threshold := distillOpts.confidenceThreshold
	if threshold <= 0 {
		threshold = defaultFactConfidenceThreshold
	}
	return distillCacheSalt(prompt, reconcilePrompt(), threshold, distillOpts), nil
}

func distillCacheSalt(prompt, reconcilePromptText string, threshold float64, distillOpts distillCommandOptions) string {
	data, err := json.Marshal(struct {
		Version         int     `json:"version"`
		Prompt          string  `json:"prompt"`
		ReconcilePrompt string  `json:"reconcile_prompt"`
		Threshold       float64 `json:"threshold"`
		Agent           string  `json:"agent"`
		AgentCommand    string  `json:"agent_command,omitempty"`
		Model           string  `json:"model,omitempty"`
		Effort          string  `json:"effort,omitempty"`
	}{
		Version:         distillCacheVersion,
		Prompt:          prompt,
		ReconcilePrompt: reconcilePromptText,
		Threshold:       threshold,
		Agent:           distillOpts.agent,
		AgentCommand:    strings.Join(distillOpts.agentCommand, "\x00"),
		Model:           distillOpts.model,
		Effort:          distillOpts.effort,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func distillSessionCacheKey(branch, sessionID string) string {
	return url.PathEscape(branch) + "/" + url.PathEscape(sessionID)
}

func factTaxonomyFingerprint(taxonomy factTaxonomy) string {
	data, err := json.Marshal(taxonomy)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// readBrainRelativeFile reads a brain-relative path, rejecting absolute paths
// and parent-directory escapes so a manifest-supplied path cannot read outside
// the brain directory.
func readBrainRelativeFile(brainDir, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe transcript path: %s", rel)
	}
	data, err := os.ReadFile(filepath.Join(brainDir, clean))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// loadDistillCache reads the incremental cache, always returning a usable value:
// any read/parse error or version mismatch yields an empty cache so the next
// run simply re-distills everything.
func loadDistillCache(brainDir string) distillCache {
	empty := distillCache{Version: distillCacheVersion, Sessions: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(brainDir, filepath.FromSlash(distillCachePath)))
	if err != nil {
		return empty
	}
	var cache distillCache
	if err := json.Unmarshal(data, &cache); err != nil || cache.Version != distillCacheVersion || cache.Sessions == nil {
		return empty
	}
	return cache
}

// saveDistillCache persists the incremental cache. Failure is non-fatal: a
// missing cache only costs a full re-distillation next run.
func saveDistillCache(brainDir string, cache distillCache) {
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Join(brainDir, factsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = writeFileAtomic(filepath.Join(brainDir, filepath.FromSlash(distillCachePath)), append(data, '\n'), 0o600)
}

// execDistillAgent is the real distillAgentRunner: it runs the agent with the
// transcript chunk on stdin and returns stdout, bounded by a timeout and an
// output-size cap.
func defaultDistillAgentRunner(agent string) distillAgentRunner {
	if agent == "ollama" {
		return execOllamaDistillAgent
	}
	return execDistillAgent
}

func execDistillAgent(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
	if len(args) == 0 {
		return "", errors.New("distill: empty agent command")
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, args[0], args[1:]...)
	command.Dir = dir
	command.Stdin = bytes.NewReader(input)
	stdout := newCappedDistillBuffer(distillMaxOutputBytes)
	stderr := newCappedDistillBuffer(distillMaxOutputBytes)
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("agent timed out after %s", timeout)
	}
	if stdout.Exceeded() {
		return "", fmt.Errorf("agent output exceeds %d bytes", distillMaxOutputBytes)
	}
	if stderr.Exceeded() {
		return "", fmt.Errorf("agent stderr exceeds %d bytes", distillMaxOutputBytes)
	}
	if err != nil {
		warning := strings.TrimSpace(stderr.String())
		if warning == "" {
			warning = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("agent failed: %w: %s", err, truncateAgentWarning(warning))
	}
	return stdout.String(), nil
}

type cappedDistillBuffer struct {
	limit    int
	exceeded bool
	buf      bytes.Buffer
}

func newCappedDistillBuffer(limit int) cappedDistillBuffer {
	return cappedDistillBuffer{limit: limit}
}

func (w *cappedDistillBuffer) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		w.exceeded = true
		return len(p), nil
	}
	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if len(p) <= remaining {
			_, _ = w.buf.Write(p)
			return len(p), nil
		}
		_, _ = w.buf.Write(p[:remaining])
	}
	w.exceeded = true
	return len(p), nil
}

func (w *cappedDistillBuffer) String() string {
	return w.buf.String()
}

func (w *cappedDistillBuffer) Exceeded() bool {
	return w.exceeded
}

func execOllamaDistillAgent(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
	_ = dir
	if len(args) < 3 || args[0] != "ollama" {
		return "", errors.New("distill: invalid ollama runner arguments")
	}
	model := strings.TrimSpace(args[1])
	if model == "" {
		return "", errors.New("distill: --agent ollama requires --model")
	}
	endpoint := strings.TrimSpace(os.Getenv("ENTIRE_BRAIN_OLLAMA_URL"))
	if endpoint == "" {
		endpoint = "http://127.0.0.1:11434/api/generate"
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("distill: parse ollama url: %w", err)
	}
	if !isLoopbackHTTPURL(u) {
		return "", fmt.Errorf("distill: ollama url must be loopback-only: %s", endpoint)
	}
	body, err := json.Marshal(map[string]any{
		"model":  model,
		"system": args[2],
		"prompt": string(input),
		"stream": false,
	})
	if err != nil {
		return "", err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !isLoopbackHTTPURL(req.URL) {
				return fmt.Errorf("ollama redirect must stay loopback-only: %s", req.URL.String())
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("ollama timed out after %s", timeout)
	}
	if err != nil {
		return "", fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, distillMaxOutputBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > distillMaxOutputBytes {
		return "", fmt.Errorf("ollama output exceeds %d bytes", distillMaxOutputBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ollama returned HTTP %d: %s", resp.StatusCode, truncateAgentWarning(string(data)))
	}
	var parsed struct {
		Response string `json:"response"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("parse ollama response: %w", err)
	}
	if parsed.Error != "" {
		return "", fmt.Errorf("ollama error: %s", parsed.Error)
	}
	return parsed.Response, nil
}

func isLoopbackHTTPURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.Trim(strings.ToLower(u.Hostname()), "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
