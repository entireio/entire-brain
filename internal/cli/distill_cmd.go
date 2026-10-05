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
	"maps"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/factmerge"
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

	// distillAgentWaitDelay is how long Wait may keep waiting on an agent's
	// output pipes after the agent process itself has been killed for exceeding
	// its timeout. It exists because the pipes are inherited by whatever the
	// agent spawned, which the kill does not reach. Long enough to collect the
	// output of an agent that is merely finishing up, short enough that the
	// timeout stays a bound.
	distillAgentWaitDelay = 5 * time.Second
)

// maxPersistedDistillWarnings bounds what reaches the manifest.
//
// The bug this change set fixes was a 50-entry DISPLAY cap applied before the
// write, so warning 51 existed nowhere. Persisting everything fixes that and
// introduces a smaller problem in its place: a pathological run -- every chunk
// timing out, say -- would append an unbounded list to a file that is read on
// every status call. My own distill run produced 278 suppressed warnings in
// one pass, so this is not hypothetical.
//
// 1000 is high enough that no real run is truncated and low enough that a
// runaway cannot grow the manifest without limit. When it does fire the
// overflow line states the true total, so the count is never wrong -- only the
// tail of a list that is already repeating itself is lost.
const maxPersistedDistillWarnings = 1000

// distillWarningsForPersist bounds the written list while keeping the total
// honest. Unlike the old display cap, the number it reports is the real one.
func distillWarningsForPersist(warnings []string) []string {
	if len(warnings) <= maxPersistedDistillWarnings {
		return warnings
	}
	kept := warnings[:maxPersistedDistillWarnings:maxPersistedDistillWarnings]
	return append(kept, fmt.Sprintf(
		"... and %d more warnings beyond the %d kept here (total %d); the tail is dropped to bound manifest growth, the count is exact",
		len(warnings)-maxPersistedDistillWarnings, maxPersistedDistillWarnings, len(warnings)))
}

// distillWarningsForDisplay caps a warning list for PRINTING only.
//
// The cap used to be applied before the manifest write, so the text of every
// warning past the 50th existed nowhere afterwards -- not in the manifest, not
// behind a flag, nowhere -- while the run still reported success. The full list
// is now persisted (manifest sources.facts.warnings, and the dry-run report's
// JSON); only the terminal rendering is truncated, and the overflow line says
// how many were held back and where to read them.
func distillWarningsForDisplay(warnings []string) []string {
	if len(warnings) <= maxDistillWarnings {
		return warnings
	}
	extra := len(warnings) - maxDistillWarnings
	return append(warnings[:maxDistillWarnings:maxDistillWarnings], fmt.Sprintf(
		"... and %d more warnings (not shown here; all %d are recorded in the brain manifest under sources.facts.warnings, and in --json output)",
		extra, len(warnings)))
}

// capWarnings truncates a warning list to max entries, replacing the overflow
// with a single summary line so a systemic failure stays legible.
//
// It is a DISPLAY cap. Never apply it to a list on its way to disk: the
// overflow text has no other home. Use distillWarningsForDisplay at the point
// of rendering instead.
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

// distillSessionLookahead bounds how many PREPARED sessions may queue between
// producer and consumer. Skipped and cache-hit sessions are near-free structs,
// and they dominate incremental runs (observed: ~95% of an interrupted-run
// resume), so this buffer must be deep enough that a skip streak never blocks
// the producer from reaching the next session with real agent work. Memory
// stays bounded because sessions HOLDING WORK (preprocessed chunks awaiting
// or in agent calls) are limited separately by the work-lookahead semaphore.
const distillSessionLookahead = 256

// preparedSession is one session's distill work, prepared by the pipeline
// ahead of the consumer. Exactly one of skip/readErr/cached/chunks describes
// its disposition; chunk agent calls (when any) were dispatched into the
// run's shared pool and are consumed strictly in order via result(i).
type preparedSession struct {
	session     exportSession
	branch      string
	skip        bool  // filtered out by --branch/--session: carry cache through
	deferred    bool  // --max-sessions budget spent: carry cache through, distill next run
	readErr     error // canonical input refusal: abort without publication
	cached      bool  // fingerprint matched: keep facts, no agent work
	fingerprint string
	rawBytes    int
	preBytes    int
	chunks      []transcriptChunk
	results     []chan agentCallResult
	sem         chan struct{}
	// release frees the session's work-lookahead slot; the consumer calls it
	// once the session's chunks are fully consumed. nil for sessions that
	// carry no work (skip/cached/readErr).
	release func()
	// seq, when set (concurrency <= 1), replaces the pool: result(i) runs the
	// agent call inline and lazily — strictly one agent process at a time, and
	// no call is ever made for a chunk the consumer never asks about (e.g.
	// after an abort) — exactly the original sequential behavior.
	seq func(i int) (string, error)
}

// result blocks until chunk i's agent call completes and returns its output,
// releasing the call's pool slot so the dispatcher may launch the next call.
func (ps *preparedSession) result(i int) (string, error) {
	if ps.seq != nil {
		return ps.seq(i)
	}
	r := <-ps.results[i]
	if r.holdsSlot {
		<-ps.sem
	}
	return r.out, r.err
}

