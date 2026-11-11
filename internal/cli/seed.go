package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/docextract"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/spf13/cobra"
)

const (
	seedDirName             = "seed"
	seedAgentDirName        = "agent"
	seedDocsDirName         = "docs"
	seedCursorFileName      = "seed.json"
	defaultSeedMaxFileBytes = 256 * 1024
	defaultSeedMaxFiles     = 2000
	defaultAgentMaxInput    = 500000
	defaultAgentMaxOutput   = 200000
)

// seedPrunePendingPath marks a seed whose manifest may be written while the
// prune of old seed/docs files has not finished (runSeed). In the brain store
// it holds seedPrunePendingText, and refresh reseeds while it is there. In any
// other --output it holds seedPrunePendingListHeader and then, one quoted path
// a line, the old documents that seed meant to remove: the next seed there
// still counts them as its own, although the new manifest no longer lists
// them.
const (
	seedPrunePendingPath       = "seed/prune-pending"
	seedPrunePendingText       = "A seed refresh may have written its manifest without finishing the removal of old seed/docs files. The next refresh reseeds while this file is here.\n"
	seedPrunePendingListHeader = "A seed of this output may have written its manifest before removing the old seed/docs files listed below. The next seed of it removes them.\n"
)

type seedCommandOptions struct {
	outputDir          string
	outputExplicit     bool
	update             bool
	force              bool
	includeTests       bool
	maxFileBytes       int
	maxFiles           int
	format             string
	worktree           bool
	agent              string
	agentCommand       []string
	model              string
	effort             string
	agentQuickTimeout  time.Duration
	agentDeepTimeout   time.Duration
	agentTimeoutAction string
	agentMaxInputBytes int
	// There is no `interactive` field. `refresh seed --interactive` was
	// registered, parsed and then never read by anything: promptSeedTimeoutAction
	// gates on noInteractive and a TTY, so prompts were already on by default and
	// the flag could only ever ask for what was already happening. A flag a user
	// can type, that the binary accepts, and that changes nothing is worse than
	// no flag -- it is a promise the program does not keep.
	noInteractive bool
	requireAgent  bool
	// progress, when set, is called as seed synthesis advances (e.g. the agent
	// quick/deep phases) so the long-running agent step reports more than a
	// static spinner during refresh.
	progress func(phase string)
}

func (o seedCommandOptions) reportProgress(phase string) {
	if o.progress != nil {
		o.progress(phase)
	}
}

type seedCursor struct {
	SchemaVersion                  int                  `json:"schema_version"`
	UpdatedAt                      time.Time            `json:"updated_at"`
	RepoRoot                       string               `json:"repo_root"`
	RepoKey                        string               `json:"repo_key"`
	LastCommit                     string               `json:"last_commit,omitempty"`
	WorktreeMode                   string               `json:"worktree_mode"`
	Files                          []seedFileIndexEntry `json:"files"`
	DeterministicPacketFingerprint string               `json:"deterministic_packet_fingerprint"`
	QuickAgent                     seedAgentPhase       `json:"quick_agent,omitempty"`
	DeepAgent                      seedAgentPhase       `json:"deep_agent,omitempty"`
}

