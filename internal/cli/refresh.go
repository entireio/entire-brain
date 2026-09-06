package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type refreshCommandOptions struct {
	outputDir        string
	checkpointLimit  int
	entireBinary     string
	rawTranscript    bool
	scope            string
	force            bool
	skipSessions     bool
	semantic         bool
	semanticWorktree bool
	allBranches      bool
	forceAllBranches bool
	historyIndex     bool
	graphBinary      string
	statusAfter      bool
	seed             seedCommandOptions
	// component, when non-nil, turns the refresh BEST-EFFORT: every stage
	// reports its outcome through this callback (err == nil means built) and a
	// failing stage no longer aborts the run. One broken source — a semantic
	// snapshot whose repo key was derived by a different CLI, say — must degrade
	// the brain, not stop it from being built at all. Only the failures that
	// leave nothing usable (no brain directory, no output path) still return.
	component func(name string, err error)
}

func newRefreshCommand(opts Options) *cobra.Command {
	refreshOpts := defaultRefreshCommandOptions()
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Create or refresh the repository brain",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --worktree belongs to seed/docs. A dirty semantic snapshot must be
			// an explicit `brain index --worktree` operation (or the deprecated
			// --semantic-worktree compatibility flag), so the ordinary
			// `refresh --worktree` path must not accidentally attempt and fail a
			// committed-tree semantic rebuild.
			if refreshOpts.seed.worktree && !refreshOpts.semanticWorktree && !cmd.Flags().Changed("semantic") {
				refreshOpts.semantic = false
			}
			return runRefresh(cmd.Context(), cmd, opts, refreshOpts)
		},
	}
	cmd.Flags().StringVarP(&refreshOpts.outputDir, "output", "o", defaultExportDir, "Output directory for the brain (default: persistent brain directory)")
	cmd.Flags().BoolVar(&refreshOpts.force, "force", false, "Force a full refresh; overwrite explicit output when used with --output")
	cmd.Flags().StringVar(&refreshOpts.seed.agent, "agent", "auto", "Agent synthesis mode for seed: auto, none, codex, claude-code, or command")
	cmd.Flags().IntVar(&refreshOpts.checkpointLimit, "checkpoint-limit", defaultCheckpointLimit, "Maximum checkpoints to inspect")
	cmd.Flags().StringVar(&refreshOpts.entireBinary, "entire-binary", "entire", "Entire CLI binary to invoke")
	cmd.Flags().BoolVar(&refreshOpts.rawTranscript, "raw", false, "Export raw agent transcripts instead of normalized compact transcripts")
	cmd.Flags().StringVar(&refreshOpts.scope, "scope", exportScopeAll, "Checkpoint discovery scope: all or branch")
	cmd.Flags().BoolVar(&refreshOpts.seed.force, "force-seed", false, "Force seed refresh")
	cmd.Flags().BoolVar(&refreshOpts.seed.worktree, "worktree", false, "Refresh seed and docs from the current worktree")
	cmd.Flags().StringArrayVar(&refreshOpts.seed.agentCommand, "agent-command", nil, "Agent command argv for --agent command")
	cmd.Flags().StringVar(&refreshOpts.seed.model, "seed-model", "", "Override the agent model for seed synthesis (e.g. a fast/cheap model)")
	cmd.Flags().StringVar(&refreshOpts.seed.effort, "seed-effort", "", "Override the reasoning effort for seed synthesis (e.g. low)")
	cmd.Flags().BoolVar(&refreshOpts.semantic, "semantic", true, "Refresh the local semantic index after session and seed refresh")
	cmd.Flags().BoolVar(&refreshOpts.semanticWorktree, "semantic-worktree", false, "Deprecated: allow semantic indexing of the current dirty worktree")
	_ = cmd.Flags().MarkDeprecated("semantic-worktree", "use `entire brain index --worktree` for an explicit semantic worktree snapshot")
	cmd.Flags().BoolVar(&refreshOpts.historyIndex, "history-index", true, "Build a decision/rationale index from exported sessions")
	cmd.Flags().StringVar(&refreshOpts.graphBinary, "graph-binary", "entire", "Entire CLI binary that exposes `graph` provider commands")
	cmd.Flags().BoolVar(&refreshOpts.allBranches, "all-branches", false, "Refresh recent local branch overlays without fetching remotes")
	cmd.Flags().BoolVar(&refreshOpts.forceAllBranches, "force-all-branches", false, "Allow all local branches instead of the bounded recent-branch default")
	for _, name := range []string{"checkpoint-limit", "entire-binary", "raw", "scope", "force-seed", "agent-command", "seed-model", "seed-effort", "semantic", "semantic-worktree", "history-index", "graph-binary", "all-branches", "force-all-branches"} {
		_ = cmd.Flags().MarkHidden(name)
	}
	// Individual refresh stages, runnable on their own: `refresh` does all of
	// them. Each stage is named after the brain source it refreshes (the same
	// vocabulary as the status sources line).
	cmd.AddCommand(newExportCommand(opts))        // refresh sessions: export transcripts from checkpoints
	cmd.AddCommand(newHistoryIndexCommand(opts))  // refresh history: decision index from exported transcripts
	cmd.AddCommand(newDeltaCommand(opts))         // refresh delta: short-term memory (changed transcripts only)
	cmd.AddCommand(newSemanticIndexCommand(opts)) // refresh index: semantic symbol graph
	cmd.AddCommand(newSeedCommand(opts))          // refresh seed
	return cmd
}

