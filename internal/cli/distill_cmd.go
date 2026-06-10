package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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

	// defaultDistillConcurrency is how many distill agent calls run in flight at
	// once. Distillation is latency-bound (each call is an LLM round-trip plus an
	// agent-CLI cold start); a small pool overlaps those waits without hammering
	// provider rate limits or spawning an unbounded number of agent processes.
	defaultDistillConcurrency = 4

	// distillFlushCallInterval is how many agent calls may elapse between
	// persistence flushes of the in-memory distill state (facts, proposals,
	// session cache). A distill run is hours of agent calls; flushing every ~50
	// calls bounds what an interruption can lose to minutes instead of the run.
	distillFlushCallInterval = 50
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

// agentCallResult carries one prefetched distill-agent call result. holdsSlot
// marks results produced by a launched worker, whose pool slot result() must
// release on consumption (cancellation fills abandoned chunks without a slot).
type agentCallResult struct {
	out       string
	err       error
	holdsSlot bool
}

// chunkPrefetcher runs the distill agent over a session's chunks up to
// `concurrency` calls ahead of the consumer. Results are delivered strictly in
// chunk order via result(i), so the consumer loop — parsing, reconciling,
// applying fact actions — keeps its exact serial semantics (reconcile mutates
// the branch fact store and must see chunks in order); only the agent
// round-trips overlap. A pool slot is held from a call's launch until its
// result is CONSUMED, not merely until the worker completes: lookahead is
// bounded by consumer position, so at most `concurrency` completed results are
// ever buffered, and an early abort stops dispatch within `concurrency` calls
// of the last one consumed (a completion-released slot would let the
// dispatcher launch — and bill — every remaining chunk while the consumer was
// stuck behind one slow result). stop() cancels unlaunched and in-flight
// calls; the per-chunk result channels are buffered so abandoned workers can
// deliver and exit without a reader.
//
// Note the consumer's reconcile agent call runs OUTSIDE the pool, so peak
// concurrent agent processes is concurrency+1. With concurrency <= 1 there is
// no pool at all: result(i) runs the agent call inline and lazily — strictly
// one agent process at a time, and no call is ever made for a chunk the
// consumer never asks about (e.g. after an abort) — exactly the original
// sequential behavior.
type chunkPrefetcher struct {
	results []chan agentCallResult
	sem     chan struct{}
	cancel  context.CancelFunc
	// seq, when set, replaces the pool: result(i) invokes it inline.
	seq func(i int) (string, error)
}

func startChunkPrefetch(ctx context.Context, run distillAgentRunner, dir string, args []string, chunks []transcriptChunk, timeout time.Duration, concurrency int) *chunkPrefetcher {
	pctx, cancel := context.WithCancel(ctx)
	if concurrency <= 1 {
		return &chunkPrefetcher{cancel: cancel, seq: func(i int) (string, error) {
			return run(pctx, dir, args, []byte(chunks[i].Text), timeout)
		}}
	}
	p := &chunkPrefetcher{results: make([]chan agentCallResult, len(chunks)), sem: make(chan struct{}, concurrency), cancel: cancel}
	for i := range p.results {
		p.results[i] = make(chan agentCallResult, 1)
	}
	go func() {
		for i := range chunks {
			if pctx.Err() != nil { // don't race a freed slot against cancellation
				p.results[i] <- agentCallResult{err: pctx.Err()}
				continue
			}
			select {
			case p.sem <- struct{}{}:
			case <-pctx.Done():
				p.results[i] <- agentCallResult{err: pctx.Err()}
				continue
			}
			go func(i int) {
				out, err := run(pctx, dir, args, []byte(chunks[i].Text), timeout)
				p.results[i] <- agentCallResult{out: out, err: err, holdsSlot: true}
			}(i)
		}
	}()
	return p
}

// result blocks until chunk i's agent call completes and returns its output,
// releasing the call's pool slot so the dispatcher may launch the next chunk.
func (p *chunkPrefetcher) result(i int) (string, error) {
	if p.seq != nil {
		return p.seq(i)
	}
	r := <-p.results[i]
	if r.holdsSlot {
		<-p.sem
	}
	return r.out, r.err
}