type seedFileIndexEntry struct {
	Path      string `json:"path"`
	Category  string `json:"category"`
	Bytes     int64  `json:"bytes,omitempty"`
	Hash      string `json:"hash,omitempty"`
	Included  bool   `json:"included"`
	Reason    string `json:"reason"`
	SeedPath  string `json:"seed_path,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type seedScanResult struct {
	MaxFileBytes int
	RepoDir      string
	// ContentDir, when set, is the directory file CONTENT is read from — a
	// materialized committed-HEAD snapshot when the worktree is dirty
	// (COR-2316). RepoDir stays the real repository for git commands,
	// identity, and display; only reads of file bytes go through here.
	ContentDir  string
	RepoKey     string
	Commit      string
	Files       []seedFileIndexEntry
	Docs        []seedDocument
	Entrypoints []string
	Commands    []seedCommand
	Coverage    *seedHistoryCoverage
	Warnings    []string
	Fingerprint string
	// writtenDocs holds the seed/docs paths writeSeedDocs wrote, for the
	// pending marker and the prune that runs once the new manifest is
	// written. It fills as each document is written, so a write that fails
	// part way still says which copies are on disk.
	writtenDocs map[string]bool
	// replacedDocs holds the written paths that already named a file when
	// this seed wrote them: an earlier copy, under this spelling or another
	// the filesystem folds to it, or a file no seed recorded. A seed that
	// stops early does not remove those (removeUnrecordedSeedCopies).
	replacedDocs map[string]bool
	// copyClaimer, when set, journals each batch of new copies in an
	// explicit --output's pending marker before writeSeedDocs makes them.
	copyClaimer *seedCopyClaimer
}

func newSeedCommand(opts Options) *cobra.Command {
	seedOpts := seedCommandOptions{
		includeTests:       true,
		maxFileBytes:       defaultSeedMaxFileBytes,
		maxFiles:           defaultSeedMaxFiles,
		format:             "markdown+json",
		agent:              "none",
		agentQuickTimeout:  2 * time.Minute,
		agentDeepTimeout:   10 * time.Minute,
		agentTimeoutAction: "keep-quick",
		agentMaxInputBytes: defaultAgentMaxInput,
	}
	cmd := &cobra.Command{
		Use:    "seed [path]",
		Short:  "Rebuild repository context from docs and code structure",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if opts.Env.RepoRoot != "" {
				target = opts.Env.RepoRoot
			}
			if len(args) == 1 {
				target = args[0]
			}
			return runSeed(cmd.Context(), cmd, opts, seedOpts, target)
		},
	}
	cmd.Flags().StringVarP(&seedOpts.outputDir, "output", "o", "", "Output directory for seed (default: persistent brain directory)")
	cmd.Flags().BoolVar(&seedOpts.update, "update", false, "Refresh an existing seed")
	cmd.Flags().BoolVar(&seedOpts.force, "force", false, "Overwrite existing seed output")
	cmd.Flags().BoolVar(&seedOpts.includeTests, "include-tests", true, "Include tests in inventory and summaries")
	cmd.Flags().IntVar(&seedOpts.maxFileBytes, "max-file-bytes", defaultSeedMaxFileBytes, "Maximum bytes to copy from any source/doc file")
	cmd.Flags().IntVar(&seedOpts.maxFiles, "max-files", defaultSeedMaxFiles, "Maximum files to scan")
	cmd.Flags().StringVar(&seedOpts.format, "format", "markdown+json", "Seed output format")
	cmd.Flags().BoolVar(&seedOpts.worktree, "worktree", false, "Include current tracked changes and selected untracked instruction/docs files")
	cmd.Flags().StringVar(&seedOpts.agent, "agent", "none", "Agent synthesis mode: none, command, codex, or claude-code")
	cmd.Flags().StringArrayVar(&seedOpts.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().StringVar(&seedOpts.model, "model", "", "Override the agent model for codex/claude-code seed synthesis (e.g. a fast/cheap model)")
	cmd.Flags().StringVar(&seedOpts.effort, "effort", "", "Override the reasoning effort for codex/claude-code seed synthesis (e.g. low)")
	cmd.Flags().DurationVar(&seedOpts.agentQuickTimeout, "agent-quick-timeout", 2*time.Minute, "Timeout for quick agent synthesis")
	cmd.Flags().DurationVar(&seedOpts.agentDeepTimeout, "agent-deep-timeout", 10*time.Minute, "Timeout for deep agent synthesis")
	cmd.Flags().StringVar(&seedOpts.agentTimeoutAction, "agent-timeout-action", "keep-quick", "Deep timeout action: keep-quick, continue, or fail")
	cmd.Flags().IntVar(&seedOpts.agentMaxInputBytes, "agent-max-input-bytes", defaultAgentMaxInput, "Maximum bytes in agent input packet")
	cmd.Flags().BoolVar(&seedOpts.noInteractive, "no-interactive", false, "Disable timeout prompts")
	cmd.Flags().BoolVar(&seedOpts.requireAgent, "require-agent", false, "Fail if required agent synthesis does not complete")
	return cmd
}

func runSeed(ctx context.Context, cmd *cobra.Command, opts Options, seedOpts seedCommandOptions, target string) error {
	if seedOpts.maxFileBytes <= 0 {
		return errors.New("--max-file-bytes must be greater than zero")
	}
	if seedOpts.maxFiles <= 0 {
		return errors.New("--max-files must be greater than zero")
	}
	if seedOpts.format != "markdown+json" {
		return errors.New("--format must be markdown+json")
	}
	if seedOpts.agentTimeoutAction != "keep-quick" && seedOpts.agentTimeoutAction != "continue" && seedOpts.agentTimeoutAction != "fail" {
		return errors.New("--agent-timeout-action must be keep-quick, continue, or fail")
	}

	repoDir, local, err := clibase.ResolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("seed target must be an existing local path: %s", target)
	}
	contentDir, contentCommit := "", ""
	if !seedOpts.worktree {
		gateIgnore, err := worktreeGateIgnore(repoDir)
		if err != nil {
			return err
		}
		dirty, dirtyPaths, err := worktreeDirtyDetail(ctx, opts.Runner, repoDir, gateIgnore)
		if err != nil {
			return nameDegenerateRepoFailure(ctx, opts.Runner, repoDir, fmt.Errorf("check worktree before repository context refresh: %w", err))
		}
		if dirty {
			// A dirty tree is a reason to exclude uncommitted content, not to
			// skip the whole seed: permanently-dirty worktrees (every Conductor
			// workspace) otherwise never get a background refresh (COR-2316).
			// Build from a materialized committed-HEAD snapshot; the refusal
			// survives only as the fallback when that snapshot cannot be built.
			dir, commit, cleanup, snapErr := materializeHeadSnapshot(ctx, opts.Runner, repoDir)
			if snapErr != nil {
				return fmt.Errorf("%w (committed-snapshot fallback failed: %v)", dirtyWorktreeRefusal("build repository context from", dirtyPaths), snapErr)
			}
			defer cleanup()
			contentDir, contentCommit = dir, commit
			seedOpts.reportProgress(fmt.Sprintf("uncommitted changes excluded; building from committed HEAD %.12s", commit))
		}
	}

	outputExplicit := seedOpts.outputExplicit || cmd.Flags().Changed("output")
	persistentSeed := !outputExplicit
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	outputDir := seedOpts.outputDir
	if !outputExplicit {
		outputDir = storage.BrainDir
	}
	if outputExplicit && !seedOpts.update {
		if seedOpts.force {
			if err := removeForcedOutputDir(outputDir, repoDir); err != nil {
				return err
			}
		} else if _, err := validateExportDirAvailable(outputDir); err != nil {
			return err
		}
	}
	outputDir, err = prepareExportDir(outputDir, true)
	if err != nil {
		return err
	}
	inBrainStore := persistentSeed || seedOutputIsBrainStore(outputDir, storage.BrainDir)
	worktreeHash := ""
	if seedOpts.worktree {
		worktreeHash, err = worktreeFingerprint(ctx, opts.Runner, repoDir)
		if err != nil {
			// --worktree routes around the wrapped worktreeDirty check above, so
			// this is the FIRST git failure a `--worktree` run in a degenerate
			// repository ever reaches -- and it reached the reader verbatim,
			// hardening flags and all:
			//
			//	fingerprint worktree for seed: git [-c core.fsmonitor=false
			//	-c core.hooksPath=/dev/null/entire-brain-hooks-disabled status
			//	--porcelain --untracked-files=all]: exit status 128: fatal: this
			//	operation must be run in a work tree
			//
			// The same repository WITHOUT --worktree already said "bare
			// repository has no working tree ... point it at a clone with a
			// checkout". Same condition, same sentence.
			return nameDegenerateRepoFailure(ctx, opts.Runner, repoDir, fmt.Errorf("fingerprint worktree for seed: %w", err))
		}
	}

	scan, err := scanSeedRepository(ctx, opts.Runner, repoDir, contentDir, contentCommit, storage.Key, seedOpts)
	if err != nil {
		return err
	}
	scan.Coverage = buildSeedHistoryCoverage(ctx, opts.Runner, repoDir, outputDir)
	if scan.Coverage != nil && scan.Coverage.MissingSessionCommits > 0 {
		scan.Warnings = append(scan.Warnings, fmt.Sprintf("%d commits after oldest session have no Entire checkpoint trailer; see seed/history-gaps.md", scan.Coverage.MissingSessionCommits))
	}
	// The manifest gets a bounded projection of the coverage record; the full
	// per-commit list stays in seed/history-gaps.md, which is where it is meant
	// to be read. See manifestSeedHistoryCoverage.
	manifestCoverage := manifestSeedHistoryCoverage(scan.Coverage)
	if manifestCoverage != nil && manifestCoverage.UncoveredCommitsTruncated {
		scan.Warnings = append(scan.Warnings, fmt.Sprintf("manifest lists %d of %d uncovered commits; the full list is in seed/history-gaps.md", len(manifestCoverage.UncoveredCommits), manifestCoverage.UncoveredCommitsTotal))
	}
	var seedManifest *seedSourceManifest
	commitSeed := func() error {
		// Read before the new manifest replaces the one that says which
		// documents under seed/docs an earlier seed there wrote.
		ownership := seedDocsOwnershipOf(outputDir, inBrainStore)
		// Once a copy is written, seed/docs no longer matches the manifest
		// there, and anything that ends the refresh before the new manifest
		// (a failed write, a failed required agent run, the final worktree
		// check) keeps that manifest. The pending marker is written first.
		// The brain store's names no documents, so it goes down before any
		// copy, and a seed that cannot write it copies none; while it is
		// there the next refresh reseeds, which puts seed/docs right. Any
		// other --output's lists the copies the manifest does not: without
		// it a copy of a file gone from the tree before the next seed would
		// be left as not a seed's, and stay searchable. So it claims each
		// new copy, by size and digest, just before the copy is made
		// (seedCopyClaimer), which also covers a seed killed part way
		// through them, and is rewritten to the copies actually made as soon
		// as the documents are written, even when writing the rest failed
		// (beginSeedPrune). A seed that cannot write a claim makes none of
		// the copies it names. A copy that seed could not record (the
		// rewrite failed, or a file of that name is not a seed's) is
		// removed again if it stops before its manifest lists the copy.
		//
		// seed/risks.md is already written, so a warning from here on (the
		// marker's, then the prune's) is reported by writing it again, and
		// the prune's by writing the manifest again too.
		if err := markBrainStorePrunePending(outputDir, ownership); err != nil {
			return err
		}
		scan.copyClaimer = newSeedCopyClaimer(outputDir, &scan, &ownership)
		writeErr := writeSeedArtifacts(outputDir, &scan)
		reported := len(scan.Warnings)
		if err := beginSeedPrune(outputDir, &scan, &ownership); err != nil {
			return removeUnrecordedSeedCopies(outputDir, ownership, errors.Join(writeErr, err))
		}
		if writeErr != nil {
			return removeUnrecordedSeedCopies(outputDir, ownership, writeErr)
		}

		seedManifest = &seedSourceManifest{
			GeneratedAt:       opts.Now().UTC(),
			Commit:            scan.Commit,
			WorktreeMode:      seedWorktreeMode(seedOpts),
			WorktreeHash:      worktreeHash,
			FileFingerprint:   scan.Fingerprint,
			SummaryPath:       filepath.ToSlash(filepath.Join(seedDirName, "repo-overview.md")),
			Documents:         scan.Docs,
			Entrypoints:       scan.Entrypoints,
			Commands:          scan.Commands,
			HistoryBaseline:   buildSeedHistoryBaseline(ctx, opts.Runner, repoDir, outputDir),
			HistoryCoverage:   manifestCoverage,
			Warnings:          slices.Clone(scan.Warnings),
			DeterministicPath: []string{"seed/repo-overview.md", "seed/architecture.md", "seed/commands.md", "seed/conventions.md", "seed/risks.md", "seed/history-gaps.md", "seed/file-index.json"},
		}

		if seedOpts.agent != "none" {
			// The agent works in the scan's content root: in snapshot mode the
			// repository itself is dirty, and an agent run there could fold
			// uncommitted content into the overview (trail #2845 finding).
			agentManifest, err := runSeedAgent(ctx, scan.contentRoot(), outputDir, scan, seedOpts)
			if err != nil {
				if seedOpts.requireAgent {
					return removeUnrecordedSeedCopies(outputDir, ownership, err)
				}
				seedManifest.Warnings = append(seedManifest.Warnings, err.Error())
			}
			if agentManifest != nil {
				seedManifest.Agent = agentManifest
			}
		}
		if seedOpts.worktree {
			currentHash, err := worktreeFingerprint(ctx, opts.Runner, repoDir)
			if err != nil {
				return removeUnrecordedSeedCopies(outputDir, ownership, fmt.Errorf("recheck worktree fingerprint for seed: %w", err))
			}
			if currentHash != worktreeHash {
				return removeUnrecordedSeedCopies(outputDir, ownership, errors.New("worktree_changed: worktree content changed during seed refresh"))
			}
		}
		// Pruning waits until the new manifest is written. A refresh that is
		// refused, or whose manifest write fails, keeps the previous manifest,
		// and with it the previous worktree hash: had it already removed a
		// document the earlier seed copied, a tree restored to that state
		// would match the hash, the next refresh would skip the reseed, and
		// the document would stay missing from search. The other way round,
		// a refresh that stops once the manifest is saved (the README write
		// after it can fail too) and before the prune and its report finish
		// leaves old documents behind a manifest that counts as current. The
		// pending marker covers that: it is written before the manifest and
		// removed last, and while it is there refresh reseeds
		// (seedPrunePending). A prune that could not remove a document for a
		// reason that may pass keeps the marker too, so the next refresh
		// tries again instead of leaving the document searchable behind a
		// current manifest once the cause is put right. Any other --output
		// gets one only to carry the seed's documents that one of the two
		// manifests does not list (beginSeedPrune).
		if err := writeBrainSeedSourceLocked(outputDir, storage.Key, seedManifest); err != nil {
			if seedManifestIsPublished(err) {
				return err
			}
			return removeUnrecordedSeedCopies(outputDir, ownership, err)
		}
		pruned := len(scan.Warnings)
		retryPrune := pruneSeedDocs(outputDir, &scan, scan.writtenDocs, ownership)
		if pruneWarnings := scan.Warnings[pruned:]; len(pruneWarnings) > 0 {
			seedManifest.Warnings = append(seedManifest.Warnings, pruneWarnings...)
			if err := writeBrainSeedSourceLocked(outputDir, storage.Key, seedManifest); err != nil {
				return err
			}
		}
		if len(scan.Warnings) > reported {
			if err := writeSeedRisks(outputDir, &scan); err != nil {
				return err
			}
		}
		if err := finishSeedPrune(outputDir, ownership, retryPrune); err != nil {
			return err
		}
		if persistentSeed {
			cursor := seedCursor{
				SchemaVersion:                  1,
				UpdatedAt:                      seedManifest.GeneratedAt,
				RepoRoot:                       repoDir,
				RepoKey:                        storage.Key,
				LastCommit:                     scan.Commit,
				WorktreeMode:                   seedManifest.WorktreeMode,
				Files:                          scan.Files,
				DeterministicPacketFingerprint: scan.Fingerprint,
			}
			if seedManifest.Agent != nil {
				cursor.QuickAgent = seedManifest.Agent.Quick
				cursor.DeepAgent = seedManifest.Agent.Deep
			}
			if err := writeSeedCursor(storage.HeadPath, cursor); err != nil {
				return err
			}
		}
		return nil
	}
	// The output's write lock covers everything from reading what it owns to
	// the prune, in the brain store (which a seed naming it with --output
	// waits for as well) and in any other output alike: two seeds into one
	// output would otherwise each prune by what they read before the other's
	// manifest, and could remove a document the other's manifest lists.
	if err := brainstore.WithBrainWriteLockContext(ctx, outputDir, commitSeed); err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "seeded brain from %d files\n", len(scan.Files))
	fmt.Fprintf(out, "output: %s\n", outputDir)
	if len(seedManifest.Warnings) > 0 {
		// A bare count is not a report. On a 50,000-file repository the entire
		// output was "seeded brain from 2000 files" followed by "warnings: 1",
		// which withholds the one sentence that says 96% of the repository was
		// never read -- a sentence already recorded, in the manifest this very
		// line is summarizing. `refresh sessions` has always printed the count
		// AND the text (runExportSessions); seed printed only the count, so the
		// stage that drops the most content explained the least.
		fmt.Fprintf(out, "warnings: %d\n", len(seedManifest.Warnings))
		for _, warning := range seedManifest.Warnings {
			fmt.Fprintf(out, "warning: %s\n", warning)
		}
	}
	return nil
}

// contentRoot is where a scan's file bytes live: the materialized snapshot in
// committed-content mode, the repository otherwise.
func (s seedScanResult) contentRoot() string {
	if s.ContentDir != "" {
		return s.ContentDir
	}
	return s.RepoDir
}

// scanSeedRepository reads file content from contentDir when it is non-empty
// (a materialized snapshot of contentCommit); enumeration then asks git for
// the committed file list, so the included set under --max-files matches a
// clean worktree at the same commit. Walking the snapshot instead would
// SkipDir denied names before the cap, while the clean path lists every
// tracked file and denies later in inspectSeedFile.
func scanSeedRepository(ctx context.Context, runner CommandRunner, repoDir, contentDir, contentCommit, repoKey string, opts seedCommandOptions) (seedScanResult, error) {
	contentRoot := repoDir
	var paths, warnings []string
	if contentDir != "" {
		contentRoot = contentDir
		paths, warnings = listCommittedSeedFiles(ctx, runner, repoDir, contentCommit, contentDir)
	} else {
		paths, warnings = listSeedFiles(ctx, runner, repoDir, opts)
	}
	if len(paths) > opts.maxFiles {
		// Name what was dropped, not just the ceiling that dropped it. "file
		// scan capped at 2000 files" is true of a 2,001-file repository and of
		// a 100,000-file one, and only the second is a reason to raise the cap.
		// The reader cannot tell which they have from the cap alone -- the
		// count of files that were never looked at is the number that decides
		// whether this warning matters.
		warnings = append(warnings, fmt.Sprintf("file scan capped at %d of %d files; %d files were not read (raise with --max-files)", opts.maxFiles, len(paths), len(paths)-opts.maxFiles))
		paths = paths[:opts.maxFiles]
	}
	commit := contentCommit
	if commit == "" {
		commit = strings.TrimSpace(string(clibase.GitOutput(ctx, runner, repoDir, "rev-parse", "HEAD")))
	}
	result := seedScanResult{MaxFileBytes: opts.maxFileBytes, RepoDir: repoDir, ContentDir: contentDir, RepoKey: repoKey, Commit: commit, Warnings: warnings}
	// Reserve source paths and their directories before allocating derived
	// names: spec.pdf.md may itself be an authored document (or a directory).
	reserved := map[string]bool{}
	for _, rel := range paths {
		name := filepath.ToSlash(filepath.Join(seedDirName, seedDocsDirName, rel))
		for name != "." {
			reserved[strings.ToLower(name)] = true
			name = filepath.ToSlash(filepath.Dir(name))
		}
	}
	for _, rel := range paths {
		entry := inspectSeedFile(contentRoot, rel, opts)
		result.Files = append(result.Files, entry)
		if !entry.Included {
			continue
		}
		if isHighSignalDoc(rel) {
			seedPath := filepath.ToSlash(filepath.Join(seedDirName, seedDocsDirName, rel))
			extracted := isExtractableSeedDocument(rel)
			if extracted {
				seedPath = extractedSeedDocPath(seedPath)
				for reserved[strings.ToLower(seedPath)] {
					seedPath += ".md"
				}
				reserved[strings.ToLower(seedPath)] = true
			}
			result.Docs = append(result.Docs, seedDocument{
				Path:      filepath.ToSlash(rel),
				SeedPath:  seedPath,
				Bytes:     entry.Bytes,
				Truncated: entry.Truncated,
				Reason:    entry.Reason,
				Extracted: extracted,
			})
		}
		if isEntrypointPath(rel) {
			result.Entrypoints = append(result.Entrypoints, filepath.ToSlash(rel))
		}
		if filepath.Base(rel) == "package.json" {
			commands, err := packageJSONCommands(filepath.Join(contentRoot, rel), rel)
			if err != nil {
				result.Warnings = append(result.Warnings, err.Error())
			}
			result.Commands = append(result.Commands, commands...)
		}
		if filepath.Base(rel) == "mise.toml" {
			commands, err := miseCommands(filepath.Join(contentRoot, rel), rel)
			if err != nil {
				result.Warnings = append(result.Warnings, err.Error())
			}
			result.Commands = append(result.Commands, commands...)
		}
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	sort.Slice(result.Docs, func(i, j int) bool { return result.Docs[i].Path < result.Docs[j].Path })
	sort.Strings(result.Entrypoints)
	sort.Slice(result.Commands, func(i, j int) bool { return result.Commands[i].Name < result.Commands[j].Name })
	result.Fingerprint = seedFingerprint(result)
	return result, nil
}

// walkSeedFiles enumerates files under root on disk: the fallback when git
// cannot list files, for the repository and for a materialized snapshot alike.
func walkSeedFiles(root string) ([]string, []string) {
	var paths []string
	var warnings []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			warnings = append(warnings, err.Error())
			return nil
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if d.IsDir() && isDeniedSeedDir(rel) {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		warnings = append(warnings, "walk repository: "+err.Error())
	}
	sort.Strings(paths)
	return paths, warnings
}

// listCommittedSeedFiles enumerates the files of commit for a materialized
// snapshot: `git ls-tree` is the committed counterpart of the clean path's
// `git ls-files`, so denied paths still reach inspectSeedFile (and the
// --max-files cap) instead of vanishing in a directory walk. The walk remains
// the fallback when git cannot answer.
func listCommittedSeedFiles(ctx context.Context, runner CommandRunner, repoDir, commit, contentDir string) ([]string, []string) {
	stdout, _, err := runner.Run(ctx, repoDir, "git", "ls-tree", "-r", "--name-only", commit)
	if err != nil {
		return walkSeedFiles(contentDir)
	}
	paths := splitNonEmptyLines(stdout)
	sort.Strings(paths)
	return compactSortedStrings(paths), nil
}

func listSeedFiles(ctx context.Context, runner CommandRunner, repoDir string, opts seedCommandOptions) ([]string, []string) {
	stdout, _, err := runner.Run(ctx, repoDir, "git", "ls-files")
	if err == nil {
		paths := splitNonEmptyLines(stdout)
		if opts.worktree {
			extra, _, extraErr := runner.Run(ctx, repoDir, "git", "ls-files", "--others", "--exclude-standard")
			if extraErr == nil {
				for _, path := range splitNonEmptyLines(extra) {
					if isSelectedUntrackedSeedFile(path) {
						paths = append(paths, path)
					}
				}
			}
		}
		sort.Strings(paths)
		return compactSortedStrings(paths), nil
	}
	return walkSeedFiles(repoDir)
}

func splitNonEmptyLines(data []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(filepath.ToSlash(line))
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func inspectSeedFile(repoDir, rel string, opts seedCommandOptions) seedFileIndexEntry {
	entry := seedFileIndexEntry{Path: filepath.ToSlash(rel), Category: seedCategory(rel), Included: true, Reason: "included"}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		entry.Included = false
		entry.Reason = "unsafe path"
		return entry
	}
	if isDeniedSeedPath(rel) {
		entry.Included = false
		entry.Reason = "denied path"
		return entry
	}
	if !opts.includeTests && isTestPath(rel) {
		entry.Included = false
		entry.Reason = "tests excluded"
		return entry
	}
	if err := clibase.RejectSymlinkPathComponents(repoDir, clean); err != nil {
		entry.Included = false
		entry.Reason = "symlink path"
		return entry
	}
	info, err := os.Stat(filepath.Join(repoDir, clean))
	if err != nil {
		entry.Included = false
		entry.Reason = "stat failed"
		return entry
	}
	entry.Bytes = info.Size()
	if info.Size() > int64(opts.maxFileBytes) {
		entry.Truncated = true
		if !isHighSignalDoc(rel) {
			entry.Included = false
			entry.Reason = "too large"
			return entry
		}
		entry.Reason = "included truncated"
	}
	// An extractable document is binary on disk and text once read, so it must
	// bypass both the extension denylist and the NUL-byte check below — every
	// PDF and every Office file fails the latter by construction. The bytes are
	// never indexed directly; writeSeedDocs converts them first.
	if isExtractableSeedDocument(rel) {
		// The NUL check below would reject every PDF and Office file by
		// construction, which is why this returns early — but the content hash
		// is what makes the scan fingerprint move when a file changes, so take
		// the digest and discard the binary verdict rather than skipping both.
		// Without it, editing a PDF under docs/ leaves the fingerprint
		// identical and the packet looks fresh when it is not.
		if sum, _, hashErr := clibase.HashFileDetectingNUL(filepath.Join(repoDir, clean)); hashErr == nil {
			entry.Hash = sum
		}
		entry.Reason = "included as an extracted document"
		return entry
	}
	if isLikelyBinaryPath(rel) {
		entry.Included = false
		entry.Reason = "binary or media"
		return entry
	}
	// Stream the file rather than slurping it. A high-signal doc over
	// opts.maxFileBytes is deliberately KEPT (marked truncated) and only its first
	// max-file-bytes are ever copied, so reading the whole thing into memory made
	// an ordinary refresh allocate the full size of the largest doc in the repo —
	// unbounded, and attacker- or accident-controlled. Hashing incrementally keeps
	// the recorded digest byte-identical while holding only one buffer.
	if sum, binary, err := clibase.HashFileDetectingNUL(filepath.Join(repoDir, clean)); err == nil {
		if binary {
			entry.Included = false
			entry.Reason = "binary content"
			return entry
		}
		entry.Hash = sum
	}
	return entry
}

func seedCategory(path string) string {
	path = filepath.ToSlash(path)
	switch {
	case isHighSignalDoc(path):
		return "docs"
	case isTestPath(path):
		return "tests"
	case strings.HasPrefix(path, ".github/") || strings.HasPrefix(path, ".codex/") || strings.HasPrefix(path, ".cursor/"):
		return "config"
	case strings.HasPrefix(path, "src/") || strings.HasPrefix(path, "internal/") || strings.HasPrefix(path, "cmd/"):
		return "source"
	default:
		return "other"
	}
}

func isDeniedSeedPath(path string) bool {
	path = filepath.ToSlash(path)
	if isDeniedSeedDir(path) || strings.Contains(path, "/.git/") {
		return true
	}
	base := strings.ToLower(filepath.Base(path))
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.Contains(base, "credential") || strings.Contains(base, "secret") || strings.Contains(base, "token") {
		return true
	}
	switch filepath.Ext(base) {
	case ".pem", ".key", ".p12", ".pfx", ".crt", ".cer":
		return true
	}
	if base == "package-lock.json" || base == "yarn.lock" || base == "pnpm-lock.yaml" || base == "go.sum" {
		return true
	}
	return false
}

func isDeniedSeedDir(path string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for _, part := range parts {
		switch part {
		case ".git", "node_modules", "dist", "build", "coverage", "vendor", ".next", ".turbo":
			return true
		}
	}
	return false
}

func isLikelyBinaryPath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf", ".zip", ".gz", ".tar", ".tgz", ".mp4", ".mov", ".woff", ".woff2", ".ttf", ".otf":
		return true
	default:
		return false
	}
}

func isHighSignalDoc(path string) bool {
	path = filepath.ToSlash(path)
	base := strings.ToLower(filepath.Base(path))
	if strings.HasPrefix(base, "readme") || base == "claude.md" || base == "agents.md" || base == "contributing.md" || base == "security.md" {
		return true
	}
	if path == ".github/copilot-instructions.md" {
		return true
	}
	if strings.HasPrefix(path, "docs/") && strings.HasSuffix(strings.ToLower(path), ".md") {
		return true
	}
	// Scoping for extracted documents lives in isExtractableSeedDocument alone,
	// so there is exactly one place it can be got wrong — and exactly one place
	// a test can hold it.
	return isExtractableSeedDocument(path)
}

// isExtractableSeedDocument reports whether a repository file is a non-code
// document this build can read: a PDF, or a Word/Excel/PowerPoint file.
//
// The rule is the same one markdown already obeys — under docs/ — rather than
// anywhere in the tree. A repository's docs/ directory is where somebody put
// material meant to be read; a .xlsx under test fixtures or vendor/ is data,
// and hoovering those into the brain would bury the documents that matter.
func isExtractableSeedDocument(path string) bool {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "docs/") {
		return false
	}
	return docextract.DocumentFormatFor(path) != docextract.FormatUnknown
}

// extractedSeedDocPath is where an extracted document lands in the brain.
//
// The `.md` suffix is load-bearing: the doc indexer walks the seed for markdown,
// so extracted text has to arrive as markdown to be retrievable at all. Keeping
// the original extension in the name (spec.pdf.md, not spec.md) means the stored
// file still says what it came from, and two documents that differ only by
// format cannot collide.
func extractedSeedDocPath(seedPath string) string {
	return seedPath + ".md"
}

func isSelectedUntrackedSeedFile(path string) bool {
	return isHighSignalDoc(path) || strings.HasPrefix(filepath.ToSlash(path), ".codex/") || strings.HasPrefix(filepath.ToSlash(path), ".cursor/")
}

func isEntrypointPath(path string) bool {
	base := filepath.Base(path)
	if base == "main.go" || base == "server.js" || base == "index.js" || base == "index.ts" || base == "index.tsx" || base == "main.jsx" || base == "main.tsx" || base == "App.jsx" || base == "App.tsx" {
		return true
	}
	return strings.HasPrefix(filepath.ToSlash(path), "bin/")
}

func isTestPath(path string) bool {
	path = filepath.ToSlash(path)
	base := filepath.Base(path)
	return strings.Contains(path, "/test/") || strings.Contains(path, "/tests/") || strings.Contains(path, "__tests__") || strings.Contains(base, "_test.") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
}

func packageJSONCommands(path, rel string) ([]seedCommand, error) {
	// A manifest is small; bound the read so a hostile or corrupt one cannot
	// exhaust memory before the parser ever sees it.
	data, err := clibase.SafeReadFile(path, clibase.MaxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
		Bin     any               `json:"bin"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, err)
	}
	var commands []seedCommand
	for name, script := range pkg.Scripts {
		commands = append(commands, seedCommand{Name: "npm run " + name, Command: script, Source: filepath.ToSlash(rel)})
	}
	return commands, nil
}