func defaultRefreshCommandOptions() refreshCommandOptions {
	return refreshCommandOptions{
		checkpointLimit: 0,
		entireBinary:    "entire",
		graphBinary:     "entire",
		scope:           exportScopeAll,
		historyIndex:    true,
		semantic:        true,
		statusAfter:     true,
		seed: seedCommandOptions{
			includeTests:       true,
			maxFileBytes:       defaultSeedMaxFileBytes,
			maxFiles:           defaultSeedMaxFiles,
			format:             "markdown+json",
			agent:              "auto",
			agentQuickTimeout:  2 * time.Minute,
			agentDeepTimeout:   10 * time.Minute,
			agentTimeoutAction: "keep-quick",
			agentMaxInputBytes: defaultAgentMaxInput,
		},
	}
}

func runRefresh(ctx context.Context, cmd *cobra.Command, opts Options, refreshOpts refreshCommandOptions) error {
	if refreshOpts.allBranches && !refreshOpts.semantic {
		return errors.New("--all-branches requires --semantic")
	}
	outputExplicit := cmd.Flags().Changed("output")
	if outputExplicit && strings.TrimSpace(refreshOpts.outputDir) == "" {
		return errors.New("--output must not be empty")
	}
	repoDir, err := exportRepoDir(opts.Env)
	if err != nil {
		return err
	}
	if refreshOpts.force {
		refreshOpts.seed.force = true
	}
	if outputExplicit {
		if refreshOpts.force {
			if err := removeForcedOutputDir(refreshOpts.outputDir, repoDir); err != nil {
				return err
			}
		} else if _, err := validateExportDirAvailable(refreshOpts.outputDir); err != nil {
			return err
		}
	}
	if refreshOpts.seed.agent == "auto" {
		refreshOpts.seed.agent = defaultRefreshAgent(ctx, opts.Runner, repoDir)
	}
	progress := newRefreshProgress(cmd.ErrOrStderr())
	// stage reports one stage's outcome and answers "must this abort the
	// refresh?". Outside best-effort mode the answer is the old one — any error
	// aborts. Inside it, nothing does: the caller collects the per-component
	// outcomes and decides.
	stage := func(name string, err error) bool {
		if refreshOpts.component != nil {
			refreshOpts.component(name, err)
			return false
		}
		return err != nil
	}
	exportCmd := &cobra.Command{Use: "export"}
	exportCmd.SetOut(io.Discard)
	exportCmd.SetErr(io.Discard)
	exportOpts := exportCommandOptions{
		outputDir:       defaultExportDir,
		outputExplicit:  outputExplicit,
		checkpointLimit: refreshOpts.checkpointLimit,
		entireBinary:    refreshOpts.entireBinary,
		rawTranscript:   refreshOpts.rawTranscript,
		scope:           refreshOpts.scope,
	}
	if outputExplicit {
		exportOpts.outputDir = refreshOpts.outputDir
	}
	var exportErr error
	finishExportTask := func(error) {}
	updateExportTask := func(string) {}
	if !refreshOpts.skipSessions {
		exportTask := progress.Begin("export sessions")
		exportOpts.progress = func(p exportProgress) {
			// The bar is fed from the SAME numbers the label prints, so the two
			// can never disagree; refreshExportProgressCounts picks whichever
			// pair the exporter is currently counting (items, else checkpoints).
			done, total := refreshExportProgressCounts(p)
			exportTask.SetProgress(done, total)
			exportTask.Update(refreshExportProgressLabel(p))
		}
		exportErr = runExport(ctx, exportCmd, opts, exportOpts)
		exportTaskFinished := false
		finishExportTask = func(err error) {
			if exportTaskFinished {
				return
			}
			exportTask.Finish(err)
			exportTaskFinished = true
		}
		updateExportTask = exportTask.Update
		if exportErr == nil {
			finishExportTask(nil)
		}
	} else {
		progress.Skip("export sessions")
	}

	storageTask := progress.Begin("locate brain")
	storage, storageErr := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if storageErr != nil {
		storageTask.Finish(storageErr)
		return storageErr
	}
	storageTask.Update("locate brain: " + storage.Key)
	storageTask.Finish(nil)
	brainDir := storage.BrainDir
	if outputExplicit {
		finishOutput := progress.Step("resolve output path")
		brainDir, err = filepath.Abs(refreshOpts.outputDir)
		if err != nil {
			finishOutput(err)
			return fmt.Errorf("resolve output directory: %w", err)
		}
		finishOutput(nil)
	}
	manifest, _ := loadBrainManifest(brainDir)
	needSeed := outputExplicit || refreshOpts.seed.force
	// seedProbeErr is a freshness check that could not RUN — `git rev-parse
	// HEAD` failing on an unborn branch, an interrupted git state, or git
	// missing from a daemon's PATH. Returning it raw punched a hole straight
	// through the never-abort contract this best-effort mode exists to keep
	// (setup.go's instant phase): the phase died before reporting a single
	// component, so instant.json was never written, `status` and `doctor` could
	// not name the reason, and the workspace, the daemon and the backfill were
	// all skipped. It is a failed SEED source, so it is reported as one.
	var seedProbeErr error
	if !needSeed {
		needSeed, seedProbeErr = seedRefreshNeeded(ctx, opts, repoDir, manifest, refreshOpts.seed.worktree)
		if seedProbeErr != nil {
			// Freshness unknown: leave the existing seed alone rather than
			// rebuilding one on the strength of a check that did not run.
			needSeed = false
		}
	}
	if exportErr != nil && (manifest == nil || manifest.Sources == nil) && seedProbeErr == nil {
		needSeed = true
	}
	// The sessions component is reported exactly once, here, where the export
	// outcome and the seed fallback are both known: a failed export that a seed
	// baseline papers over is still a failed sessions source, and saying so is
	// the whole point of a per-component report.
	if !refreshOpts.skipSessions {
		if exportErr != nil && !needSeed {
			finishExportTask(exportErr)
			if stage(brainComponentSessions, exportErr) {
				return exportErr
			}
		} else {
			stage(brainComponentSessions, exportErr)
		}
	}
	if exportErr != nil {
		updateExportTask("export sessions: unavailable, using seed baseline")
	}
	finishExportTask(nil)
	switch {
	case seedProbeErr != nil:
		progress.Skip("seed baseline: freshness unknown (" + seedProbeErr.Error() + ")")
		if stage(brainComponentSeed, seedProbeErr) {
			return seedProbeErr
		}
	case needSeed:
		seedTask := progress.Begin("seed baseline")
		seedCmd := &cobra.Command{Use: "seed"}
		seedCmd.SetOut(io.Discard)
		seedCmd.SetErr(io.Discard)
		seedOpts := refreshOpts.seed
		seedOpts.update = true
		seedOpts.outputExplicit = outputExplicit
		seedOpts.progress = func(phase string) {
			seedTask.Update("seed baseline: " + phase)
		}
		if outputExplicit {
			seedOpts.outputDir = brainDir
		}
		if err := runSeed(ctx, seedCmd, opts, seedOpts, repoDir); err != nil {
			seedTask.Finish(err)
			if stage(brainComponentSeed, err) {
				return err
			}
		} else {
			manifest, _ = loadBrainManifest(brainDir)
			seedTask.Update(refreshSeedLabel(manifest))
			seedTask.Finish(nil)
			stage(brainComponentSeed, nil)
		}
	default:
		progress.Skip(refreshSeedLabel(manifest))
		stage(brainComponentSeed, nil)
	}
	// The projector reads the brain directory directly, and on a first run where
	// EVERY source failed there is no brain directory to read: a repo enabled a
	// minute ago has no sessions to export, and if the worktree is also dirty
	// (which `entire enable` leaves it) the seed baseline does not build either.
	// Running the projector anyway answered "you have no sessions yet" with a
	// second, rawer line naming an internal path and no remedy --
	// "lstat <brainDir>: no such file or directory". An empty history index over
	// a brain directory that DOES exist is still built, as before.
	if refreshOpts.historyIndex && !historyProjectionTargetExists(brainDir) {
		progress.Skip("history index: skipped, no brain directory was built")
	} else if refreshOpts.historyIndex && (refreshOpts.force || !historyIndexCurrent(brainDir, manifest)) {
		historyTask := progress.Begin("history index")
		historyProgress := func(done, total int) {
			if total <= 0 {
				return
			}
			historyTask.SetProgress(done, total)
			historyTask.Update(fmt.Sprintf("history index: %d/%d %s scanned", done, total, pluralUnit("session", total)))
		}
		historySource, err := writeBrainHistoryIndexAndSourceContext(ctx, brainDir, opts.Now().UTC(), historyProgress)
		if err != nil {
			historyTask.Finish(err)
			if stage(brainComponentHistory, err) {
				return err
			}
		} else {
			historyTask.Update(refreshHistoryLabel(historySource))
			historyTask.Finish(nil)
			manifest, _ = loadBrainManifest(brainDir)
			stage(brainComponentHistory, nil)
		}
	} else if refreshOpts.historyIndex {
		progress.Skip(refreshHistoryLabel(existingHistorySource(manifest)))
		stage(brainComponentHistory, nil)
	}
	// History vectors: the persisted semantic arm behind the fusion gate (see
	// history_vec.go). The stage exists only when the user has opted into an
	// external embedder — without ENTIRE_BRAIN_EMBEDDER there is no line at
	// all, so the default refresh output is unchanged. The first sync of a
	// large repo embeds every record (potentially hours, one HTTP embed at a
	// time); progress is batched to disk, so interrupting and re-running
	// refresh resumes rather than restarts.
	if refreshOpts.historyIndex && strings.TrimSpace(os.Getenv("ENTIRE_BRAIN_EMBEDDER")) != "" &&
		manifest != nil && manifest.Sources != nil && manifest.Sources.History != nil {
		e := historySemanticEmbedder(defaultEmbedder())
		store, storeOK := historyVectorStoreFor(brainDir, e)
		switch {
		case e == nil:
			// Opt-in set but not honored (server unreachable → Model2Vec
			// fallback, which is a closed negative on history). defaultEmbedder
			// already printed the fallback warning.
			progress.Skip("history vectors: embed server unavailable")
		case !storeOK:
			progress.Skip("history vectors: requires the brain_cgo build")
		default:
			_ = store // availability was checked above; the guarded writer reopens it.
			vecTask := progress.Begin("history vectors")
			vectors, serr := syncMemoryProjectionVectorsFully(ctx, brainDir, opts.Now().UTC(), e, func(current memoryVectorSyncPass) {
				vecTask.Update(fmt.Sprintf("history vectors: %d new %s embedded", current.HistoryAdded, pluralUnit("record", current.HistoryAdded)))
			})
			if serr != nil {
				vecTask.Finish(serr)
				if stage(brainComponentHistoryVectors, serr) {
					return serr
				}
				break
			}
			vecTask.Update(fmt.Sprintf("history vectors: %d embedded, %d pruned (%d total)", vectors.HistoryAdded, vectors.HistoryDropped, vectors.HistoryTotal))
			vecTask.Finish(nil)
			stage(brainComponentHistoryVectors, nil)
			// Conversation vectors ride the same stage and gate but live in
			// their own store (separate identity; general history KNN never
			// spends budget on exchanges). Skipped silently when the store is
			// unavailable; the history stage above already reported why.
			if _, convOK := newConversationVectorStore(brainDir, conversationVectorModelID(e.ID()), e.Dim()); convOK {
				convTask := progress.Begin("conversation vectors")
				convTask.Update(fmt.Sprintf("conversation vectors: %d embedded, %d pruned (%d total)", vectors.ConversationAdded, vectors.ConversationDrop, vectors.ConversationTotal))
				convTask.Finish(nil)
			}
		}
	}
	// Doc index: retrievable chunks of the brain's own markdown (seed summaries
	// + copied repo docs). It derives from seed, not history, so gate it on its own
	// inputs — rebuild when seed ran, when the docs source is missing, on --force,
	// or alongside a history-index build — never silently skip it just because
	// --history-index=false.
	docsMissing := manifest == nil || manifest.Sources == nil || manifest.Sources.Docs == nil
	if refreshOpts.historyIndex || needSeed || docsMissing || refreshOpts.force {
		docTask := progress.Begin("doc index")
		// A doc-index write failure (permissions, disk full, corrupt manifest) is a
		// real error: fail the refresh like the history index does, so automation
		// relying on the exit status isn't told success on an incomplete brain.
		// (A missing seed is not an error — writeDocIndexAndSource writes an empty
		// index in that case.)
		docSource, derr := writeDocIndexAndSource(brainDir, opts.Now().UTC())
		if derr != nil {
			docTask.Finish(derr)
			if stage(brainComponentDocs, derr) {
				return derr
			}
		} else {
			docLabel := fmt.Sprintf("doc index: %d chunks / %d files", docSource.Records, docSource.Files)
			if n := len(docSource.Warnings); n > 0 {
				docLabel += fmt.Sprintf(" (%d %s)", n, pluralUnit("warning", n))
			}
			docTask.Update(docLabel)
			docTask.Finish(nil)
			manifest, _ = loadBrainManifest(brainDir)
			stage(brainComponentDocs, nil)
		}
	} else {
		stage(brainComponentDocs, nil)
	}
	if refreshOpts.semantic {
		semanticCheckTask := progress.Begin(refreshSemanticCheckLabel(manifest))
		semanticWorktree := refreshOpts.semanticWorktree
		needSemantic, err := semanticRefreshNeeded(ctx, opts, brainDir, repoDir, manifest, semanticWorktree)
		switch {
		case err != nil:
			semanticCheckTask.Finish(err)
			if stage(brainComponentSemantic, err) {
				return err
			}
		default:
			if !needSemantic {
				semanticCheckTask.Update(refreshSemanticCheckLabel(manifest) + ": current")
			}
			semanticCheckTask.Finish(nil)
			if refreshOpts.force {
				needSemantic = true
			}
			if needSemantic {
				semanticTask := progress.Begin("semantic index")
				indexCmd := &cobra.Command{Use: "index"}
				indexCmd.SetOut(io.Discard)
				indexCmd.SetErr(io.Discard)
				semanticProgress := func(phase string) {
					semanticTask.Update("semantic index: " + phase)
				}
				// The provider does not announce how many files it is about to
				// emit, so the bar is scaled by the PREVIOUS generation's file
				// count. That is an estimate and is treated as one: a repo that
				// grew simply pins the bar at full for the tail of the run, and
				// a first-ever index (no previous count) gets an honest
				// indeterminate spinner instead of a fabricated fraction.
				expectedFiles := 0
				if previous := existingSemanticSource(manifest); previous != nil {
					expectedFiles = previous.Files
				}
				semanticCounts := func(files, _, _ int) {
					semanticTask.SetProgress(files, expectedFiles)
				}
				if err := runSemanticIndex(ctx, indexCmd, opts, semanticIndexOptions{force: true, graphBinary: refreshOpts.graphBinary, worktree: semanticWorktree, outputDir: brainDir, outputExplicit: outputExplicit, progress: semanticProgress, progressCounts: semanticCounts}, repoDir); err != nil {
					semanticTask.Finish(err)
					if stage(brainComponentSemantic, err) {
						return err
					}
				} else {
					manifest, _ = loadBrainManifest(brainDir)
					semanticTask.Update(refreshSemanticLabel(existingSemanticSource(manifest)))
					semanticTask.Finish(nil)
					stage(brainComponentSemantic, nil)
				}
			} else {
				progress.Skip(refreshSemanticLabel(existingSemanticSource(manifest)))
				stage(brainComponentSemantic, nil)
			}
		}
	} else {
		progress.Skip("semantic index")
	}
	if refreshOpts.allBranches {
		finishBranches := progress.Step("branch overlays")
		if err := runSemanticRefreshAllBranches(ctx, opts, refreshOpts, repoDir); err != nil {
			finishBranches(err)
			if stage(brainComponentBranches, err) {
				return err
			}
		} else {
			finishBranches(nil)
			stage(brainComponentBranches, nil)
		}
	}
	// Backfill deterministic fact kinds/locus when a fact store exists, so a kind
	// distilled before kind-storage (or otherwise left empty) is repaired here
	// rather than needing a manual `facts reclassify`. No agent, no tokens.
	manifest, _ = loadBrainManifest(brainDir)
	if manifest != nil && manifest.Sources != nil && manifest.Sources.Facts != nil {
		finishReclass := progress.Step("reclassify facts")
		if _, err := reclassifyAllFactBranches(brainDir, opts.Now().UTC()); err != nil {
			finishReclass(err)
			if stage(brainComponentFacts, err) {
				return err
			}
		} else {
			finishReclass(nil)
			stage(brainComponentFacts, nil)
		}
	}
	// Pattern layer (episodes -> tasks/procedures/practices). Deterministic and
	// token-free, so it is part of the normal refresh — the user never has to
	// discover a second build command. Rebuilt when sessions changed or --force.
	manifest, _ = loadBrainManifest(brainDir)
	if !refreshOpts.skipSessions && manifest != nil && manifest.Sources != nil && manifest.Sources.Sessions != nil {
		finishPatterns := progress.Step("pattern layer")
		if _, err := refreshPatternLayer(brainDir, refreshOpts.force, opts.Now().UTC()); err != nil {
			finishPatterns(err)
			if stage(brainComponentPatterns, err) {
				return err
			}
		} else {
			// The internal pattern corpus is a rebuildable cache; a corpus failure must
			// not break the rest of the brain, so it is a warning here (the explicit
			// `patterns refresh` is the stricter surface that fails on corpus errors).
			if cerr := buildPatternCorpus(brainDir, opts.Now().UTC()); cerr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: pattern corpus: %v\n", cerr)
			}
			finishPatterns(nil)
			stage(brainComponentPatterns, nil)
		}
	}
	printAroundLiveLine(cmd.OutOrStdout(), "refreshed brain: %s\n", brainDir)
	if refreshOpts.statusAfter && !outputExplicit {
		statusCmd := &cobra.Command{Use: "status"}
		statusCmd.SetOut(cmd.OutOrStdout())
		statusCmd.SetErr(cmd.ErrOrStderr())
		if err := runAgentStatus(ctx, statusCmd, opts, agentStatusOptions{}, repoDir); err != nil {
			return fmt.Errorf("status after refresh: %w", err)
		}
	}
	return nil
}