// stop cancels outstanding work; safe to call multiple times.
func (p *chunkPrefetcher) stop() { p.cancel() }

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
	// concurrency is the distill agent-call pool size; <=1 means strictly
	// sequential, lazy agent calls (the zero value keeps tests and library
	// callers on the old behavior). See chunkPrefetcher for the pool semantics.
	concurrency int
	// flushEvery overrides distillFlushCallInterval when >0 (tests use 1 to
	// observe mid-run persistence).
	flushEvery int
	run        distillAgentRunner
	// progress, when set, is called as each session is processed so the command
	// can render a spinner/progress line. It is nil in tests and for callers
	// that do not want progress output.
	progress func(distillProgress)
}

// distillProgress reports how far the distillation loop has advanced. Distill
// runs one agent call per transcript chunk and can take minutes, so this drives
// a refresh-style progress line.
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

func newDistillCommand(opts Options) *cobra.Command {
	distillOpts := distillCommandOptions{agent: "auto", timeout: defaultDistillTimeout, maxChunkBytes: defaultDistillChunkSize, confidenceThreshold: defaultFactConfidenceThreshold, concurrency: defaultDistillConcurrency}
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
	cmd.Flags().StringVar(&distillOpts.agent, "agent", "auto", "Distillation agent: auto, codex, claude-code, or command")
	cmd.Flags().StringArrayVar(&distillOpts.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().Float64Var(&distillOpts.confidenceThreshold, "confidence", defaultFactConfidenceThreshold, "Minimum agent confidence to auto-apply a merge/supersede; below this it is queued for review")
	cmd.Flags().StringVar(&distillOpts.model, "model", "", "Override the agent model for codex/claude-code (e.g. a fast/cheap model like gpt-5.4-mini)")
	cmd.Flags().StringVar(&distillOpts.effort, "effort", "", "Override the reasoning effort for codex/claude-code (e.g. low) — pairs with --model for a cheap run")
	cmd.Flags().IntVar(&distillOpts.concurrency, "concurrency", defaultDistillConcurrency, "Distill agent calls to run in flight at once (1 = strictly sequential; higher values may add one concurrent reconcile call)")
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
	if distillOpts.agent == "none" {
		return errors.New("distillation requires an agent (codex or claude-code); none found on PATH")
	}
	if distillOpts.run == nil {
		distillOpts.run = execDistillAgent
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
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return nil, err
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil || len(manifest.Sources.Sessions.Sessions) == 0 {
		return nil, errors.New("no exported sessions; run `entire brain refresh` first")
	}
	if distillOpts.run == nil {
		return nil, errors.New("distill: no agent runner configured")
	}
	if distillOpts.maxChunkBytes <= 0 {
		distillOpts.maxChunkBytes = defaultDistillChunkSize
	}

	taxonomy, err := loadFactTaxonomy(brainDir, now)
	if err != nil {
		return nil, err
	}
	prompt, err := renderDistillPrompt(taxonomy)
	if err != nil {
		return nil, err
	}
	args, err := distillAgentCommandArgs(distillOpts.agent, distillOpts.agentCommand, prompt)
	if err != nil {
		return nil, err
	}
	reconcileArgs, err := distillAgentCommandArgs(distillOpts.agent, distillOpts.agentCommand, reconcilePrompt())
	if err != nil {
		return nil, err
	}
	// Pin the agent model + effort when set, so distill AND reconcile run on the same
	// (cheap) model. Both are no-ops when empty.
	args = injectAgentEffort(injectAgentModel(args, distillOpts.agent, distillOpts.model), distillOpts.agent, distillOpts.effort)
	reconcileArgs = injectAgentEffort(injectAgentModel(reconcileArgs, distillOpts.agent, distillOpts.model), distillOpts.agent, distillOpts.effort)
	threshold := distillOpts.confidenceThreshold
	if threshold <= 0 {
		threshold = defaultFactConfidenceThreshold
	}

	sessions := append([]exportSession(nil), manifest.Sources.Sessions.Sessions...)
	// Chronological order so any future supersession chain reconstructs
	// deterministically regardless of incremental vs --force.
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].CreatedAt.Before(sessions[j].CreatedAt) })

	prevCache := loadDistillCache(brainDir)
	newCache := distillCache{Version: distillCacheVersion, Sessions: make(map[string]string, len(sessions))}

	byBranch := map[string][]factRecord{}
	proposalsByBranch := map[string][]factProposal{}
	loaded := map[string]bool{}
	// dirtyBranches tracks branches whose in-memory facts/proposals have
	// diverged from disk since the last successful flush, so a flush rewrites
	// only what changed instead of re-marshaling every loaded branch's full
	// store every interval.
	dirtyBranches := map[string]bool{}
	var warnings []string
	chunksScanned, chunksDistilled := 0, 0

	// resolveBranch maps a session to the branch its facts are written under,
	// falling back to the manifest default (then distillDefaultBranch) when the
	// session's branch field is empty. The --branch filter must compare against
	// this resolved value, not session.Branch, so sessions with an empty branch
	// are not silently excluded from a `--branch <default>` run.
	resolveBranch := func(session exportSession) string {
		branch := session.Branch
		if branch == "" {
			branch = strings.TrimSpace(manifest.DefaultBranch)
		}
		if branch == "" {
			branch = distillDefaultBranch
		}
		return branch
	}

	// Denominator for progress: sessions that pass the branch filter. Cached
	// (unchanged) sessions still advance the counter so the line reaches N/N.
	totalSessions := 0
	for _, session := range sessions {
		if distillOpts.branch != "" && resolveBranch(session) != distillOpts.branch {
			continue
		}
		totalSessions++
	}
	sessionsDone, factsFound := 0, 0
	// Track agent-call outcomes so a misconfiguration that fails every call (e.g.
	// an invalid --model) aborts fast with the agent's own error, instead of
	// silently churning through every session reporting "0 facts found".
	agentFailures, anyAgentSuccess := 0, false

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
			// The in-memory store now differs from disk; the rebuild must be
			// written out even if the branch gains no new facts.
			dirtyBranches[branch] = true
		}
		byBranch[branch] = existing
		loaded[branch] = true
	}

	// flushFactStores persists the in-memory distill state — dirty branches'
	// facts and proposals, plus the session cache — so a killed or crashed run
	// resumes from the last flush instead of losing everything (a distill run is
	// hours of agent calls; the original write-once-at-the-end design lost the
	// entire run on any interruption). Facts are idempotent across a resume: ids are
	// content-derived and reconcile merges near-duplicates, so re-distilling the
	// partially-flushed session at the kill point is safe. A mid-run (non-final)
	// cache write keeps prevCache entries for sessions not yet visited — dropping
	// manifest-removed sessions is the final write's job, and dropping unvisited
	// entries mid-run would force a needless full re-distill of them after a
	// restart. Under --force the opposite holds: ensureBranch has already dropped
	// the unvisited sessions' distilled facts from byBranch, so persisting their
	// still-matching prevCache fingerprints alongside the truncated fact store
	// would make a plain rerun after a kill skip those sessions forever — their
	// facts silently lost. A force run therefore writes newCache only, and an
	// interrupted force run re-distills the sessions it never reached.
	flushFactStores := func(final bool) error {
		for branch := range dirtyBranches {
			if err := writeFacts(brainDir, branch, byBranch[branch]); err != nil {
				return err
			}
			// Union this run's proposals with any already queued. Re-loading on
			// every flush is idempotent: earlier flushes' proposals come back as
			// priors and dedupe away. A --force rebuild starts fresh (it rebuilds
			// the distilled facts), but drops the prior backlog only on the FINAL
			// flush — a mid-run flush that skipped priors would wipe the review
			// queue ~50 calls in, and a kill there would lose it before the
			// rebuild produced its replacement.
			var prior []factProposal
			if !(distillOpts.force && final) {
				if loadedProposals, loadErr := loadFactProposals(brainDir, branch); loadErr != nil {
					warnings = append(warnings, fmt.Sprintf("load proposals for %s: %v", branch, loadErr))
				} else {
					prior = loadedProposals
				}
			}
			merged := dedupeProposals(append(prior, proposalsByBranch[branch]...))
			if err := writeFactProposals(brainDir, branch, merged); err != nil {
				return err
			}
			// A force run keeps branches dirty between mid-run flushes so the
			// final flush always rewrites them — that write is what drops the
			// prior proposal backlog. Otherwise the branch is clean until new
			// fact actions touch it.
			if final || !distillOpts.force {
				delete(dirtyBranches, branch)
			}
		}
		cache := newCache
		if !final && !distillOpts.force {
			cache = distillCache{Version: distillCacheVersion, Sessions: make(map[string]string, len(prevCache.Sessions)+len(newCache.Sessions))}
			maps.Copy(cache.Sessions, prevCache.Sessions)
			maps.Copy(cache.Sessions, newCache.Sessions)
		}
		saveDistillCache(brainDir, cache)
		return nil
	}
	flushInterval := distillOpts.flushEvery
	if flushInterval <= 0 {
		flushInterval = distillFlushCallInterval
	}
	callsSinceFlush := 0
	maybeFlush := func() {
		if callsSinceFlush < flushInterval {
			return
		}
		// Reset even on failure so a persistent write error is retried once per
		// interval, not on every call.
		callsSinceFlush = 0
		if err := flushFactStores(false); err != nil {
			// Non-fatal: a mid-run flush only narrows the loss window, and
			// aborting an hours-long run over a transient write error would lose
			// far more than the flush protects. Failed branches stay dirty, so
			// the next interval (and the final flush) retries them.
			warnings = append(warnings, fmt.Sprintf("mid-run flush failed (will retry): %v", err))
		}
	}

	for _, session := range sessions {
		branch := resolveBranch(session)
		if distillOpts.branch != "" && branch != distillOpts.branch {
			// Carry the session's cache entry through unchanged: the final flush
			// persists newCache only, so dropping filtered sessions here would
			// make the next unfiltered run re-distill every other branch from
			// scratch. (The fingerprint-match skip below does the same.)
			if prev, ok := prevCache.Sessions[session.SessionID]; ok {
				newCache.Sessions[session.SessionID] = prev
			}
			continue
		}
		ensureBranch(branch)

		sessionsDone++
		// Report once per session, at every exit path, so the running fact count
		// includes the session just processed (an emit at session start would lag
		// by one session and always show 0 for a single-session run).
		reportProgress := func() {
			if distillOpts.progress != nil {
				distillOpts.progress(distillProgress{SessionsDone: sessionsDone, SessionsTotal: totalSessions, Branch: branch, Facts: factsFound})
			}
		}

		// Read the transcript before the cache check so the fingerprint can hash
		// the actual bytes: a re-export that rewrites the same path with different
		// content (e.g. compact vs raw mode, or an exporter fix) under the same
		// checkpoint must invalidate the cache and re-run the agent.
		content, readErr := readBrainRelativeFile(brainDir, session.TranscriptPath)
		if readErr != nil {
			warnings = append(warnings, fmt.Sprintf("read transcript %s: %v", session.TranscriptPath, readErr))
			reportProgress()
			continue // not cached: retried next run
		}

		// Strip tool calls/outputs and meta records up front: this is the actual
		// input the agent distills (they carry no durable facts and dominate the
		// transcript bytes, so removing them cuts chunk count — and agent calls —
		// sharply). Blanked records keep their line slot so provenance anchors stay
		// aligned to the original transcript. Fingerprinting this preprocessed input
		// (not the raw bytes) means churn confined to stripped tool I/O no longer
		// invalidates the cache and forces a needless re-distill.
		distillInput := preprocessTranscriptForDistill(content)

		fingerprint := distillSessionFingerprint(session, branch, distillInput)
		if !distillOpts.force {
			if prev, ok := prevCache.Sessions[session.SessionID]; ok && prev == fingerprint {
				newCache.Sessions[session.SessionID] = prev // unchanged; retain in cache and keep existing facts
				reportProgress()
				continue
			}
		}

		chunks := chunkTranscript(distillInput, distillOpts.maxChunkBytes)
		// Agent calls for this session's chunks run up to `concurrency` in
		// flight; results are consumed strictly in order below so reconcile and
		// fact application keep their serial semantics.
		prefetch := startChunkPrefetch(ctx, distillOpts.run, repoDir, args, chunks, distillOpts.timeout, distillOpts.concurrency)
		sessionFailed := false
		for chunkIdx, chunk := range chunks {
			chunksScanned++
			anchor := factAnchor{
				SessionID:    session.SessionID,
				CheckpointID: session.LatestCheckpoint,
				Transcript:   filepath.ToSlash(session.TranscriptPath),
				Line:         chunk.StartLine,
			}
			out, runErr := prefetch.result(chunkIdx)
			callsSinceFlush++
			if runErr != nil {
				warnings = append(warnings, fmt.Sprintf("agent failed on %s:%d: %v", session.SessionID, chunk.StartLine, runErr))
				sessionFailed = true
				agentFailures++
				// Fail fast on a misconfiguration: nothing has distilled yet and the
				// agent keeps failing, so every call is almost certainly erroring the
				// same way (bad --model, missing agent, auth). Abort with the agent's
				// own error instead of churning through every remaining session.
				if !anyAgentSuccess && agentFailures >= distillAgentAbortThreshold {
					prefetch.stop()
					return nil, fmt.Errorf("distill aborted after %d agent failures with no facts distilled — check --agent and --model. Last error: %v", agentFailures, runErr)
				}
				// Flush on the failure path too: a long failure streak (rate
				// limiting, timeouts) is exactly when a run tends to get killed,
				// and skipping the flush here would leave pre-streak facts
				// unpersisted for the streak's entire duration.
				maybeFlush()
				continue
			}
			anyAgentSuccess = true
			records, chunkWarnings := distilledFactsFromOutput(out, taxonomy, anchor, branch, now)
			warnings = append(warnings, chunkWarnings...)
			if len(records) == 0 {
				maybeFlush()
				continue
			}
			factsFound += len(records)
			chunksDistilled++
			// Reconcile the chunk's candidates against the branch's active facts
			// so near-duplicates merge and contradictions supersede instead of
			// piling up. Low-confidence decisions are queued for review.
			actions, recWarnings := reconcileChunkCandidates(ctx, distillOpts.run, reconcileArgs, records, byBranch[branch], repoDir, distillOpts.timeout)
			warnings = append(warnings, recWarnings...)
			callsSinceFlush++ // reconcile is an agent call too
			var chunkProposals []factProposal
			byBranch[branch], chunkProposals = applyFactActions(byBranch[branch], actions, threshold, now)
			proposalsByBranch[branch] = append(proposalsByBranch[branch], chunkProposals...)
			dirtyBranches[branch] = true
			maybeFlush()
		}
		prefetch.stop()
		// Only cache a session as distilled when every chunk succeeded, so a
		// session whose agent calls failed is retried on the next run rather
		// than being silently treated as done.
		if !sessionFailed {
			newCache.Sessions[session.SessionID] = fingerprint
		}
		reportProgress()
	}

	warnings = capWarnings(warnings, maxDistillWarnings)

	if err := flushFactStores(true); err != nil {
		return nil, err
	}
	if err := writeFactTaxonomy(brainDir, taxonomy); err != nil {
		return nil, err
	}

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
	if doc, ok := distillDocumentConversation(content); ok {
		return doc
	}
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