func miseCommands(path, rel string) ([]seedCommand, error) {
	// A manifest is small; bound the read so a hostile or corrupt one cannot
	// exhaust memory before the parser ever sees it.
	data, err := clibase.SafeReadFile(path, clibase.MaxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	var commands []seedCommand
	var current string
	var collecting bool
	var multiline strings.Builder
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if collecting {
			if end, _, ok := strings.Cut(line, "'''"); ok {
				multiline.WriteString(strings.TrimSpace(end))
				commands = append(commands, seedCommand{Name: "mise run " + current, Command: strings.TrimSpace(multiline.String()), Source: filepath.ToSlash(rel)})
				collecting = false
				multiline.Reset()
				continue
			}
			multiline.WriteString(strings.TrimSpace(raw))
			multiline.WriteByte('\n')
			continue
		}
		if strings.HasPrefix(line, "[tasks.") && strings.HasSuffix(line, "]") {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "[tasks."), "]")
			name = strings.Trim(name, `"`)
			current = name
			continue
		}
		if current == "" || !strings.HasPrefix(line, "run") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "run" {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "'''") {
			value = strings.TrimPrefix(value, "'''")
			if before, _, ok := strings.Cut(value, "'''"); ok {
				commands = append(commands, seedCommand{Name: "mise run " + current, Command: strings.TrimSpace(before), Source: filepath.ToSlash(rel)})
				continue
			}
			collecting = true
			multiline.WriteString(strings.TrimSpace(value))
			if strings.TrimSpace(value) != "" {
				multiline.WriteByte('\n')
			}
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		commands = append(commands, seedCommand{Name: "mise run " + current, Command: value, Source: filepath.ToSlash(rel)})
	}
	return commands, nil
}

