package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	run                 distillAgentRunner
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
	cmd.Flags().StringVar(&distillOpts.agent, "agent", "auto", "Distillation agent: auto, codex, claude-code, or command")
	cmd.Flags().StringArrayVar(&distillOpts.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().Float64Var(&distillOpts.confidenceThreshold, "confidence", defaultFactConfidenceThreshold, "Minimum agent confidence to auto-apply a merge/supersede; below this it is queued for review")
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
	source, err := runDistillForBrain(ctx, repoDir, storage.BrainDir, distillOpts, opts.Now().UTC())
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
	var warnings []string
	chunksScanned, chunksDistilled := 0, 0

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

	for _, session := range sessions {
		if distillOpts.branch != "" && session.Branch != distillOpts.branch {
			continue
		}
		branch := session.Branch
		if branch == "" {
			branch = strings.TrimSpace(manifest.DefaultBranch)
		}
		if branch == "" {
			branch = distillDefaultBranch
		}
		ensureBranch(branch)

		fingerprint := distillSessionFingerprint(session)
		if !distillOpts.force {
			if prev, ok := prevCache.Sessions[session.SessionID]; ok && prev == fingerprint {
				newCache.Sessions[session.SessionID] = prev // unchanged; retain in cache and keep existing facts
				continue
			}
		}

		content, readErr := readBrainRelativeFile(brainDir, session.TranscriptPath)
		if readErr != nil {
			warnings = append(warnings, fmt.Sprintf("read transcript %s: %v", session.TranscriptPath, readErr))
			continue // not cached: retried next run
		}
		chunks := chunkTranscript(content, distillOpts.maxChunkBytes)
		sessionFailed := false
		for _, chunk := range chunks {
			chunksScanned++
			anchor := factAnchor{
				SessionID:    session.SessionID,
				CheckpointID: session.LatestCheckpoint,
				Transcript:   filepath.ToSlash(session.TranscriptPath),
				Line:         chunk.StartLine,
			}
			out, runErr := distillOpts.run(ctx, repoDir, args, []byte(chunk.Text), distillOpts.timeout)
			if runErr != nil {
				warnings = append(warnings, fmt.Sprintf("agent failed on %s:%d: %v", session.SessionID, chunk.StartLine, runErr))
				sessionFailed = true
				continue
			}
			records, chunkWarnings := distilledFactsFromOutput(out, taxonomy, anchor, branch, now)
			warnings = append(warnings, chunkWarnings...)
			if len(records) == 0 {
				continue
			}
			chunksDistilled++
			// Reconcile the chunk's candidates against the branch's active facts
			// so near-duplicates merge and contradictions supersede instead of
			// piling up. Low-confidence decisions are queued for review.
			actions, recWarnings := reconcileChunkCandidates(ctx, distillOpts.run, reconcileArgs, records, byBranch[branch], repoDir, distillOpts.timeout)
			warnings = append(warnings, recWarnings...)
			var chunkProposals []factProposal
			byBranch[branch], chunkProposals = applyFactActions(byBranch[branch], actions, threshold, now)
			proposalsByBranch[branch] = append(proposalsByBranch[branch], chunkProposals...)
		}
		// Only cache a session as distilled when every chunk succeeded, so a
		// session whose agent calls failed is retried on the next run rather
		// than being silently treated as done.
		if !sessionFailed {
			newCache.Sessions[session.SessionID] = fingerprint
		}
	}

	warnings = capWarnings(warnings, maxDistillWarnings)

	totalProposals := 0
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
		totalProposals += len(merged)
	}
	if err := writeFactTaxonomy(brainDir, taxonomy); err != nil {
		return nil, err
	}
	saveDistillCache(brainDir, newCache)

	source := summarizeFactSource(now, byBranch, chunksScanned, chunksDistilled, totalProposals, warnings)
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

// chunkTranscript splits a transcript into line-numbered chunks no larger than
// maxBytes (measured on the rendered, line-numbered text). A single line that
// exceeds maxBytes still becomes its own chunk rather than being dropped, so no
// content is silently lost. Blank input yields no chunks.
func chunkTranscript(content string, maxBytes int) []transcriptChunk {
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
			continue
		}
		rendered := fmt.Sprintf("%d\t%s\n", lineNo, line)
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

// distillSessionFingerprint is the incremental-skip signal for one session.
// Session transcripts are content-stable across refreshes, so the session id
// plus its latest checkpoint and transcript path is a reliable change marker.
func distillSessionFingerprint(session exportSession) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		session.SessionID,
		session.LatestCheckpoint,
		filepath.ToSlash(session.TranscriptPath),
		session.Branch,
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