// historyProjectionTargetExists reports whether the brain directory the history
// projector reads is actually there. See the call site in runRefresh.
func historyProjectionTargetExists(brainDir string) bool {
	if strings.TrimSpace(brainDir) == "" {
		return false
	}
	info, err := os.Stat(brainDir)
	return err == nil && info.IsDir()
}

func historyIndexCurrent(brainDir string, manifest *exportManifest) bool {
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Sessions == nil || manifest.Sources.History == nil {
		return false
	}
	history := manifest.Sources.History
	if history.IndexPath == "" || history.SessionsFingerprint == "" || history.IndexBytes <= 0 ||
		!validHistorySHA256(history.IndexSHA256) || !validHistorySHA256(history.RecordsFingerprint) ||
		!validHistorySHA256(history.TranscriptsFingerprint) {
		return false
	}
	if history.SessionsFingerprint != sessionSourceFingerprint(manifest.Sources.Sessions) {
		return false
	}
	clean, err := validateHistoryIndexPath(history.IndexPath)
	if err != nil {
		return false
	}
	if err := rejectSymlinkPathComponents(brainDir, clean); err != nil {
		return false
	}
	indexPath := filepath.Join(brainDir, clean)
	info, err := os.Stat(indexPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() != history.IndexBytes {
		return false
	}
	// Unsafe/unreadable entries are deliberately excluded by the same collector
	// used to build the index. Comparing the safe included set keeps those
	// exclusions fail-closed without forcing an endless rebuild.
	excludedByPath := excludedTranscriptPaths(manifest, loadSessionTombstones(brainDir))
	files, err := collectHistorySessionDigests(context.Background(), brainDir, excludedByPath)
	if err != nil {
		return false
	}
	return historyTranscriptFilesFingerprint(files) == history.TranscriptsFingerprint
}

// refreshExportProgressCounts reports the pair the exporter is currently
// counting: its own unit when it has one, checkpoints otherwise, and (0, 0) —
// indeterminate — when it has neither.
func refreshExportProgressCounts(p exportProgress) (int, int) {
	if p.Total > 0 {
		return p.Current, p.Total
	}
	if p.TotalCheckpoints > 0 {
		return p.CurrentCheckpoint, p.TotalCheckpoints
	}
	return 0, 0
}

func refreshExportProgressLabel(p exportProgress) string {
	label := "export sessions"
	if status := strings.TrimSpace(p.Status); status != "" {
		label += ": " + status
	}
	current := p.Current
	total := p.Total
	unit := strings.TrimSpace(p.Unit)
	if total <= 0 {
		current = p.CurrentCheckpoint
		total = p.TotalCheckpoints
		unit = "checkpoint"
	}
	if total <= 0 {
		return label
	}
	if unit == "" {
		unit = "item"
	}
	return fmt.Sprintf("%s: %d/%d %s, %s", label, current, total, pluralUnit(unit, total), pluralCount(p.Sessions, "session"))
}

func pluralUnit(unit string, count int) string {
	if count == 1 {
		return unit
	}
	if strings.HasSuffix(unit, "s") {
		return unit
	}
	return unit + "s"
}

func refreshSeedLabel(manifest *exportManifest) string {
	seed := existingSeedSource(manifest)
	if seed == nil {
		return "seed baseline"
	}
	return fmt.Sprintf("seed baseline: %s, %s, %s",
		pluralCount(len(seed.Documents), "document"),
		pluralCount(len(seed.Entrypoints), "entrypoint"),
		pluralCount(len(seed.Commands), "command"))
}

func refreshHistoryLabel(source *historySourceManifest) string {
	if source == nil {
		return "history index"
	}
	return fmt.Sprintf("history index: %s, %s, %s",
		pluralCount(source.Records, "record"),
		pluralCount(source.Decisions, "decision"),
		pluralCount(source.ToolCalls, "tool call"))
}

func refreshSemanticCheckLabel(manifest *exportManifest) string {
	source := existingSemanticSource(manifest)
	if source == nil {
		return "check semantic index"
	}
	return "check semantic index: " + refreshSemanticCounts(source)
}

func refreshSemanticLabel(source *semanticSourceManifest) string {
	if source == nil {
		return "semantic index"
	}
	return "semantic index: " + refreshSemanticCounts(source)
}

func refreshSemanticCounts(source *semanticSourceManifest) string {
	return fmt.Sprintf("%s, %s, %s",
		pluralCount(source.Symbols, "symbol"),
		pluralCount(source.Relations, "relation"),
		pluralCount(source.Files, "file"))
}

func existingSeedSource(manifest *exportManifest) *seedSourceManifest {
	if manifest == nil || manifest.Sources == nil {
		return nil
	}
	return manifest.Sources.Seed
}

func existingHistorySource(manifest *exportManifest) *historySourceManifest {
	if manifest == nil || manifest.Sources == nil {
		return nil
	}
	return manifest.Sources.History
}

func existingSemanticSource(manifest *exportManifest) *semanticSourceManifest {
	if manifest == nil || manifest.Sources == nil {
		return nil
	}
	return manifest.Sources.Semantic
}

func pluralCount(count int, singular string) string {
	if count == 1 {
		return fmt.Sprintf("%d %s", count, singular)
	}
	return fmt.Sprintf("%d %ss", count, singular)
}

func semanticRefreshNeeded(ctx context.Context, opts Options, brainDir, repoDir string, manifest *exportManifest, worktree bool) (bool, error) {
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return true, nil
	}
	source := manifest.Sources.Semantic
	if worktree {
		hash, err := worktreeFingerprint(ctx, opts.Runner, repoDir)
		if err != nil {
			return false, fmt.Errorf("fingerprint worktree for semantic refresh: %w", err)
		}
		if source.WorktreeMode != "worktree" || source.WorktreeHash != hash {
			return true, nil
		}
		return !semanticRefreshArtifactsUsable(brainDir, source), nil
	}
	dirty, err := worktreeDirty(ctx, opts.Runner, repoDir)
	if err != nil {
		return false, fmt.Errorf("check worktree dirtiness for semantic refresh: %w", err)
	}
	if dirty {
		return true, nil
	}
	tree, err := gitScalar(ctx, opts.Runner, repoDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return false, fmt.Errorf("resolve HEAD tree for semantic refresh: %w", err)
	}
	if source.WorktreeMode == "worktree" || source.DirtyWorktree || source.Tree != tree {
		return true, nil
	}
	return !semanticRefreshArtifactsUsable(brainDir, source), nil
}