func seedFingerprint(scan seedScanResult) string {
	var b strings.Builder
	b.WriteString(scan.Commit)
	for _, file := range scan.Files {
		b.WriteString("\n")
		b.WriteString(file.Path)
		b.WriteString("\x00")
		b.WriteString(file.Hash)
		b.WriteString("\x00")
		b.WriteString(file.Reason)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeSeedArtifacts takes the scan by POINTER because writing the documents
// corrects it: a document whose extraction fails loses its seed path and gains
// a reason, and a warning is appended. Taking it by value silently discarded
// every one of those corrections, so the durable manifest recorded an encrypted
// or scanned PDF as extracted, with a seed path that was never written, and the
// "document not indexed" warning never reached anybody.
func writeSeedArtifacts(outputDir string, scan *seedScanResult) error {
	if err := os.MkdirAll(filepath.Join(outputDir, seedDirName, seedDocsDirName), 0o700); err != nil {
		return err
	}
	// Documents are written FIRST, because doing so is what discovers which of
	// them could not be read. file-index.json is the documented place to see
	// why a file was included or skipped; writing it before extraction was
	// attempted meant it could only ever report the optimistic answer.
	if err := writeSeedDocs(outputDir, scan); err != nil {
		return err
	}
	fileIndexData, err := json.MarshalIndent(scan.Files, "", "  ")
	if err != nil {
		return err
	}
	fileIndexData = append(fileIndexData, '\n')
	if err := brainstore.WriteBrainRelativeFileAtomic(outputDir, filepath.ToSlash(filepath.Join(seedDirName, "file-index.json")), fileIndexData, 0o600); err != nil {
		return err
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(outputDir, filepath.ToSlash(filepath.Join(seedDirName, "repo-overview.md")), []byte(renderSeedOverview(*scan)), 0o600); err != nil {
		return err
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(outputDir, filepath.ToSlash(filepath.Join(seedDirName, "architecture.md")), []byte(renderSeedArchitecture(*scan)), 0o600); err != nil {
		return err
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(outputDir, filepath.ToSlash(filepath.Join(seedDirName, "commands.md")), []byte(renderSeedCommands(*scan)), 0o600); err != nil {
		return err
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(outputDir, filepath.ToSlash(filepath.Join(seedDirName, "conventions.md")), []byte(renderSeedConventions(*scan)), 0o600); err != nil {
		return err
	}
	if err := writeSeedRisks(outputDir, scan); err != nil {
		return err
	}
	if err := brainstore.WriteBrainRelativeFileAtomic(outputDir, filepath.ToSlash(filepath.Join(seedDirName, "history-gaps.md")), []byte(renderSeedHistoryGaps(*scan)), 0o600); err != nil {
		return err
	}
	return nil
}

// seedPrunePending reports whether the seed in brainDir may have stopped
// between its manifest write and the end of its prune (runSeed), so that the
// manifest counting as current is no reason to skip a reseed.
func seedPrunePending(brainDir string) bool {
	_, err := os.Lstat(filepath.Join(brainDir, filepath.FromSlash(seedPrunePendingPath)))
	return err == nil
}

// seedOutputIsBrainStore reports whether an explicit --output is the
// repository's brain store, judged by filesystem identity rather than by how
// the path is spelled.
func seedOutputIsBrainStore(outputDir, brainDir string) bool {
	output, err := os.Stat(outputDir)
	if err != nil {
		return false
	}
	brain, err := os.Stat(brainDir)
	return err == nil && os.SameFile(output, brain)
}

// writeSeedRisks writes seed/risks.md from the scan's warnings. The prune runs
// after seed/risks.md is first written, so what it adds is written again with
// this.
func writeSeedRisks(outputDir string, scan *seedScanResult) error {
	return brainstore.WriteBrainRelativeFileAtomic(outputDir, filepath.ToSlash(filepath.Join(seedDirName, "risks.md")), []byte(renderSeedRisks(*scan)), 0o600)
}

// dropSeedDocument records that a selected document did not make it into the
// seed, everywhere a reader would look: on the document, in the file index, and
// in the warnings the refresh prints.
//
// One function because there are several ways to fail here and they were not
// reported consistently — a failed extraction warned, an unreadable file did
// not, and a rejected path said nothing at all. A document that is selected and
// then silently dropped is the failure this whole area exists to prevent.
func dropSeedDocument(scan *seedScanResult, doc *seedDocument, reason string) {
	doc.Reason = reason
	doc.SeedPath = ""
	doc.Extracted = false
	scan.Warnings = append(scan.Warnings, fmt.Sprintf("document not indexed: %s: %s", doc.Path, reason))
	markSeedFileNotIndexed(scan, doc.Path, reason)
}

// markSeedFileNotIndexed corrects the file index for a document that was
// selected and then could not be read.
//
// inspectSeedFile marks an extractable document included before extraction is
// attempted — it cannot know whether a PDF has a text layer without opening it
// — so the index records the optimistic answer and this is where it is
// corrected. Leaving it uncorrected reports a scanned PDF as successfully
// included in the one file people are told to consult for exactly that.
func markSeedFileNotIndexed(scan *seedScanResult, path, reason string) {
	path = filepath.ToSlash(path)
	for i := range scan.Files {
		if filepath.ToSlash(scan.Files[i].Path) != path {
			continue
		}
		scan.Files[i].Included = false
		scan.Files[i].Reason = reason
		return
	}
}

// writeSeedDocs copies the selected high-signal docs into the seed, truncating each
// to the configured maximum source bytes. The copy updates document metadata
// to describe the bytes actually read, excluding any truncation marker.
func writeSeedDocs(outputDir string, scan *seedScanResult) error {
	scan.writtenDocs = map[string]bool{}
	scan.replacedDocs = map[string]bool{}
	copies := &seedDocCopies{outputDir: outputDir, scan: scan}
	err := prepareSeedDocs(outputDir, scan, copies)
	// The copies prepared before a failure are still made, as they were when
	// each was written in turn.
	return errors.Join(err, copies.flush())
}

// prepareSeedDocs reads each document and queues its copy in copies, apart
// from the writes so that writeSeedDocs still makes the copies queued before
// a document that fails.
func prepareSeedDocs(outputDir string, scan *seedScanResult, copies *seedDocCopies) error {
	limit := scan.MaxFileBytes
	if limit <= 0 {
		limit = defaultSeedMaxFileBytes
	}
	for i := range scan.Docs {
		doc := &scan.Docs[i]
		cleanSrc := filepath.Clean(filepath.FromSlash(doc.Path))
		// Every branch that drops a document says so. These two used to
		// `continue` in silence, so a document selected during the scan and
		// rejected here vanished with no warning and no entry in the file
		// index — the same optimistic answer the rest of this function was
		// changed to stop giving.
		if filepath.IsAbs(cleanSrc) || cleanSrc == "." || strings.HasPrefix(cleanSrc, ".."+string(filepath.Separator)) {
			dropSeedDocument(scan, doc, "unsafe source path")
			continue
		}
		if err := clibase.RejectSymlinkPathComponents(scan.contentRoot(), cleanSrc); err != nil {
			dropSeedDocument(scan, doc, fmt.Sprintf("not read: %v", err))
			continue
		}
		cleanDst := filepath.Clean(filepath.FromSlash(doc.SeedPath))
		if filepath.IsAbs(cleanDst) || cleanDst == "." || strings.HasPrefix(cleanDst, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe seed document path: %s", doc.SeedPath)
		}
		dir := filepath.Dir(cleanDst)
		if dir != "." {
			if err := clibase.RejectExistingSymlinkPathComponents(outputDir, dir); err != nil {
				return fmt.Errorf("validate seed document directory %s: %w", dir, err)
			}
		}
		src := filepath.Join(scan.contentRoot(), cleanSrc)
		if err := clibase.RejectExistingSymlinkPathComponents(outputDir, cleanDst); err != nil {
			return fmt.Errorf("validate seed document path %s: %w", cleanDst, err)
		}
		if doc.Extracted {
			// A container has to be read whole — a PDF's cross-references and a
			// zip's central directory both live at the end, so the prefix read
			// below would hand the extractor a file it cannot parse. The output
			// is bounded instead, by docextract.ExtractDocumentText.
			data, err := clibase.SafeReadFile(src, docextract.MaxExtractDocumentBytes)
			if err != nil {
				dropSeedDocument(scan, doc, fmt.Sprintf("not read: %v", err))
				continue
			}
			text, err := docextract.ExtractDocumentText(doc.Path, data)
			if err != nil {
				// Record why and move on. A document nobody can read must not
				// fail the refresh, and it must not be silently absent either:
				// the reason lands in the seed's file index and the warning
				// list, where `status` and the overview surface it.
				dropSeedDocument(scan, doc, documentExtractionReason(err))
				continue
			}
			rendered := renderExtractedDocument(doc.Path, text)
			doc.Bytes = int64(len(rendered))
			if err := copies.add(filepath.ToSlash(cleanDst), []byte(rendered)); err != nil {
				return err
			}
			continue
		}
		// Read only what can survive the truncation below, plus one byte to
		// detect that truncation is needed. Slurping the whole file first made
		// the peak allocation the SOURCE file's size, which for a doc that is
		// kept precisely because it is oversized is pure waste.
		data, truncated, err := clibase.ReadFilePrefix(src, limit)
		if err != nil {
			// Said, like every other drop: the prune that follows a committed
			// refresh removes an earlier copy of a document this run did not
			// write, so a silent skip would leave it recorded as seeded with
			// nothing on disk.
			dropSeedDocument(scan, doc, fmt.Sprintf("not read: %v", err))
			continue
		}
		doc.Bytes = int64(len(data))
		doc.Truncated = truncated
		if truncated {
			data = append(data, []byte("\n\n[truncated]\n")...)
		}
		if err := copies.add(filepath.ToSlash(cleanDst), data); err != nil {
			return err
		}
	}
	return nil
}

// Bounds on the copies held in memory between two claims: each claim
// rewrites the whole pending marker, so claiming copy by copy would cost
// time quadratic in their number.
const (
	seedCopyBatchDocs  = 64
	seedCopyBatchBytes = 8 << 20
)

// seedDocCopy is one document's copy, ready to write. replaced says its
// path already named a file, or could not be checked, before this seed.
type seedDocCopy struct {
	rel      string
	data     []byte
	replaced bool
}

// seedDocCopies makes a seed's copies in batches, each claimed in the
// pending marker first when the scan has a copyClaimer.
type seedDocCopies struct {
	outputDir string
	scan      *seedScanResult
	batch     []seedDocCopy
	bytes     int
}

// add queues a copy at rel, whose symlinked components the caller has
// refused, and makes the batch once it is full.
func (c *seedDocCopies) add(rel string, data []byte) error {
	_, err := os.Lstat(filepath.Join(c.outputDir, filepath.FromSlash(rel)))
	c.batch = append(c.batch, seedDocCopy{rel: rel, data: data, replaced: !errors.Is(err, fs.ErrNotExist)})
	c.bytes += len(data)
	if len(c.batch) < seedCopyBatchDocs && c.bytes < seedCopyBatchBytes {
		return nil
	}
	return c.flush()
}

// flush claims and writes the queued copies, stopping at the first that
// fails. A copy goes in scan.writtenDocs once it is on disk, which includes
// a write that reports the replacement live but not durably synced: the copy
// is there, and the marker and the prune must know it is this seed's. One
// whose path named a file before also goes in scan.replacedDocs.
func (c *seedDocCopies) flush() error {
	batch := c.batch
	c.batch, c.bytes = nil, 0
	if len(batch) == 0 {
		return nil
	}
	if c.scan.copyClaimer != nil {
		if err := c.scan.copyClaimer.claim(batch); err != nil {
			return err
		}
	}
	for _, cp := range batch {
		if cp.replaced {
			c.scan.replacedDocs[cp.rel] = true
		}
		err := brainstore.WriteBrainRelativeFileAtomic(c.outputDir, cp.rel, cp.data, 0o600)
		if err == nil || seedWriteIsLive(err) {
			c.scan.writtenDocs[cp.rel] = true
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// seedWriteIsLive reports whether a failed atomic write replaced its file all
// the same: the rename took effect and only the directory sync after it
// failed.
func seedWriteIsLive(err error) bool {
	var live *brainstore.AtomicReplaceDurabilityError
	return errors.As(err, &live)
}

// seedManifestIsPublished reports whether a failed manifest write still left
// the new manifest in place, listing every copy this seed wrote: its rename
// took effect (seedWriteIsLive), or only the README written after it failed.
// Those copies are then recorded and stay.
func seedManifestIsPublished(err error) bool {
	var readme *brainReadmeWriteError
	return seedWriteIsLive(err) || errors.As(err, &readme)
}

// seedDocsOwnership says which files under seed/docs a prune may remove.
type seedDocsOwnership struct {
	// all is set for the brain store, where nothing but a seed writes
	// seed/docs.
	all bool
	// manifested holds, for any other --output, the documents the seed there
	// before this one listed in its manifest, and recorded those and the ones
	// an earlier seed's pending marker still lists. Only those are a seed's
	// to remove: --update accepts any existing directory, which can hold
	// files of its own under seed/docs (another repository's, say).
	manifested map[string]bool
	recorded   map[string]bool
	// claims holds, for a recorded document only a claim in the pending
	// marker lists (seedCopyClaimer), the claim its file matched, so that a
	// rewrite of the marker keeps it a claim. unreadClaims holds the claims
	// whose file could not be checked, by the error that stopped it: the
	// path is not the seed's to remove, but the marker keeps the claims for
	// a seed that can read the file.
	claims       map[string][]seedCopyClaim
	unreadClaims map[string]seedUnreadClaims
	// marker says what is at seedPrunePendingPath in an explicit --output.
	marker seedPruneMarker
	// stale lists, once beginSeedPrune has run, the recorded documents this
	// seed did not write: the ones its prune is to remove.
	stale []string
	// unrecorded lists, once beginSeedPrune has run, the copies this seed
	// made where no file was before (not scan.replacedDocs) and that no
	// record lists, when it could not record them: a seed that then stops
	// before its manifest removes them (removeUnrecordedSeedCopies).
	unrecorded []string
}

// seedPruneMarker is the state of an explicit --output's pending marker.
type seedPruneMarker int

const (
	seedPruneMarkerAbsent  seedPruneMarker = iota
	seedPruneMarkerOurs                    // a list a seed wrote (seedPrunePendingListHeader)
	seedPruneMarkerForeign                 // anything else by that name, never written or removed
)

// seedDocsOwnershipOf reads which files under outputDir's seed/docs a prune
// may remove. It runs before the new manifest replaces the one it reads.
func seedDocsOwnershipOf(outputDir string, inBrainStore bool) seedDocsOwnership {
	if inBrainStore {
		return seedDocsOwnership{all: true}
	}
	manifested := map[string]bool{}
	manifest, err := bmanifest.Load(outputDir)
	if err == nil && manifest.Sources != nil && manifest.Sources.Seed != nil {
		for _, doc := range manifest.Sources.Seed.Documents {
			if doc.SeedPath != "" {
				manifested[filepath.ToSlash(filepath.Clean(filepath.FromSlash(doc.SeedPath)))] = true
			}
		}
	}
	ownership := seedDocsOwnership{
		manifested:   manifested,
		recorded:     maps.Clone(manifested),
		claims:       map[string][]seedCopyClaim{},
		unreadClaims: map[string]seedUnreadClaims{},
	}
	listed, claims, marker := readSeedPruneMarkerEntries(outputDir)
	ownership.marker = marker
	for _, rel := range listed {
		pathClaims, claimed := claims[rel]
		if !claimed || manifested[rel] {
			ownership.recorded[rel] = true
			continue
		}
		claim, matched, err := seedCopyClaimMatch(outputDir, rel, pathClaims)
		switch {
		case err != nil:
			ownership.unreadClaims[rel] = seedUnreadClaims{claims: pathClaims, err: err}
		case matched:
			ownership.recorded[rel] = true
			ownership.claims[rel] = []seedCopyClaim{claim}
		}
	}
	return ownership
}

// seedUnreadClaims are the claims for one path whose file a seed could not
// check, and why.
type seedUnreadClaims struct {
	claims []seedCopyClaim
	err    error
}

// readSeedPruneMarker reads the paths an explicit --output's pending marker
// lists (readSeedPruneMarkerEntries).
func readSeedPruneMarker(outputDir string) ([]string, seedPruneMarker) {
	listed, _, marker := readSeedPruneMarkerEntries(outputDir)
	return listed, marker
}

// readSeedPruneMarkerEntries reads an explicit --output's pending marker:
// the paths it lists and, for a copy a seed claimed before making it, the
// claims (seedCopyClaimer): a path may carry several, one line each. A path
// listed on a line of its own is the seed's whatever its file holds, so it
// has no claims. Only a list in the form writeSeedPruneMarker writes, naming
// files under seed/docs, is taken as one: a file of that name in a directory
// --update was pointed at may be that directory's own.
func readSeedPruneMarkerEntries(outputDir string) ([]string, map[string][]seedCopyClaim, seedPruneMarker) {
	text, err := brainstore.ReadBrainRelativeStateFile(outputDir, seedPrunePendingPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, seedPruneMarkerAbsent
	}
	body, ours := strings.CutPrefix(text, seedPrunePendingListHeader)
	if err != nil || !ours {
		return nil, nil, seedPruneMarkerForeign
	}
	var listed []string
	claims := map[string][]seedCopyClaim{}
	plain := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}
		quoted, err := strconv.QuotedPrefix(line)
		if err != nil {
			return nil, nil, seedPruneMarkerForeign
		}
		rel, err := strconv.Unquote(quoted)
		if err != nil || !seedPruneListable(rel) {
			return nil, nil, seedPruneMarkerForeign
		}
		_, seen := claims[rel]
		if !seen && !plain[rel] {
			listed = append(listed, rel)
		}
		rest := line[len(quoted):]
		if rest == "" {
			plain[rel] = true
			continue
		}
		claim, ok := parseSeedCopyClaim(rest)
		if !ok {
			return nil, nil, seedPruneMarkerForeign
		}
		claims[rel] = append(claims[rel], claim)
	}
	for rel := range plain {
		delete(claims, rel)
	}
	return listed, claims, seedPruneMarkerOurs
}

// parseSeedCopyClaim reads the " <size> <sha256>" a claimed path carries.
func parseSeedCopyClaim(text string) (seedCopyClaim, bool) {
	fields := strings.Split(text, " ")
	if len(fields) != 3 || fields[0] != "" {
		return seedCopyClaim{}, false
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || size < 0 || strconv.FormatInt(size, 10) != fields[1] {
		return seedCopyClaim{}, false
	}
	if digest, err := hex.DecodeString(fields[2]); err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != fields[2] {
		return seedCopyClaim{}, false
	}
	return seedCopyClaim{size: size, digest: fields[2]}, true
}

// seedPruneListable reports whether rel is a path a seed's pending marker
// may list: a clean slash path under seed/docs that does not climb out.
func seedPruneListable(rel string) bool {
	return strings.HasPrefix(rel, seedDirName+"/"+seedDocsDirName+"/") &&
		filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel))) == rel &&
		!slices.Contains(strings.Split(rel, "/"), "..")
}

// seedCopyClaim is what an explicit --output's pending marker records for
// a copy a seed is about to make: its size and SHA-256. The path counts as
// the seed's only while the file there still has both, so a claim whose copy
// was never made does not make a file someone puts there later a seed's.
type seedCopyClaim struct {
	size   int64
	digest string
}

func seedCopyClaimOf(data []byte) seedCopyClaim {
	sum := sha256.Sum256(data)
	return seedCopyClaim{size: int64(len(data)), digest: hex.EncodeToString(sum[:])}
}

// seedCopyClaimMatch returns the one of claims that describes the file at
// rel, if one does. A path through a symlink, a file that is gone or not
// regular, and one with other bytes match none. A failure to check that may
// pass (seedPruneFailureMayPass), such as a permission, is returned instead,
// since it says nothing of what the file holds.
func seedCopyClaimMatch(outputDir, rel string, claims []seedCopyClaim) (seedCopyClaim, bool, error) {
	if err := clibase.RejectExistingSymlinkPathComponents(outputDir, filepath.FromSlash(rel)); err != nil {
		return seedCopyClaim{}, false, seedClaimCheckFailure(err)
	}
	path := filepath.Join(outputDir, filepath.FromSlash(rel))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !slices.ContainsFunc(claims, func(c seedCopyClaim) bool { return c.size == info.Size() }) {
		return seedCopyClaim{}, false, seedClaimCheckFailure(err)
	}
	data, err := clibase.SafeReadFile(path, info.Size()+1)
	if err != nil {
		return seedCopyClaim{}, false, seedClaimCheckFailure(err)
	}
	got := seedCopyClaimOf(data)
	return got, slices.Contains(claims, got), nil
}

// seedClaimCheckFailure is err when it may pass, and nil otherwise: a file
// that is gone, or a check that refused a symlink, is a definite mismatch.
func seedClaimCheckFailure(err error) error {
	if err == nil || errors.Is(err, fs.ErrNotExist) || !seedPruneFailureMayPass(err) {
		return nil
	}
	return err
}

// seedCopyClaimer journals an explicit --output's new copies in its pending
// marker before they are made: writeSeedDocs hands it each batch of copies
// just before writing them, so that a seed killed while copying, which no
// error path can clean up after, still leaves each copy it made the next
// seed's to remove. It claims a path that no record lists and where no file
// is yet, with the copy's size and digest (seedCopyClaim); a path that
// already names a file may be the output's own and is not claimed. The
// marker keeps what it already listed, claims with their digests: a copy
// written over a claimed one gets its own digest added, not put in place of
// the other, since the seed may stop before writing it. beginSeedPrune
// rewrites the marker to the copies actually made.
type seedCopyClaimer struct {
	outputDir string
	ownership *seedDocsOwnership
	claims    map[string][]seedCopyClaim
}

// newSeedCopyClaimer returns the claimer for an explicit --output, or nil
// where nothing is claimed: the brain store, whose marker goes down before
// any copy, a seed/docs that overlaps the repository, and an output whose
// marker file is not a seed's.
func newSeedCopyClaimer(outputDir string, scan *seedScanResult, ownership *seedDocsOwnership) *seedCopyClaimer {
	if ownership.all || ownership.marker == seedPruneMarkerForeign || seedDocsOverlapRepository(filepath.Join(outputDir, seedDirName, seedDocsDirName), scan.RepoDir) {
		return nil
	}
	claims := map[string][]seedCopyClaim{}
	for rel, pathClaims := range ownership.claims {
		claims[rel] = slices.Clone(pathClaims)
	}
	for rel, unread := range ownership.unreadClaims {
		claims[rel] = slices.Clone(unread.claims)
	}
	return &seedCopyClaimer{outputDir: outputDir, ownership: ownership, claims: claims}
}

// claim writes the marker with the batch's new copies added, if it adds
// any. A seed that cannot write it must not make those copies.
func (c *seedCopyClaimer) claim(batch []seedDocCopy) error {
	added := false
	for _, cp := range batch {
		prior, claimed := c.claims[cp.rel]
		if !claimed && (cp.replaced || c.ownership.recorded[cp.rel] || !seedPruneListable(cp.rel)) {
			continue
		}
		if claim := seedCopyClaimOf(cp.data); !slices.Contains(prior, claim) {
			c.claims[cp.rel] = append(prior, claim)
			added = true
		}
	}
	if !added {
		return nil
	}
	listed := map[string]bool{}
	for rel := range c.ownership.recorded {
		if !c.ownership.manifested[rel] {
			listed[rel] = true
		}
	}
	for rel := range c.claims {
		listed[rel] = true
	}
	if err := writeSeedPruneMarker(c.outputDir, sortedMapKeys(listed), c.claims); err != nil {
		return err
	}
	c.ownership.marker = seedPruneMarkerOurs
	return nil
}

// beginSeedPrune writes an explicit --output's pending marker once the
// documents are written, before anything else can refuse the refresh (the
// brain store's went down before the copies: markBrainStorePrunePending).
// It lists the seed's documents that one of the two manifests does not: the
// recorded ones this seed did not write, which the new manifest drops, and
// the ones it wrote that the manifest there now does not list, which a
// refresh refused before the new manifest is written, or a failed write of
// it, would leave unrecorded. There is none when there are none, when
// seed/docs overlaps the repository (nothing is pruned there), or when a
// file of that name is not a seed's. In that last case, and when the
// marker cannot be written, ownership.unrecorded lists the copies no record
// holds that are where no file was before this seed.
func beginSeedPrune(outputDir string, scan *seedScanResult, ownership *seedDocsOwnership) error {
	if ownership.all || seedDocsOverlapRepository(filepath.Join(outputDir, seedDirName, seedDocsDirName), scan.RepoDir) {
		return nil
	}
	for rel := range ownership.recorded {
		if !scan.writtenDocs[rel] {
			ownership.stale = append(ownership.stale, rel)
		}
	}
	sort.Strings(ownership.stale)
	pending := slices.Clone(ownership.stale)
	claims := map[string][]seedCopyClaim{}
	for _, rel := range ownership.stale {
		if pathClaims, claimed := ownership.claims[rel]; claimed {
			claims[rel] = pathClaims
		}
	}
	// A claim this seed could not check stays for the next one, unless the
	// seed wrote that path again: the copy there is then its own.
	for rel, unread := range ownership.unreadClaims {
		if scan.writtenDocs[rel] {
			delete(ownership.unreadClaims, rel)
			continue
		}
		pending = append(pending, rel)
		claims[rel] = unread.claims
	}
	var unrecorded []string
	for rel := range scan.writtenDocs {
		if !ownership.manifested[rel] {
			pending = append(pending, rel)
		}
		if !ownership.recorded[rel] && !scan.replacedDocs[rel] {
			unrecorded = append(unrecorded, rel)
		}
	}
	if len(pending) == 0 {
		if ownership.marker != seedPruneMarkerOurs {
			return nil
		}
		// What is there holds only claims whose copies were not made
		// (seedCopyClaimer): nothing for the marker to carry.
		if err := brainstore.RemoveBrainRelativeFile(outputDir, seedPrunePendingPath); err != nil {
			return err
		}
		ownership.marker = seedPruneMarkerAbsent
		return nil
	}
	sort.Strings(unrecorded)
	if ownership.marker == seedPruneMarkerForeign {
		ownership.unrecorded = unrecorded
		if len(ownership.stale) > 0 {
			scan.Warnings = append(scan.Warnings, fmt.Sprintf("old seed documents not recorded for a retry: %s is not a seed's, so a document this seed cannot remove now is left for good", seedPrunePendingPath))
		}
		return nil
	}
	sort.Strings(pending)
	err := writeSeedPruneMarker(outputDir, pending, claims)
	if err != nil && !seedWriteIsLive(err) {
		ownership.unrecorded = unrecorded
		return err
	}
	ownership.marker = seedPruneMarkerOurs
	return err
}

// markBrainStorePrunePending writes the brain store's pending marker, which
// lists nothing: every file under its seed/docs is a seed's.
func markBrainStorePrunePending(outputDir string, ownership seedDocsOwnership) error {
	if !ownership.all {
		return nil
	}
	return brainstore.WriteBrainRelativeFileAtomic(outputDir, seedPrunePendingPath, []byte(seedPrunePendingText), 0o600)
}

// removeUnrecordedSeedCopies removes the copies a seed made that no record
// lists (ownership.unrecorded) because it is stopping, with err, before its
// manifest lists them: left there, a copy whose source is gone before the
// next seed would be kept as not a seed's and stay searchable. Each was
// written by this seed, at a path no seed recorded and where no file was
// before it: a path that named one, such as a recorded document under
// another spelling the filesystem folds to it or a file that is not a
// seed's, is left as a file, though with this seed's bytes. A copy it cannot
// remove is named in the error returned with err.
func removeUnrecordedSeedCopies(outputDir string, ownership seedDocsOwnership, err error) error {
	errs := []error{err}
	for _, rel := range ownership.unrecorded {
		if removeErr := brainstore.RemoveBrainRelativeFile(outputDir, rel); removeErr != nil {
			errs = append(errs, fmt.Errorf("remove seed document copy no record lists: %s: %w", rel, removeErr))
		}
	}
	return errors.Join(errs...)
}

// writeSeedPruneMarker writes an explicit --output's pending marker listing
// pending.
func writeSeedPruneMarker(outputDir string, pending []string, claims map[string][]seedCopyClaim) error {
	var text strings.Builder
	text.WriteString(seedPrunePendingListHeader)
	for _, rel := range pending {
		if len(claims[rel]) == 0 {
			text.WriteString(strconv.Quote(rel) + "\n")
		}
		for _, claim := range claims[rel] {
			text.WriteString(strconv.Quote(rel) + " " + strconv.FormatInt(claim.size, 10) + " " + claim.digest + "\n")
		}
	}
	return brainstore.WriteBrainRelativeFileAtomic(outputDir, seedPrunePendingPath, []byte(text.String()), 0o600)
}

// finishSeedPrune settles the pending marker once the new manifest is saved
// and the prune has run. In the brain store it stays when the prune asked
// for a retry, so that refresh reseeds. In any other --output it then lists
// just the stale documents still there, since the new manifest lists the
// rest: a path the prune emptied must not stay the seed's, or a file someone
// puts there later would be removed as one.
func finishSeedPrune(outputDir string, ownership seedDocsOwnership, retry bool) error {
	if ownership.all {
		if retry {
			return nil
		}
		return brainstore.RemoveBrainRelativeFile(outputDir, seedPrunePendingPath)
	}
	if ownership.marker != seedPruneMarkerOurs {
		return nil
	}
	var left []string
	claims := map[string][]seedCopyClaim{}
	if retry {
		for _, rel := range ownership.stale {
			if _, err := os.Lstat(filepath.Join(outputDir, filepath.FromSlash(rel))); !errors.Is(err, fs.ErrNotExist) {
				left = append(left, rel)
				if pathClaims, claimed := ownership.claims[rel]; claimed {
					claims[rel] = pathClaims
				}
			}
		}
	}
	for rel, unread := range ownership.unreadClaims {
		left = append(left, rel)
		claims[rel] = unread.claims
	}
	if len(left) == 0 {
		return brainstore.RemoveBrainRelativeFile(outputDir, seedPrunePendingPath)
	}
	sort.Strings(left)
	return writeSeedPruneMarker(outputDir, left, claims)
}

// pruneSeedDocs removes every file under seed/docs that this seed did not
// write and that ownership says is a seed's, so the directory holds exactly
// the documents of the current scan. Without it a reseed only ever added: a
// document deleted from the repository, or one an earlier --worktree seed
// copied while it was untracked, stayed on disk, and the doc index
// (loadDocRecordsFromSeed reads every .md under seed/) kept returning it
// although file-index.json no longer listed it.
//
// A file that cannot be removed safely is left and reported as a warning
// rather than failing the seed, as is a file that is not a seed's, and the
// whole directory when a component of seed/docs is a symlink, or when
// seed/docs and the repository overlap: an --update into the repository
// itself (which skips the output checks a fresh or --force seed makes) would
// otherwise delete the repository's own files under seed/docs.
//
// It reports whether a later seed should try again: reading the directory,
// checking a path or removing a file failed with a filesystem error, which
// may pass. A symlink, a file that is not a seed's and a directory that
// overlaps the repository are left by design, and stay until someone removes
// them, so they never ask for a retry.
func pruneSeedDocs(outputDir string, scan *seedScanResult, written map[string]bool, ownership seedDocsOwnership) (retry bool) {
	docsRel := filepath.Join(seedDirName, seedDocsDirName)
	if err := clibase.RejectExistingSymlinkPathComponents(outputDir, docsRel); err != nil {
		scan.Warnings = append(scan.Warnings, fmt.Sprintf("old seed documents not removed: %v", err))
		return seedPruneFailureMayPass(err)
	}
	if docsDir := filepath.Join(outputDir, docsRel); seedDocsOverlapRepository(docsDir, scan.RepoDir) {
		scan.Warnings = append(scan.Warnings, fmt.Sprintf("old seed documents not removed: %s overlaps the repository, so a file there may be one of its own", docsDir))
		return false
	}
	prune := &seedDocsPrune{
		outputDir:     outputDir,
		docsRel:       filepath.ToSlash(docsRel),
		scan:          scan,
		all:           ownership.all,
		unreadClaims:  ownership.unreadClaims,
		listed:        map[string]bool{},
		emptied:       map[string]bool{},
		uncheckedSeen: map[string]bool{},
	}
	if err := filepath.WalkDir(filepath.Join(outputDir, docsRel), prune.visit); err != nil {
		prune.unchecked(err)
	}
	// Matched once the whole directory is listed: a recorded name is the
	// seed's under another spelling only when it is not listed itself. When
	// the walk could not list everything, a recorded name it missed may be
	// an entry of its own all the same, and identity would take a hard link
	// to it for another spelling, so recorded documents then match by name
	// alone. The walk failure asks for a retry, which gets the rest.
	prune.written = seedDocsMatcher(outputDir, written, nil, prune.uncheckedIdentity)
	if prune.incomplete {
		prune.recorded = func(rel string) bool { return ownership.recorded[rel] }
	} else {
		prune.recorded = seedDocsMatcher(outputDir, ownership.recorded, prune.listed, prune.uncheckedIdentity)
	}
	for _, file := range prune.files {
		prune.consider(file)
	}
	prune.removeEmptiedDirs()
	return prune.retry
}

// seedPruneFailureMayPass reports whether a prune failure is a filesystem
// error, such as a permission, that a later try may get past, rather than a
// refusal (a symlink, a path that escapes) that it never will.
func seedPruneFailureMayPass(err error) bool {
	var pathErr *fs.PathError
	return errors.As(err, &pathErr)
}

// seedDocsPrune is one pruneSeedDocs pass.
type seedDocsPrune struct {
	outputDir string
	docsRel   string
	scan      *seedScanResult
	all       bool
	written   func(rel string) bool
	recorded  func(rel string) bool
	// unreadClaims are the claimed paths whose file could not be checked
	// (seedDocsOwnership.unreadClaims): left, and reported with the reason.
	unreadClaims map[string]seedUnreadClaims
	// files and dirs list what is under seed/docs, in walk order, and listed
	// the names of those files; incomplete says the walk could not read all
	// of it. emptied holds the directories a removal took a file from,
	// directly or below.
	files      []seedDocsFile
	dirs       []string
	listed     map[string]bool
	incomplete bool
	emptied    map[string]bool
	retry      bool
	// uncheckedSeen holds the identity warnings already given, since both
	// matchers may check the same path.
	uncheckedSeen map[string]bool
}

// seedDocsFile is a non-directory entry under seed/docs.
type seedDocsFile struct {
	rel     string
	symlink bool
}

func (p *seedDocsPrune) visit(path string, d fs.DirEntry, err error) error {
	// What is not there has nothing to remove: a seed that copied no
	// document never made seed/docs, and a retry for it would come back on
	// every refresh.
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		p.unchecked(err)
		return nil
	}
	rel, relErr := filepath.Rel(p.outputDir, path)
	if relErr != nil {
		return nil
	}
	rel = filepath.ToSlash(rel)
	if d.IsDir() {
		if rel != p.docsRel {
			p.dirs = append(p.dirs, rel)
		}
		return nil
	}
	p.listed[rel] = true
	p.files = append(p.files, seedDocsFile{rel: rel, symlink: d.Type()&fs.ModeSymlink != 0})
	return nil
}

// consider removes one file, if this seed did not write it and it is a
// seed's.
func (p *seedDocsPrune) consider(file seedDocsFile) {
	switch {
	case p.written(file.rel):
	case p.unreadClaims[file.rel].err != nil:
		p.scan.Warnings = append(p.scan.Warnings, fmt.Sprintf("old seed document not removed: %s: its claim could not be checked: %v", file.rel, p.unreadClaims[file.rel].err))
		p.retry = true
	case !p.all && !p.recorded(file.rel):
		p.scan.Warnings = append(p.scan.Warnings, fmt.Sprintf("old seed document not removed: %s: no earlier seed of this output recorded it, so it may not be a seed's", file.rel))
	default:
		if err := brainstore.RemoveBrainRelativeFile(p.outputDir, file.rel); err != nil {
			p.scan.Warnings = append(p.scan.Warnings, fmt.Sprintf("old seed document not removed: %s: %v", file.rel, err))
			p.retry = p.retry || (!file.symlink && seedPruneFailureMayPass(err))
			return
		}
		for dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(file.rel))); dir != p.docsRel && dir != "."; dir = filepath.ToSlash(filepath.Dir(filepath.FromSlash(dir))) {
			p.emptied[dir] = true
		}
	}
}

// uncheckedIdentity records a path whose identity a matcher could not
// check. The answer may then be wrong for a file that is another spelling
// of it, so a failure that may pass asks for a retry, which keeps the
// pending marker and with it the recorded path.
func (p *seedDocsPrune) uncheckedIdentity(err error) {
	p.retry = p.retry || seedPruneFailureMayPass(err)
	warning := fmt.Sprintf("old seed document not checked: %v", err)
	if !p.uncheckedSeen[warning] {
		p.uncheckedSeen[warning] = true
		p.scan.Warnings = append(p.scan.Warnings, warning)
	}
}

// unchecked records a part of seed/docs the walk could not read.
func (p *seedDocsPrune) unchecked(err error) {
	p.scan.Warnings = append(p.scan.Warnings, fmt.Sprintf("old seed documents not checked: %v", err))
	p.incomplete = true
	p.retry = true
}

// removeEmptiedDirs removes the directories the prune left empty, deepest
// first, so a directory emptied by removing its children goes too. In the
// brain store that is any empty directory; elsewhere only one a removal took
// a file from, since an empty directory there may be someone else's. A
// directory that still holds anything is left by os.Remove.
func (p *seedDocsPrune) removeEmptiedDirs() {
	for i := len(p.dirs) - 1; i >= 0; i-- {
		dir := p.dirs[i]
		if !p.all && !p.emptied[dir] {
			continue
		}
		if clibase.RejectExistingSymlinkPathComponents(p.outputDir, dir) == nil {
			_ = os.Remove(filepath.Join(p.outputDir, filepath.FromSlash(dir)))
		}
	}
}

// seedDocsMatcher reports whether a file under outputDir is one of names: by
// name, or else by filesystem identity. On a case-insensitive filesystem a
// document renamed only in case is written over the old directory entry,
// which can keep the old spelling, so a name alone does not tell.
//
// With listed nil, for the names a prune keeps, identity alone counts, since
// the filesystem's folding need not match Go's. With listed given, for the
// names a prune removes by, identity counts only where it is a name's other
// spelling: for a name that is not itself listed (a listed name is an entry
// of its own), and a file whose name folds to the same (strings.EqualFold,
// which also folds Σ, σ and ς together). Another file with that identity is
// a hard link, a second entry that names never made one of names. A name
// reached through a symlinked directory lends no identity either: os.Lstat
// follows every component but the last, so the name would stand for whatever
// file the link points at, which the walk lists under its own path and no
// seed recorded. A filesystem that folds more than Go does leaves a file,
// which is the safe way to miss. A name or file whose identity cannot be
// read for a reason other than its absence goes to unchecked: a file is
// then left, but the caller must retry rather than take the answer as
// final.
func seedDocsMatcher(outputDir string, names, listed map[string]bool, unchecked func(error)) func(rel string) bool {
	type spelling struct {
		name string
		info os.FileInfo
	}
	others := make([]spelling, 0, len(names))
	for name := range names {
		if listed[name] {
			continue
		}
		info, err := os.Lstat(filepath.Join(outputDir, filepath.FromSlash(name)))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				unchecked(err)
			}
			continue
		}
		if listed != nil {
			if err := clibase.RejectExistingSymlinkPathComponents(outputDir, filepath.FromSlash(name)); err != nil {
				if seedPruneFailureMayPass(err) {
					unchecked(err)
				}
				continue
			}
		}
		others = append(others, spelling{name: name, info: info})
	}
	return func(rel string) bool {
		if names[rel] {
			return true
		}
		if len(others) == 0 {
			return false
		}
		have, err := os.Lstat(filepath.Join(outputDir, filepath.FromSlash(rel)))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				unchecked(err)
			}
			return false
		}
		for _, other := range others {
			if (listed == nil || strings.EqualFold(rel, other.name)) && os.SameFile(have, other.info) {
				return true
			}
		}
		return false
	}
}