// distillDocumentConversation handles transcripts exported as one pretty-printed
// JSON document (e.g. opencode sessions) rather than JSONL. The line-oriented
// preprocessor cannot strip these — every line of an indented document fails
// json.Unmarshal individually — so before this path existed they passed through
// *unstripped*, feeding megabytes of tool output, diffs, and token accounting to
// the distill agent (observed: a 22.6 MB opencode session was 1.3% conversation
// text, so the no-op strip cost ~75x the agent calls it should have).
//
// The document shape is {info, messages: [{info, parts: [{type, text, ...}]}]};
// only parts of type "text" are conversation (tool, patch, reasoning,
// step-start/finish, and file parts are mechanics). The output has exactly as
// many lines as the document, with each message's collapsed text placed on the
// line where that message's object begins — the same "output line N is input
// file line N" contract the JSONL path keeps, so a fact's provenance anchor
// points at a real location in the original transcript (consumers like `facts
// read` render anchors as <transcript>:<line>). ok=false when the content is
// not a document-form transcript, leaving the JSONL path to handle it.
func distillDocumentConversation(content string) (string, bool) {
	messages, ok := parseDocumentConversation(content)
	if !ok {
		return "", false
	}
	out := make([]string, strings.Count(content, "\n")+1)
	for _, message := range messages {
		if message.Text == "" {
			continue // tool-only message: leave its lines blank
		}
		i := min(max(message.Line-1, 0), len(out)-1)
		if out[i] != "" {
			out[i] += " " + message.Text // two messages on one line: minified fragments
		} else {
			out[i] = message.Text
		}
	}
	return strings.Join(out, "\n"), true
}