func semanticRefreshArtifactsUsable(brainDir string, source *semanticSourceManifest) bool {
	if source == nil || source.SnapshotPath == "" || source.StorePath == "" {
		return false
	}
	snapshotRel, err := validateSemanticSnapshotPath(source.SnapshotPath)
	if err != nil {
		return false
	}
	if err := rejectSymlinkPathComponents(brainDir, snapshotRel); err != nil {
		return false
	}
	snapshotInfo, err := os.Stat(filepath.Join(brainDir, snapshotRel))
	if err != nil || snapshotInfo.IsDir() {
		return false
	}
	if _, err := validateSemanticDeclaredStore(brainDir, source); err != nil {
		return false
	}
	return true
}

func defaultRefreshAgent(ctx context.Context, runner CommandRunner, repoDir string) string {
	if brainNoEgressMode() {
		return "none"
	}
	if commandLooksAvailable(ctx, runner, repoDir, "codex") {
		return "codex"
	}
	if commandLooksAvailable(ctx, runner, repoDir, "claude") {
		return "claude-code"
	}
	return "none"
}

func commandLooksAvailable(ctx context.Context, runner CommandRunner, repoDir, name string) bool {
	if runner == nil {
		return false
	}
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _, err := runner.Run(runCtx, repoDir, name, "--version")
	return err == nil
}