// seedDocsOverlapRepository reports whether docsDir and the repository share
// any path: either one inside the other.
func seedDocsOverlapRepository(docsDir, repoDir string) bool {
	if strings.TrimSpace(repoDir) == "" {
		return false
	}
	return seedPathWithin(docsDir, repoDir) || seedPathWithin(repoDir, docsDir)
}

// seedPathWithin reports whether path is root or lies under it. Each ancestor
// of path, as given and with symlinks resolved, is compared with root by
// filesystem identity rather than by spelling: on a case-insensitive
// filesystem /work/Repo and /work/repo are one directory, which no comparison
// of the two strings can tell.
func seedPathWithin(path, root string) bool {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	for _, spelling := range seedPathSpellings(path) {
		for dir := spelling; ; dir = filepath.Dir(dir) {
			if info, err := os.Stat(dir); err == nil && os.SameFile(info, rootInfo) {
				return true
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return false
}

// seedPathSpellings returns path made absolute, and also with its symlinks
// resolved when that differs. A path that does not exist yet is resolved
// through its deepest ancestor that does.
func seedPathSpellings(path string) []string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	spellings := []string{abs}
	rest := ""
	for dir := abs; ; dir = filepath.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			spellings = append(spellings, filepath.Join(resolved, rest))
			break
		}
		if filepath.Dir(dir) == dir {
			break
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
	return uniqueCleanPaths(spellings)
}

// renderExtractedDocument wraps extracted text as markdown, with a header that
// says where it came from. Without it a reader of the brain directory finds a
// .md file with no indication that it is a derived rendering of a PDF — and a
// retrieved chunk would cite a path that is not the document anybody has.
func renderExtractedDocument(sourcePath, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", filepath.Base(sourcePath))
	fmt.Fprintf(&b, "> Text extracted from `%s`. This file is derived; edit the source document.\n\n", sourcePath)
	b.WriteString(text)
	b.WriteString("\n")
	return b.String()
}

// documentExtractionReason is the short form for the seed file index, where a
// full error would crowd out every other entry.
func documentExtractionReason(err error) string {
	var extractErr *docextract.DocumentExtractionError
	if errors.As(err, &extractErr) {
		return "not indexed: " + extractErr.Reason
	}
	return "not indexed"
}

func renderSeedOverview(scan seedScanResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Seeded Repository Overview")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- Repo: `%s`\n", scan.RepoDir)
	if scan.Commit != "" {
		fmt.Fprintf(&b, "- Commit: `%s`\n", scan.Commit)
	}
	fmt.Fprintf(&b, "- Files indexed: %d\n", len(scan.Files))
	fmt.Fprintf(&b, "- Documents copied: %d\n", len(scan.Docs))
	fmt.Fprintf(&b, "- Entrypoints detected: %d\n", len(scan.Entrypoints))
	fmt.Fprintf(&b, "- Commands detected: %d\n", len(scan.Commands))
	if scan.Coverage != nil {
		fmt.Fprintf(&b, "- Commits without session coverage after oldest session: %d\n", scan.Coverage.MissingSessionCommits)
	}
	return b.String()
}

func renderSeedArchitecture(scan seedScanResult) string {
	counts := make(map[string]int)
	for _, file := range scan.Files {
		counts[file.Category]++
	}
	var b strings.Builder
	fmt.Fprintln(&b, "# Architecture")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## File Categories")
	fmt.Fprintln(&b)
	for _, key := range sortedMapKeys(counts) {
		fmt.Fprintf(&b, "- %s: %d\n", key, counts[key])
	}
	if len(scan.Entrypoints) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Entrypoints")
		fmt.Fprintln(&b)
		for _, path := range scan.Entrypoints {
			fmt.Fprintf(&b, "- `%s`\n", path)
		}
	}
	return b.String()
}