// documentMessage is one message of a document-form transcript as extracted by
// parseDocumentConversation.
type documentMessage struct {
	Role string // the message's info.role ("user", "assistant", ...); "" when absent
	Text string // whitespace-collapsed conversation text; "" for tool-only messages
	Line int    // 1-based line of the message object's opening brace in the document
}

// parseDocumentConversation extracts the conversation messages from a
// document-form transcript (see distillDocumentConversation for the shape),
// recording for each message the document line its object starts on so callers
// can anchor extracted text to the original file. It streams with json.Decoder
// and decodes each message into a typed struct, so the tool outputs, patches,
// and token accounting that dominate the document are scanned past rather than
// materialized. ok=false when content is not a document-form transcript, or
// when no message yields any conversation text: a document in some OTHER chat
// shape (e.g. {"messages":[{role,content}]} with no opencode-style parts)
// must fall through to the JSONL path, which passes unparseable lines through
// unstripped so the agent can still mine them — claiming such a document here
// would silently blank the whole session and cache it as distilled with zero
// facts.
func parseDocumentConversation(content string) ([]documentMessage, bool) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "{") {
		return nil, false
	}
	// A JSONL transcript has a complete JSON object on its first line; a
	// pretty-printed document does not. Probe before paying the full parse
	// (validity is the whole question — the "{" prefix already restricts the
	// shape to an object).
	firstLine, _, _ := strings.Cut(trimmed, "\n")
	if json.Valid([]byte(firstLine)) {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(content))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	// lineAt maps a byte offset to its 1-based line, walking forward from the
	// previous query; offsets arrive in increasing order so each newline is
	// counted once.
	line, pos := 1, 0
	lineAt := func(offset int) int {
		line += strings.Count(content[pos:offset], "\n")
		pos = offset
		return line
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, isString := keyTok.(string)
		if !isString {
			return nil, false
		}
		if key != "messages" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, false
			}
			continue
		}
		if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
			return nil, false
		}
		var messages []documentMessage
		for dec.More() {
			// InputOffset sits just past the previous token; skip the separator
			// and whitespace so the recorded line is the opening brace's line.
			start := int(dec.InputOffset())
			for start < len(content) && (content[start] == ',' || content[start] == ' ' || content[start] == '\t' || content[start] == '\r' || content[start] == '\n') {
				start++
			}
			msgLine := lineAt(start)
			var msg struct {
				Info struct {
					Role string `json:"role"`
				} `json:"info"`
				Parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parts"`
			}
			if err := dec.Decode(&msg); err != nil {
				return nil, false
			}
			var words []string
			for _, part := range msg.Parts {
				if part.Type != "text" || part.Text == "" {
					continue // tool, patch, reasoning, step-start/finish, file, ...
				}
				words = append(words, strings.Fields(part.Text)...)
			}
			messages = append(messages, documentMessage{
				Role: msg.Info.Role,
				Text: strings.Join(words, " "),
				Line: msgLine,
			})
		}
		for _, message := range messages {
			if message.Text != "" {
				return messages, true
			}
		}
		return nil, false // no conversation text at all: not our document shape
	}
	return nil, false // no top-level "messages" key
}