// startSessionPrefetch is the distill pipeline's producer: it prepares
// sessions in chronological order and dispatches their chunk agent calls into
// ONE shared pool, up to `concurrency` calls in flight ACROSS sessions — the
// per-session pool it replaces gave a 1-chunk session (the common case after
// stripping; the corpus median) no parallelism at all, serializing the run on
// agent latency.
//
// Two properties carry over from the per-session prefetcher and one is new:
//   - In-order delivery: the consumer applies chunks strictly in global
//     (session, chunk) order, so reconcile and fact application keep their
//     exact serial semantics; only agent round-trips overlap.
//   - Consumption-bounded lookahead: a pool slot is held from a call's launch
//     until its result is CONSUMED, so at most `concurrency` completed results
//     are ever buffered and an early abort stops dispatch within `concurrency`
//     calls of the last one consumed.
//   - Single dispatcher, global dispatch order: slots are acquired in exactly
//     the order the consumer will need results. Competing per-session
//     dispatchers over a shared pool could hand a freed slot to a session the
//     consumer is not ready for, buffering its result while the chunk the
//     consumer is blocked on starves — a deadlock at small pool sizes.
//
// The returned channel delivers sessions in order with lookahead
// distillSessionLookahead; it closes when all sessions are produced or ctx is
// canceled. Cancel ctx to stop preparation, dispatch, and in-flight calls.
func startSessionPrefetch(ctx context.Context, brainDir, repoDir string, args []string, sessions []exportSession, prevCache distillCache, distillOpts distillCommandOptions, resolveBranch func(exportSession) string) <-chan preparedSession {
	prepared := make(chan preparedSession, distillSessionLookahead)
	duplicateKeys := distillDuplicateSessionKeys(sessions, resolveBranch)
	var sem chan struct{}
	if distillOpts.concurrency > 1 {
		sem = make(chan struct{}, distillOpts.concurrency)
	}
	// workSem bounds sessions HOLDING WORK — preprocessed chunk text awaiting
	// or in agent calls — independently of the session buffer. The buffer must
	// be deep so skip/cache streaks never starve dispatch (observed: a ~95%
	// cache-hit region left a concurrency-8 pool running ONE call, because a
	// 4-session lookahead held ~0.2 work-sessions); workSem keeps the memory
	// for that depth bounded to a handful of sessions' chunk text. A slot is
	// held from just before a session's transcript read (the chunks' memory is
	// born there) until the consumer finishes its chunks.
	workAhead := distillOpts.concurrency
	if workAhead < 2 {
		workAhead = 2
	}
	workSem := make(chan struct{}, workAhead)
	// Stage A launches per-session PREP workers (transcript read,
	// preprocessing, fingerprint/cache check, chunking) bounded by workSem and
	// acquired in order; stage B awaits each session's future in order,
	// delivers it, and dispatches its chunk calls. Prep parallelism matters as
	// much as call parallelism on document-heavy corpora: preprocessing a
	// multi-MB document costs seconds, and a serial prep stage was observed
	// feeding only ~2 of 8 pool slots — the producer couldn't prepare sessions
	// as fast as the pool retired their calls.
	prepQueue := make(chan chan preparedSession, distillSessionLookahead)
	go func() { // stage A: in-order prep launcher
		defer close(prepQueue)
		enqueue := func(f chan preparedSession) bool {
			select {
			case prepQueue <- f:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for _, session := range sessions {
			if ctx.Err() != nil {
				return
			}
			ps := preparedSession{session: session, branch: resolveBranch(session)}
			f := make(chan preparedSession, 1) // buffered: a worker outliving a canceled consumer delivers and exits
			if (distillOpts.branch != "" && ps.branch != distillOpts.branch) ||
				(distillOpts.session != "" && session.SessionID != distillOpts.session) {
				ps.skip = true
				f <- ps
				if !enqueue(f) {
					return
				}
				continue
			}
			select {
			case workSem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func(ps preparedSession) {
				releaseWork := func() { <-workSem }
				data, readErr := readCanonicalHistoryTranscript(ctx, brainDir, ps.session.TranscriptPath)
				if readErr != nil {
					releaseWork()
					ps.readErr = readErr
					f <- ps
					return
				}
				content := string(data)
				// Strip tool calls/outputs and meta records up front: this is
				// the actual input the agent distills. Fingerprinting the
				// preprocessed input (not raw bytes) means churn confined to
				// stripped tool I/O does not invalidate the cache.
				distillInput := preprocessTranscriptForDistill(content)
				ps.rawBytes = len(content)
				ps.preBytes = len(distillInput)
				ps.fingerprint = distillSessionFingerprint(ps.session, ps.branch, distillInput, distillOpts.cacheSalt)
				if !distillOpts.force {
					cacheKey := distillSessionCacheKeyFor(ps.branch, ps.session, duplicateKeys)
					if prev, ok := cachedDistillFingerprintForKey(prevCache, cacheKey); ok && prev == ps.fingerprint {
						releaseWork()
						ps.cached = true
						f <- ps
						return
					}
					// Pre-upgrade entry (legacy key + formula): the session is
					// unchanged; the consumer rewrites it under the new
					// key/format — lazy migration, zero agent calls. Skipped for
					// a duplicated session id: the single legacy entry cannot
					// say WHICH of the colliding exports produced it, and
					// grandfathering both would mark a genuinely undistilled
					// export as done. One re-distill, once, then both hold
					// distinct keys.
					if !duplicateKeys[distillSessionCacheKey(ps.branch, ps.session.SessionID)] &&
						grandfatheredDistillFingerprint(prevCache, ps.session, ps.branch, distillInput) {
						releaseWork()
						ps.cached = true
						f <- ps
						return
					}
				}
				ps.chunks = chunkTranscript(distillInput, distillOpts.maxChunkBytes)
				ps.release = releaseWork
				f <- ps
			}(ps)
			if !enqueue(f) {
				return
			}
		}
	}()
	go func() { // stage B: in-order delivery + global-order dispatch
		defer close(prepared)
		// distilled counts sessions this run actually dispatched agent calls
		// for. The --max-sessions budget is enforced HERE, in the single
		// in-order dispatcher, so the decision is deterministic (the same N
		// sessions every time for a given order) and, crucially, no agent call
		// is ever dispatched for a deferred session.
		distilled := 0
		for f := range prepQueue {
			var ps preparedSession
			select {
			case ps = <-f:
			case <-ctx.Done():
				return
			}
			if ps.skip || ps.readErr != nil || ps.cached {
				select {
				case prepared <- ps:
					continue
				case <-ctx.Done():
					return
				}
			}
			if distillOpts.maxSessions > 0 && distilled >= distillOpts.maxSessions {
				// Budget spent. Release the work slot the prep stage took (the
				// consumer will never consume these chunks) and drop the chunk
				// text so a deferred tail costs no memory.
				if ps.release != nil {
					ps.release()
					ps.release = nil
				}
				ps.chunks = nil
				ps.deferred = true
				select {
				case prepared <- ps:
					continue
				case <-ctx.Done():
					return
				}
			}
			distilled++
			if sem == nil {
				ps.seq = func(chunks []transcriptChunk) func(int) (string, error) {
					return func(i int) (string, error) {
						return distillOpts.run(ctx, repoDir, args, []byte(chunks[i].Text), distillOpts.timeout)
					}
				}(ps.chunks)
				select {
				case prepared <- ps:
				case <-ctx.Done():
					return
				}
				continue
			}
			ps.sem = sem
			ps.results = make([]chan agentCallResult, len(ps.chunks))
			for i := range ps.results {
				ps.results[i] = make(chan agentCallResult, 1) // buffered: abandoned workers deliver and exit
			}
			// Deliver BEFORE dispatching: a session with more chunks than the
			// pool must be consumable while its tail still dispatches.
			select {
			case prepared <- ps:
			case <-ctx.Done():
				return
			}
			for i := range ps.chunks {
				if ctx.Err() != nil { // don't race a freed slot against cancellation
					ps.results[i] <- agentCallResult{err: ctx.Err()}
					continue
				}
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					ps.results[i] <- agentCallResult{err: ctx.Err()}
					continue
				}
				go func(i int, chunks []transcriptChunk, results []chan agentCallResult) {
					// A panic while processing one (untrusted) chunk must not crash the
					// whole distill run and lose every other chunk's work; convert it to
					// a per-chunk error so the slot is released and the run continues.
					defer func() {
						if r := recover(); r != nil {
							results[i] <- agentCallResult{err: fmt.Errorf("distill worker panicked: %v", r), holdsSlot: true}
						}
					}()
					out, err := distillOpts.run(ctx, repoDir, args, []byte(chunks[i].Text), distillOpts.timeout)
					results[i] <- agentCallResult{out: out, err: err, holdsSlot: true}
				}(i, ps.chunks, ps.results)
			}
		}
	}()
	return prepared
}

// validateDistillCanonicalInputs establishes complete-or-refused input safety
// before any provider work can be dispatched or any incremental fact/cache
// state can be flushed. The pipeline re-reads selected transcripts for bounded
// work scheduling; this preflight deliberately favors the egress/publication
// boundary over avoiding one extra descriptor-rooted read.
func validateDistillCanonicalInputs(ctx context.Context, brainDir string, sessions []exportSession, distillOpts distillCommandOptions, resolveBranch func(exportSession) string) error {
	for _, session := range sessions {
		if distillOpts.branch != "" && resolveBranch(session) != distillOpts.branch {
			continue
		}
		if distillOpts.session != "" && session.SessionID != distillOpts.session {
			continue
		}
		if _, err := readCanonicalHistoryTranscript(ctx, brainDir, session.TranscriptPath); err != nil {
			return fmt.Errorf("read canonical transcript %s: %w", filepath.ToSlash(session.TranscriptPath), err)
		}
	}
	return nil
}

type distillCommandOptions struct {
	branch string
	// session restricts distillation to one session id — the fast
	// single-session path behind `hook session-end` (Phase 2 item 5). Other
	// sessions carry their cache entries through untouched, exactly like the
	// branch filter. Incompatible with force: a forced rebuild drops every
	// distilled fact on the session's branch but would re-derive only the one
	// session's, silently losing the rest.
	session             string
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
	cacheSalt           string
	// newestFirst distills the most recent sessions BEFORE older ones. The
	// default (false) keeps the established oldest-first order, where a later
	// session's fact naturally supersedes an earlier one's. Newest-first is for
	// the background backfill `setup` starts: a long O(sessions) pass whose
	// early output is the only output a user sees soon, so the most useful
	// (most recent) facts must land first. Ordering only decides which sessions
	// are visited first — every session is still visited, and the persisted
	// distill cache means neither order re-spends on unchanged sessions.
	newestFirst bool
	// onPassSkipped fires when the cross-process pass lock was already held, so
	// this run did nothing and spent nothing. `distill` still exits 0 (another
	// process is doing the work), but a caller that RESERVED a spend window
	// before calling has to be able to give it back.
	onPassSkipped func()
	// onDeferredSessions reports work left by a successfully persisted capped pass.
	onDeferredSessions func(int)
	// maxSessions caps how many UNCACHED sessions one run distills (0 =
	// unlimited). Paired with newestFirst it is "distill the N newest sessions
	// that still need it", the token budget for a backfill pass; the remaining
	// sessions stay uncached and are picked up by the next run or the watcher.
	maxSessions int
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
	// entityProvenance, when set, sharpens each new fact's anchor from the
	// entity -> checkpoint index: the checkpoint that actually changed the
	// entity the fact's locus names, instead of the session's LAST checkpoint.
	// nil (the zero value, and the value in every pre-index caller) keeps the
	// original coarse anchoring exactly.
	entityProvenance *entityProvenanceResolver
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

type distillPlan struct {
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
	ChunksIfUncached   int
	Warnings           []string
	CacheSalt          string
	// BudgetDeferredSessions counts sessions that still need distilling but
	// were held back by --max-sessions. They remain uncached, so a later run
	// picks them up.
	BudgetDeferredSessions int
}

type distillSessionPlan struct {
	Session     exportSession
	Branch      string
	Fingerprint string
	Cached      bool
	// BudgetDeferred marks a session that needs distilling but was held back
	// by --max-sessions. It is NOT cached: the next run distills it.
	BudgetDeferred    bool
	ReadFailed        bool
	RawBytes          int
	PreprocessedBytes int
	ChunksIfUncached  int
	ChunkIndexes      []int
}

// distillChunkWork is one planned (session, chunk) extraction call. Only the
// indexes are kept: the dry-run reports counts and per-session linkage, and
// carrying full session/chunk/anchor copies for every planned call held the
// whole corpus's chunk text in the plan for nothing.
type distillChunkWork struct {
	SessionIndex int
	ChunkIndex   int
	Branch       string
}

type distillDryRunReport struct {
	SchemaVersion                 int                    `json:"schema_version"`
	GeneratedAt                   time.Time              `json:"generated_at"`
	BrainPath                     string                 `json:"brain_path"`
	Branch                        string                 `json:"branch,omitempty"`
	Force                         bool                   `json:"force"`
	Agent                         string                 `json:"agent,omitempty"`
	Model                         string                 `json:"model,omitempty"`
	Effort                        string                 `json:"effort,omitempty"`
	Jobs                          int                    `json:"jobs"`
	ExtractionJobsCap             int                    `json:"extraction_jobs_cap"`
	MaxChunkBytes                 int                    `json:"max_chunk_bytes"`
	Confidence                    float64                `json:"confidence_threshold"`
	Sessions                      int                    `json:"sessions"`
	CachedSessions                int                    `json:"cached_sessions"`
	SessionsToDistill             int                    `json:"sessions_to_distill"`
	MissingTranscripts            int                    `json:"missing_transcripts"`
	RawBytes                      int64                  `json:"raw_bytes"`
	PreprocessedBytes             int64                  `json:"preprocessed_bytes"`
	Chunks                        int                    `json:"chunks"`
	ChunksIfUncached              int                    `json:"chunks_if_uncached"`
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
	ChunksIfUncached  int    `json:"chunks_if_uncached"`
	PreprocessedBytes int64  `json:"preprocessed_bytes"`
}

type distillDryRunSession struct {
	SessionID         string `json:"session_id"`
	Branch            string `json:"branch"`
	Transcript        string `json:"transcript"`
	Cached            bool   `json:"cached"`
	Chunks            int    `json:"chunks"`
	ChunksIfUncached  int    `json:"chunks_if_uncached"`
	RawBytes          int    `json:"raw_bytes"`
	PreprocessedBytes int    `json:"preprocessed_bytes"`
}

func newDistillCommand(opts Options) *cobra.Command {
	distillOpts := distillCommandOptions{agent: "auto", timeout: defaultDistillTimeout, maxChunkBytes: defaultDistillChunkSize, confidenceThreshold: defaultFactConfidenceThreshold, concurrency: defaultDistillConcurrency}
	cmd := &cobra.Command{
		Use:   "distill [path]",
		Short: "Extract durable facts from sessions (requires an agent)",
		Long:  "Distill captured sessions into durable facts (agent-required)",
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
	cmd.Flags().StringVar(&distillOpts.session, "session", "", "Limit distillation to a single session id (the fast per-session path; incompatible with --force)")
	cmd.Flags().BoolVar(&distillOpts.force, "force", false, "Recompute all distilled facts from scratch instead of skipping unchanged sessions")
	cmd.Flags().BoolVar(&distillOpts.json, "json", false, "Emit the fact source summary as JSON")
	cmd.Flags().StringVar(&distillOpts.agent, "agent", "auto", "Distillation agent: auto, codex, claude-code, ollama, or command")
	cmd.Flags().StringArrayVar(&distillOpts.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().Float64Var(&distillOpts.confidenceThreshold, "confidence", defaultFactConfidenceThreshold, "Minimum agent confidence to auto-apply a merge/supersede; below this it is queued for review")
	cmd.Flags().StringVar(&distillOpts.model, "model", "", "Override the agent model for codex/claude-code, or select the local Ollama model")
	cmd.Flags().StringVar(&distillOpts.effort, "effort", "", "Override the reasoning effort for codex/claude-code (e.g. low) — pairs with --model for a cheap run")
	cmd.Flags().BoolVar(&distillOpts.dryRun, "dry-run", false, "Estimate distill work without calling an agent or writing facts")
	cmd.Flags().IntVar(&distillOpts.concurrency, "concurrency", defaultDistillConcurrency, "Distill agent calls to run in flight at once, shared across sessions (1 = strictly sequential; higher values may add one concurrent reconcile call)")
	cmd.Flags().IntVar(&distillOpts.jobs, "jobs", 0, "Compatibility alias for --concurrency")
	cmd.Flags().DurationVar(&distillOpts.timeout, "timeout", defaultDistillTimeout,
		"Per-chunk agent timeout. A chunk the agent cannot finish inside this budget fails and is retried on the next run, so a corpus of large transcripts can crawl forward one session per timeout. Raise it for big sessions or a slow local model; lower --max-chunk-bytes instead if the model is the limit")
	cmd.Flags().IntVar(&distillOpts.maxChunkBytes, "max-chunk-bytes", defaultDistillChunkSize, "Transcript chunk size in bytes; larger chunks mean fewer agent calls per session, but a chunk must fit the model's context window — transcript text runs ~3.65 chars/token, so a 32K-context model (ollama's default for many local models) truncates past roughly 116KB")
	cmd.Flags().BoolVar(&distillOpts.newestFirst, "newest-first", false, "Distill the most recent sessions first so the most useful facts land early (default: oldest first)")
	cmd.Flags().IntVar(&distillOpts.maxSessions, "max-sessions", 0, "Cap how many not-yet-distilled sessions this run processes (0 = no cap); pairs with --newest-first as a backfill budget")
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
	if distillOpts.maxChunkBytes <= 0 {
		return fmt.Errorf("--max-chunk-bytes must be greater than 0")
	}
	if distillOpts.concurrency <= 0 {
		return fmt.Errorf("--concurrency must be greater than 0")
	}
	if cmd.Flags().Changed("timeout") && distillOpts.timeout <= 0 {
		return fmt.Errorf("--timeout must be greater than 0")
	}
	// Only when the user actually typed it: the zero value of the option means
	// "use the default" for every in-process caller (runDistillForBrain reads
	// threshold <= 0 that way), so the library contract has to stay intact while
	// the flag stops silently swallowing a value it cannot honour.
	if cmd.Flags().Changed("confidence") && (math.IsNaN(distillOpts.confidenceThreshold) || distillOpts.confidenceThreshold <= 0 || distillOpts.confidenceThreshold > 1) {
		return fmt.Errorf("--confidence must be greater than 0 and at most 1 (agent confidence is a probability); got %g", distillOpts.confidenceThreshold)
	}
	if cmd.Flags().Changed("jobs") {
		if distillOpts.jobs <= 0 {
			return fmt.Errorf("--jobs must be greater than 0")
		}
		if cmd.Flags().Changed("concurrency") && distillOpts.jobs != distillOpts.concurrency {
			return fmt.Errorf("--jobs conflicts with --concurrency; use --concurrency")
		}
		distillOpts.concurrency = distillOpts.jobs
	}
	if err := rejectAgentForNoEgress(distillOpts.agent); err != nil {
		return err
	}
	// Resolve the agent BEFORE the dry-run branch: the cache salt hashes the
	// agent name, so a dry-run salted with the literal "auto" while real runs
	// salt with the resolved agent would predict every cached session as
	// to-distill — defeating the dry-run's whole purpose of matching the run.
	if distillOpts.agent == "auto" {
		distillOpts.agent = defaultRefreshAgent(ctx, opts.Runner, repoDir)
		if warning := ollamaModelMissingWarning(ctx, opts.Runner, repoDir, distillOpts.agent); warning != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning:", warning)
		}
	}
	if distillOpts.session != "" && distillOpts.force {
		// Mirror runDistillForBrain's guard so a dry-run rejects the same
		// combination the real run rejects, instead of planning it.
		return errors.New("--session cannot be combined with --force: a forced rebuild drops every distilled fact on the session's branch but would re-derive only that session's")
	}
	if distillOpts.dryRun {
		report, err := buildDistillDryRunReportContext(ctx, storage.BrainDir, distillOpts, opts.Now().UTC())
		if err != nil {
			return err
		}
		if distillOpts.json {
			return writeJSON(cmd, report)
		}
		printDistillDryRunReport(cmd, report)
		return nil
	}
	// The agent check is deliberately NOT here any more; it moved below the
	// pass lock. A pass that finds the lock held does nothing, so the agent it
	// would have used is irrelevant, and complaining about the agent first
	// reports the wrong reason: a supervised watcher told "no agent on PATH"
	// treats a transient contention as a configuration error and gives up,
	// where "another pass holds this brain" is a wait-and-retry.
	if distillOpts.run == nil && distillOpts.agent != "none" {
		distillOpts.run = defaultDistillAgentRunner(distillOpts.agent)
	}
	// Progress goes to stderr so it never corrupts the --json summary on stdout.
	progress := newProgress(cmd.ErrOrStderr(), "distill")
	task := progress.Begin("distill sessions")
	distillOpts.progress = func(p distillProgress) {
		// Event, not Update: each callback is ONE session finished, and a
		// detached backfill's log is the only place that per-session heartbeat
		// exists. Update coalesces labels that differ only in counters — right
		// for a repaint, fatal for a completion event.
		task.Event(distillProgressLabel(p))
	}
	// One pass at a time per brain. Every in-process caller funnels through here
	// — the detached `setup` backfill child, the watcher's gated distill step,
	// the session-end hook, and a human running `distill` — so this is the one
	// place that can stop two of them spending tokens on the same sessions.
	var source *factSourceManifest
	skipped := false
	// Contention is decided before the agent is: see above.
	err = withDistillPassLock(storage.BrainDir, func() {
		skipped = true
		// Tell the CALLER, not just the terminal. A skipped pass spends
		// nothing, and a supervised watcher that cannot tell "done" from
		// "someone else was holding the lock" burns its whole --distill-every
		// window on a no-op and logs "spent tokens" while doing it — which is
		// exactly what `setup` produces, since it spawns the hours-long
		// detached backfill and installs a watcher that ticks immediately.
		if distillOpts.onPassSkipped != nil {
			distillOpts.onPassSkipped()
		}
		fmt.Fprintf(cmd.ErrOrStderr(),
			"distill: another distill pass already holds %s for this brain; skipping so the same sessions are not distilled twice\n",
			filepath.Join(storage.BrainDir, brainLockDirName, brainDistillLockName))
	}, func() error {
		// We hold the lock, so this pass will really run: NOW the agent has to
		// exist. ollama is named because it is auto-selected ahead of the
		// cloud agents (issue #328); the old text listed only codex and
		// claude-code, so a user whose local model was missing was told to
		// install something else entirely.
		if distillOpts.agent == "none" {
			return errors.New("distillation requires an agent (ollama, codex, or claude-code); none found on PATH")
		}

		// Built once per run, before any agent call: reading the derived index
		// is cheap and bounded, and a missing/empty index yields nil, which
		// restores the pre-index anchoring exactly. Built INSIDE the pass lock
		// so a run that skips never pays for it and a run that proceeds reads
		// the index as of the moment it actually owns the pass — the session-end
		// hook refreshes that index just before calling in.
		distillOpts.entityProvenance = newEntityProvenanceResolver(ctx, opts, repoDir)
		var runErr error
		source, runErr = runDistillForBrain(ctx, repoDir, storage.BrainDir, distillOpts, opts.Now().UTC())
		return runErr
	})
	task.Finish(err)
	if err != nil {
		return err
	}
	if skipped {
		return nil
	}
	if distillOpts.json {
		data, err := json.MarshalIndent(source, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	for _, warning := range distillWarningsForDisplay(source.Warnings) {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
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
	if math.IsNaN(distillOpts.confidenceThreshold) || math.IsInf(distillOpts.confidenceThreshold, 0) || distillOpts.confidenceThreshold > 1 {
		return nil, errors.New("confidence must be finite and at most 1")
	}
	runStarted := time.Now()
	ctx, usageCollector := withDistillUsageCollector(ctx)
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
	if distillOpts.session != "" && distillOpts.force {
		return nil, errors.New("--session cannot be combined with --force: a forced rebuild drops every distilled fact on the session's branch but would re-derive only that session's")
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
	reconcilePromptText := reconcilePrompt()
	reconcileArgs, err := distillAgentCommandArgs(distillOpts.agent, distillOpts.agentCommand, reconcilePromptText)
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
	distillOpts.cacheSalt = distillCacheSalt(prompt, reconcilePromptText, threshold, distillOpts)

	sessions := append([]exportSession(nil), manifest.Sources.Sessions.Sessions...)
	// Excluded sessions must never produce new derived facts.
	sessions, err = filterTombstonedSessions(brainDir, sessions)
	if err != nil {
		return nil, err
	}
	// Chronological order so any future supersession chain reconstructs
	// deterministically regardless of incremental vs --force. --newest-first
	// reverses it for the backfill pass (see sortDistillSessions).
	sortDistillSessions(sessions, distillOpts.newestFirst)

	prevCache := loadDistillCache(brainDir)
	newCache := distillCache{Version: distillCacheVersion, Sessions: make(map[string]string, len(sessions))}
	// Computed from the same (sessions, resolveBranch) pair the prefetch uses,
	// so producer and consumer agree on every session's key.
	duplicateKeys := distillDuplicateSessionKeys(sessions, func(session exportSession) string {
		return resolveDistillBranch(manifest, session)
	})

	byBranch := map[string][]factRecord{}
	baselineByBranch := map[string][]factRecord{}
	proposalsByBranch := map[string][]factProposal{}
	loaded := map[string]bool{}
	// dirtyBranches tracks branches whose in-memory facts/proposals have
	// diverged from disk since the last successful flush, so a flush rewrites
	// only what changed instead of re-marshaling every loaded branch's full
	// store every interval.
	dirtyBranches := map[string]bool{}
	var warnings []string
	chunksScanned, chunksDistilled := 0, 0
	cacheHits, failedChunks, budgetDeferred := 0, 0, 0
	extractionAgentCalls, reconcileAgentCalls := 0, 0
	var preprocessedBytes int64
	var extractionSeconds, reconcileSeconds float64

	// resolveBranch maps a session to the branch its facts are written under,
	// falling back to the manifest default (then distillDefaultBranch) when the
	// session's branch field is empty. The --branch filter must compare against
	// this resolved value, not session.Branch, so sessions with an empty branch
	// are not silently excluded from a `--branch <default>` run.
	resolveBranch := func(session exportSession) string {
		return resolveDistillBranch(manifest, session)
	}
	if err := validateDistillCanonicalInputs(ctx, brainDir, sessions, distillOpts, resolveBranch); err != nil {
		return nil, err
	}

	// Denominator for progress: sessions that pass the branch/session filters.
	// Cached (unchanged) sessions still advance the counter so the line
	// reaches N/N.
	totalSessions := 0
	for _, session := range sessions {
		if distillOpts.branch != "" && resolveBranch(session) != distillOpts.branch {
			continue
		}
		if distillOpts.session != "" && session.SessionID != distillOpts.session {
			continue
		}
		totalSessions++
	}
	// A filter that selects nothing is a typo, not an empty corpus. Left silent
	// it reports zero chunks, zero agent calls and exit 0 — indistinguishable
	// from "everything is already distilled" — while every other misdirected
	// flag on this command errors outright.
	if totalSessions == 0 && len(sessions) > 0 {
		if distillOpts.branch != "" {
			warnings = append(warnings, fmt.Sprintf("--branch %s matched no exported session; nothing was distilled", distillOpts.branch))
		}
		if distillOpts.session != "" {
			warnings = append(warnings, fmt.Sprintf("--session %s matched no exported session; nothing was distilled", distillOpts.session))
		}
	}
	sessionsDone, factsFound := 0, 0
	// Track agent-call outcomes so a misconfiguration that fails every call (e.g.
	// an invalid --model) aborts fast with the agent's own error, instead of
	// silently churning through every session reporting "0 facts found".
	agentFailures, anyAgentSuccess := 0, false
	// The cause of the LAST failure, so the end-of-run abort can name it. The
	// fail-fast threshold already reports it; a corpus smaller than that
	// threshold reached the end-of-run abort instead and lost it entirely —
	// "check --agent, --model, authentication, and executable PATH" when the
	// answer was "agent timed out after 10m0s".
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
		baselineByBranch[branch] = cloneDistillFacts(existing)
		if distillOpts.force {
			kept := existing[:0]
			for _, record := range existing {
				// A force rebuild drops the distilled facts it is about to
				// regenerate — but never a RETIRED one. A retracted or superseded
				// record is a tombstone for a decision the rebuild does not replay
				// (`facts retract`, an applied review proposal, a promote, a synced
				// settlement); dropping it lets the very next chunk re-add the same
				// statement as ACTIVE and silently resurrect a fact somebody
				// declared false. Retaining it costs nothing: the regenerated
				// candidate has the same content-derived id, so upsertFact lands on
				// the tombstone and leaves its status alone, and a supersede the
				// rebuild does replay re-marks it identically.
				if record.Origin != factOriginDistilled || record.Status != factStatusActive {
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
	flushFactStoresLocked := func(final bool) error {
		priorByBranch := make(map[string][]factProposal, len(dirtyBranches))
		for branch := range dirtyBranches {
			prior, err := loadFactProposals(brainDir, branch)
			if err != nil {
				return fmt.Errorf("load proposals for %s: %w", branch, err)
			}
			priorByBranch[branch] = prior
		}
		for branch := range dirtyBranches {
			// Rebase onto the current on-disk set before writing. The in-memory
			// set was loaded when the branch was first seen, which for a run that
			// is hours of agent calls is long before this flush; writing it back
			// wholesale silently reverts everything another writer committed in
			// the meantime — a `remember`, a `facts retract`, a review the user
			// applied, a settlement `facts sync` mirrored back. Those writers hold
			// the same brain write lock this flush does, so the lock alone cannot
			// see them: the run's read-modify-write spans the whole run.
			rebased, rebaseErr := rebaseFactsOntoDisk(brainDir, branch, byBranch[branch], baselineByBranch[branch], distillOpts.force)
			if rebaseErr != nil {
				return rebaseErr
			}
			byBranch[branch] = rebased
			if err := writeFacts(brainDir, branch, byBranch[branch]); err != nil {
				return err
			}
			baselineByBranch[branch] = cloneDistillFacts(byBranch[branch])
			// Union this run's proposals with any already queued. Re-loading on
			// every flush is idempotent: earlier flushes' proposals come back as
			// priors and dedupe away. A --force rebuild starts fresh (it rebuilds
			// the distilled facts), but drops the prior backlog only on the FINAL
			// flush — a mid-run flush that skipped priors would wipe the review
			// queue ~50 calls in, and a kill there would lose it before the
			// rebuild produced its replacement.
			//
			// A force run may only drop the proposals it OWNS. The queue is shared:
			// distill's single-user backlog carries no ProposedBy, while cross-member
			// conflicts raised by `facts sync` are member-attributed — and distill
			// never regenerates those, so dropping them silently deleted conflicts
			// nobody had reviewed. Retain the attributed ones across a force rebuild.
			var prior []factProposal
			loadedProposals := priorByBranch[branch]
			if distillOpts.force && final {
				for _, p := range loadedProposals {
					if strings.TrimSpace(p.ProposedBy) != "" {
						prior = append(prior, p)
					}
				}
			} else {
				prior = loadedProposals
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
		return saveDistillCache(brainDir, cache)
	}
	flushFactStores := func(final bool) error {
		return withBrainWriteLock(brainDir, func() error {
			return flushFactStoresLocked(final)
		})
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

	// Session pipeline: a producer goroutine prepares sessions ahead —
	// transcript read, preprocessing, fingerprint/cache check, chunking — and
	// dispatches chunk agent calls into one pool shared ACROSS sessions (see
	// startSessionPrefetch), while this loop consumes results strictly in
	// chronological order, so reconcile and fact application keep their exact
	// serial semantics. Canceling pipeCtx (deferred; covers the abort return
	// too) stops preparation, dispatch, and in-flight agent calls.
	pipeCtx, pipeCancel := context.WithCancel(ctx)
	defer pipeCancel()
	for ps := range startSessionPrefetch(pipeCtx, brainDir, repoDir, args, sessions, prevCache, distillOpts, resolveBranch) {
		session, branch := ps.session, ps.branch
		if ps.skip || ps.deferred {
			if ps.deferred {
				budgetDeferred++
				sessionsDone++
				if distillOpts.progress != nil {
					distillOpts.progress(distillProgress{SessionsDone: sessionsDone, SessionsTotal: totalSessions, Branch: branch, Facts: factsFound})
				}
			}
			// Carry the session's cache entry through unchanged: the final flush
			// persists newCache only, so dropping filtered sessions here would
			// make the next unfiltered run re-distill every other branch (or
			// session) from scratch. (The fingerprint-match skip below does the
			// same.) A --max-sessions deferral carries nothing when the session
			// was never distilled, which is exactly right: it stays uncached and
			// the next run picks it up.
			//
			// EXCEPT under --force. A forced pass drops every distilled fact on
			// each branch it visits (see ensureBranch), and a session deferred by
			// --max-sessions `continue`s before ensureBranch — so its facts can
			// already have been deleted by a SIBLING session on the same branch
			// while its fingerprint says "distilled". Carrying that entry marks
			// the session done forever: its facts are gone, no later run will
			// re-derive them, and `status` counts it as distilled because it
			// counts cache keys. --session + --force is rejected outright for the
			// same reason; this pair needed the same guard. Dropping the entry
			// costs at most one re-distill of a session whose facts survived.
			if distillOpts.force && ps.deferred {
				continue
			}
			key := distillSessionCacheKeyFor(branch, session, duplicateKeys)
			if prev, ok := prevCache.Sessions[key]; ok {
				newCache.Sessions[key] = prev
			} else if prev, ok := prevCache.Sessions[session.SessionID]; ok {
				// A legacy entry stays under its legacy key (and formula) until
				// its session is actually visited and grandfathered — copying it
				// under the new key would make it permanently unmatchable.
				newCache.Sessions[session.SessionID] = prev
			}
			continue
		}
		if ps.readErr != nil {
			pipeCancel()
			return nil, fmt.Errorf("read canonical transcript %s: %w", filepath.ToSlash(session.TranscriptPath), ps.readErr)
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
		preprocessedBytes += int64(ps.preBytes)
		if ps.cached {
			cacheHits++
			newCache.Sessions[distillSessionCacheKeyFor(branch, session, duplicateKeys)] = ps.fingerprint // unchanged; retain in cache and keep existing facts
			reportProgress()
			continue
		}

		sessionFailed := false
		for chunkIdx, chunk := range ps.chunks {
			chunksScanned++
			anchor := factAnchor{
				SessionID:    session.SessionID,
				CheckpointID: session.LatestCheckpoint,
				Transcript:   filepath.ToSlash(session.TranscriptPath),
				Line:         chunk.StartLine,
			}
			extractionStarted := time.Now()
			out, runErr := ps.result(chunkIdx)
			extractionSeconds += time.Since(extractionStarted).Seconds()
			extractionAgentCalls++
			callsSinceFlush++
			if runErr != nil {
				warnings = append(warnings, fmt.Sprintf("agent failed on %s:%d: %v", session.SessionID, chunk.StartLine, runErr))
				lastAgentErr = runErr
				sessionFailed = true
				failedChunks++
				agentFailures++
				// Fail fast on a misconfiguration: nothing has distilled yet and the
				// agent keeps failing, so every call is almost certainly erroring the
				// same way (bad --model, missing agent, auth). Abort with the agent's
				// own error instead of churning through every remaining session.
				if !anyAgentSuccess && agentFailures >= distillAgentAbortThreshold {
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
			// Sharpen provenance where the entity index can place the fact's
			// locus; a no-op (and never an error) when the index is absent.
			records = applyEntityProvenance(records, distillOpts.entityProvenance)
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
			reconcileCalled := false
			countingReconcileRun := func(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
				reconcileCalled = true
				return distillOpts.run(ctx, dir, args, input, timeout)
			}
			reconcileStarted := time.Now()
			actions, recWarnings := reconcileChunkCandidates(ctx, countingReconcileRun, reconcileArgs, records, byBranch[branch], repoDir, distillOpts.timeout)
			if reconcileCalled {
				reconcileAgentCalls++
				reconcileSeconds += time.Since(reconcileStarted).Seconds()
			}
			warnings = append(warnings, recWarnings...)
			callsSinceFlush++ // reconcile is an agent call too
			var chunkProposals []factProposal
			for _, action := range actions {
				actionThreshold := threshold
				if distillOpts.newestFirst && action.Kind == "supersede" {
					// Extraction order is not evidence that a candidate is newer.
					// In reverse-order backfill, preserve both facts for review.
					actionThreshold = 2 // above every valid agent confidence
					warnings = append(warnings, "newest-first supersession requires review; both facts retained (run facts review)")
				}
				var pending []factProposal
				byBranch[branch], pending = applyFactActions(byBranch[branch], []factAction{action}, actionThreshold, now)
				chunkProposals = append(chunkProposals, pending...)
			}
			proposalsByBranch[branch] = append(proposalsByBranch[branch], chunkProposals...)
			dirtyBranches[branch] = true
			maybeFlush()
		}
		if ps.release != nil {
			ps.release() // free the work-lookahead slot: chunks fully consumed
		}
		// Only cache a session as distilled when every chunk succeeded, so a
		// session whose agent calls failed is retried on the next run rather
		// than being silently treated as done.
		if !sessionFailed {
			newCache.Sessions[distillSessionCacheKeyFor(branch, session, duplicateKeys)] = ps.fingerprint
		}
		reportProgress()
	}
	if agentFailures > 0 && !anyAgentSuccess {
		return nil, fmt.Errorf(
			"distill aborted after all %d agent calls failed with no successful response; check --agent, --model, authentication, and executable PATH. Last error: %v",
			agentFailures, lastAgentErr,
		)
	}

	if budgetDeferred > 0 {
		// Prepended so the budget notice survives the warning cap: it is the
		// one line that explains why the run stopped short of the corpus.
		warnings = append([]string{fmt.Sprintf(
			"--max-sessions %d reached: %d session(s) deferred to a later run (they stay undistilled, not skipped)",
			distillOpts.maxSessions, budgetDeferred)}, warnings...)
	}
	// The 50-entry DISPLAY cap must not run here: `warnings` goes straight into
	// the manifest below, and truncating now would delete the only copy of
	// everything past it. The display cap lives at the print site
	// (distillWarningsForDisplay).
	//
	// A much higher persist bound does apply, so a pathological run cannot grow
	// the manifest without limit; it keeps the exact total either way.
	warnings = distillWarningsForPersist(warnings)

	writeStarted := time.Now()
	var source *factSourceManifest
	err = withBrainWriteLock(brainDir, func() error {
		manifest, err := loadBrainManifest(brainDir)
		if err != nil {
			return err
		}
		if err := flushFactStoresLocked(true); err != nil {
			return err
		}
		if err := writeFactTaxonomy(brainDir, taxonomy); err != nil {
			return err
		}

		// Summarize the manifest from the whole on-disk store, not just the branches
		// touched this run. A branch-limited (`--branch`) or incremental run only
		// loads a subset into byBranch; folding that subset into the manifest would
		// drop counts/branches/proposals for facts that still exist on other
		// branches. Re-reading the store after writing keeps sources.facts whole.
		allBranches, err := loadAllFactBranches(brainDir)
		if err != nil {
			return err
		}
		branchNames := make([]string, 0, len(allBranches))
		for branch := range allBranches {
			branchNames = append(branchNames, branch)
		}
		totalProposals := countFactProposals(brainDir, branchNames)

		source = summarizeFactSource(now, allBranches, chunksScanned, chunksDistilled, totalProposals, warnings)
		source.LastDistilledAt = now
		source.CacheHits = cacheHits
		source.FailedChunks = failedChunks
		source.PreprocessedBytes = preprocessedBytes
		source.Agent = strings.TrimSpace(distillOpts.agent)
		source.Model = strings.TrimSpace(distillOpts.model)
		source.Effort = strings.TrimSpace(distillOpts.effort)
		source.Branch = strings.TrimSpace(distillOpts.branch)
		source.Force = distillOpts.force
		source.Jobs = distillRequestedJobs(distillOpts)
		source.ExtractionJobsCap = distillEffectiveExtractionJobs(extractionAgentCalls, distillOpts)
		source.MaxChunkBytes = distillMaxChunkBytes(distillOpts)
		source.Confidence = threshold
		source.ExtractionCalls = extractionAgentCalls
		source.ReconcileCalls = reconcileAgentCalls
		source.TotalAgentCalls = extractionAgentCalls + reconcileAgentCalls
		source.TokenUsage = usageCollector.summary(source.TotalAgentCalls)
		source.ExtractionWaitSeconds = extractionSeconds
		source.ReconcileSeconds = reconcileSeconds
		source.WriteSeconds = time.Since(writeStarted).Seconds()
		source.TotalSeconds = time.Since(runStarted).Seconds()
		if manifest.Sources == nil {
			manifest.Sources = &brainSources{}
		}
		manifest.Sources.Facts = source
		if manifest.GeneratedAt.IsZero() {
			manifest.GeneratedAt = now
		}
		return writeBrainManifestAndReadme(brainDir, *manifest)
	})
	if err != nil {
		return nil, err
	}
	if distillOpts.onDeferredSessions != nil {
		distillOpts.onDeferredSessions(budgetDeferred)
	}
	return source, nil
}

func buildDistillDryRunReport(brainDir string, distillOpts distillCommandOptions, now time.Time) (distillDryRunReport, error) {
	return buildDistillDryRunReportContext(context.Background(), brainDir, distillOpts, now)
}

func buildDistillDryRunReportContext(ctx context.Context, brainDir string, distillOpts distillCommandOptions, now time.Time) (distillDryRunReport, error) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil {
		return distillDryRunReport{}, err
	}
	if manifest.Sources == nil || manifest.Sources.Sessions == nil || len(manifest.Sources.Sessions.Sessions) == 0 {
		return distillDryRunReport{}, errors.New("no exported sessions; run `entire brain refresh` first")
	}
	distillOpts.dryRun = true
	cacheSalt, err := distillCacheSaltForBrain(brainDir, distillOpts, now)
	if err != nil {
		return distillDryRunReport{}, err
	}
	plan, err := buildDistillPlanContext(ctx, brainDir, manifest, distillOpts, cacheSalt)
	if err != nil {
		return distillDryRunReport{}, err
	}
	report := distillDryRunReport{
		SchemaVersion:                 1,
		GeneratedAt:                   now,
		BrainPath:                     brainDir,
		Branch:                        strings.TrimSpace(distillOpts.branch),
		Force:                         distillOpts.force,
		Agent:                         strings.TrimSpace(distillOpts.agent),
		Model:                         strings.TrimSpace(distillOpts.model),
		Effort:                        strings.TrimSpace(distillOpts.effort),
		Jobs:                          distillRequestedJobs(distillOpts),
		ExtractionJobsCap:             distillEffectiveExtractionJobs(len(plan.Work), distillOpts),
		MaxChunkBytes:                 distillMaxChunkBytes(distillOpts),
		Confidence:                    distillConfidenceThreshold(distillOpts),
		Sessions:                      plan.TotalSessions,
		CachedSessions:                plan.CachedSessions,
		SessionsToDistill:             plan.SessionsToDistill,
		MissingTranscripts:            plan.MissingTranscripts,
		RawBytes:                      plan.RawBytes,
		PreprocessedBytes:             plan.PreprocessedBytes,
		Chunks:                        plan.Chunks,
		ChunksIfUncached:              plan.ChunksIfUncached,
		ExtractionAgentCalls:          plan.Chunks,
		ReconcileAgentCallsUpperBound: plan.Chunks,
		EstimatedAgentCallsUpperBound: plan.Chunks * 2,
		// Whole, not capped: this report IS the artifact (`--json` emits it verbatim).
		// printDistillDryRunReport caps the terminal rendering.
		Warnings: append([]string(nil), plan.Warnings...),
	}
	branchStats := map[string]*distillDryRunBranch{}
	for _, branch := range plan.BranchOrder {
		branchStats[branch] = &distillDryRunBranch{Branch: branch}
	}
	for _, session := range plan.Sessions {
		stat := branchStats[session.Branch]
		stat.Sessions++
		stat.PreprocessedBytes += int64(session.PreprocessedBytes)
		stat.ChunksIfUncached += session.ChunksIfUncached
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
			ChunksIfUncached:  session.ChunksIfUncached,
			RawBytes:          session.RawBytes,
			PreprocessedBytes: session.PreprocessedBytes,
		})
	}
	for _, branch := range plan.BranchOrder {
		report.Branches = append(report.Branches, *branchStats[branch])
	}
	sort.Slice(report.LargestSessions, func(i, j int) bool {
		if report.LargestSessions[i].ChunksIfUncached != report.LargestSessions[j].ChunksIfUncached {
			return report.LargestSessions[i].ChunksIfUncached > report.LargestSessions[j].ChunksIfUncached
		}
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
	if report.ChunksIfUncached != report.Chunks {
		fmt.Fprintf(out, "chunks if uncached: %d\n", report.ChunksIfUncached)
	}
	fmt.Fprintf(out, "estimated agent calls: %d extraction + up to %d reconcile = up to %d total\n",
		report.ExtractionAgentCalls, report.ReconcileAgentCallsUpperBound, report.EstimatedAgentCallsUpperBound)
	fmt.Fprintf(out, "bytes: %d raw, %d preprocessed\n", report.RawBytes, report.PreprocessedBytes)
	for _, branch := range report.Branches {
		fmt.Fprintf(out, "branch %s: %d sessions, %d cached, %d chunks\n",
			branch.Branch, branch.Sessions, branch.CachedSessions, branch.Chunks)
		if branch.ChunksIfUncached != branch.Chunks {
			fmt.Fprintf(out, "branch %s chunks if uncached: %d\n", branch.Branch, branch.ChunksIfUncached)
		}
	}
	if len(report.LargestSessions) > 0 {
		fmt.Fprintln(out, "largest sessions:")
		for _, session := range report.LargestSessions {
			cached := ""
			if session.Cached {
				cached = " cached"
			}
			chunkText := fmt.Sprintf("%d chunks", session.Chunks)
			if session.ChunksIfUncached != session.Chunks {
				chunkText = fmt.Sprintf("%d chunks scheduled, %d if uncached", session.Chunks, session.ChunksIfUncached)
			}
			fmt.Fprintf(out, "- %s %s: %s%s, %d preprocessed bytes, %d raw bytes\n",
				session.Branch, session.Transcript, chunkText, cached, session.PreprocessedBytes, session.RawBytes)
		}
	}
	for _, warning := range distillWarningsForDisplay(report.Warnings) {
		fmt.Fprintf(out, "warning: %s\n", warning)
	}
}

func distillRequestedJobs(distillOpts distillCommandOptions) int {
	if distillOpts.jobs > 0 {
		return distillOpts.jobs
	}
	if distillOpts.concurrency > 0 {
		return distillOpts.concurrency
	}
	return 1
}

func distillEffectiveExtractionJobs(work int, distillOpts distillCommandOptions) int {
	if work <= 0 {
		return 0
	}
	jobs := distillRequestedJobs(distillOpts)
	if jobs > work {
		return work
	}
	return jobs
}

func distillMaxChunkBytes(distillOpts distillCommandOptions) int {
	if distillOpts.maxChunkBytes > 0 {
		return distillOpts.maxChunkBytes
	}
	return defaultDistillChunkSize
}

func distillConfidenceThreshold(distillOpts distillCommandOptions) float64 {
	if distillOpts.confidenceThreshold > 0 {
		return distillOpts.confidenceThreshold
	}
	return defaultFactConfidenceThreshold
}

func buildDistillPlan(brainDir string, manifest *exportManifest, distillOpts distillCommandOptions, cacheSalt string) (distillPlan, error) {
	return buildDistillPlanContext(context.Background(), brainDir, manifest, distillOpts, cacheSalt)
}

func buildDistillPlanContext(ctx context.Context, brainDir string, manifest *exportManifest, distillOpts distillCommandOptions, cacheSalt string) (distillPlan, error) {
	if distillOpts.maxChunkBytes <= 0 {
		distillOpts.maxChunkBytes = defaultDistillChunkSize
	}
	sessions := append([]exportSession(nil), manifest.Sources.Sessions.Sessions...)
	// Excluded sessions must never produce new derived facts.
	sessions, err := filterTombstonedSessions(brainDir, sessions)
	if err != nil {
		return distillPlan{}, err
	}
	sortDistillSessions(sessions, distillOpts.newestFirst)
	prevCache := loadDistillCache(brainDir)
	// Same disambiguation the run uses, so the plan a --dry-run prints is the
	// plan the run executes. Computed over the UNFILTERED session list for the
	// same reason the run does: a --branch/--session filter must not change
	// which keys collide.
	duplicateKeys := distillDuplicateSessionKeys(sessions, func(session exportSession) string {
		return resolveDistillBranch(manifest, session)
	})
	branchSeen := map[string]struct{}{}
	plan := distillPlan{CacheSalt: cacheSalt}
	for _, session := range sessions {
		branch := resolveDistillBranch(manifest, session)
		if distillOpts.branch != "" && branch != distillOpts.branch {
			continue
		}
		if distillOpts.session != "" && session.SessionID != distillOpts.session {
			continue
		}
		if _, ok := branchSeen[branch]; !ok {
			branchSeen[branch] = struct{}{}
			plan.BranchOrder = append(plan.BranchOrder, branch)
		}
		sessionPlan := distillSessionPlan{Session: session, Branch: branch}
		plan.TotalSessions++

		data, readErr := readCanonicalHistoryTranscript(ctx, brainDir, session.TranscriptPath)
		if readErr != nil {
			return distillPlan{}, fmt.Errorf("read canonical transcript %s: %w", filepath.ToSlash(session.TranscriptPath), readErr)
		}
		content := string(data)
		distillInput := preprocessTranscriptForDistill(content)
		sessionPlan.RawBytes = len(content)
		sessionPlan.PreprocessedBytes = len(distillInput)
		plan.RawBytes += int64(sessionPlan.RawBytes)
		plan.PreprocessedBytes += int64(sessionPlan.PreprocessedBytes)
		sessionPlan.Fingerprint = distillSessionFingerprint(session, branch, distillInput, plan.CacheSalt)
		chunks := chunkTranscript(distillInput, distillOpts.maxChunkBytes)
		sessionPlan.ChunksIfUncached = len(chunks)
		plan.ChunksIfUncached += sessionPlan.ChunksIfUncached
		if !distillOpts.force {
			legacyKey := distillSessionCacheKey(branch, session.SessionID)
			prev, ok := cachedDistillFingerprintForKey(prevCache, distillSessionCacheKeyFor(branch, session, duplicateKeys))
			if (ok && prev == sessionPlan.Fingerprint) ||
				(!duplicateKeys[legacyKey] && grandfatheredDistillFingerprint(prevCache, session, branch, distillInput)) {
				sessionPlan.Cached = true
				plan.CachedSessions++
				plan.Sessions = append(plan.Sessions, sessionPlan)
				continue
			}
		}
		// Budget: once the cap is reached, remaining sessions are planned but
		// carry no work. They stay uncached, so the next run (or the watcher)
		// distills them — the cap defers spend, it never drops a session.
		if distillOpts.maxSessions > 0 && plan.SessionsToDistill >= distillOpts.maxSessions {
			sessionPlan.BudgetDeferred = true
			plan.BudgetDeferredSessions++
			plan.Sessions = append(plan.Sessions, sessionPlan)
			continue
		}
		plan.SessionsToDistill++
		for chunkIndex := range chunks {
			workIndex := len(plan.Work)
			sessionPlan.ChunkIndexes = append(sessionPlan.ChunkIndexes, workIndex)
			plan.Work = append(plan.Work, distillChunkWork{
				SessionIndex: len(plan.Sessions),
				ChunkIndex:   chunkIndex,
				Branch:       branch,
			})
		}
		plan.Chunks += len(chunks)
		plan.Sessions = append(plan.Sessions, sessionPlan)
	}
	sort.Strings(plan.BranchOrder)
	return plan, nil
}

// sortDistillSessions orders the corpus for one distill pass. Oldest-first is
// the default and the established order (a later session's fact supersedes an
// earlier one's naturally). Newest-first is the backfill order: the same set of
// sessions, visited so the most recent — and most useful — facts land first.
// Both are total and stable (session id breaks CreatedAt ties) so a plan is
// reproducible.
func sortDistillSessions(sessions []exportSession, newestFirst bool) {
	sort.SliceStable(sessions, func(i, j int) bool {
		left, right := sessions[i], sessions[j]
		if !left.CreatedAt.Equal(right.CreatedAt) {
			if newestFirst {
				return right.CreatedAt.Before(left.CreatedAt)
			}
			return left.CreatedAt.Before(right.CreatedAt)
		}
		if newestFirst {
			return right.SessionID < left.SessionID
		}
		return left.SessionID < right.SessionID
	})
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
	// ToolOutputs carries each tool part's state.output verbatim. The JSONL
	// path mines tool results for durable code facts; without this field the
	// document path silently dropped the same evidence, so a flag default or a
	// constant echoed by a command was unindexable from document-form sessions.
	ToolOutputs []string
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
					Type  string `json:"type"`
					Text  string `json:"text"`
					State struct {
						Output string `json:"output"`
					} `json:"state"`
				} `json:"parts"`
			}
			if err := dec.Decode(&msg); err != nil {
				return nil, false
			}
			var words []string
			var toolOutputs []string
			for _, part := range msg.Parts {
				if part.Type == "tool" && strings.TrimSpace(part.State.Output) != "" {
					toolOutputs = append(toolOutputs, part.State.Output)
					continue
				}
				if part.Type != "text" || part.Text == "" {
					continue // patch, reasoning, step-start/finish, file, ...
				}
				words = append(words, strings.Fields(part.Text)...)
			}
			messages = append(messages, documentMessage{
				Role:        msg.Info.Role,
				Text:        strings.Join(words, " "),
				Line:        msgLine,
				ToolOutputs: toolOutputs,
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
		// Two shapes carry the same record type. The claude-code dialect nests
		// the turn under "message"; the dialect the CLI writes today
		// ({"v":1,"agent":"codex",...,"content":[...]}) puts the content blocks
		// at the RECORD's top level with no wrapper at all. Reading only the
		// nested one stripped every record of a modern transcript to "" — the
		// same silent total loss the "message" case below documents for pi, but
		// on the dialect that is now the default (observed: 1.7 MB of real Codex
		// sessions preprocessed to 1.6 KB and distilled zero facts).
		if message, ok := obj["message"]; ok {
			return distillTextBlocks(jsonMap(message)["content"])
		}
		return distillTextBlocks(obj["content"])
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
// The transcript path is deliberately NOT part of the fingerprint: it is
// storage layout, not identity (the content digest already covers what the
// agent distills), and hashing it means any layout change — like the
// stable-branch-dir rename this release ships — re-invalidates the entire
// corpus for free. Identity is (session id, checkpoint, resolved branch,
// configuration salt, preprocessed content).
func distillSessionFingerprint(session exportSession, branch, content, cacheSalt string) string {
	contentSum := sha256.Sum256([]byte(content))
	sum := sha256.Sum256([]byte(strings.Join([]string{
		session.SessionID,
		session.LatestCheckpoint,
		branch,
		cacheSalt,
		hex.EncodeToString(contentSum[:]),
	}, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// legacyDistillSessionFingerprint reproduces the pre-salt formula EXACTLY —
// including the transcript path it hashed and the absence of the salt — so a
// pre-upgrade cache entry can prove a session unchanged. Without this, the
// first run after upgrading re-distills the whole corpus (hours of agent
// calls) even though nothing about the sessions changed.
func legacyDistillSessionFingerprint(session exportSession, branch, transcriptPath, content string) string {
	contentSum := sha256.Sum256([]byte(content))
	sum := sha256.Sum256([]byte(strings.Join([]string{
		session.SessionID,
		session.LatestCheckpoint,
		filepath.ToSlash(transcriptPath),
		branch,
		hex.EncodeToString(contentSum[:]),
	}, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// legacyBranchDirSuffixPattern matches the "-<sha8>" suffix
// stableBranchDirComponent appends to branch directory components.
var legacyBranchDirSuffixPattern = regexp.MustCompile(`^(.+)-[0-9a-f]{8}$`)

// legacyTranscriptPaths returns the candidate paths a pre-upgrade export may
// have recorded for this session: the current path as-is, plus the pre-rename
// layout with the stable "-<sha8>" suffix stripped from the directory
// component. (Pre-rename collision-numbered directories are not derivable
// from the new layout; sessions in them miss the grandfather and re-distill —
// rare and safe.)
func legacyTranscriptPaths(transcriptPath string) []string {
	current := filepath.ToSlash(transcriptPath)
	out := []string{current}
	dir := path.Dir(current)
	if m := legacyBranchDirSuffixPattern.FindStringSubmatch(path.Base(dir)); m != nil {
		out = append(out, path.Join(path.Dir(dir), m[1], path.Base(current)))
	}
	return out
}

// grandfatheredDistillFingerprint reports whether a PRE-UPGRADE cache entry
// (legacy session-id key, legacy formula) proves this session unchanged. The
// caller then treats the session as cached and rewrites the entry under the
// new key/format — lazy migration, zero agent calls. Grandfathered entries
// reflect whatever configuration produced them (exactly the pre-salt
// semantics, so no regression); the salt governs all NEW entries going
// forward.
func grandfatheredDistillFingerprint(cache distillCache, session exportSession, branch, content string) bool {
	if cache.Sessions == nil {
		return false
	}
	legacy, ok := cache.Sessions[session.SessionID]
	if !ok {
		return false
	}
	for _, transcriptPath := range legacyTranscriptPaths(session.TranscriptPath) {
		if legacyDistillSessionFingerprint(session, branch, transcriptPath, content) == legacy {
			return true
		}
	}
	return false
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
	threshold := distillConfidenceThreshold(distillOpts)
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

// distillDuplicateSessionKeys returns the set of branch+session cache keys that
// MORE THAN ONE exported session maps onto. A real `refresh sessions` produces
// exactly that whenever one session was checkpointed under two branches — most
// commonly a branch-bearing entry plus an empty-branch ("sessions/unknown")
// entry, which resolveBranch collapses onto the SAME default branch.
//
// Both entries then shared one cache key with two different fingerprints, so
// each run the second entry overwrote the first's, the next run missed, and one
// extraction plus one reconcile agent call were re-spent on every pass forever
// — real money per watch tick, and the cache never converged.
func distillDuplicateSessionKeys(sessions []exportSession, resolveBranch func(exportSession) string) map[string]bool {
	if len(sessions) < 2 {
		return nil
	}
	seen := make(map[string]int, len(sessions))
	for _, session := range sessions {
		seen[distillSessionCacheKey(resolveBranch(session), session.SessionID)]++
	}
	var duplicates map[string]bool
	for key, count := range seen {
		if count > 1 {
			if duplicates == nil {
				duplicates = make(map[string]bool)
			}
			duplicates[key] = true
		}
	}
	return duplicates
}

// distillSessionCacheKeyFor is the cache key one exported session is stored
// under. It is the plain branch+session key — byte-identical to every key
// already on disk, so no existing brain is invalidated — EXCEPT for the
// sessions distillDuplicateSessionKeys flagged, which get the checkpoint id
// interposed so the colliding entries stay distinct.
//
// The checkpoint goes BEFORE the session id, never after: `privacy purge`
// (purgeDistillCacheEntries) and the post-purge leftover check
// (verifySessionPrivacy) both evict by the "/<escaped session id>" SUFFIX, and
// an id appended to the tail would make a purged session's entry unmatchable.
func distillSessionCacheKeyFor(branch string, session exportSession, duplicates map[string]bool) string {
	key := distillSessionCacheKey(branch, session.SessionID)
	if !duplicates[key] {
		return key
	}
	// The checkpoint is what distinguishes two exports of one session, but it is
	// an optional field; the transcript path is not, and the export writes one
	// file per entry. Falling back to it keeps two checkpoint-less duplicates
	// apart instead of re-colliding them under an empty segment.
	discriminator := session.LatestCheckpoint
	if discriminator == "" {
		discriminator = filepath.ToSlash(session.TranscriptPath)
	}
	return url.PathEscape(branch) + "/" + url.PathEscape(discriminator) + "/" + url.PathEscape(session.SessionID)
}

// cachedDistillSessionFingerprint answers "has this session been distilled on
// this branch", by branch and id alone. It is the progress lookup
// (factsBackfillStatusForBrain): the CALLERS that must match a specific
// exported entry use cachedDistillFingerprintForKey with the key
// distillSessionCacheKeyFor built for them.
//
// A session the export lists twice under one branch lives under
// checkpoint-disambiguated keys, so the exact key misses for BOTH copies. The
// scan below finds them, which is what keeps `setup status` from reporting a
// finished backfill as permanently unfinished. It is ordered so the answer does
// not depend on Go's map iteration order.
func cachedDistillSessionFingerprint(cache distillCache, branch, sessionID string) (string, bool) {
	if fp, ok := cachedDistillFingerprintForKey(cache, distillSessionCacheKey(branch, sessionID)); ok {
		return fp, ok
	}
	prefix := url.PathEscape(branch) + "/"
	suffix := "/" + url.PathEscape(sessionID)
	best := ""
	for key := range cache.Sessions {
		// The length guard keeps the two affixes from overlapping, so a key is
		// only a match when it really carries a discriminator BETWEEN them.
		if len(key) <= len(prefix)+len(suffix) || !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
			continue
		}
		if best == "" || key < best {
			best = key
		}
	}
	if best == "" {
		return "", false
	}
	return cache.Sessions[best], true
}

// cachedDistillFingerprintForKey looks a fingerprint up under an already-built
// cache key, so callers that must use the duplicate-disambiguated key (see
// distillSessionCacheKeyFor) share one lookup with the plain-key callers.
func cachedDistillFingerprintForKey(cache distillCache, key string) (string, bool) {
	if cache.Sessions == nil {
		return "", false
	}
	fp, ok := cache.Sessions[key]
	// No legacy-key fallback here: a legacy-FORMAT fingerprint can never equal
	// a new-format one, so returning it only manufactures false mismatches.
	// Pre-upgrade entries are honored by grandfatheredDistillFingerprint.
	return fp, ok
}

// readBrainRelativeStateFile is for small derived Brain state, never canonical
// session transcripts. It shares the hardened state reader and an explicit
// whole-file bound so derived JSON cannot revive the legacy unbounded read.
func readBrainRelativeStateFile(brainDir, rel string) (string, error) {
	data, present, err := readMemoryStateFile(brainDir, filepath.ToSlash(rel), "derived Brain state", defaultMaxReadBytes)
	if err != nil {
		return "", err
	}
	if !present {
		return "", os.ErrNotExist
	}
	return string(data), nil
}

// loadDistillCache reads the incremental cache, always returning a usable value:
// any read/parse error or version mismatch yields an empty cache so the next
// run simply re-distills everything.
func loadDistillCache(brainDir string) distillCache {
	empty := distillCache{Version: distillCacheVersion, Sessions: map[string]string{}}
	data, _, err := readMemoryStateFile(brainDir, distillCachePath, "distill cache", defaultMaxReadBytes)
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
func saveDistillCache(brainDir string, cache distillCache) error {
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return writeBrainRelativeFileAtomic(brainDir, distillCachePath, append(data, '\n'), 0o600)
}

// execDistillAgent is the real distillAgentRunner: it runs the agent with the
// transcript chunk on stdin and returns stdout, bounded by a timeout and an
// output-size cap.
func defaultDistillAgentRunner(agent string) distillAgentRunner {
	if agent == "ollama" {
		return execOllamaDistillAgent
	}
	if agent == "command" {
		return execCustomDistillAgent
	}
	return execDistillAgent
}

// prepareAgentExec turns the builder's slice into the argv actually handed to
// CreateProcess/execve plus the bytes the agent should read on stdin, keeping
// the ~7.5 KiB system prompt off the command line (issue #322 — cmd.exe caps a
// command line at 8191 characters and the npm `claude.cmd`/`codex.cmd` shims
// route every exec through it).
//
// It returns a cleanup that the caller MUST defer before any other error
// return, because the claude channel stages the prompt in a temp file. The file
// is written 0600 inside a 0700 MkdirTemp directory, so the prompt never lands
// in a world-readable location, and cleanup removes the directory on every exit
// path including failure and timeout.
//
// Slices with no marker (the `command` agent, which was never given the prompt,
// and the handcrafted argv used by tests) pass through untouched.
func prepareAgentExec(args []string, input []byte) (argv []string, stdin []byte, cleanup func(), err error) {
	noop := func() {}
	argv, prompt, ok := splitAgentPromptArg(args)
	if !ok {
		return args, input, noop, nil
	}
	if len(argv) == 0 {
		return nil, nil, noop, errors.New("distill: agent command carries a prompt but no executable")
	}
	switch strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe") {
	case "claude":
		// claude reads its system prompt from a file with --system-prompt-file,
		// which keeps the system/user split intact (folding the instructions into
		// the stdin user message would change behaviour on every platform, not
		// just Windows).
		dir, mkErr := os.MkdirTemp("", "entire-brain-prompt-")
		if mkErr != nil {
			return nil, nil, noop, fmt.Errorf("distill: stage system prompt: %w", mkErr)
		}
		cleanup = func() { _ = os.RemoveAll(dir) }
		path := filepath.Join(dir, "system-prompt.md")
		if wErr := os.WriteFile(path, []byte(prompt), 0o600); wErr != nil {
			cleanup()
			return nil, nil, noop, fmt.Errorf("distill: stage system prompt: %w", wErr)
		}
		return append(argv, "--system-prompt-file", path), input, cleanup, nil
	case "codex":
		// `codex exec -` reads its instructions from stdin. codex itself appends
		// piped stdin to a positional prompt as a <stdin> block, so composing the
		// same shape here keeps the transcript distinguishable from the
		// instructions while the instructions leave argv.
		var composed bytes.Buffer
		composed.WriteString(prompt)
		if len(input) > 0 {
			composed.WriteString("\n\n<stdin>\n")
			composed.Write(input)
			composed.WriteString("\n</stdin>\n")
		}
		return append(argv, "-"), composed.Bytes(), noop, nil
	default:
		return nil, nil, noop, fmt.Errorf("distill: no off-argv prompt channel for agent %q", argv[0])
	}
}

func execDistillAgent(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
	return execDistillAgentWithPrompt(ctx, dir, args, input, timeout, true)
}

func execCustomDistillAgent(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration) (string, error) {
	return execDistillAgentWithPrompt(ctx, dir, args, input, timeout, false)
}

func execDistillAgentWithPrompt(ctx context.Context, dir string, args []string, input []byte, timeout time.Duration, carriesPrompt bool) (string, error) {
	if len(args) == 0 {
		return "", errors.New("distill: empty agent command")
	}
	argv, stdin, cleanup := args, input, func() {}
	var err error
	// Custom commands own their complete argv. Only built-in agent builders
	// encode a system prompt behind the private marker.
	if carriesPrompt {
		argv, stdin, cleanup, err = prepareAgentExec(args, input)
		if err != nil {
			return "", err
		}
	}
	defer cleanup()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	command.Dir = dir
	command.Stdin = bytes.NewReader(stdin)
	stdoutLimit := distillStdoutLimit(argv)
	stdout := newCappedDistillBuffer(stdoutLimit)
	stderr := newCappedDistillBuffer(distillMaxOutputBytes)
	command.Stdout = &stdout
	command.Stderr = &stderr
	// CommandContext kills the agent process itself, but Run also waits for the
	// stdout/stderr copying goroutines, and every real agent CLI (codex exec,
	// claude, a --agent-command wrapper) spawns CHILDREN that inherit those
	// pipes. Killing the agent does not close them, so without a WaitDelay the
	// timeout is not a bound at all: Run blocks until the orphaned grandchild
	// exits on its own. Measured: a 150ms deadline on `sh -c "sleep 30; ..."`
	// returned after 30s. WaitDelay closes the descriptors shortly after the
	// kill and lets Wait return, so `timeout` is the real ceiling and a --jobs N
	// pool cannot be wedged by one hung call.
	command.WaitDelay = distillAgentWaitDelay
	err = command.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("agent timed out after %s", timeout)
	}
	rawOutput := stdout.String()
	decodedOutput := rawOutput
	var decodeErr error
	if !stdout.Exceeded() {
		var usage distillProviderUsage
		decodedOutput, usage, decodeErr = decodeStructuredDistillOutput(argv, rawOutput)
		// Provider usage is the cost of the attempted call, even when the CLI
		// reports a logical error that makes the extraction itself fail.
		recordDistillProviderUsage(ctx, usage)
	}
	if err != nil {
		// The agent's own error first: reporting a byte-cap instead of the
		// real failure buries the actionable message. The captured (possibly
		// truncated) stderr still rides along as context.
		warning := strings.TrimSpace(stderr.String())
		if warning == "" {
			warning = strings.TrimSpace(rawOutput)
		}
		return "", fmt.Errorf("agent failed: %w: %s", err, truncateAgentWarning(warning))
	}
	if stdout.Exceeded() {
		// Truncated STDOUT on success is still an error: the structured envelope
		// may have lost either the final fact text or its usage event.
		return "", fmt.Errorf("agent output exceeds %d bytes", stdoutLimit)
	}
	if decodeErr != nil {
		return "", fmt.Errorf("distill: %w", decodeErr)
	}
	if len(decodedOutput) > distillMaxOutputBytes {
		return "", fmt.Errorf("agent result exceeds %d bytes", distillMaxOutputBytes)
	}
	// Chatty stderr on a SUCCESSFUL run is diagnostics, not failure — the
	// capped buffer already bounds memory; failing the call would turn a
	// verbose-but-correct agent into a fake outage.
	return decodedOutput, nil
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
	var tr *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = dt.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.Proxy = nil
	tr.DialContext = loopbackOnlyDialContext
	tr.DialTLS = nil
	tr.DialTLSContext = nil
	client := &http.Client{
		Timeout:   timeout,
		Transport: tr,
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
	// /api/generate echoes a `context` token array proportional to the prompt we
	// sent, so the JSON envelope is several times larger than the generated text.
	// Bounding the envelope by distillMaxOutputBytes therefore rejects responses on
	// prompt size rather than output size: a 48 KiB chunk produces a ~123 KiB
	// envelope even when the model answers in two bytes, so chunks much past that
	// fail with an error that blames output the model never produced. Scale the
	// envelope allowance off the request we control, and keep
	// distillMaxOutputBytes as the bound on the text we actually consume.
	envelopeLimit := min(int64(distillMaxOutputBytes)+int64(len(body))*distillOllamaEnvelopeFactor+distillOllamaEnvelopeSlack,
		int64(distillOllamaMaxEnvelopeBytes))
	data, err := io.ReadAll(io.LimitReader(resp.Body, envelopeLimit+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > envelopeLimit {
		return "", fmt.Errorf("ollama response envelope exceeds %d bytes", envelopeLimit)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("ollama returned HTTP %d: %s", resp.StatusCode, truncateAgentWarning(string(data)))
	}
	var parsed struct {
		Response        string `json:"response"`
		Error           string `json:"error"`
		PromptEvalCount *int64 `json:"prompt_eval_count"`
		EvalCount       *int64 `json:"eval_count"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("parse ollama response: %w", err)
	}
	if (parsed.PromptEvalCount != nil && *parsed.PromptEvalCount < 0) || (parsed.EvalCount != nil && *parsed.EvalCount < 0) {
		return "", errors.New("ollama returned negative token counts")
	}
	// Recorded before the checks below, not after. Each of them fails a call the
	// model has already been paid for, and truncation is the costliest of all —
	// it burns a full context window. Recording after them dropped exactly the
	// most expensive calls from accounting, while TotalAgentCalls still counted
	// them, so the two disagreed.
	usage := distillProviderUsage{Source: distillUsageSourceOllama}
	if parsed.PromptEvalCount != nil {
		usage.Reported = true
		usage.InputReported = true
		usage.InputTokens = *parsed.PromptEvalCount
	}
	if parsed.EvalCount != nil {
		usage.Reported = true
		usage.OutputReported = true
		usage.OutputTokens = *parsed.EvalCount
	}
	// Error responses may still consume model tokens; TotalAgentCalls also
	// counts failed attempts, so retain provider-reported usage before failing.
	recordDistillProviderUsage(ctx, usage)
	if len(parsed.Response) > distillMaxOutputBytes {
		return "", fmt.Errorf("ollama output exceeds %d bytes", distillMaxOutputBytes)
	}
	// Ollama silently truncates a prompt that exceeds the context window the
	// model was loaded with (gemma4:12b loads at num_ctx 32768 by default), so a
	// chunk past roughly 116 KB is distilled in part and nothing says so. A fact
	// store is read as authoritative, so partial extraction must fail loudly
	// rather than quietly produce fewer facts.
	//
	// The test is prompt_eval_count — the server's own count of tokens it read —
	// against the window the model is actually loaded with. A truncated prompt
	// fills the window exactly, so the count pins to it. An estimate from prompt
	// bytes was tried first and rejected: it needs a chars-per-token assumption,
	// and measured text ran 3.65 chars/token on one tokenizer and 4.89 on
	// another, leaving a 2% margin against a divisor of 5. This comparison has no
	// tokenizer term in it at all.
	if parsed.PromptEvalCount != nil {
		if window := ollamaLoadedContextWindow(ctx, u, model); window > 0 && *parsed.PromptEvalCount >= window {
			// Report the chunk and the system prompt separately: the request
			// carries both, but --max-chunk-bytes only moves the chunk, so a
			// single total would misdirect anyone sizing it down.
			return "", fmt.Errorf("ollama read %d tokens of a %d-byte prompt (%d-byte chunk plus %d-byte system prompt), filling the model's %d-token context window: the prompt was truncated and this chunk would be distilled in part; lower --max-chunk-bytes or load %s with a larger num_ctx", *parsed.PromptEvalCount, len(input)+len(args[2]), len(input), len(args[2]), window, model)
		}
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
	host := strings.Trim(u.Hostname(), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loopbackOnlyDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("ollama dial target must include host and port: %s", address)
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve ollama loopback target %s: %w", host, err)
	}
	var lastErr error
	for _, ip := range ips {
		if !ip.IsLoopback() {
			continue
		}
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, fmt.Errorf("ollama loopback dial failed for %s: %w", address, lastErr)
	}
	return nil, fmt.Errorf("ollama dial target is not loopback-only: %s", address)
}

// cloneDistillFacts freezes the last observed disk state. Reconciliation may
// reuse record slices, so a shallow copy would change the merge baseline.
func cloneDistillFacts(records []factRecord) []factRecord {
	out := slices.Clone(records)
	for i := range out {
		out[i].Paths = slices.Clone(out[i].Paths)
		out[i].Locus = slices.Clone(out[i].Locus)
		out[i].Provenance = slices.Clone(out[i].Provenance)
		out[i].RelatedIDs = slices.Clone(out[i].RelatedIDs)
	}
	return out
}

// rebaseFactsOntoDisk performs a three-way merge under the writer lock. Fields
// changed on disk since the previous read/flush win conflicts; unchanged fields
// retain this run's reconciliation. Provenance is additive on both sides.
func rebaseFactsOntoDisk(brainDir, branch string, inMemory, baseline []factRecord, force bool) ([]factRecord, error) {
	onDisk, err := loadFacts(brainDir, branch)
	if err != nil {
		return nil, err
	}
	before := make(map[string]factRecord, len(baseline))
	for _, record := range baseline {
		before[record.ID] = record
	}
	diskIDs := make(map[string]bool, len(onDisk))
	out := cloneDistillFacts(inMemory)
	for _, record := range onDisk {
		diskIDs[record.ID] = true
		old, existed := before[record.ID]
		i := indexOfFact(out, record.ID)
		if i < 0 {
			if force && record.Origin == factOriginDistilled && record.Status == factStatusActive && existed && reflect.DeepEqual(old, record) {
				continue // only discard the unchanged facts this force run owns
			}
			out = append(out, record)
			continue
		}
		local := out[i]
		if !existed {
			// Concurrent insertion of the same content-derived ID: retain the
			// committed record and combine the independent source anchors.
			out[i] = record
		} else {
			merged := reflect.ValueOf(&out[i]).Elem()
			original, current := reflect.ValueOf(old), reflect.ValueOf(record)
			for field := 0; field < merged.NumField(); field++ {
				if !reflect.DeepEqual(original.Field(field).Interface(), current.Field(field).Interface()) {
					merged.Field(field).Set(current.Field(field))
				}
			}
		}
		out[i].Provenance = factmerge.UnionAnchors(record.Provenance, local.Provenance)
		out[i].RelatedIDs = mergeDistillRelatedIDs(old.RelatedIDs, record.RelatedIDs, local.RelatedIDs)
		if local.UpdatedAt.After(out[i].UpdatedAt) {
			out[i].UpdatedAt = local.UpdatedAt
		}
	}
	// A record removed from the disk since our baseline must not be resurrected.
	out = slices.DeleteFunc(out, func(record factRecord) bool {
		_, existed := before[record.ID]
		return existed && !diskIDs[record.ID]
	})
	return out, nil
}

// Relationship links can be added by either writer, but admin resolution also
// removes links. Preserve independent additions without resurrecting a link
// that either side explicitly removed from the common baseline.
func mergeDistillRelatedIDs(baseline, disk, local []string) []string {
	var out []string
	for _, ids := range [][]string{disk, local} {
		for _, id := range ids {
			if slices.Contains(baseline, id) && (!slices.Contains(disk, id) || !slices.Contains(local, id)) {
				continue
			}
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// ollamaLoadedContextWindow reports the context window the named model is
// currently loaded with, via /api/ps. This is deliberately not /api/show: show
// reports the model's architectural maximum (262144 for gemma4:12b) while ps
// reports what it was actually loaded with (32768 by default), and only the
// latter is the size a prompt gets truncated to.
//
// Returns 0 when the window cannot be determined — the endpoint is unreachable,
// the model is not resident, or the payload does not carry the field. Callers
// treat 0 as "cannot tell" and skip the check rather than guessing.
func ollamaLoadedContextWindow(ctx context.Context, generateURL *url.URL, model string) int64 {
	// Derive a fresh budget from the caller's context rather than reusing the
	// one the generate call ran under. That context carries the whole-call
	// timeout, and a chunk near the context window — the case this probe exists
	// to catch — spends most of it generating. Reusing it would leave the probe
	// no time, return 0 for "cannot tell", and stand the truncation guard down
	// precisely when it is needed.
	ctx, cancel := context.WithTimeout(ctx, ollamaContextProbeTimeout)
	defer cancel()
	psURL := *generateURL
	psURL.Path = strings.TrimSuffix(strings.TrimSuffix(psURL.Path, "/api/generate"), "/") + "/api/ps"
	psURL.RawQuery = ""
	if !isLoopbackHTTPURL(&psURL) {
		return 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, psURL.String(), nil)
	if err != nil {
		return 0
	}
	tr := &http.Transport{Proxy: nil, DialContext: loopbackOnlyDialContext}
	defer tr.CloseIdleConnections()
	client := &http.Client{Timeout: ollamaContextProbeTimeout, Transport: tr,
		CheckRedirect: func(r *http.Request, _ []*http.Request) error {
			if !isLoopbackHTTPURL(r.URL) {
				return fmt.Errorf("ollama redirect must stay loopback-only: %s", r.URL.String())
			}
			return nil
		}}
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0
	}
	var parsed struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			ContextLength int64  `json:"context_length"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return 0
	}
	for _, m := range parsed.Models {
		if m.Name == model || m.Model == model {
			if m.ContextLength > 0 {
				return m.ContextLength
			}
			return 0
		}
	}
	return 0
}