func renderSeedCommands(scan seedScanResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Commands")
	fmt.Fprintln(&b)
	if len(scan.Commands) == 0 {
		fmt.Fprintln(&b, "No commands were detected from recognized project metadata.")
		return b.String()
	}
	for _, command := range scan.Commands {
		fmt.Fprintf(&b, "- `%s`: `%s` (%s)\n", command.Name, command.Command, command.Source)
	}
	return b.String()
}

func renderSeedConventions(scan seedScanResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Conventions")
	fmt.Fprintln(&b)
	if len(scan.Docs) == 0 {
		fmt.Fprintln(&b, "No high-signal convention documents were detected.")
		return b.String()
	}
	fmt.Fprintln(&b, "Review these copied documents first:")
	for _, doc := range scan.Docs {
		fmt.Fprintf(&b, "- `%s`\n", doc.SeedPath)
	}
	return b.String()
}

func renderSeedRisks(scan seedScanResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Risks And Gaps")
	fmt.Fprintln(&b)
	for _, warning := range scan.Warnings {
		fmt.Fprintf(&b, "- %s\n", warning)
	}
	var skipped int
	for _, file := range scan.Files {
		if !file.Included {
			skipped++
		}
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "- %d files were skipped. See `file-index.json` for reasons.\n", skipped)
	}
	if scan.Coverage != nil && scan.Coverage.MissingSessionCommits > 0 {
		fmt.Fprintf(&b, "- %d commits after the oldest exported session have no Entire checkpoint trailer. See `history-gaps.md`.\n", scan.Coverage.MissingSessionCommits)
	}
	if skipped == 0 && len(scan.Warnings) == 0 && (scan.Coverage == nil || scan.Coverage.MissingSessionCommits == 0) {
		fmt.Fprintln(&b, "No deterministic seed risks were detected.")
	}
	return b.String()
}