// distillConversationText returns the human/assistant text worth distilling from
// one transcript record, or "" for tool calls, tool outputs, reasoning, and
// session/meta records. It mirrors the record routing in
// extractHistoryJSONFragments (codex `event_msg`/`response_item`, Claude
// `assistant`/`user`, and pi `message` shapes — keep the two switches in sync
// when adding a format) but deliberately keeps user turns — where standing
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
	case "message":
		// pi sessions wrap every turn as {"type":"message","message":{role,content}}.
		// Without this case they fell to the default branch, whose
		// jsonString(obj["message"]) is "" for a map — silently stripping the
		// ENTIRE session (observed: 13.4 MB pi transcript -> 0 bytes kept, so pi
		// sessions contributed zero facts). Keep user/assistant text; the
		// toolResult role and thinking/toolCall blocks are mechanics.
		payload := jsonMap(obj["message"])
		if role := jsonString(payload["role"]); role != "assistant" && role != "user" {
			return "" // toolResult, ...
		}
		return distillTextBlocks(payload["content"])
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
func distillSessionFingerprint(session exportSession, branch, content string) string {
	contentSum := sha256.Sum256([]byte(content))
	sum := sha256.Sum256([]byte(strings.Join([]string{
		session.SessionID,
		session.LatestCheckpoint,
		filepath.ToSlash(session.TranscriptPath),
		branch,
		hex.EncodeToString(contentSum[:]),
	}, "\x00")))
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
func execDistillAgent(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
	if len(args) == 0 {
		return "", errors.New("distill: empty agent command")
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, args[0], args[1:]...)
	command.Dir = dir
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("agent timed out after %s", timeout)
	}
	if err != nil {
		warning := strings.TrimSpace(stderr.String())
		if warning == "" {
			warning = strings.TrimSpace(stdout.String())
		}
		return "", fmt.Errorf("agent failed: %w: %s", err, truncateAgentWarning(warning))
	}
	if stdout.Len() > distillMaxOutputBytes {
		return "", fmt.Errorf("agent output exceeds %d bytes", distillMaxOutputBytes)
	}
	return stdout.String(), nil
}