func runSemanticRefreshAllBranches(ctx context.Context, opts Options, refreshOpts refreshCommandOptions, repoDir string) error {
	if !refreshOpts.semantic {
		return errors.New("--all-branches requires --semantic")
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	unlock, err := acquireSemanticIndexLock(storage.BrainDir)
	if err != nil {
		return err
	}
	defer unlock()
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil || manifest.Sources.Semantic.SnapshotPath == "" {
		return errors.New("semantic index missing; run `entire brain refresh index --force` first")
	}
	defaultBranch, _ := detectDefaultBranch(ctx, opts.Runner, repoDir)
	defaultHead := ""
	if defaultBranch != "" {
		defaultHead = strings.TrimSpace(string(runGitOutput(ctx, opts.Runner, repoDir, "rev-parse", "refs/heads/"+defaultBranch)))
		if defaultHead == "" {
			defaultHead = strings.TrimSpace(string(runGitOutput(ctx, opts.Runner, repoDir, "rev-parse", "refs/remotes/origin/"+defaultBranch)))
		}
	}
	stdout, _, err := opts.Runner.Run(ctx, repoDir, "git", "for-each-ref", "--format=%(refname:short)%00%(committerdate:unix)", "refs/heads")
	if err != nil {
		return fmt.Errorf("list local branches for semantic refresh: %w", err)
	}
	now := opts.Now().UTC()
	type branchRefresh struct {
		Branch      string `json:"branch"`
		State       string `json:"state"`
		Reason      string `json:"reason,omitempty"`
		OverlayPath string `json:"overlay_path,omitempty"`
	}
	report := struct {
		GeneratedAt      time.Time       `json:"generated_at"`
		RetentionDays    int             `json:"retention_days"`
		ForceAllBranches bool            `json:"force_all_branches"`
		Branches         []branchRefresh `json:"branches"`
	}{GeneratedAt: now, RetentionDays: 30, ForceAllBranches: refreshOpts.forceAllBranches}
	for _, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		branch := strings.TrimSpace(parts[0])
		if !refreshOpts.forceAllBranches {
			seconds, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
			if seconds > 0 && now.Sub(time.Unix(seconds, 0).UTC()) > 30*24*time.Hour {
				report.Branches = append(report.Branches, branchRefresh{Branch: branch, State: "skipped", Reason: "older_than_30d"})
				continue
			}
		}
		if defaultHead == "" || branch == defaultBranch {
			report.Branches = append(report.Branches, branchRefresh{Branch: branch, State: "skipped", Reason: "no_branch_delta"})
			continue
		}
		head := strings.TrimSpace(string(runGitOutput(ctx, opts.Runner, repoDir, "rev-parse", "refs/heads/"+branch)))
		if head == "" || head == defaultHead {
			report.Branches = append(report.Branches, branchRefresh{Branch: branch, State: "skipped", Reason: "no_branch_delta"})
			continue
		}
		overlayPath, err := writeSemanticOverlayFile(storage.BrainDir, defaultHead, head, branch, "", now)
		if err != nil {
			return err
		}
		report.Branches = append(report.Branches, branchRefresh{Branch: branch, State: "overlay_written", OverlayPath: overlayPath})
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	rel := filepath.Join(semanticDirName, "overlays", "all-branches.json")
	if err := rejectExistingSymlinkPathComponents(storage.BrainDir, rel); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(storage.BrainDir, rel), data, 0o600); err != nil {
		return err
	}
	return nil
}

func seedNeededForBrain(manifest *exportManifest) bool {
	// Seed freshness is independent of checkpoint/session availability. MCP's
	// default refresh intentionally omits sessions, so using an empty session
	// list as a seed-missing signal rebuilt an otherwise current seed every time.
	return manifest == nil || manifest.Sources == nil || manifest.Sources.Seed == nil
}

func seedRefreshNeeded(ctx context.Context, opts Options, repoDir string, manifest *exportManifest, worktree bool) (bool, error) {
	if seedNeededForBrain(manifest) {
		return true, nil
	}
	seed := manifest.Sources.Seed
	head, err := gitScalar(ctx, opts.Runner, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return false, fmt.Errorf("resolve HEAD for seed refresh: %w", err)
	}
	if seed.Commit != head {
		return true, nil
	}
	if worktree {
		hash, err := worktreeFingerprint(ctx, opts.Runner, repoDir)
		if err != nil {
			return false, fmt.Errorf("fingerprint worktree for seed refresh: %w", err)
		}
		return seed.WorktreeMode != "worktree" || seed.WorktreeHash != hash, nil
	}
	return seed.WorktreeMode == "worktree", nil
}