func renderSeedHistoryGaps(scan seedScanResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# History Coverage")
	fmt.Fprintln(&b)
	if scan.Coverage == nil {
		fmt.Fprintln(&b, "Commit coverage could not be computed.")
		return b.String()
	}
	coverage := scan.Coverage
	fmt.Fprintf(&b, "- Total commits: %d\n", coverage.TotalCommits)
	fmt.Fprintf(&b, "- Pre-session commits: %d\n", coverage.PreSessionCommits)
	fmt.Fprintf(&b, "- Covered (checkpoint trailer, at or after oldest session): %d\n", coverage.CoveredCommits)
	fmt.Fprintf(&b, "- Missing session coverage after oldest session: %d\n", coverage.MissingSessionCommits)
	fmt.Fprintf(&b, "- Commits with no session history available: %d\n", coverage.NoSessionHistoryCommits)
	fmt.Fprintf(&b, "- Merge commits: %d\n", coverage.MergeCommits)
	if coverage.OldestSessionAt != nil {
		fmt.Fprintf(&b, "- Oldest exported session: `%s`\n", coverage.OldestSessionAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "- Exported checkpoints known to manifest: %d\n", coverage.ExportedCheckpoints)
	fmt.Fprintln(&b)
	if len(coverage.UncoveredCommits) == 0 {
		fmt.Fprintln(&b, "No commits need fallback history coverage.")
		return b.String()
	}
	fmt.Fprintln(&b, "## Commits Needing Fallback Context")
	fmt.Fprintln(&b)
	for _, commit := range coverage.UncoveredCommits {
		hash := shortCommitHash(commit.Hash)
		fmt.Fprintf(&b, "- `%s` `%s` %s", hash, commit.CommittedAt.Format(time.RFC3339), commit.Subject)
		if commit.Merge {
			fmt.Fprint(&b, " (merge)")
		}
		fmt.Fprintf(&b, " [%s]\n", commit.Coverage)
		if len(commit.Checkpoints) > 0 {
			fmt.Fprintf(&b, "  Checkpoints: `%s`\n", strings.Join(commit.Checkpoints, "`, `"))
		}
		if commit.BodyExcerpt != "" && commit.BodyExcerpt != commit.Subject {
			fmt.Fprintf(&b, "  Notes: %s\n", commit.BodyExcerpt)
		}
	}
	return b.String()
}

func sortedMapKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func buildSeedHistoryBaseline(ctx context.Context, runner CommandRunner, repoDir, outputDir string) *seedHistoryBaseline {
	var baseline seedHistoryBaseline
	baseline.Confidence = "heuristic"
	for _, root := range splitNonEmptyLines(clibase.GitOutput(ctx, runner, repoDir, "rev-list", "--max-parents=0", "HEAD")) {
		stdout := clibase.GitOutput(ctx, runner, repoDir, "show", "-s", "--format=%aI", root)
		if text := strings.TrimSpace(string(stdout)); text != "" {
			if parsed, err := time.Parse(time.RFC3339, text); err == nil {
				parsed = parsed.UTC()
				if baseline.OldestCommitAt == nil || parsed.Before(*baseline.OldestCommitAt) {
					baseline.OldestCommitAt = &parsed
				}
			}
		}
	}
	manifest, err := bmanifest.Load(outputDir)
	if err == nil && manifest.Sources != nil && manifest.Sources.Sessions != nil {
		baseline.OldestSessionAt = manifest.Sources.Sessions.OldestSessionAt
	}
	switch {
	case baseline.OldestCommitAt != nil && baseline.OldestSessionAt != nil && baseline.OldestCommitAt.Before(*baseline.OldestSessionAt):
		baseline.SeedRequired = true
		baseline.Reason = "oldest commit predates oldest session"
	case baseline.OldestSessionAt == nil:
		baseline.SeedRequired = true
		baseline.Reason = "no session history available"
	default:
		baseline.Reason = "session history covers initial commit or timestamp unavailable"
	}
	return &baseline
}

func buildSeedHistoryCoverage(ctx context.Context, runner CommandRunner, repoDir, outputDir string) *seedHistoryCoverage {
	manifest, err := bmanifest.Load(outputDir)
	if err != nil {
		return &seedHistoryCoverage{
			Path:          filepath.ToSlash(filepath.Join(seedDirName, "history-gaps.md")),
			GeneratedFrom: "git log",
		}
	}
	var sessions *sessionSourceManifest
	if manifest.Sources != nil {
		sessions = manifest.Sources.Sessions
	}
	exportedCheckpoints := make(map[string]struct{})
	var oldestSession *time.Time
	if sessions != nil {
		oldestSession = sessions.OldestSessionAt
		for _, session := range sessions.Sessions {
			if session.LatestCheckpoint != "" {
				exportedCheckpoints[session.LatestCheckpoint] = struct{}{}
			}
		}
	}

	coverage := &seedHistoryCoverage{
		Path:                filepath.ToSlash(filepath.Join(seedDirName, "history-gaps.md")),
		OldestSessionAt:     oldestSession,
		ExportedCheckpoints: len(exportedCheckpoints),
		GeneratedFrom:       "git log",
	}
	commits := parseSeedGitLog(clibase.GitOutput(ctx, runner, repoDir, "log", "--reverse", "--format=%H%x00%P%x00%aI%x00%an%x00%ae%x00%B%x1e"))
	for _, commit := range commits {
		classifySeedCommitCoverage(&commit, oldestSession)
		coverage.TotalCommits++
		if commit.Merge {
			coverage.MergeCommits++
		}
		switch commit.Coverage {
		case "pre_session":
			coverage.PreSessionCommits++
		case "covered":
			coverage.CoveredCommits++
		case "missing_session":
			coverage.MissingSessionCommits++
			coverage.UncoveredCommits = append(coverage.UncoveredCommits, commit)
		case "no_session_history":
			coverage.NoSessionHistoryCommits++
			coverage.UncoveredCommits = append(coverage.UncoveredCommits, commit)
		}
	}
	return coverage
}

// seedManifestMaxUncoveredCommits bounds the per-commit gap list that is copied
// into the brain manifest.
//
// The list had no bound at all, and it is one record per commit that carries no
// Entire checkpoint trailer -- which is EVERY commit of every repository that
// predates Entire. At roughly 420 bytes each (hash, parents, timestamp, author,
// subject, body excerpt) it crosses maxManifestBytes at about 40,000 commits, a
// size an ordinary five-year-old repository reaches, and the manifest writer had
// no ceiling to stop it. The brain then wrote a manifest it could never read
// back: every subsequent command, status and doctor included, failed with
// "manifest.json exceeds maximum size of 16777216 bytes", and nothing short of
// `reset` recovered it. Measured on a 50,000-commit fixture: 28 MB manifest,
// 21 MB of it this one field, and the brain bricked on the very next stage.
//
// 500 rows is a sample, and the point of a sample here is to show the shape of
// the gap (which commits, from when, by whom) rather than to enumerate it. The
// enumeration already has a home: seed/history-gaps.md is rendered from the
// unbounded in-memory record and keeps every row. The counts in this struct are
// totals over all commits and stay exact, so nothing that reads them learns a
// smaller number than the truth.
const seedManifestMaxUncoveredCommits = 500

// manifestSeedHistoryCoverage returns the copy of a coverage record that is safe
// to embed in the brain manifest: identical in every count, with the per-commit
// list bounded and, when it was bounded, saying so.
//
// It copies rather than mutating because the caller's record is what
// writeSeedArtifacts renders seed/history-gaps.md from, and that document is
// supposed to be complete.
func manifestSeedHistoryCoverage(coverage *seedHistoryCoverage) *seedHistoryCoverage {
	if coverage == nil {
		return nil
	}
	if len(coverage.UncoveredCommits) <= seedManifestMaxUncoveredCommits {
		return coverage
	}
	bounded := *coverage
	bounded.UncoveredCommitsTotal = len(coverage.UncoveredCommits)
	bounded.UncoveredCommitsTruncated = true
	bounded.UncoveredCommits = append([]seedCoveredCommit(nil), coverage.UncoveredCommits[:seedManifestMaxUncoveredCommits]...)
	return &bounded
}

func parseSeedGitLog(data []byte) []seedCoveredCommit {
	var commits []seedCoveredCommit
	for _, raw := range strings.Split(string(data), gitLogRecordSeparator) {
		raw = strings.Trim(raw, "\n")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		fields := strings.SplitN(raw, gitLogFieldSeparator, 6)
		if len(fields) < 6 {
			continue
		}
		committedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[2]))
		if err != nil {
			continue
		}
		body := strings.TrimSpace(fields[5])
		parents := splitCommitParents(fields[1])
		commits = append(commits, seedCoveredCommit{
			Hash:        strings.TrimSpace(fields[0]),
			Parents:     parents,
			CommittedAt: committedAt.UTC(),
			AuthorName:  strings.TrimSpace(fields[3]),
			AuthorEmail: strings.TrimSpace(fields[4]),
			Subject:     firstCommitSubject(body),
			Checkpoints: uniqueStrings(checkpointTrailers(body)),
			Merge:       len(parents) > 1,
			BodyExcerpt: commitBodyExcerpt(body),
		})
	}
	return commits
}

// classifySeedCommitCoverage labels a commit by how it relates to the brain's
// session history. Classification is ordered by precedence, not by the trailer
// alone: a commit is no_session_history when no session was exported at all, and
// pre_session when it predates the oldest exported session — both regardless of
// any checkpoint trailer. Only commits at or after the oldest exported session are
// judged by their trailer, where a trailer marks them covered and its absence
// missing_session. In other words a checkpoint trailer makes a commit "covered"
// only at or after the oldest session; an older trailered commit is still
// pre_session. There is no upper bound — every commit from the oldest session
// onward qualifies.
//
// For those commits we do NOT require the trailer to match the session's exported
// latest_checkpoint_id: the manifest records only one checkpoint per session, not
// every intermediate one, so matching on that set would mislabel genuine in-session
// commits as uncovered (e.g. a repo with 47 checkpointed commits across 8 sessions
// showed only ~5 "covered" and the rest "checkpointed_unexported", though all were
// scanned and exported into sessions).
func classifySeedCommitCoverage(commit *seedCoveredCommit, oldestSession *time.Time) {
	if oldestSession == nil {
		commit.Coverage = "no_session_history"
		return
	}
	if commit.CommittedAt.Before(*oldestSession) {
		commit.Coverage = "pre_session"
		return
	}
	if len(commit.Checkpoints) > 0 {
		commit.Coverage = "covered"
		return
	}
	commit.Coverage = "missing_session"
}

func splitCommitParents(value string) []string {
	return strings.Fields(strings.TrimSpace(value))
}

func checkpointTrailers(body string) []string {
	var checkpoints []string
	for _, match := range clibase.CheckpointTrailerRegex.FindAllStringSubmatch(body, -1) {
		if len(match) > 1 {
			checkpoints = append(checkpoints, match[1])
		}
	}
	return checkpoints
}

func firstCommitSubject(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func commitBodyExcerpt(body string) string {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, clibase.CheckpointTrailerKey+":") {
			continue
		}
		lines = append(lines, line)
		if len(strings.Join(lines, " ")) >= 240 {
			break
		}
	}
	excerpt := strings.Join(lines, " ")
	if len(excerpt) > 240 {
		excerpt = excerpt[:240]
	}
	return excerpt
}

func shortCommitHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func seedWorktreeMode(opts seedCommandOptions) string {
	if opts.worktree {
		return "worktree"
	}
	return "tracked"
}

func writeSeedCursor(headPath string, cursor seedCursor) error {
	path := filepath.Join(filepath.Dir(headPath), seedCursorFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeJSONFile(path, cursor)
}

func renderCombinedBrainReadme(manifest exportManifest) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Entire Brain")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "This brain combines seeded repository context with Entire session history when available.")
	if manifest.Sources != nil && manifest.Sources.Seed != nil {
		seed := manifest.Sources.Seed
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Seeded Baseline")
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "- Generated at: `%s`\n", seed.GeneratedAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "- Summary: `%s`\n", seed.SummaryPath)
		fmt.Fprintf(&b, "- Documents: %d\n", len(seed.Documents))
		fmt.Fprintf(&b, "- Entrypoints: %d\n", len(seed.Entrypoints))
		fmt.Fprintf(&b, "- Commands: %d\n", len(seed.Commands))
		if seed.HistoryBaseline != nil {
			fmt.Fprintf(&b, "- Historical gap: %s (confidence: %s)\n", seed.HistoryBaseline.Reason, seed.HistoryBaseline.Confidence)
		}
		if seed.HistoryCoverage != nil {
			fmt.Fprintf(&b, "- Commit coverage: %d total — %d pre-session, %d covered, %d missing session coverage after oldest session, %d with no session history (`%s`)\n", seed.HistoryCoverage.TotalCommits, seed.HistoryCoverage.PreSessionCommits, seed.HistoryCoverage.CoveredCommits, seed.HistoryCoverage.MissingSessionCommits, seed.HistoryCoverage.NoSessionHistoryCommits, seed.HistoryCoverage.Path)
		}
		if seed.Agent != nil {
			fmt.Fprintf(&b, "- Agent quick status: %s\n", seed.Agent.Quick.Status)
			if seed.Agent.Deep.Status != "" {
				fmt.Fprintf(&b, "- Agent deep status: %s\n", seed.Agent.Deep.Status)
			}
		}
	}
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		sessions := manifest.Sources.Sessions
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Session History")
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "- Sessions: %d\n", len(sessions.Sessions))
		fmt.Fprintf(&b, "- Checkpoints scanned: %d\n", sessions.CheckpointsScanned)
		if sessions.OldestSessionAt != nil {
			fmt.Fprintf(&b, "- Oldest session: `%s`\n", sessions.OldestSessionAt.Format(time.RFC3339))
		}
		for _, branch := range sessions.Branches {
			label := branch.Branch
			if label == "" {
				label = "(unknown)"
			}
			fmt.Fprintf(&b, "- `%s`: %d sessions in `%s`\n", label, branch.SessionCount, branch.Directory)
		}
	}
	if manifest.Sources != nil && manifest.Sources.History != nil {
		history := manifest.Sources.History
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## History Index")
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "- Generated at: `%s`\n", history.GeneratedAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "- Index: `%s`\n", history.IndexPath)
		fmt.Fprintf(&b, "- Records: %d\n", history.Records)
		fmt.Fprintf(&b, "- Decisions: %d\n", history.Decisions)
		fmt.Fprintf(&b, "- Learnings: %d\n", history.Learnings)
		fmt.Fprintf(&b, "- Validations: %d\n", history.Validations)
		fmt.Fprintf(&b, "- Tool calls: %d\n", history.ToolCalls)
	}
	if manifest.Sources != nil && manifest.Sources.Semantic != nil {
		semantic := manifest.Sources.Semantic
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Code Index (Graph-owned)")
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "- Generated at: `%s`\n", semantic.GeneratedAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "- Provider: `%s`", semantic.Provider)
		if semantic.ProviderVersion != "" {
			fmt.Fprintf(&b, " `%s`", semantic.ProviderVersion)
		}
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "- Schema: `%s`\n", semantic.SchemaVersion)
		fmt.Fprintf(&b, "- Symbols: %d\n", semantic.Symbols)
		fmt.Fprintf(&b, "- Relations: %d\n", semantic.Relations)
		if semantic.Commit != "" {
			fmt.Fprintf(&b, "- Commit: `%s`\n", semantic.Commit)
		}
		if semantic.Branch != "" {
			fmt.Fprintf(&b, "- Branch: `%s`\n", semantic.Branch)
		}
		fmt.Fprintln(&b, "- Intake: run `entire brain inspect code <symbol> --json` then `entire brain inspect context <symbol> --json` for task-specific code context.")
	}
	var warnings []string
	warnings = append(warnings, manifest.Warnings...)
	if manifest.Sources != nil && manifest.Sources.Seed != nil {
		warnings = append(warnings, manifest.Sources.Seed.Warnings...)
	}
	if manifest.Sources != nil && manifest.Sources.Sessions != nil {
		warnings = append(warnings, manifest.Sources.Sessions.Warnings...)
	}
	if manifest.Sources != nil && manifest.Sources.History != nil {
		warnings = append(warnings, manifest.Sources.History.Warnings...)
	}
	if manifest.Sources != nil && manifest.Sources.Semantic != nil {
		for _, warning := range manifest.Sources.Semantic.Warnings {
			warnings = append(warnings, warning.Code)
		}
		for _, failure := range manifest.Sources.Semantic.PartialFailures {
			warnings = append(warnings, failure.Code)
		}
	}
	warnings = uniqueStrings(warnings)
	if len(warnings) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Warnings")
		fmt.Fprintln(&b)
		for _, warning := range warnings {
			fmt.Fprintf(&b, "- %s\n", warning)
		}
	}
	return b.String()
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

type seedAgentInput struct {
	SchemaVersion          int                  `json:"schema_version"`
	Phase                  string               `json:"phase"`
	Repo                   map[string]string    `json:"repo"`
	Limits                 map[string]int       `json:"limits"`
	Inventory              []seedFileIndexEntry `json:"inventory"`
	Documents              []seedDocument       `json:"documents"`
	Metadata               map[string]any       `json:"metadata"`
	DeterministicSummaries map[string]string    `json:"deterministic_summaries"`
	QuickResult            map[string]string    `json:"quick_result,omitempty"`
}

type seedAgentOutput struct {
	SchemaVersion int               `json:"schema_version"`
	Status        string            `json:"status"`
	Model         string            `json:"model,omitempty"`
	Artifacts     map[string]string `json:"artifacts"`
	Warnings      []string          `json:"warnings,omitempty"`
}

func runSeedAgent(ctx context.Context, repoDir, outputDir string, scan seedScanResult, opts seedCommandOptions) (*seedAgentManifest, error) {
	manifest := &seedAgentManifest{Mode: opts.agent}
	quickInput, err := buildAgentInput("quick", repoDir, scan, nil, opts)
	if err != nil {
		return manifest, err
	}
	opts.reportProgress(fmt.Sprintf("synthesizing with %s (quick)", opts.agent))
	quick, quickArtifacts, err := runSeedAgentPhase(ctx, repoDir, outputDir, opts, "quick", opts.agentQuickTimeout, quickInput, []string{"quick-overview.md"})
	manifest.Quick = quick
	if err != nil {
		if opts.requireAgent {
			return manifest, err
		}
		manifest.Deep = seedAgentPhase{Status: "skipped", Timeout: opts.agentDeepTimeout.String()}
		return manifest, err
	}
	deepInput, err := buildAgentInput("deep", repoDir, scan, quickArtifacts, opts)
	if err != nil {
		return manifest, err
	}
	opts.reportProgress(fmt.Sprintf("synthesizing with %s (deep)", opts.agent))
	deep, _, err := runSeedAgentPhase(ctx, repoDir, outputDir, opts, "deep", opts.agentDeepTimeout, deepInput, []string{"overview.md", "architecture.md", "risks.md", "maintenance-guide.md", "open-questions.md"})
	manifest.Deep = deep
	if err != nil && opts.requireAgent {
		return manifest, err
	}
	return manifest, nil
}

func buildAgentInput(phase, repoDir string, scan seedScanResult, quick map[string]string, opts seedCommandOptions) ([]byte, error) {
	input := seedAgentInput{
		SchemaVersion: 1,
		Phase:         phase,
		Repo: map[string]string{
			"root":   repoDir,
			"key":    scan.RepoKey,
			"commit": scan.Commit,
		},
		Limits:    map[string]int{"max_output_bytes": defaultAgentMaxOutput},
		Inventory: scan.Files,
		Documents: scan.Docs,
		Metadata: map[string]any{
			"entrypoints":      scan.Entrypoints,
			"commands":         scan.Commands,
			"history_coverage": scan.Coverage,
		},
		DeterministicSummaries: map[string]string{
			"overview":      renderSeedOverview(scan),
			"architecture":  renderSeedArchitecture(scan),
			"commands":      renderSeedCommands(scan),
			"conventions":   renderSeedConventions(scan),
			"risks_and_gap": renderSeedRisks(scan),
			"history_gaps":  renderSeedHistoryGaps(scan),
		},
		QuickResult: quick,
	}
	data, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if len(data) > opts.agentMaxInputBytes {
		return nil, fmt.Errorf("agent input packet exceeds --agent-max-input-bytes (%d > %d)", len(data), opts.agentMaxInputBytes)
	}
	return data, nil
}

func runSeedAgentPhase(ctx context.Context, repoDir, outputDir string, opts seedCommandOptions, phase string, timeout time.Duration, input []byte, required []string) (seedAgentPhase, map[string]string, error) {
	started := time.Now().UTC()
	phaseManifest := seedAgentPhase{Status: "running", Timeout: timeout.String(), StartedAt: started, InputFingerprint: bytesFingerprint(input)}
	args, err := seedAgentCommandArgs(repoDir, phase, opts)
	if err != nil {
		phaseManifest.Status = "failed"
		return phaseManifest, nil, err
	}
	// Pin a fast/cheap model + effort for seed synthesis when requested (no-ops when empty),
	// reusing the same injectors as distill so a full refresh can run cheaply end to end.
	args = clibase.InjectAgentEffort(clibase.InjectAgentModel(args, opts.agent, opts.model), opts.agent, opts.effort)
	for {
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		command := exec.CommandContext(runCtx, args[0], args[1:]...)
		command.Dir = repoDir
		command.Stdin = bytes.NewReader(input)
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		err = command.Run()
		cancel()
		phaseManifest.CompletedAt = time.Now().UTC()
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			phaseManifest.Status = "timeout"
			action := opts.agentTimeoutAction
			if phase == "deep" {
				action = promptSeedTimeoutAction(opts, timeout, action)
			}
			phaseManifest.TimeoutAction = action
			if phase == "quick" {
				return phaseManifest, nil, fmt.Errorf("agent quick synthesis timed out after %s", timeout)
			}
			if action == "continue" {
				continue
			}
			if action == "fail" {
				return phaseManifest, nil, fmt.Errorf("agent %s synthesis timed out after %s", phase, timeout)
			}
			return phaseManifest, nil, nil
		}
		if err != nil {
			phaseManifest.Status = "failed"
			warning := strings.TrimSpace(stderr.String())
			if warning == "" {
				warning = strings.TrimSpace(stdout.String())
			}
			if warning != "" {
				phaseManifest.Warnings = append(phaseManifest.Warnings, truncateAgentWarning(warning))
			}
			return phaseManifest, nil, fmt.Errorf("agent %s synthesis failed: %w", phase, err)
		}
		if stdout.Len() > defaultAgentMaxOutput {
			phaseManifest.Status = "failed"
			return phaseManifest, nil, fmt.Errorf("agent %s output exceeds limit", phase)
		}
		var out seedAgentOutput
		if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
			phaseManifest.Status = "failed"
			return phaseManifest, nil, fmt.Errorf("parse agent %s output: %w", phase, err)
		}
		if out.Artifacts == nil {
			out.Artifacts = make(map[string]string)
		}
		if err := validateSeedAgentOutput(out, phase); err != nil {
			phaseManifest.Status = "failed"
			return phaseManifest, out.Artifacts, err
		}
		for _, name := range required {
			if _, ok := out.Artifacts[name]; !ok {
				phaseManifest.Status = "incomplete"
				return phaseManifest, out.Artifacts, fmt.Errorf("agent %s output missing required artifact %s", phase, name)
			}
		}
		if err := validateSeedAgentArtifactSet(out.Artifacts, required, phase); err != nil {
			phaseManifest.Status = "failed"
			return phaseManifest, out.Artifacts, err
		}
		written, err := writeAgentArtifacts(outputDir, out.Artifacts)
		if err != nil {
			phaseManifest.Status = "failed"
			return phaseManifest, out.Artifacts, err
		}
		phaseManifest.Status = "success"
		phaseManifest.Model = out.Model
		phaseManifest.Warnings = append(phaseManifest.Warnings, out.Warnings...)
		phaseManifest.OutputPaths = written
		return phaseManifest, out.Artifacts, nil
	}
}

func validateSeedAgentArtifactSet(artifacts map[string]string, required []string, phase string) error {
	allowed := make(map[string]struct{}, len(required))
	for _, name := range required {
		allowed[name] = struct{}{}
	}
	for name := range artifacts {
		if _, ok := allowed[name]; !ok {
			return fmt.Errorf("agent %s output included unexpected artifact %s", phase, name)
		}
	}
	return nil
}

func validateSeedAgentOutput(out seedAgentOutput, phase string) error {
	if out.SchemaVersion != 1 {
		return fmt.Errorf("agent %s output has unsupported schema_version %d", phase, out.SchemaVersion)
	}
	// status is the agent's self-reported outcome; accept any non-failure value
	// (models reasonably emit "success", "ok", "complete", …) because the artifact
	// set is validated separately and is the real success signal. Only an explicit
	// failure status is rejected — gating on the exact string "success" made
	// synthesis flaky (codex models returned "ok" and were wrongly rejected).
	switch strings.ToLower(strings.TrimSpace(out.Status)) {
	case "error", "fail", "failed", "failure":
		return fmt.Errorf("agent %s output reported failure status %q", phase, out.Status)
	}
	return nil
}

func truncateAgentWarning(warning string) string {
	const maxWarningBytes = 4000
	if len(warning) <= maxWarningBytes {
		return warning
	}
	return warning[:maxWarningBytes] + "\n[truncated]"
}

func promptSeedTimeoutAction(opts seedCommandOptions, timeout time.Duration, fallback string) string {
	if opts.noInteractive || !clibase.StdinIsTerminal() {
		return fallback
	}
	fmt.Fprintf(os.Stderr, "Deep synthesis has been running for %s. Keep quick result, continue, or fail?\n[k]eep quick / [c]ontinue / [f]ail: ", timeout)
	var response string
	if _, err := fmt.Fscanln(os.Stdin, &response); err != nil {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(response)) {
	case "c", "continue":
		return "continue"
	case "f", "fail":
		return "fail"
	default:
		return "keep-quick"
	}
}

func seedAgentCommandArgs(repoDir, phase string, opts seedCommandOptions) ([]string, error) {
	_ = repoDir
	if err := clibase.RejectAgentForNoEgress(opts.agent); err != nil {
		return nil, err
	}
	switch opts.agent {
	case "command":
		if len(opts.agentCommand) == 0 {
			return nil, errors.New("--agent-command is required when --agent command")
		}
		return append([]string(nil), opts.agentCommand...), nil
	case "codex":
		return []string{"codex", "exec", "--skip-git-repo-check", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--sandbox", "read-only", seedAgentPrompt(phase)}, nil
	case "claude-code":
		return []string{"claude", "--print", "--no-session-persistence", "--setting-sources", "user", "--strict-mcp-config", "--mcp-config", "{\"mcpServers\":{}}", "--disable-slash-commands", "--permission-mode", "dontAsk", "--tools", "", "--system-prompt", seedAgentPrompt(phase)}, nil
	default:
		return nil, fmt.Errorf("unsupported --agent %q", opts.agent)
	}
}

func seedAgentPrompt(phase string) string {
	required := strings.Join(seedAgentRequiredArtifacts(phase), ", ")
	common := fmt.Sprintf(`Read the JSON seed packet on stdin and return only raw JSON with keys schema_version, status, model, artifacts, and warnings.
schema_version must be the integer 1 and status must be the string "success".
artifacts must be an object whose keys are artifact filenames and values are markdown content.
For phase %q, artifacts must include exactly these required filenames: %s.
Do not include markdown fences or prose outside the JSON object.

Use the deterministic summaries, documents list, inventory, commands, entrypoints, and history_coverage metadata.
Pay special attention to history gaps:
- distinguish pre-session commits, checkpointed-but-unexported commits, and missing_session commits.
- explain missing history as uncertainty, not as if session transcripts exist.
- use commit subjects/notes from history_gaps when describing likely development history.
- call out skipped files and denied paths only when they affect confidence.

Keep all artifacts factual, repo-specific, and useful for a future coding agent.
Do not invent APIs, test results, CI status, release process, or rationale not present in the packet.
Prefer concrete filenames, exported symbols, commands, renderer names, and documented constraints over generic advice.
Use compact markdown.`, phase, required)
	switch phase {
	case "quick":
		return common + `

quick-overview.md must contain these sections:
# Quick Overview
## Project
## Entry Points
## Core Modules
## Commands
## History Coverage
## Immediate Risks`
	case "deep":
		return common + `

overview.md must summarize purpose, public surface, commands, tests, docs, and history confidence.
architecture.md must explain module boundaries and data flow across the actual files in the packet.
risks.md must rank concrete code/process risks and include history-gap risk separately.
maintenance-guide.md must tell a future agent where to edit, what to run, and what parity checks matter.
open-questions.md must list only questions supported by packet evidence; include any history backfill question caused by missing_session commits.`
	default:
		return common
	}
}

func seedAgentRequiredArtifacts(phase string) []string {
	switch phase {
	case "quick":
		return []string{"quick-overview.md"}
	case "deep":
		return []string{"overview.md", "architecture.md", "risks.md", "maintenance-guide.md", "open-questions.md"}
	default:
		return nil
	}
}

func writeAgentArtifacts(outputDir string, artifacts map[string]string) ([]string, error) {
	var written []string
	for name, content := range artifacts {
		clean := filepath.Clean(name)
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return nil, fmt.Errorf("reject unsafe agent artifact path %q", name)
		}
		rel := filepath.Join(seedDirName, seedAgentDirName, clean)
		if err := brainstore.WriteBrainRelativeFileAtomic(outputDir, filepath.ToSlash(rel), []byte(content), 0o600); err != nil {
			return nil, err
		}
		written = append(written, filepath.ToSlash(rel))
	}
	sort.Strings(written)
	return written, nil
}

func bytesFingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// materializeHeadSnapshot extracts the committed HEAD tree into a temporary
// directory so a dirty worktree can still seed from committed state
// (COR-2316). A scratch index plus `git checkout-index` writes every committed
// file verbatim. `git archive` was rejected here: it applies export-ignore and
// export-subst, so the snapshot could silently omit committed files (docs are
// commonly export-ignored) or rewrite their bytes. Committed symlinks are
// materialized as symlinks and excluded later by the scan's symlink rejection,
// exactly as on the clean path. The caller owns cleanup.
func materializeHeadSnapshot(ctx context.Context, runner CommandRunner, repoDir string) (dir, commit string, cleanup func(), err error) {
	envRunner, ok := runner.(EnvironmentCommandRunner)
	if !ok {
		return "", "", nil, errors.New("command runner cannot set GIT_INDEX_FILE for the snapshot checkout")
	}
	commit, err = clibase.GitScalar(ctx, runner, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", nil, fmt.Errorf("resolve HEAD for committed snapshot: %w", err)
	}
	dir, err = os.MkdirTemp("", "entire-brain-seed-head-")
	if err != nil {
		return "", "", nil, fmt.Errorf("create snapshot directory: %w", err)
	}
	// The scratch index lives next to dir, not inside it, so the content scan
	// never sees it; it also keeps the repository's real index untouched.
	indexFile := dir + ".index"
	cleanup = func() {
		_ = os.Remove(indexFile)
		_ = os.RemoveAll(dir)
	}
	env := map[string]string{"GIT_INDEX_FILE": indexFile}
	if _, _, runErr := envRunner.RunWithEnv(ctx, repoDir, env, "git", "read-tree", commit); runErr != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("read committed tree %s: %w", commit, runErr)
	}
	// attr.tree pins attribute lookup (text/eol/ident) to the committed tree:
	// checkout-index otherwise reads .gitattributes from the dirty worktree,
	// letting uncommitted attribute edits rewrite snapshot bytes. Git older
	// than 2.43 ignores the unknown key and keeps the worktree lookup.
	if _, _, runErr := envRunner.RunWithEnv(ctx, repoDir, env, "git", "-c", "attr.tree="+commit, "checkout-index", "--all", "--force", "--prefix="+dir+string(filepath.Separator)); runErr != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("extract committed tree %s: %w", commit, runErr)
	}
	return dir, commit, cleanup, nil
}
