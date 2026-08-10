package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	defaultExportDir        = ""
	defaultCheckpointLimit  = 10000
	exportManifestFileName  = "manifest.json"
	exportReadmeFileName    = "README.md"
	exportSessionsDirectory = "sessions"
	exportBranchesDirectory = "branches"
	exportUnknownDirectory  = "unknown"

	exportScopeAll    = "all"
	exportScopeBranch = "branch"

	v2MainRef           = "refs/entire/checkpoints/v2/main"
	v1MainRef           = "refs/heads/entire/checkpoints/v1"
	v1OriginRef         = "refs/remotes/origin/entire/checkpoints/v1"
	v1RemoteFetchRef    = "refs/heads/entire/checkpoints/v1"
	v1RemoteRef         = v1RemoteFetchRef
	checkpointRefPrefix = "refs/entire/checkpoints/"

	checkpointBackendGitBranch = "git-branch"
	checkpointBackendGitRefs   = "git-refs"
	checkpointPrimaryEnv       = "ENTIRE_CHECKPOINTS_PRIMARY"
	checkpointMirrorsEnv       = "ENTIRE_CHECKPOINTS_MIRRORS"
	gitNoLazyFetchEnv          = "GIT_NO_LAZY_FETCH"

	checkpointRemoteProviderGitHub = "github"
	checkpointRemoteBlobFilter     = "blob:limit=128k"

	checkpointStorageV1 = 1
	checkpointStorageV2 = 2

	v1TranscriptFileName = "full.jsonl"
	v2TranscriptFileName = "transcript.jsonl"

	checkpointTrailerKey = "Entire-Checkpoint"

	gitLogRecordSeparator = "\x1e"
	gitLogFieldSeparator  = "\x00"
)

var checkpointTrailerRegex = regexp.MustCompile(
	checkpointTrailerKey + `:\s*([a-f0-9]{12}|[0-7][0-9A-HJKMNP-TV-Z]{25})(?:\s|$)`,
)

type exportCommandOptions struct {
	outputDir       string
	outputExplicit  bool
	checkpointLimit int
	entireBinary    string
	rawTranscript   bool
	scope           string
	historyIndex    bool
	debug           bool
	progress        func(exportProgress)
}

type exportProgress struct {
	Status            string
	Current           int
	Total             int
	Unit              string
	CurrentCheckpoint int
	TotalCheckpoints  int
	Sessions          int
}

func newExportCommand(opts Options) *cobra.Command {
	exportOpts := exportCommandOptions{
		outputDir:       defaultExportDir,
		checkpointLimit: defaultCheckpointLimit,
		entireBinary:    "entire",
		scope:           exportScopeAll,
	}

	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Export latest known transcript for each Entire session",
		Long: `Export writes an agent-reviewable snapshot of Entire session history.

The export contains one transcript file per unique session ID. When a session
appears in multiple checkpoints, the copy with the newest checkpoint timestamp
is selected.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExport(cmd.Context(), cmd, opts, exportOpts)
		},
	}

	cmd.Flags().StringVarP(&exportOpts.outputDir, "output", "o", defaultExportDir, "Output directory for the export (default: plugin data brain directory)")
	cmd.Flags().IntVar(&exportOpts.checkpointLimit, "checkpoint-limit", defaultCheckpointLimit, "Maximum checkpoints to inspect (0 means all)")
	cmd.Flags().StringVar(&exportOpts.entireBinary, "entire-binary", "entire", "Entire CLI binary to invoke")
	cmd.Flags().BoolVar(&exportOpts.rawTranscript, "raw", false, "Export raw agent transcripts instead of normalized compact transcripts")
	cmd.Flags().StringVar(&exportOpts.scope, "scope", exportScopeAll, "Checkpoint discovery scope: all or branch")
	cmd.Flags().BoolVar(&exportOpts.historyIndex, "history-index", false, "Build a decision/rationale index from exported sessions")
	cmd.Flags().BoolVar(&exportOpts.debug, "debug", false, "Show diagnostic warnings for successful fallbacks")

	return cmd
}

func runExport(ctx context.Context, cmd *cobra.Command, opts Options, exportOpts exportCommandOptions) error {
	if exportOpts.checkpointLimit < 0 {
		return errors.New("--checkpoint-limit must be greater than or equal to zero")
	}
	outputExplicit := exportOpts.outputExplicit || cmd.Flags().Changed("output")
	if outputExplicit && strings.TrimSpace(exportOpts.outputDir) == "" {
		return errors.New("--output must not be empty")
	}
	if strings.TrimSpace(exportOpts.entireBinary) == "" {
		return errors.New("--entire-binary must not be empty")
	}
	if exportOpts.scope != exportScopeAll && exportOpts.scope != exportScopeBranch {
		return errors.New("--scope must be either all or branch")
	}

	repoDir, err := exportRepoDir(opts.Env)
	if err != nil {
		return err
	}
	if outputExplicit {
		if _, err := validateExportDirAvailable(exportOpts.outputDir); err != nil {
			return err
		}
	}

	var warnings []string
	var selected map[string]selectedSession
	var checkpointsScanned int
	var snapshot *checkpointSnapshot

	reportExportStatus(exportOpts.progress, "detecting default branch")
	defaultBranch, defaultBranchWarnings := detectDefaultBranch(ctx, opts.Runner, repoDir)
	warnings = append(warnings, defaultBranchWarnings...)
	reportExportStatus(exportOpts.progress, "mapping checkpoint branches")
	branchDestinations, branchDestinationWarnings := buildCheckpointBranchDestinations(ctx, opts.Runner, repoDir, defaultBranch)
	warnings = append(warnings, branchDestinationWarnings...)
	reportExportStatus(exportOpts.progress, "reading checkpoint authors")
	authorIndex, authorWarnings := buildCheckpointAuthorIndex(ctx, opts.Runner, repoDir)
	warnings = append(warnings, authorWarnings...)

	// The persistent brain caches checkpoint metadata blobs by git object id so
	// repeated refreshes do not re-run `git cat-file` over the whole history.
	// An explicit --output target writes a throwaway brain, so caching is skipped.
	var metadataCache *checkpointMetadataCache
	var metadataCachePath string
	var metadataCacheBrainDir string
	if !outputExplicit {
		if storage, storageErr := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir); storageErr == nil {
			metadataCacheBrainDir = storage.BrainDir
			metadataCachePath = filepath.Join(storage.BrainDir, filepath.FromSlash(checkpointMetadataCachePath))
			metadataCache = loadCheckpointMetadataCache(metadataCachePath)
		}
	}

	if exportOpts.scope == exportScopeAll {
		loadedSnapshot, snapshotWarnings, snapshotErr := loadConfiguredCheckpointSnapshot(ctx, opts.Runner, repoDir, exportOpts.rawTranscript, exportOpts.checkpointLimit, branchDestinations, exportOpts.progress, metadataCache)
		warnings = append(warnings, snapshotWarnings...)
		if snapshotErr == nil {
			snapshot = loadedSnapshot
			defer snapshot.Cleanup()
			selected = loadedSnapshot.Selected
			checkpointsScanned = loadedSnapshot.CheckpointCount
			reportExportProgress(exportOpts.progress, checkpointsScanned, checkpointsScanned, len(selected))
			if metadataCachePath != "" && metadataCacheBrainDir != "" {
				_ = withBrainWriteLock(metadataCacheBrainDir, func() error {
					saveCheckpointMetadataCache(metadataCachePath, metadataCache)
					return nil
				})
			}
		} else if !errors.Is(snapshotErr, errCheckpointSnapshotUnavailable) {
			warnings = append(warnings, "direct checkpoint export unavailable: "+snapshotErr.Error())
		}
	}

	if selected == nil {
		if brainNoEgressMode() {
			if exportOpts.scope == exportScopeBranch {
				return errors.New("no_egress: branch-scoped checkpoint visibility requires routed discovery; refusing a false-empty export")
			}
			return errors.New("no_egress: a complete readable local checkpoint catalog could not be established; refusing routed checkpoint discovery")
		}
		checkpoints, discoverWarnings, err := discoverCheckpoints(ctx, opts.Runner, repoDir, exportOpts.entireBinary, exportOpts.scope, exportOpts.checkpointLimit)
		if err != nil {
			return err
		}
		warnings = append(warnings, discoverWarnings...)
		checkpointsScanned = len(checkpoints)

		routedSelected, routedWarnings, selectErr := selectRoutedCheckpointSessions(ctx, opts.Runner, repoDir, exportOpts.entireBinary, checkpoints, branchDestinations, exportOpts.progress)
		warnings = append(warnings, routedWarnings...)
		if selectErr != nil {
			return selectErr
		}
		selected = routedSelected
	}

	manifest := exportManifest{
		SchemaVersion:      1,
		GeneratedAt:        opts.Now().UTC(),
		RepoRoot:           repoDir,
		EntireCLIVersion:   opts.Env.CLIVersion,
		DefaultBranch:      defaultBranch,
		TranscriptMode:     actualTranscriptMode(exportOpts.rawTranscript, snapshot),
		Scope:              exportOpts.scope,
		CheckpointLimit:    exportOpts.checkpointLimit,
		CheckpointsScanned: checkpointsScanned,
		Warnings:           warnings,
	}

	sessions := flattenSessions(selected)
	applySessionAuthors(sessions, authorIndex)
	branchDirs := buildBranchDirectories(sessions, defaultBranch)
	outputDir := exportOpts.outputDir
	persistentBrain := !outputExplicit
	var cursor *exportCursor
	var cursorFile string
	var repoKey string
	if persistentBrain {
		storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
		if err != nil {
			return err
		}
		repoKey = storage.Key
		outputDir = storage.BrainDir
		cursorFile = storage.HeadPath
		cursor = loadExportCursor(cursorFile)
	}

	if persistentBrain {
		// The exclusive brain lock brackets only the SHARED-ARTIFACT phases:
		// directory preparation, then manifest/history-index/cursor writes.
		// The transcript-writing phase between them runs UNLOCKED — it shells
		// `entire checkpoint explain` per uncached session (minutes on a large
		// brain), while every other lock user times out at 10s: holding the
		// lock across it failed a concurrent distill's FINAL flush, killing an
		// hours-long run at its last step. Transcript files are per-session
		// and atomically replaced, and concurrent exports of one brain are
		// already unsupported, so the unlocked window mutates nothing another
		// writer reads mid-flight (a concurrent distill touches facts/* only).
		if err := withBrainWriteLock(outputDir, func() error {
			var err error
			outputDir, err = prepareExportDir(outputDir, true)
			if err != nil {
				return err
			}
			return ensureExportDirectories(outputDir, branchDirs)
		}); err != nil {
			return err
		}
		var transcriptWarnings []string
		if snapshot != nil {
			sessions, transcriptWarnings, err = writeSnapshotSessionTranscripts(ctx, opts.Runner, snapshot, outputDir, sessions, branchDirs, cursor)
			if err != nil {
				return err
			}
		} else {
			transcriptWarnings, err = writeSessionTranscripts(ctx, opts.Runner, repoDir, outputDir, exportOpts.entireBinary, exportOpts.rawTranscript, sessions, branchDirs, cursor)
			if err != nil {
				return err
			}
		}
		if err := withBrainWriteLock(outputDir, func() error {
			manifest.Warnings = usefulExportWarnings(append(manifest.Warnings, transcriptWarnings...), exportOpts.debug)
			manifest.Sessions = sessions
			manifest.Branches = summarizeBranchExports(sessions, branchDirs, defaultBranch)
			if err := writeBrainSessionSourceLocked(outputDir, repoKey, manifest); err != nil {
				return err
			}
			if exportOpts.historyIndex {
				if _, err := writeBrainHistoryIndexAndSourceLocked(outputDir, opts.Now().UTC(), nil); err != nil {
					return err
				}
			}
			if err := writeExportCursor(cursorFile, manifest); err != nil {
				return err
			}
			return cleanupStaleSessionFiles(outputDir, sessions)
		}); err != nil {
			return err
		}
	} else {
		outputDir, err = prepareExportDir(outputDir, false)
		if err != nil {
			return err
		}
		if err := ensureExportDirectories(outputDir, branchDirs); err != nil {
			return err
		}
		var transcriptWarnings []string
		if snapshot != nil {
			sessions, transcriptWarnings, err = writeSnapshotSessionTranscripts(ctx, opts.Runner, snapshot, outputDir, sessions, branchDirs, cursor)
			if err != nil {
				return err
			}
		} else {
			transcriptWarnings, err = writeSessionTranscripts(ctx, opts.Runner, repoDir, outputDir, exportOpts.entireBinary, exportOpts.rawTranscript, sessions, branchDirs, cursor)
			if err != nil {
				return err
			}
		}
		manifest.Warnings = usefulExportWarnings(append(manifest.Warnings, transcriptWarnings...), exportOpts.debug)
		manifest.Sessions = sessions
		manifest.Branches = summarizeBranchExports(sessions, branchDirs, defaultBranch)
		if err := writeBrainManifestAndReadme(outputDir, manifest); err != nil {
			return err
		}
		if exportOpts.historyIndex {
			if _, err := writeBrainHistoryIndexAndSourceLocked(outputDir, opts.Now().UTC(), nil); err != nil {
				return err
			}
		}
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "exported %d sessions from %d checkpoints\n", len(manifest.Sessions), checkpointsScanned)
	fmt.Fprintf(out, "output: %s\n", outputDir)
	if len(manifest.Warnings) > 0 {
		fmt.Fprintf(out, "warnings: %d\n", len(manifest.Warnings))
		for _, warning := range manifest.Warnings {
			fmt.Fprintf(out, "warning: %s\n", warning)
		}
	}
	return nil
}

func reportExportProgress(progress func(exportProgress), current, total, sessions int) {
	reportExportItemProgress(progress, "", current, total, "checkpoint", sessions)
}

func reportExportStatus(progress func(exportProgress), status string) {
	if progress == nil {
		return
	}
	progress(exportProgress{
		Status: status,
	})
}

func reportExportItemProgress(progress func(exportProgress), status string, current, total int, unit string, sessions int) {
	if progress == nil {
		return
	}
	progress(exportProgress{
		Status:            status,
		Current:           current,
		Total:             total,
		Unit:              unit,
		CurrentCheckpoint: current,
		TotalCheckpoints:  total,
		Sessions:          sessions,
	})
}

func exportRepoDir(env EntireEnv) (string, error) {
	if env.RepoRoot != "" {
		return env.RepoRoot, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	return wd, nil
}

func prepareExportDir(path string, allowExisting bool) (string, error) {
	var abs string
	var err error
	if allowExisting {
		abs, err = filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve output directory: %w", err)
		}
		info, statErr := os.Lstat(abs)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("output directory must not be a symlink: %s", abs)
			}
			if !info.IsDir() {
				return "", fmt.Errorf("output path exists and is not a directory: %s", abs)
			}
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return "", fmt.Errorf("stat output directory: %w", statErr)
		}
	} else {
		abs, err = validateExportDirAvailable(path)
		if err != nil {
			return "", err
		}
	}

	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}

	sessionsDir := filepath.Join(abs, exportSessionsDirectory)
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		return "", fmt.Errorf("create sessions directory: %w", err)
	}
	return abs, nil
}

func validateExportDirAvailable(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve output directory: %w", err)
	}
	info, err := os.Lstat(abs)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("output directory must not be a symlink: %s", abs)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("output path exists and is not a directory: %s", abs)
		}
		entries, readErr := os.ReadDir(abs)
		if readErr != nil {
			return "", fmt.Errorf("read output directory: %w", readErr)
		}
		if len(entries) > 0 {
			return "", fmt.Errorf("output directory is not empty: %s", abs)
		}
	case os.IsNotExist(err):
		return abs, nil
	default:
		return "", fmt.Errorf("stat output directory: %w", err)
	}
	return abs, nil
}

func removeForcedOutputDir(path, repoDir string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve forced output directory: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat forced output directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("forced output directory must not be a symlink: %s", abs)
	}
	if !info.IsDir() {
		return fmt.Errorf("forced output path exists and is not a directory: %s", abs)
	}
	if err := rejectDangerousForcedOutputDir(abs, repoDir); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("resolve forced output symlinks: %w", err)
	}
	if !sameCleanPath(abs, resolved) {
		if err := rejectDangerousForcedOutputDir(resolved, repoDir); err != nil {
			return err
		}
	}
	manifestPath := filepath.Join(abs, exportManifestFileName)
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("--force refuses to remove %s: missing %s", abs, exportManifestFileName)
		}
		return fmt.Errorf("stat forced output manifest: %w", err)
	}
	if manifestInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("--force refuses to remove %s: %s must not be a symlink", abs, exportManifestFileName)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read forced output manifest: %w", err)
	}
	if err := validateForcedOutputManifest(data); err != nil {
		return fmt.Errorf("--force refuses to remove %s: invalid %s: %w", abs, exportManifestFileName, err)
	}
	if err := removeAllWithRetry(abs); err != nil {
		return fmt.Errorf("remove forced output directory: %w", err)
	}
	return nil
}

func validateForcedOutputManifest(data []byte) error {
	var manifest exportManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	normalizeBrainManifest(&manifest)
	if manifest.SchemaVersion != brainManifestSchemaVersion {
		return fmt.Errorf("schema_version %d does not match brain schema %d", manifest.SchemaVersion, brainManifestSchemaVersion)
	}
	if manifest.Sources == nil {
		return errors.New("missing sources")
	}
	if manifest.Sources.Sessions != nil ||
		manifest.Sources.Seed != nil ||
		manifest.Sources.Semantic != nil ||
		manifest.Sources.History != nil ||
		manifest.Sources.Facts != nil ||
		manifest.Sources.Docs != nil {
		return nil
	}
	return errors.New("missing recognized brain sources")
}

func removeAllWithRetry(path string) error {
	var lastErr error
	for i := 0; i < 5; i++ {
		if err := os.RemoveAll(path); err != nil {
			lastErr = err
			time.Sleep(time.Duration(i+1) * 25 * time.Millisecond)
			continue
		}
		return nil
	}
	return lastErr
}

func rejectDangerousForcedOutputDir(abs, repoDir string) error {
	clean := filepath.Clean(abs)
	if isFilesystemRoot(clean) {
		return fmt.Errorf("--force refuses to remove filesystem root: %s", abs)
	}
	if home, err := os.UserHomeDir(); err == nil && sameCleanPath(clean, home) {
		return fmt.Errorf("--force refuses to remove home directory: %s", abs)
	}
	if cwd, err := os.Getwd(); err == nil && sameCleanPath(clean, cwd) {
		return fmt.Errorf("--force refuses to remove current working directory: %s", abs)
	}
	if strings.TrimSpace(repoDir) != "" {
		repoAbs, err := filepath.Abs(repoDir)
		if err == nil {
			repoCandidates := []string{filepath.Clean(repoAbs)}
			if repoResolved, resolveErr := filepath.EvalSymlinks(repoAbs); resolveErr == nil {
				repoCandidates = append(repoCandidates, filepath.Clean(repoResolved))
			}
			for _, repoCandidate := range uniqueCleanPaths(repoCandidates) {
				if pathInside(clean, repoCandidate) {
					return fmt.Errorf("--force refuses to remove a directory containing the repository: %s", abs)
				}
				if pathInside(repoCandidate, clean) {
					return fmt.Errorf("--force refuses to remove a directory inside the repository: %s", abs)
				}
			}
		}
	}
	return nil
}

func uniqueCleanPaths(paths []string) []string {
	seen := map[string]struct{}{}
	var result []string
	for _, path := range paths {
		clean := filepath.Clean(path)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		result = append(result, clean)
	}
	return result
}

func isFilesystemRoot(path string) bool {
	return filepath.Dir(path) == path
}

func sameCleanPath(a, b string) bool {
	absB, err := filepath.Abs(b)
	if err != nil {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(absB)
}

func detectDefaultBranch(ctx context.Context, runner CommandRunner, repoDir string) (string, []string) {
	if branch, ok := parseOriginHeadBranch(runGitOutput(ctx, runner, repoDir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")); ok {
		return branch, nil
	}

	for _, branch := range []string{"main", "master"} {
		if gitRefExists(ctx, runner, repoDir, "refs/heads/"+branch) || gitRefExists(ctx, runner, repoDir, "refs/remotes/origin/"+branch) {
			return branch, nil
		}
	}

	current := strings.TrimSpace(string(runGitOutput(ctx, runner, repoDir, "branch", "--show-current")))
	if current != "" {
		return current, []string{"default branch not found; using current branch " + current}
	}

	return "main", []string{"default branch not found; using main"}
}

func runGitOutput(ctx context.Context, runner CommandRunner, repoDir string, args ...string) []byte {
	stdout, _, err := runner.Run(ctx, repoDir, "git", args...)
	if err != nil {
		return nil
	}
	return stdout
}

// runCheckpointGitLocal reads local Git state with lazy promisor-object fetches
// disabled. That environment bit is the process boundary behind no-egress
// checkpoint reads: invoking a nominally read-only `git cat-file` in a partial
// clone can otherwise contact a promisor remote. A custom runner must explicitly
// implement the environment capability; silently dropping the override would
// turn a local-only promise into best effort.
func runCheckpointGitLocal(ctx context.Context, runner CommandRunner, repoDir string, args ...string) ([]byte, []byte, error) {
	if len(args) == 0 {
		return nil, nil, errors.New("local checkpoint git command is empty")
	}
	switch args[0] {
	case "fetch", "pull", "push", "ls-remote":
		return nil, nil, fmt.Errorf("local checkpoint git command refuses network-capable subcommand %q", args[0])
	}
	envRunner, ok := runner.(EnvironmentCommandRunner)
	if !ok {
		return nil, nil, errors.New("no_egress: command runner cannot enforce GIT_NO_LAZY_FETCH=1")
	}
	return envRunner.RunWithEnv(ctx, repoDir, map[string]string{gitNoLazyFetchEnv: "1"}, "git", args...)
}

func gitRefExists(ctx context.Context, runner CommandRunner, repoDir, ref string) bool {
	_, _, err := runner.Run(ctx, repoDir, "git", "show-ref", "--verify", "--quiet", ref)
	return err == nil
}

func parseOriginHeadBranch(data []byte) (string, bool) {
	ref := strings.TrimSpace(string(data))
	if ref == "" {
		return "", false
	}
	if branch, ok := strings.CutPrefix(ref, "origin/"); ok && branch != "" {
		return branch, true
	}
	parts := strings.Split(ref, "/")
	branch := parts[len(parts)-1]
	return branch, branch != ""
}

func buildCheckpointBranchDestinations(ctx context.Context, runner CommandRunner, repoDir, defaultBranch string) (checkpointBranchDestinations, []string) {
	destinations := checkpointBranchDestinations{
		DefaultBranch:        defaultBranch,
		DefaultCheckpointIDs: make(map[string]struct{}),
	}
	if defaultBranch == "" {
		return destinations, nil
	}

	defaultRef, ok := resolveDefaultBranchRef(ctx, runner, repoDir, defaultBranch)
	if !ok {
		return destinations, []string{"default branch checkpoint reachability unavailable: could not resolve " + defaultBranch}
	}

	stdout, stderr, err := runCheckpointGitWithPolicy(ctx, runner, repoDir, brainNoEgressMode(), "log", "--format=%B", "--grep", checkpointTrailerKey+":", defaultRef)
	warnings := warningLines("default branch checkpoint reachability", stderr)
	if err != nil {
		warnings = append(warnings, "default branch checkpoint reachability unavailable: "+err.Error())
		return destinations, warnings
	}
	destinations.DefaultCheckpointIDs = checkpointIDsFromCommitMessages(stdout)
	return destinations, warnings
}

func resolveDefaultBranchRef(ctx context.Context, runner CommandRunner, repoDir, defaultBranch string) (string, bool) {
	localRef := "refs/heads/" + defaultBranch
	if gitRefExists(ctx, runner, repoDir, localRef) {
		return localRef, true
	}
	remoteRef := "refs/remotes/origin/" + defaultBranch
	if gitRefExists(ctx, runner, repoDir, remoteRef) {
		return remoteRef, true
	}
	return "", false
}

func checkpointIDsFromCommitMessages(data []byte) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, checkpointID := range checkpointIDsFromCommitMessageText(string(data)) {
		ids[checkpointID] = struct{}{}
	}
	return ids
}

func checkpointIDsFromCommitMessageText(message string) []string {
	matches := checkpointTrailerRegex.FindAllStringSubmatch(message, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(matches))
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		checkpointID := strings.TrimSpace(match[1])
		if _, ok := seen[checkpointID]; ok {
			continue
		}
		seen[checkpointID] = struct{}{}
		ids = append(ids, checkpointID)
	}
	return ids
}

func buildCheckpointAuthorIndex(ctx context.Context, runner CommandRunner, repoDir string) (checkpointAuthorIndex, []string) {
	stdout, stderr, err := runCheckpointGitWithPolicy(ctx, runner, repoDir, brainNoEgressMode(), "log", "--all", "--format=%H%x00%an%x00%ae%x00%aI%x00%B%x1e", "--grep", checkpointTrailerKey+":")
	warnings := warningLines("checkpoint author index", stderr)
	if err != nil {
		warnings = append(warnings, "checkpoint author index unavailable: "+err.Error())
		return checkpointAuthorIndex{}, warnings
	}
	return checkpointAuthorsFromGitLog(stdout), warnings
}

func checkpointAuthorsFromGitLog(data []byte) checkpointAuthorIndex {
	accumulators := make(map[string]map[string]*checkpointAuthorAccumulator)

	for _, record := range strings.Split(string(data), gitLogRecordSeparator) {
		record = strings.TrimSpace(record)
		if record == "" {
			continue
		}

		parts := strings.SplitN(record, gitLogFieldSeparator, 5)
		if len(parts) != 5 {
			continue
		}

		commitSHA := strings.TrimSpace(parts[0])
		author := exportAuthor{
			Name:  strings.TrimSpace(parts[1]),
			Email: strings.TrimSpace(parts[2]),
		}
		authoredAt, _ := time.Parse(time.RFC3339, strings.TrimSpace(parts[3])) //nolint:errcheck // Timestamp is optional metadata.
		checkpointIDs := checkpointIDsFromCommitMessageText(parts[4])
		if commitSHA == "" || len(checkpointIDs) == 0 {
			continue
		}

		authorKey := exportAuthorKey(author)
		for _, checkpointID := range checkpointIDs {
			if accumulators[checkpointID] == nil {
				accumulators[checkpointID] = make(map[string]*checkpointAuthorAccumulator)
			}
			acc := accumulators[checkpointID][authorKey]
			if acc == nil {
				acc = &checkpointAuthorAccumulator{
					Author:  author,
					Commits: make(map[string]struct{}),
				}
				accumulators[checkpointID][authorKey] = acc
			}
			if _, ok := acc.Commits[commitSHA]; !ok {
				acc.Commits[commitSHA] = struct{}{}
				acc.Author.Commits++
			}
			if authoredAt.After(acc.LastCommitAt) {
				acc.LastCommitAt = authoredAt.UTC()
			}
		}
	}

	index := checkpointAuthorIndex{AuthorsByCheckpoint: make(map[string][]exportAuthor, len(accumulators))}
	for checkpointID, byAuthor := range accumulators {
		authors := make([]exportAuthor, 0, len(byAuthor))
		for _, acc := range byAuthor {
			author := acc.Author
			if !acc.LastCommitAt.IsZero() {
				lastCommitAt := acc.LastCommitAt
				author.LastCommitAt = &lastCommitAt
			}
			authors = append(authors, author)
		}
		sort.Slice(authors, func(i, j int) bool {
			if authors[i].Commits != authors[j].Commits {
				return authors[i].Commits > authors[j].Commits
			}
			if authorLastCommitAt(authors[i]).Equal(authorLastCommitAt(authors[j])) {
				return exportAuthorKey(authors[i]) < exportAuthorKey(authors[j])
			}
			return authorLastCommitAt(authors[i]).After(authorLastCommitAt(authors[j]))
		})
		index.AuthorsByCheckpoint[checkpointID] = authors
	}
	return index
}

func exportAuthorKey(author exportAuthor) string {
	email := strings.ToLower(strings.TrimSpace(author.Email))
	if email != "" {
		return email
	}
	return strings.ToLower(strings.TrimSpace(author.Name))
}

func authorLastCommitAt(author exportAuthor) time.Time {
	if author.LastCommitAt == nil {
		return time.Time{}
	}
	return *author.LastCommitAt
}

func discoverCheckpoints(ctx context.Context, runner CommandRunner, repoDir, entireBinary, scope string, limit int) ([]checkpointListEntry, []string, error) {
	if scope == exportScopeBranch {
		if brainNoEgressMode() {
			return nil, []string{"no_egress: branch checkpoint fallback skipped"}, nil
		}
		return listBranchCheckpoints(ctx, runner, repoDir, entireBinary, limit)
	}

	checkpoints, warnings, err := listAllCheckpointRefs(ctx, runner, repoDir, limit)
	if err != nil {
		return nil, nil, err
	}
	if brainNoEgressMode() {
		warnings = append(warnings, "no_egress: branch checkpoint fallback skipped")
		return checkpoints, warnings, nil
	}

	fallback, fallbackWarnings, fallbackErr := listAllRoutedCheckpoints(ctx, runner, repoDir, entireBinary, limit)
	warnings = append(warnings, fallbackWarnings...)
	scopeIncomplete := hasCheckpointScopeIncompleteWarning(fallbackWarnings)
	if fallbackErr != nil {
		if len(checkpoints) > 0 {
			if scopeIncomplete {
				warnings = append(warnings, checkpointScopeIncompleteCode+": routed checkpoint discovery failed after reporting partial scope; using locally enumerated ref names only")
			} else {
				warnings = append(warnings, "checkpoint_scope_incomplete: complete --search-all checkpoint discovery unavailable; using locally enumerated refs only: "+fallbackErr.Error())
			}
			return checkpoints, warnings, nil
		}
		if scopeIncomplete {
			return nil, warnings, fmt.Errorf("%s: routed checkpoint discovery failed and no local checkpoint IDs were enumerable", checkpointScopeIncompleteCode)
		}
		return nil, warnings, fmt.Errorf("complete routed checkpoint discovery failed: %w", fallbackErr)
	}
	if scopeIncomplete && len(checkpoints) == 0 && len(fallback) == 0 {
		return nil, warnings, fmt.Errorf("%s: Entire returned no readable checkpoint IDs from an incomplete persistent-store inventory", checkpointScopeIncompleteCode)
	}
	if !scopeIncomplete && len(checkpoints) == 0 {
		warnings = append(warnings, "used Entire's complete routed checkpoint list because no local metadata refs were enumerable")
	} else if !scopeIncomplete {
		warnings = append(warnings, "combined locally enumerated checkpoint refs with Entire's complete routed checkpoint list")
	}
	return mergeCheckpointListEntries(fallback, checkpoints, limit), warnings, nil
}

func hasCheckpointScopeIncompleteWarning(warnings []string) bool {
	for _, warning := range warnings {
		if strings.HasPrefix(strings.TrimSpace(warning), checkpointScopeIncompleteCode+":") {
			return true
		}
	}
	return false
}

func mergeCheckpointListEntries(preferred, additional []checkpointListEntry, limit int) []checkpointListEntry {
	merged := make([]checkpointListEntry, 0, len(preferred)+len(additional))
	seen := make(map[string]struct{}, len(preferred)+len(additional))
	for _, entries := range [][]checkpointListEntry{preferred, additional} {
		for _, entry := range entries {
			if entry.CheckpointID == "" {
				continue
			}
			if _, ok := seen[entry.CheckpointID]; ok {
				continue
			}
			seen[entry.CheckpointID] = struct{}{}
			merged = append(merged, entry)
		}
	}
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

func listBranchCheckpoints(ctx context.Context, runner CommandRunner, repoDir, entireBinary string, limit int) ([]checkpointListEntry, []string, error) {
	return listRoutedCheckpoints(ctx, runner, repoDir, entireBinary, limit, false)
}

func listAllRoutedCheckpoints(ctx context.Context, runner CommandRunner, repoDir, entireBinary string, limit int) ([]checkpointListEntry, []string, error) {
	return listRoutedCheckpoints(ctx, runner, repoDir, entireBinary, limit, true)
}

const (
	checkpointScopeIncompleteCode = "checkpoint_scope_incomplete"
	entireCheckpointScopePrefix   = "ENTIRE_CHECKPOINT_SCOPE_V1 "
)

type routedCheckpointScopeStatus struct {
	SchemaVersion int                          `json:"schema_version"`
	Code          string                       `json:"code"`
	Complete      bool                         `json:"complete"`
	Issues        []routedCheckpointScopeIssue `json:"issues"`
}

type routedCheckpointScopeIssue struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

func listRoutedCheckpoints(ctx context.Context, runner CommandRunner, repoDir, entireBinary string, limit int, searchAll bool) ([]checkpointListEntry, []string, error) {
	args := []string{"checkpoint", "explain", "--json"}
	if searchAll {
		args = append(args, "--search-all", "--limit", strconv.Itoa(limit))
	} else if limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	}
	stdout, stderr, err := runner.Run(ctx, repoDir, entireBinary, args...)
	warnings := routedCheckpointWarnings(stderr)
	if err != nil {
		return nil, warnings, fmt.Errorf("list checkpoints: %w", err)
	}

	var checkpoints []checkpointListEntry
	if err := json.Unmarshal(stdout, &checkpoints); err != nil {
		return nil, nil, fmt.Errorf("parse checkpoint list json: %w", err)
	}
	return checkpoints, warnings, nil
}

// routedCheckpointWarnings preserves ordinary stderr diagnostics while
// recognizing Entire's additive complete-scope status line. The checkpoint
// array on stdout intentionally remains backward-compatible; this stable
// stderr record is what prevents an empty or partial array from being mistaken
// for a complete inventory. Its payload contains codes and counts only.
func routedCheckpointWarnings(data []byte) []string {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	warnings := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if payload, ok := strings.CutPrefix(line, entireCheckpointScopePrefix); ok {
			warnings = append(warnings, checkpointScopeWarning(payload))
			continue
		}
		// Older Entire versions exposed remote discovery degradation as a human
		// warning only. Classify that legacy line conservatively too, so upgrading
		// Brain before Entire cannot reintroduce a false-complete manifest.
		if strings.Contains(line, "could not reach checkpoint remote; showing local checkpoints only") {
			warnings = append(warnings, checkpointScopeIncompleteCode+": Entire could not enumerate the configured checkpoint remote")
			continue
		}
		warnings = append(warnings, "checkpoint list: "+line)
	}
	return warnings
}

func checkpointScopeWarning(payload string) string {
	var status routedCheckpointScopeStatus
	if err := json.Unmarshal([]byte(payload), &status); err != nil || status.SchemaVersion != 1 || status.Code != checkpointScopeIncompleteCode || status.Complete {
		return checkpointScopeIncompleteCode + ": Entire emitted an invalid or contradictory checkpoint scope status"
	}
	parts := make([]string, 0, len(status.Issues))
	for _, issue := range status.Issues {
		code := strings.TrimSpace(issue.Code)
		if code == "" {
			continue
		}
		count := issue.Count
		if count <= 0 {
			count = 1
		}
		parts = append(parts, fmt.Sprintf("%s=%d", code, count))
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return checkpointScopeIncompleteCode + ": Entire reported incomplete checkpoint enumeration"
	}
	return checkpointScopeIncompleteCode + ": Entire reported incomplete checkpoint enumeration (" + strings.Join(parts, ", ") + ")"
}

func listAllCheckpointRefs(ctx context.Context, runner CommandRunner, repoDir string, limit int) ([]checkpointListEntry, []string, error) {
	settings, settingsErr := readEntireSettings(repoDir)
	includeV2 := settingsErr == nil && settings.CheckpointsV2Enabled()
	ids, warnings := listLocalCheckpointRefIDs(ctx, runner, repoDir, localCheckpointRefs(includeV2))
	refIDs, refWarnings := listLocalPerCheckpointRefIDs(ctx, runner, repoDir)
	for id := range refIDs {
		ids[id] = struct{}{}
	}
	warnings = append(warnings, refWarnings...)
	if len(ids) == 0 && brainNoEgressMode() {
		warnings = append(warnings, "no_egress: checkpoint remote discovery skipped")
	}

	return checkpointEntriesFromIDs(ids, limit), warningsFromLimit(ids, limit, warnings), nil
}

// listLocalPerCheckpointRefIDs enumerates the current git-refs checkpoint
// backend without reading checkpoint payloads. The ref name is an untrusted
// boundary: accept only Entire's documented two-component layout and require
// the shard to match the checkpoint ID before passing an ID to the CLI.
func listLocalPerCheckpointRefIDs(ctx context.Context, runner CommandRunner, repoDir string) (map[string]struct{}, []string) {
	ids := make(map[string]struct{})
	stdout, stderr, err := runCheckpointGitWithPolicy(ctx, runner, repoDir, brainNoEgressMode(), "for-each-ref", "--format=%(refname)", checkpointRefPrefix)
	warnings := warningLines("checkpoint refs", stderr)
	if err != nil {
		warnings = append(warnings, "checkpoint ref namespace unavailable: "+err.Error())
		return ids, warnings
	}
	for _, line := range strings.Split(string(stdout), "\n") {
		if id, ok := checkpointIDFromRefName(strings.TrimSpace(line)); ok {
			ids[id] = struct{}{}
		}
	}
	return ids, warnings
}

func checkpointIDFromRefName(ref string) (string, bool) {
	relative, ok := strings.CutPrefix(ref, checkpointRefPrefix)
	if !ok {
		return "", false
	}
	parts := strings.Split(relative, "/")
	if len(parts) != 2 || len(parts[0]) != 2 {
		return "", false
	}
	id := parts[1]
	if !isCheckpointRefID(id) || parts[0] != id[len(id)-2:] {
		return "", false
	}
	return id, true
}

func isCheckpointRefID(value string) bool {
	if isCheckpointID(value) {
		return true
	}
	if len(value) != 26 || value[0] < '0' || value[0] > '7' {
		return false
	}
	for _, r := range value {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", r) {
			return false
		}
	}
	return true
}

func checkpointULIDTime(value string) (time.Time, bool) {
	if len(value) != 26 || !isCheckpointRefID(value) || isCheckpointID(value) {
		return time.Time{}, false
	}
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var millis uint64
	for _, r := range value[:10] {
		index := strings.IndexRune(alphabet, r)
		if index < 0 {
			return time.Time{}, false
		}
		millis = millis<<5 | uint64(index)
	}
	return time.UnixMilli(int64(millis)).UTC(), true
}

func localCheckpointRefs(includeV2 bool) []string {
	if includeV2 {
		return []string{v2MainRef, v1MainRef, v1OriginRef}
	}
	return []string{v1MainRef, v1OriginRef}
}

func listLocalCheckpointRefIDs(ctx context.Context, runner CommandRunner, repoDir string, refs []string) (map[string]struct{}, []string) {
	ids := make(map[string]struct{})
	var refErrors []string
	var warnings []string

	for _, ref := range refs {
		stdout, stderr, err := runCheckpointGitWithPolicy(ctx, runner, repoDir, brainNoEgressMode(), "ls-tree", "-r", "--name-only", ref)
		if err != nil {
			refErrors = append(refErrors, fmt.Sprintf("checkpoint ref %s unavailable: %v", ref, err))
			continue
		}
		warnings = append(warnings, warningLines("checkpoint ref "+ref, stderr)...)
		for _, id := range checkpointIDsFromTreeListing(stdout) {
			ids[id] = struct{}{}
		}
	}
	if len(ids) == 0 {
		warnings = append(warnings, refErrors...)
	}
	return ids, warnings
}

func checkpointEntriesFromIDs(ids map[string]struct{}, limit int) []checkpointListEntry {
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(sorted)))
	if limit > 0 && len(sorted) > limit {
		sorted = sorted[:limit]
	}

	checkpoints := make([]checkpointListEntry, 0, len(sorted))
	for _, id := range sorted {
		checkpoints = append(checkpoints, checkpointListEntry{
			CheckpointID: id,
			IsLogsOnly:   true,
		})
	}
	return checkpoints
}

func warningsFromLimit(ids map[string]struct{}, limit int, warnings []string) []string {
	if limit > 0 && len(ids) > limit {
		warnings = append(warnings, fmt.Sprintf("checkpoint ref discovery capped at %d checkpoints; rerun with --checkpoint-limit <N> to inspect more", limit))
	}
	return warnings
}

func readEntireSettings(repoDir string) (entireSettingsFile, error) {
	data, err := readSettingsFileConfined(filepath.Join(repoDir, ".entire", "settings.json"))
	if err != nil {
		return entireSettingsFile{}, fmt.Errorf("read .entire/settings.json: %w", err)
	}

	var settings entireSettingsFile
	if err := json.Unmarshal(data, &settings); err != nil {
		return entireSettingsFile{}, fmt.Errorf("parse .entire/settings.json: %w", err)
	}
	return settings, nil
}

func (s entireSettingsFile) CheckpointRemoteFetchURL() (string, error) {
	remote := s.StrategyOptions.CheckpointRemote
	if remote == nil || remote.Provider == "" || remote.Repo == "" {
		return "", errors.New("strategy_options.checkpoint_remote is not configured")
	}

	switch strings.ToLower(strings.TrimSpace(remote.Provider)) {
	case checkpointRemoteProviderGitHub:
		repo := strings.Trim(strings.TrimSpace(remote.Repo), "/")
		if err := validateGitHubRepoSlug(repo); err != nil {
			return "", fmt.Errorf("invalid checkpoint_remote repo %q: %w", remote.Repo, err)
		}
		return "https://github.com/" + repo + ".git", nil
	default:
		return "", fmt.Errorf("unsupported checkpoint_remote provider %q", remote.Provider)
	}
}

var githubRepoComponent = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validateGitHubRepoSlug ensures a checkpoint_remote repo from .entire/settings.json
// is a plain "owner/name" slug. The fetch host is already hardcoded to github.com,
// but an unvalidated repo string could otherwise smuggle path traversal, extra
// path segments, or query/fragment junk into the fetch URL; constraining it to two
// clean components removes that ambiguity.
func validateGitHubRepoSlug(repo string) error {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return errors.New("must be of the form owner/name")
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || !githubRepoComponent.MatchString(p) {
			return fmt.Errorf("invalid path component %q", p)
		}
	}
	return nil
}

func (s entireSettingsFile) CheckpointsV2Enabled() bool {
	if s.StrategyOptions.CheckpointsVersionValue() == 2 {
		return true
	}
	if s.StrategyOptions.CheckpointsV2 != nil {
		return *s.StrategyOptions.CheckpointsV2
	}
	return false
}

func (s entireStrategyOptions) CheckpointsVersionValue() int {
	if s.CheckpointsVersion == nil {
		return 1
	}
	switch v := s.CheckpointsVersion.(type) {
	case int:
		if v == 1 || v == 2 {
			return v
		}
	case float64:
		if v == 1 || v == 2 {
			return int(v)
		}
	case string:
		parsed, err := strconv.Atoi(v)
		if err == nil && (parsed == 1 || parsed == 2) {
			return parsed
		}
	}
	return 1
}

var errCheckpointSnapshotUnavailable = errors.New("checkpoint snapshot unavailable")

func loadConfiguredCheckpointSnapshot(ctx context.Context, runner CommandRunner, repoDir string, raw bool, limit int, branchDestinations checkpointBranchDestinations, progress func(exportProgress), cache *checkpointMetadataCache) (*checkpointSnapshot, []string, error) {
	settings, settingsErr := readEntireSettings(repoDir)
	primary, configured, primaryErr := configuredCheckpointPrimary(repoDir, settings)
	if primaryErr != nil {
		warning := "direct checkpoint snapshot skipped: " + primaryErr.Error()
		return nil, []string{warning}, fmt.Errorf("%w: %s", errCheckpointSnapshotUnavailable, warning)
	}
	if !configured {
		primary = checkpointBackendGitBranch
	}
	if settingsErr != nil && !errors.Is(settingsErr, os.ErrNotExist) && !configured {
		warning := "direct checkpoint snapshot skipped because base settings are malformed or unreadable: " + settingsErr.Error()
		return nil, []string{warning}, fmt.Errorf("%w: %s", errCheckpointSnapshotUnavailable, warning)
	}

	// A configured remote can contain names that do not exist in the local ref
	// namespace. The routed --search-all list is the only complete inventory for
	// that topology, so do not let a locally readable fast path hide remote-only
	// checkpoints. No-egress mode intentionally reads the local catalog only.
	if !brainNoEgressMode() && checkpointRemoteMayExtendCatalog(repoDir, settings, settingsErr) {
		return nil, nil, fmt.Errorf("%w: configured checkpoint remote requires complete routed discovery", errCheckpointSnapshotUnavailable)
	}

	// The legacy compact v2 reader cannot be a complete scope-all fast path.
	// New git-refs checkpoints are v1-shaped, and one export manifest cannot
	// honestly label a mixed compact-v2/full-v1 snapshot with one transcript
	// mode. Route through Entire, which resolves the active topology per ID.
	if settingsErr == nil && settings.CheckpointsV2Enabled() && primary == checkpointBackendGitBranch {
		refNames, probeWarnings, probeErr := probeLocalPerCheckpointRefs(ctx, runner, repoDir, brainNoEgressMode())
		if probeErr != nil {
			if brainNoEgressMode() {
				probeWarnings = append(probeWarnings, "no_egress: checkpoint topology probe failed closed")
			}
			return nil, probeWarnings, fmt.Errorf("%w: cannot prove whether the v2 catalog is mixed with git-refs: %v", errCheckpointSnapshotUnavailable, probeErr)
		}
		if len(refNames) > 0 {
			if brainNoEgressMode() {
				return nil, probeWarnings, fmt.Errorf("%w: no-egress mode cannot route mixed v2 and git-refs transcript formats", errCheckpointSnapshotUnavailable)
			}
			return nil, probeWarnings, fmt.Errorf("%w: mixed v2 and git-refs checkpoint topology requires routed discovery", errCheckpointSnapshotUnavailable)
		}
		if raw {
			return nil, probeWarnings, fmt.Errorf("%w: direct v2 raw transcript export is not implemented", errCheckpointSnapshotUnavailable)
		}
		treeOut, treeStderr, treeErr := runCheckpointGitWithPolicy(ctx, runner, repoDir, brainNoEgressMode(), "ls-tree", "-r", "--name-only", v2MainRef)
		probeWarnings = append(probeWarnings, warningLines("checkpoint ref "+v2MainRef, treeStderr)...)
		if treeErr != nil {
			if brainNoEgressMode() {
				probeWarnings = append(probeWarnings, "no_egress: checkpoint remote snapshot fetch skipped")
			}
			return nil, probeWarnings, fmt.Errorf("%w: read checkpoint ref %s: %v", errCheckpointSnapshotUnavailable, v2MainRef, treeErr)
		}
		checkpointCount := len(checkpointIDsFromTreeListing(treeOut))
		if limit > 0 && checkpointCount > limit {
			return nil, probeWarnings, fmt.Errorf("%w: v2 local catalog has %d checkpoints and cannot apply newest-first limit %d without routed metadata", errCheckpointSnapshotUnavailable, checkpointCount, limit)
		}
		snapshot, snapshotWarnings, err := loadCheckpointSnapshotFromGitDirPolicy(ctx, runner, repoDir, v2MainRef, checkpointStorageV2, v2TranscriptFileName, "compact", 0, branchDestinations, progress, cache, brainNoEgressMode())
		probeWarnings = append(probeWarnings, snapshotWarnings...)
		if err != nil {
			return nil, probeWarnings, err
		}
		probeWarnings = append(probeWarnings, fmt.Sprintf("exporting compact transcripts directly from local checkpoint ref %s", v2MainRef))
		return snapshot, probeWarnings, nil
	}

	snapshot, warnings, err := loadLocalCheckpointUnionSnapshot(ctx, runner, repoDir, primary, limit, branchDestinations, progress, cache, brainNoEgressMode())
	if err == nil {
		warnings = append(warnings, "exporting raw transcripts directly from complete local checkpoint store union")
		if !raw {
			warnings = append(warnings, "compact transcript unavailable for v1 checkpoints; exported raw full.jsonl logs")
		}
		return snapshot, warnings, nil
	}
	if brainNoEgressMode() {
		warnings = append(warnings, "no_egress: checkpoint remote snapshot fetch skipped")
		return nil, warnings, fmt.Errorf("%w: local checkpoint refs unavailable and checkpoint remote disabled by no-egress mode", errCheckpointSnapshotUnavailable)
	}
	return nil, warnings, err
}

func checkpointRemoteMayExtendCatalog(repoDir string, base entireSettingsFile, baseErr error) bool {
	if baseErr == nil && base.StrategyOptions.CheckpointRemote != nil {
		return true
	}
	localPath := filepath.Join(repoDir, ".entire", "settings.local.json")
	data, err := readSettingsFileConfined(localPath)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	var envelope struct {
		StrategyOptions map[string]json.RawMessage `json:"strategy_options"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		// Entire's merged settings cannot be established either. Route through
		// the CLI so a direct local snapshot cannot turn ambiguity into omission.
		return true
	}
	_, present := envelope.StrategyOptions["checkpoint_remote"]
	return present
}

// configuredCheckpointPrimary resolves just the checkpoint selection needed to
// decide whether the aggregate-branch snapshot is authoritative. It mirrors the
// public precedence contract (environment, then local settings, then base
// settings) without interpreting any backend-specific configuration.
func configuredCheckpointPrimary(repoDir string, base entireSettingsFile) (string, bool, error) {
	if value := strings.TrimSpace(os.Getenv(checkpointPrimaryEnv)); value != "" {
		selected := entireCheckpointsSettings{Primary: entireCheckpointBackendSettings{Type: value}}
		for _, mirror := range strings.Split(os.Getenv(checkpointMirrorsEnv), ",") {
			if mirror = strings.TrimSpace(mirror); mirror != "" {
				selected.Mirrors = append(selected.Mirrors, entireCheckpointBackendSettings{Type: mirror})
			}
		}
		if err := validateDirectCheckpointTopology(selected); err != nil {
			return "", true, err
		}
		return value, true, nil
	}

	basePath := filepath.Join(repoDir, ".entire", "settings.json")
	localPath := filepath.Join(repoDir, ".entire", "settings.local.json")
	raw, source, ok := rawCheckpointSettingsBlock(localPath)
	if !ok {
		raw, source, ok = rawCheckpointSettingsBlock(basePath)
	}
	if !ok {
		// Retain the in-memory fallback for focused callers that do not create a
		// settings file. Production callers resolve from the raw file so strict
		// unknown-field handling stays identical to Entire's loader.
		if base.Checkpoints == nil {
			return "", false, nil
		}
		if err := validateDirectCheckpointTopology(*base.Checkpoints); err != nil {
			return "", true, err
		}
		return base.Checkpoints.Primary.Type, true, nil
	}

	var selected entireCheckpointsSettings
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&selected); err != nil {
		return "", true, fmt.Errorf("parse %s checkpoints: %w", source, err)
	}
	if err := validateDirectCheckpointTopology(selected); err != nil {
		return "", true, fmt.Errorf("%s: %w", source, err)
	}
	return selected.Primary.Type, true, nil
}

// rawCheckpointSettingsBlock deliberately mirrors Entire's fail-soft envelope
// extraction: an absent/unreadable/syntactically-invalid whole file contributes
// no checkpoint selection, while a present checkpoints block is decoded
// strictly by configuredCheckpointPrimary.
func rawCheckpointSettingsBlock(filePath string) (json.RawMessage, string, bool) {
	data, err := readSettingsFileConfined(filePath)
	if err != nil {
		return nil, filePath, false
	}
	var envelope struct {
		Checkpoints json.RawMessage `json:"checkpoints"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || len(envelope.Checkpoints) == 0 {
		return nil, filePath, false
	}
	return envelope.Checkpoints, filePath, true
}

func readSettingsFileConfined(filePath string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(filePath))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(filepath.Base(filePath))
}

func validateDirectCheckpointTopology(selected entireCheckpointsSettings) error {
	if selected.Primary.Type == "" {
		return errors.New("checkpoints.primary.type is required")
	}
	if selected.Primary.Type != checkpointBackendGitBranch && selected.Primary.Type != checkpointBackendGitRefs {
		return fmt.Errorf("checkpoint primary backend %q is not supported by direct local reads", selected.Primary.Type)
	}
	seen := map[string]struct{}{selected.Primary.Type: {}}
	for i, mirror := range selected.Mirrors {
		if mirror.Type == "" {
			return fmt.Errorf("checkpoints.mirrors[%d].type is required", i)
		}
		if mirror.Type != checkpointBackendGitBranch && mirror.Type != checkpointBackendGitRefs {
			return fmt.Errorf("checkpoint mirror backend %q is not supported by direct local reads", mirror.Type)
		}
		if _, exists := seen[mirror.Type]; exists {
			return fmt.Errorf("checkpoint backend %q is configured more than once", mirror.Type)
		}
		seen[mirror.Type] = struct{}{}
	}
	return nil
}

type loadedLocalCheckpoint struct {
	id        string
	sourceKey string
	source    checkpointSnapshotSource
	selected  map[string]selectedSession
	createdAt time.Time
}

// loadLocalCheckpointUnionSnapshot reads the same local persistent-store view
// as Entire's kind-routing store without invoking Entire or fetching objects.
// The aggregate git-branch tree and each git-refs checkpoint use different tree
// roots, so every selected session retains a source key for later transcript
// reads. This also prevents equal virtual paths from colliding during migration.
func loadLocalCheckpointUnionSnapshot(ctx context.Context, runner CommandRunner, repoDir, primary string, limit int, branchDestinations checkpointBranchDestinations, progress func(exportProgress), cache *checkpointMetadataCache, localOnly bool) (*checkpointSnapshot, []string, error) {
	if primary == "" {
		primary = checkpointBackendGitBranch
	}
	if primary != checkpointBackendGitBranch && primary != checkpointBackendGitRefs {
		return nil, nil, fmt.Errorf("%w: unsupported local checkpoint primary %q", errCheckpointSnapshotUnavailable, primary)
	}
	reportExportStatus(progress, "reading local checkpoint stores")

	branchSources, branchWarnings := loadAggregateCheckpointSources(ctx, runner, repoDir, localOnly, cache)
	refSources, refWarnings := loadPerCheckpointSources(ctx, runner, repoDir, localOnly, cache)
	warnings := append(branchWarnings, refWarnings...)

	ids := make(map[string]struct{}, len(branchSources)+len(refSources))
	for id := range branchSources {
		ids[id] = struct{}{}
	}
	for id := range refSources {
		ids[id] = struct{}{}
	}
	if len(ids) == 0 {
		return nil, warnings, fmt.Errorf("%w: no local checkpoint refs readable", errCheckpointSnapshotUnavailable)
	}

	sortedIDs := make([]string, 0, len(ids))
	for id := range ids {
		sortedIDs = append(sortedIDs, id)
	}
	sort.Strings(sortedIDs)
	loaded := make([]loadedLocalCheckpoint, 0, len(sortedIDs))
	unreadable := make(map[string]struct{})
	for _, id := range sortedIDs {
		var candidates []checkpointSnapshotSource
		if isCheckpointID(id) {
			if primary == checkpointBackendGitRefs {
				if source, ok := refSources[id]; ok {
					candidates = append(candidates, source)
				}
				if source, ok := branchSources[id]; ok {
					candidates = append(candidates, source)
				}
			} else if source, ok := branchSources[id]; ok {
				// Legacy IDs are authoritative on git-branch when it is primary.
				candidates = append(candidates, source)
			}
		} else if source, ok := refSources[id]; ok {
			// ULIDs are kind-routed exclusively to git-refs under either primary.
			candidates = append(candidates, source)
		}
		if len(candidates) == 0 {
			unreadable[id] = struct{}{}
			warnings = append(warnings, fmt.Sprintf("%s: skipped checkpoint %s because no configured local backend could read it", checkpointScopeIncompleteCode, id))
			continue
		}

		var candidateWarnings []string
		resolved := false
		for _, source := range candidates {
			reader := &checkpointBlobReader{
				runner:      runner,
				gitDir:      source.GitDir,
				ref:         source.Ref,
				virtualRoot: source.VirtualRoot,
				actualRoot:  source.ActualRoot,
				localOnly:   source.LocalOnly,
				oids:        source.OIDs,
				cache:       cache,
			}
			if cache != nil && len(source.OIDs) > 0 {
				cache.active = true
			}
			selected, _, sourceWarnings, err := readCheckpointSnapshotMetadata(ctx, reader, v1TranscriptFileName, source.TreePaths, 0, branchDestinations, nil)
			candidateWarnings = append(candidateWarnings, sourceWarnings...)
			if err != nil {
				candidateWarnings = append(candidateWarnings, fmt.Sprintf("checkpoint ref %s unreadable for %s: %v", source.Ref, id, err))
				continue
			}

			sourceKey := source.Ref + "\x00" + id
			var createdAt time.Time
			for key, session := range selected {
				if session.CheckpointID != id {
					delete(selected, key)
					continue
				}
				session.SourceKey = sourceKey
				selected[key] = session
				if session.CreatedAt.After(createdAt) {
					createdAt = session.CreatedAt
				}
			}
			if createdAt.IsZero() {
				if ulidTime, ok := checkpointULIDTime(id); ok {
					createdAt = ulidTime
				}
			}
			if len(selected) == 0 {
				candidateWarnings = append(candidateWarnings, fmt.Sprintf("checkpoint ref %s contained no matching sessions for %s", source.Ref, id))
				continue
			}
			loaded = append(loaded, loadedLocalCheckpoint{id: id, sourceKey: sourceKey, source: source, selected: selected, createdAt: createdAt})
			resolved = true
			break
		}
		if !resolved {
			unreadable[id] = struct{}{}
			warnings = append(warnings, fmt.Sprintf("%s: skipped unreadable local checkpoint %s", checkpointScopeIncompleteCode, id))
		}
		warnings = append(warnings, candidateWarnings...)
	}
	if len(loaded) == 0 {
		if len(unreadable) > 0 {
			return nil, warnings, fmt.Errorf("%w: %s: none of %d requested local checkpoints were readable", errCheckpointSnapshotUnavailable, checkpointScopeIncompleteCode, len(unreadable))
		}
		return nil, warnings, fmt.Errorf("%w: local checkpoint stores contained no readable sessions", errCheckpointSnapshotUnavailable)
	}

	sort.SliceStable(loaded, func(i, j int) bool {
		if !loaded[i].createdAt.Equal(loaded[j].createdAt) {
			return loaded[i].createdAt.After(loaded[j].createdAt)
		}
		return loaded[i].id > loaded[j].id
	})
	if limit > 0 && len(loaded) > limit {
		loaded = loaded[:limit]
		warnings = append(warnings, fmt.Sprintf("checkpoint ref discovery capped at %d checkpoints; rerun with --checkpoint-limit <N> to inspect more", limit))
	}

	selected := make(map[string]selectedSession)
	sources := make(map[string]checkpointSnapshotSource, len(loaded))
	treePaths := make(map[string]struct{})
	for _, checkpoint := range loaded {
		sources[checkpoint.sourceKey] = checkpoint.source
		for path := range checkpoint.source.TreePaths {
			treePaths[path] = struct{}{}
		}
		for key, candidate := range checkpoint.selected {
			current, exists := selected[key]
			if !exists || shouldReplaceSession(current, candidate) {
				selected[key] = candidate
			}
		}
	}
	first := loaded[0].source
	return &checkpointSnapshot{
		GitDir:                first.GitDir,
		Ref:                   first.Ref,
		Version:               checkpointStorageV1,
		TranscriptMode:        "raw",
		TranscriptFileName:    v1TranscriptFileName,
		Selected:              selected,
		CheckpointCount:       len(loaded),
		TreePaths:             treePaths,
		Sources:               sources,
		UnreadableCheckpoints: unreadable,
	}, warnings, nil
}

func loadAggregateCheckpointSources(ctx context.Context, runner CommandRunner, repoDir string, localOnly bool, cache *checkpointMetadataCache) (map[string]checkpointSnapshotSource, []string) {
	sources := make(map[string]checkpointSnapshotSource)
	var deferredErrors []string
	for _, ref := range []string{v1MainRef, v1OriginRef} {
		pathsOut, stderr, err := runCheckpointGitWithPolicy(ctx, runner, repoDir, localOnly, "ls-tree", "-r", "--name-only", ref)
		if err != nil {
			deferredErrors = append(deferredErrors, fmt.Sprintf("checkpoint ref %s unavailable: %v", ref, err))
			continue
		}
		paths := treePathSet(pathsOut)
		oids := map[string]string(nil)
		if cache != nil {
			if oidOut, _, oidErr := runCheckpointGitWithPolicy(ctx, runner, repoDir, localOnly, "ls-tree", "-r", ref); oidErr == nil {
				oids = treePathOIDs(oidOut)
			}
		}
		for _, id := range checkpointIDsFromTreeListing(pathsOut) {
			root := checkpointPath(id)
			sourcePaths := pathsUnderCheckpointRoot(paths, root)
			if len(sourcePaths) == 0 {
				continue
			}
			sources[id] = checkpointSnapshotSource{
				GitDir:      repoDir,
				Ref:         ref,
				VirtualRoot: root,
				ActualRoot:  root,
				TreePaths:   sourcePaths,
				OIDs:        oidsUnderCheckpointRoot(oids, root),
				LocalOnly:   localOnly,
			}
		}
		warnings := warningLines("checkpoint ref "+ref, stderr)
		if len(sources) > 0 {
			return sources, warnings
		}
		deferredErrors = append(deferredErrors, warnings...)
	}
	return sources, deferredErrors
}

func loadPerCheckpointSources(ctx context.Context, runner CommandRunner, repoDir string, localOnly bool, cache *checkpointMetadataCache) (map[string]checkpointSnapshotSource, []string) {
	sources := make(map[string]checkpointSnapshotSource)
	refs, warnings, err := probeLocalPerCheckpointRefs(ctx, runner, repoDir, localOnly)
	if err != nil {
		return sources, warnings
	}
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ref := refs[id]
		pathsOut, treeStderr, treeErr := runCheckpointGitWithPolicy(ctx, runner, repoDir, localOnly, "ls-tree", "-r", "--name-only", ref)
		if treeErr != nil {
			warnings = append(warnings, fmt.Sprintf("checkpoint ref %s unreadable: %v", ref, treeErr))
			sources[id] = checkpointSnapshotSource{
				GitDir:      repoDir,
				Ref:         ref,
				VirtualRoot: checkpointPath(id),
				TreePaths:   map[string]struct{}{},
				LocalOnly:   localOnly,
			}
			continue
		}
		warnings = append(warnings, warningLines("checkpoint ref "+ref, treeStderr)...)
		root := checkpointPath(id)
		virtualPaths := virtualizePerCheckpointTree(treePathSet(pathsOut), root)
		var virtualOIDs map[string]string
		if cache != nil {
			if oidOut, _, oidErr := runCheckpointGitWithPolicy(ctx, runner, repoDir, localOnly, "ls-tree", "-r", ref); oidErr == nil {
				virtualOIDs = virtualizePerCheckpointOIDs(treePathOIDs(oidOut), root)
			}
		}
		sources[id] = checkpointSnapshotSource{
			GitDir:      repoDir,
			Ref:         ref,
			VirtualRoot: root,
			TreePaths:   virtualPaths,
			OIDs:        virtualOIDs,
			LocalOnly:   localOnly,
		}
	}
	return sources, warnings
}

func probeLocalPerCheckpointRefs(ctx context.Context, runner CommandRunner, repoDir string, localOnly bool) (map[string]string, []string, error) {
	refs := make(map[string]string)
	stdout, stderr, err := runCheckpointGitWithPolicy(ctx, runner, repoDir, localOnly, "for-each-ref", "--format=%(refname)", checkpointRefPrefix)
	warnings := warningLines("checkpoint refs", stderr)
	if err != nil {
		warnings = append(warnings, "checkpoint ref namespace unavailable: "+err.Error())
		return refs, warnings, err
	}
	for _, line := range strings.Split(string(stdout), "\n") {
		ref := strings.TrimSpace(line)
		id, ok := checkpointIDFromRefName(ref)
		if !ok {
			continue
		}
		refs[id] = ref
	}
	return refs, warnings, nil
}

func runCheckpointGitWithPolicy(ctx context.Context, runner CommandRunner, repoDir string, localOnly bool, args ...string) ([]byte, []byte, error) {
	if localOnly {
		return runCheckpointGitLocal(ctx, runner, repoDir, args...)
	}
	return runner.Run(ctx, repoDir, "git", args...)
}

func pathsUnderCheckpointRoot(paths map[string]struct{}, root string) map[string]struct{} {
	subset := make(map[string]struct{})
	prefix := strings.Trim(root, "/") + "/"
	for candidate := range paths {
		if strings.HasPrefix(candidate, prefix) {
			subset[candidate] = struct{}{}
		}
	}
	return subset
}

func oidsUnderCheckpointRoot(oids map[string]string, root string) map[string]string {
	subset := make(map[string]string)
	prefix := strings.Trim(root, "/") + "/"
	for candidate, oid := range oids {
		if strings.HasPrefix(candidate, prefix) {
			subset[candidate] = oid
		}
	}
	return subset
}

func virtualizePerCheckpointTree(paths map[string]struct{}, root string) map[string]struct{} {
	virtual := make(map[string]struct{}, len(paths))
	for candidate := range paths {
		candidate = strings.Trim(candidate, "/")
		if candidate == "" || candidate == "." || candidate == ".." || strings.HasPrefix(candidate, "../") {
			continue
		}
		virtual[path.Join(root, candidate)] = struct{}{}
	}
	return virtual
}

func virtualizePerCheckpointOIDs(oids map[string]string, root string) map[string]string {
	virtual := make(map[string]string, len(oids))
	for candidate, oid := range oids {
		candidate = strings.Trim(candidate, "/")
		if candidate == "" || candidate == "." || candidate == ".." || strings.HasPrefix(candidate, "../") {
			continue
		}
		virtual[path.Join(root, candidate)] = oid
	}
	return virtual
}

func loadCheckpointSnapshotFromGitDirPolicy(ctx context.Context, runner CommandRunner, gitDir, ref string, version int, transcriptFileName, mode string, limit int, branchDestinations checkpointBranchDestinations, progress func(exportProgress), cache *checkpointMetadataCache, localOnly bool) (*checkpointSnapshot, []string, error) {
	reportExportStatus(progress, "reading checkpoint tree")
	var stdout, stderr []byte
	var err error
	if localOnly {
		stdout, stderr, err = runCheckpointGitLocal(ctx, runner, gitDir, "ls-tree", "-r", "--name-only", ref)
	} else {
		stdout, stderr, err = runner.Run(ctx, gitDir, "git", "ls-tree", "-r", "--name-only", ref)
	}
	if err != nil {
		warnings := warningLines("checkpoint ref "+ref, stderr)
		return nil, warnings, fmt.Errorf("%w: read checkpoint ref %s: %v", errCheckpointSnapshotUnavailable, ref, err)
	}
	treePaths := treePathSet(stdout)
	reader := &checkpointBlobReader{runner: runner, gitDir: gitDir, ref: ref, localOnly: localOnly}
	if cache != nil {
		// One extra ls-tree resolves the object id for each metadata path,
		// keying the cache. Negligible next to the thousands of cat-file calls
		// it lets us skip. Failure to read object ids just disables caching.
		var oidOut []byte
		var oidErr error
		if localOnly {
			oidOut, _, oidErr = runCheckpointGitLocal(ctx, runner, gitDir, "ls-tree", "-r", ref)
		} else {
			oidOut, _, oidErr = runner.Run(ctx, gitDir, "git", "ls-tree", "-r", ref)
		}
		if oidErr == nil {
			reader.oids = treePathOIDs(oidOut)
			reader.cache = cache
			cache.active = true // caching genuinely ran this pass; safe to persist
		}
	}
	selected, checkpointCount, warnings, err := readCheckpointSnapshotMetadata(ctx, reader, transcriptFileName, treePaths, limit, branchDestinations, progress)
	if err != nil {
		return nil, append(warnings, warningLines("checkpoint ref "+ref, stderr)...), err
	}
	warnings = append(warnings, warningLines("checkpoint ref "+ref, stderr)...)
	sourceKey := ref + "\x00aggregate"
	for key, session := range selected {
		session.SourceKey = sourceKey
		selected[key] = session
	}
	source := checkpointSnapshotSource{
		GitDir:    gitDir,
		Ref:       ref,
		TreePaths: treePaths,
		OIDs:      reader.oids,
		LocalOnly: localOnly,
	}
	return &checkpointSnapshot{
		GitDir:             gitDir,
		Ref:                ref,
		Version:            version,
		TranscriptMode:     mode,
		TranscriptFileName: transcriptFileName,
		Selected:           selected,
		CheckpointCount:    checkpointCount,
		TreePaths:          treePaths,
		Sources:            map[string]checkpointSnapshotSource{sourceKey: source},
	}, warnings, nil
}

func readCheckpointSnapshotMetadata(ctx context.Context, reader *checkpointBlobReader, transcriptFileName string, treePaths map[string]struct{}, limit int, branchDestinations checkpointBranchDestinations, progress func(exportProgress)) (map[string]selectedSession, int, []string, error) {
	checkpointIDs := make(map[string]struct{})
	sessionMetadataPaths := make([]string, 0)
	rootMetadataPaths := make([]string, 0)
	var warnings []string

	for path := range treePaths {
		if checkpointID, ok := checkpointIDFromRootMetadataPath(path); ok {
			checkpointIDs[checkpointID] = struct{}{}
			rootMetadataPaths = append(rootMetadataPaths, path)
			continue
		}

		checkpointID, _, ok := sessionMetadataPathParts(path)
		if !ok {
			continue
		}
		checkpointIDs[checkpointID] = struct{}{}
		sessionMetadataPaths = append(sessionMetadataPaths, path)
	}
	sort.Strings(sessionMetadataPaths)
	sort.Strings(rootMetadataPaths)

	allowed := limitedCheckpointSet(checkpointIDs, limit)
	metadataProgressTotal := countAllowedSnapshotMetadataPaths(rootMetadataPaths, sessionMetadataPaths, allowed)
	metadataProgressCurrent := 0
	reportMetadataProgress := func(sessions int) {
		if metadataProgressTotal <= 0 {
			return
		}
		reportExportItemProgress(progress, "reading checkpoint metadata", metadataProgressCurrent, metadataProgressTotal, "metadata file", sessions)
	}
	advanceMetadataProgress := func(sessions int) {
		metadataProgressCurrent++
		reportMetadataProgress(sessions)
	}
	reportMetadataProgress(0)

	rootMetadataByCheckpoint, transcriptPathsByMetadata, branchByCheckpoint, unsafeCheckpointIDs, rootWarnings := readRootMetadataForSnapshot(ctx, reader, rootMetadataPaths, allowed, func() {
		advanceMetadataProgress(0)
	})
	warnings = append(warnings, rootWarnings...)

	selected := make(map[string]selectedSession)
	checkpointsWithSessionMetadata := make(map[string]struct{})
	missingTranscriptCount := 0

	for _, path := range sessionMetadataPaths {
		checkpointID, sessionIndex, _ := sessionMetadataPathParts(path)
		if _, ok := allowed[checkpointID]; !ok {
			continue
		}
		if _, unsafe := unsafeCheckpointIDs[checkpointID]; unsafe {
			advanceMetadataProgress(len(selected))
			continue
		}
		checkpointsWithSessionMetadata[checkpointID] = struct{}{}
		progressed := false
		deferProgress := func() {
			if !progressed {
				progressed = true
				advanceMetadataProgress(len(selected))
			}
		}

		transcriptPath := transcriptPathsByMetadata[path]
		if transcriptPath == "" {
			transcriptPath = snapshotTranscriptPath(checkpointID, sessionIndex, transcriptFileName)
		}
		if !hasSnapshotTranscript(transcriptPath, treePaths) {
			missingTranscriptCount++
			deferProgress()
			continue
		}

		data, err := reader.read(ctx, path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped checkpoint %s session %d: read metadata: %v", checkpointID, sessionIndex, err))
			deferProgress()
			continue
		}
		var meta checkpointExportSession
		if err := json.Unmarshal(data, &meta); err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped checkpoint %s session %d: parse metadata: %v", checkpointID, sessionIndex, err))
			deferProgress()
			continue
		}
		addSnapshotSession(selected, checkpointID, sessionIndex, transcriptPath, meta, branchByCheckpoint[checkpointID], branchDestinations)
		deferProgress()
	}

	for _, path := range rootMetadataPaths {
		checkpointID, ok := checkpointIDFromRootMetadataPath(path)
		if !ok {
			continue
		}
		if _, ok := allowed[checkpointID]; !ok {
			continue
		}
		if _, unsafe := unsafeCheckpointIDs[checkpointID]; unsafe {
			continue
		}
		if _, ok := checkpointsWithSessionMetadata[checkpointID]; ok {
			continue
		}

		transcriptPath := snapshotRootTranscriptPath(checkpointID, transcriptFileName)
		if !hasSnapshotTranscript(transcriptPath, treePaths) {
			missingTranscriptCount++
			continue
		}

		data := rootMetadataByCheckpoint[checkpointID]
		if data == nil {
			continue
		}
		var meta checkpointExportSession
		if err := json.Unmarshal(data, &meta); err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped checkpoint %s root metadata: parse metadata: %v", checkpointID, err))
			continue
		}
		addSnapshotSession(selected, checkpointID, 0, transcriptPath, meta, branchByCheckpoint[checkpointID], branchDestinations)
		reportMetadataProgress(len(selected))
	}

	if missingTranscriptCount > 0 {
		warnings = append(warnings, fmt.Sprintf("skipped %d session metadata entries with no transcript bytes in %s", missingTranscriptCount, reader.ref))
	}

	if len(selected) == 0 {
		return nil, limitedCheckpointCount(len(checkpointIDs), limit), warnings, fmt.Errorf("%w: checkpoint ref %s contained no readable sessions", errCheckpointSnapshotUnavailable, reader.ref)
	}

	if limit > 0 && len(checkpointIDs) > limit {
		warnings = append(warnings, fmt.Sprintf("checkpoint ref discovery capped at %d checkpoints; rerun with --checkpoint-limit <N> to inspect more", limit))
	}
	return selected, limitedCheckpointCount(len(checkpointIDs), limit), warnings, nil
}

func limitedCheckpointCount(count, limit int) int {
	if limit > 0 && count > limit {
		return limit
	}
	return count
}

func countAllowedSnapshotMetadataPaths(rootMetadataPaths, sessionMetadataPaths []string, allowed map[string]struct{}) int {
	total := 0
	for _, path := range rootMetadataPaths {
		checkpointID, ok := checkpointIDFromRootMetadataPath(path)
		if !ok {
			continue
		}
		if _, ok := allowed[checkpointID]; ok {
			total++
		}
	}
	for _, path := range sessionMetadataPaths {
		checkpointID, _, ok := sessionMetadataPathParts(path)
		if !ok {
			continue
		}
		if _, ok := allowed[checkpointID]; ok {
			total++
		}
	}
	return total
}

func readRootMetadataForSnapshot(ctx context.Context, reader *checkpointBlobReader, rootMetadataPaths []string, allowed map[string]struct{}, progress func()) (map[string][]byte, map[string]string, map[string]string, map[string]struct{}, []string) {
	rootMetadataByCheckpoint := make(map[string][]byte)
	transcriptPathsByMetadata := make(map[string]string)
	branchByCheckpoint := make(map[string]string)
	unsafeCheckpointIDs := make(map[string]struct{})
	metadataOwners := make(map[string]string)
	transcriptOwners := make(map[string]string)
	var warnings []string

	for _, path := range rootMetadataPaths {
		checkpointID, ok := checkpointIDFromRootMetadataPath(path)
		if !ok {
			continue
		}
		if _, ok := allowed[checkpointID]; !ok {
			continue
		}
		if progress != nil {
			progress()
		}
		data, err := reader.read(ctx, path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped checkpoint %s root metadata: read metadata: %v", checkpointID, err))
			continue
		}
		rootMetadataByCheckpoint[checkpointID] = data

		var summary checkpointSummaryPaths
		if err := json.Unmarshal(data, &summary); err != nil {
			continue
		}
		if summary.Branch != "" {
			branchByCheckpoint[checkpointID] = summary.Branch
		}
		for sessionIndex, session := range summary.Sessions {
			metadataPath, metadataOK := reader.virtualizePointer(checkpointID, session.Metadata)
			if !metadataOK {
				warnings = append(warnings, fmt.Sprintf("skipped unsafe checkpoint %s session file pointer", checkpointID))
				continue
			}
			transcriptPath := ""
			if strings.TrimSpace(session.Transcript) != "" {
				var transcriptOK bool
				transcriptPath, transcriptOK = reader.virtualizePointer(checkpointID, session.Transcript)
				if !transcriptOK {
					warnings = append(warnings, fmt.Sprintf("skipped unsafe checkpoint %s session file pointer", checkpointID))
					continue
				}
			}
			owner := fmt.Sprintf("%s/%d", checkpointID, sessionIndex)
			if metadataPath != "" {
				if previous, duplicate := metadataOwners[metadataPath]; duplicate && previous != owner {
					unsafeCheckpointIDs[checkpointID] = struct{}{}
				}
				metadataOwners[metadataPath] = owner
			}
			if transcriptPath != "" {
				if previous, duplicate := transcriptOwners[transcriptPath]; duplicate && previous != owner {
					unsafeCheckpointIDs[checkpointID] = struct{}{}
				}
				transcriptOwners[transcriptPath] = owner
			}
			if metadataPath != "" && transcriptPath != "" {
				transcriptPathsByMetadata[metadataPath] = transcriptPath
			}
		}
		if _, unsafe := unsafeCheckpointIDs[checkpointID]; unsafe {
			warnings = append(warnings, fmt.Sprintf("%s: skipped checkpoint %s because its root summary assigns a retained path to multiple sessions", checkpointScopeIncompleteCode, checkpointID))
		}
	}

	return rootMetadataByCheckpoint, transcriptPathsByMetadata, branchByCheckpoint, unsafeCheckpointIDs, warnings
}

func (r *checkpointBlobReader) virtualizePointer(checkpointID, storedPath string) (string, bool) {
	storedPath = strings.TrimSpace(filepath.ToSlash(storedPath))
	storedPath = strings.TrimPrefix(storedPath, "/")
	if storedPath == "" {
		return "", false
	}
	clean := path.Clean(storedPath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != storedPath {
		return "", false
	}
	checkpointRoot := checkpointPath(checkpointID)
	if r.virtualRoot != "" {
		root := strings.Trim(r.virtualRoot, "/")
		if clean == root || strings.HasPrefix(clean, root+"/") {
			return clean, true
		}
		parts := strings.Split(clean, "/")
		if len(parts) >= 2 && isCheckpointRefID(parts[0]+parts[1]) {
			// A per-checkpoint ref stores paths relative to its own root. An
			// aggregate-shaped pointer naming another checkpoint is not relative;
			// reject it instead of accidentally rebasing B beneath A.
			return "", false
		}
		return path.Join(root, clean), true
	}
	if clean == checkpointRoot || strings.HasPrefix(clean, checkpointRoot+"/") {
		return clean, true
	}
	return "", false
}

func addSnapshotSession(selected map[string]selectedSession, checkpointID string, sessionIndex int, transcriptPath string, meta checkpointExportSession, checkpointBranch string, branchDestinations checkpointBranchDestinations) {
	if meta.SessionID == "" {
		return
	}
	if meta.CheckpointID != "" && meta.CheckpointID != checkpointID {
		return
	}

	var createdAt time.Time
	if meta.CreatedAt != nil {
		createdAt = *meta.CreatedAt
	}
	sourceBranch := sessionBranch(meta.Branch, checkpointBranch)
	candidate := selectedSession{
		CheckpointID:         checkpointID,
		SessionIndex:         sessionIndex,
		SessionID:            meta.SessionID,
		Branch:               sourceBranch,
		SourceBranch:         sourceBranch,
		Agent:                meta.Agent,
		Model:                meta.Model,
		Kind:                 meta.Kind,
		ReviewSkills:         meta.ReviewSkills,
		CreatedAt:            createdAt,
		TurnID:               meta.TurnID,
		IsTask:               meta.IsTask,
		ToolUseID:            meta.ToolUseID,
		FilesTouched:         meta.FilesTouched,
		TokenUsage:           meta.TokenUsage,
		Summary:              meta.Summary,
		CheckpointsCount:     meta.CheckpointsCount,
		SourceTranscriptPath: transcriptPath,
	}

	addSelectedSession(selected, candidate, branchDestinations)
}

func addSelectedSession(selected map[string]selectedSession, candidate selectedSession, branchDestinations checkpointBranchDestinations) {
	sourceBranch := candidate.Branch
	candidate.SourceBranch = sourceBranch
	for _, branch := range branchDestinations.BranchesFor(candidate.CheckpointID, sourceBranch) {
		scoped := candidate
		scoped.Branch = branch
		scoped.SourceBranch = sourceBranch

		key := selectedSessionKey(scoped.Branch, scoped.SessionID)
		current, ok := selected[key]
		if !ok || shouldReplaceSession(current, scoped) {
			selected[key] = scoped
		}
	}
}

func (d checkpointBranchDestinations) BranchesFor(checkpointID, metadataBranch string) []string {
	if d.DefaultBranch != "" && len(d.DefaultCheckpointIDs) > 0 {
		if _, ok := d.DefaultCheckpointIDs[checkpointID]; ok {
			return []string{d.DefaultBranch}
		}
	}
	branch := strings.TrimSpace(metadataBranch)
	if branch == "" {
		return []string{""}
	}
	return []string{branch}
}

func limitedCheckpointSet(ids map[string]struct{}, limit int) map[string]struct{} {
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	if limit > 0 && len(sorted) > limit {
		sorted = sorted[:limit]
	}
	allowed := make(map[string]struct{}, len(sorted))
	for _, id := range sorted {
		allowed[id] = struct{}{}
	}
	return allowed
}

func checkpointIDFromRootMetadataPath(path string) (string, bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 3 || parts[2] != "metadata.json" {
		return "", false
	}
	checkpointID := parts[0] + parts[1]
	return checkpointID, isCheckpointRefID(checkpointID)
}

func sessionMetadataPathParts(path string) (string, int, bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 4 || parts[3] != "metadata.json" {
		return "", 0, false
	}
	checkpointID := parts[0] + parts[1]
	if !isCheckpointRefID(checkpointID) {
		return "", 0, false
	}
	sessionIndex, err := strconv.Atoi(parts[2])
	if err != nil || sessionIndex < 0 {
		return "", 0, false
	}
	return checkpointID, sessionIndex, true
}

func checkpointIDsFromTreeListing(data []byte) []string {
	var ids []string
	for _, line := range strings.Split(string(data), "\n") {
		path := strings.TrimSpace(line)
		parts := strings.Split(path, "/")
		if len(parts) != 3 || parts[2] != "metadata.json" {
			continue
		}
		id := parts[0] + parts[1]
		if isCheckpointRefID(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func treePathSet(data []byte) map[string]struct{} {
	paths := make(map[string]struct{})
	for _, line := range strings.Split(string(data), "\n") {
		path := strings.TrimSpace(filepath.ToSlash(line))
		if path != "" {
			paths[path] = struct{}{}
		}
	}
	return paths
}

// treePathOIDs parses `git ls-tree -r <ref>` output (mode, type, object-id,
// then a tab and the path) into a path -> object-id map. Lines that do not
// match the expected shape are skipped.
func treePathOIDs(data []byte) map[string]string {
	oids := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			continue
		}
		fields := strings.Fields(line[:tab])
		if len(fields) < 3 {
			continue
		}
		path := strings.TrimSpace(filepath.ToSlash(line[tab+1:]))
		if path != "" {
			oids[path] = fields[2]
		}
	}
	return oids
}

const checkpointMetadataCacheVersion = 1
const checkpointMetadataCachePath = "export/checkpoint-metadata-cache.json.gz"

// checkpointMetadataCache memoizes checkpoint metadata blobs by their git
// object id so a refresh does not re-run `git cat-file` for every checkpoint
// and session on every export. Checkpoint history is append-only and
// content-addressed, so an object id present from a previous run is guaranteed
// to carry identical bytes — making the cache safe by construction. prev holds
// entries loaded from disk; next accumulates the entries actually seen this
// run and is what gets persisted (so blobs for dropped checkpoints age out).
type checkpointMetadataCache struct {
	prev   map[string][]byte
	next   map[string][]byte
	active bool // true once a reader actually populated `next` this run
}

type checkpointMetadataCacheFile struct {
	Version int               `json:"version"`
	Blobs   map[string][]byte `json:"blobs"`
}

func newCheckpointMetadataCache(prev map[string][]byte) *checkpointMetadataCache {
	if prev == nil {
		prev = map[string][]byte{}
	}
	return &checkpointMetadataCache{prev: prev, next: make(map[string][]byte, len(prev))}
}

// The cache holds the raw bytes of every checkpoint metadata blob, which for a
// large history is hundreds of megabytes of JSON. It is gzip-compressed on disk
// (the content compresses heavily) to keep the footprint reasonable.
func loadCheckpointMetadataCache(path string) *checkpointMetadataCache {
	data, err := os.ReadFile(path)
	if err != nil {
		return newCheckpointMetadataCache(nil)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return newCheckpointMetadataCache(nil)
	}
	defer gz.Close()
	// Bound the decompressed stream: a gzip decompression bomb (a few KB on disk
	// expanding to terabytes) would otherwise drive the JSON decoder out of memory.
	// Exceeding the cap makes Decode fail, which falls through to rebuilding the
	// cache — the same graceful degradation as any other corrupt-cache case.
	limited := io.LimitReader(gz, semanticSnapshotMaxBytes())
	var file checkpointMetadataCacheFile
	if err := json.NewDecoder(limited).Decode(&file); err != nil || file.Version != checkpointMetadataCacheVersion || file.Blobs == nil {
		return newCheckpointMetadataCache(nil)
	}
	return newCheckpointMetadataCache(file.Blobs)
}

func saveCheckpointMetadataCache(path string, cache *checkpointMetadataCache) {
	if cache == nil {
		return
	}
	// If caching never actually ran this pass (e.g. object-id resolution failed),
	// do not clobber a good on-disk cache with an empty blob map — that would
	// silently defeat the cache on the next refresh. The `active` flag cleanly
	// distinguishes "caching disabled" from "legitimately zero blobs".
	if !cache.active {
		return
	}
	file := checkpointMetadataCacheFile{Version: checkpointMetadataCacheVersion, Blobs: cache.next}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if err := json.NewEncoder(gz).Encode(file); err != nil {
		return
	}
	if err := gz.Close(); err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = writeFileAtomic(path, buf.Bytes(), 0o600)
}

// checkpointBlobReader reads checkpoint metadata blobs, consulting an optional
// object-id-keyed cache. When cache is nil it is a thin wrapper over catFile
// and preserves the previous behavior exactly (no extra git calls, no caching).
type checkpointBlobReader struct {
	runner      CommandRunner
	gitDir      string
	ref         string
	virtualRoot string
	actualRoot  string
	localOnly   bool
	oids        map[string]string
	cache       *checkpointMetadataCache
}

func (r *checkpointBlobReader) read(ctx context.Context, virtualPath string) ([]byte, error) {
	actualPath, err := (checkpointSnapshotSource{VirtualRoot: r.virtualRoot, ActualRoot: r.actualRoot}).actualPath(virtualPath)
	if err != nil {
		return nil, err
	}
	if r.cache == nil {
		return catFileWithPolicy(ctx, r.runner, r.gitDir, r.ref, actualPath, r.localOnly)
	}
	oid := r.oids[virtualPath]
	if oid != "" {
		if data, ok := r.cache.prev[oid]; ok {
			r.cache.next[oid] = data
			return data, nil
		}
	}
	data, err := catFileWithPolicy(ctx, r.runner, r.gitDir, r.ref, actualPath, r.localOnly)
	if err != nil {
		return nil, err
	}
	if oid != "" {
		r.cache.next[oid] = data
	}
	return data, nil
}

func checkpointPath(checkpointID string) string {
	if len(checkpointID) < 3 {
		return checkpointID
	}
	return checkpointID[:2] + "/" + checkpointID[2:]
}

func snapshotTranscriptPath(checkpointID string, sessionIndex int, transcriptFileName string) string {
	return checkpointPath(checkpointID) + "/" + strconv.Itoa(sessionIndex) + "/" + transcriptFileName
}

func snapshotRootTranscriptPath(checkpointID string, transcriptFileName string) string {
	return checkpointPath(checkpointID) + "/" + transcriptFileName
}

func hasSnapshotTranscript(basePath string, treePaths map[string]struct{}) bool {
	if _, ok := treePaths[basePath]; ok {
		return true
	}
	return len(snapshotTranscriptChunks(basePath, treePaths)) > 0
}

func catFile(ctx context.Context, runner CommandRunner, gitDir, ref, path string) ([]byte, error) {
	return catFileWithPolicy(ctx, runner, gitDir, ref, path, false)
}

func catFileWithPolicy(ctx context.Context, runner CommandRunner, gitDir, ref, filePath string, localOnly bool) ([]byte, error) {
	var stdout []byte
	var err error
	if localOnly {
		stdout, _, err = runCheckpointGitLocal(ctx, runner, gitDir, "cat-file", "-p", ref+":"+filePath)
	} else {
		stdout, _, err = runner.Run(ctx, gitDir, "git", "cat-file", "-p", ref+":"+filePath)
	}
	if err != nil {
		return nil, err
	}
	return stdout, nil
}

func isCheckpointID(value string) bool {
	if len(value) != 12 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func checkpointDetail(ctx context.Context, runner CommandRunner, repoDir, entireBinary, checkpointID string) (checkpointExportEnvelope, error) {
	stdout, _, err := runner.Run(ctx, repoDir, entireBinary, "checkpoint", "explain", "--json", checkpointID)
	if err != nil {
		return checkpointExportEnvelope{}, fmt.Errorf("read checkpoint json: %w", err)
	}

	var detail checkpointExportEnvelope
	if err := json.Unmarshal(stdout, &detail); err != nil {
		return checkpointExportEnvelope{}, fmt.Errorf("parse checkpoint json: %w", err)
	}
	if detail.CheckpointID == "" {
		detail.CheckpointID = checkpointID
	}
	return detail, nil
}

func selectRoutedCheckpointSessions(ctx context.Context, runner CommandRunner, repoDir, entireBinary string, checkpoints []checkpointListEntry, branchDestinations checkpointBranchDestinations, progress func(exportProgress)) (map[string]selectedSession, []string, error) {
	selected := make(map[string]selectedSession)
	var warnings []string
	requestedCheckpointDetails := 0
	readableCheckpointDetails := 0
	for i, checkpoint := range checkpoints {
		reportExportProgress(progress, i+1, len(checkpoints), len(selected))
		if !checkpoint.IsLogsOnly {
			warnings = append(warnings, fmt.Sprintf("skipped non-committed checkpoint list entry %s", checkpoint.CheckpointID))
			continue
		}
		if checkpoint.CheckpointID == "" {
			warnings = append(warnings, "skipped checkpoint list entry with no checkpoint_id")
			continue
		}

		requestedCheckpointDetails++
		detail, detailErr := checkpointDetail(ctx, runner, repoDir, entireBinary, checkpoint.CheckpointID)
		if detailErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: skipped checkpoint %s because its routed detail metadata was unreadable", checkpointScopeIncompleteCode, checkpoint.CheckpointID))
			continue
		}
		readableCheckpointDetails++

		for _, session := range detail.Sessions {
			if session.Error != "" {
				warnings = append(warnings, fmt.Sprintf("skipped checkpoint %s session %d: %s", detail.CheckpointID, session.Index, session.Error))
				continue
			}
			if session.SessionID == "" {
				warnings = append(warnings, fmt.Sprintf("skipped checkpoint %s session %d: missing session_id", detail.CheckpointID, session.Index))
				continue
			}

			createdAt := checkpoint.Date
			if session.CreatedAt != nil && !session.CreatedAt.IsZero() {
				createdAt = *session.CreatedAt
			}
			candidate := selectedSession{
				CheckpointID:     detail.CheckpointID,
				SessionIndex:     session.Index,
				SessionID:        session.SessionID,
				Branch:           sessionBranch(session.Branch, detail.Branch),
				Agent:            session.Agent,
				Model:            session.Model,
				Kind:             session.Kind,
				ReviewSkills:     session.ReviewSkills,
				CreatedAt:        createdAt,
				TurnID:           session.TurnID,
				IsTask:           session.IsTask,
				ToolUseID:        session.ToolUseID,
				FilesTouched:     session.FilesTouched,
				TokenUsage:       session.TokenUsage,
				Summary:          session.Summary,
				CheckpointsCount: detail.CheckpointsCount,
			}
			addSelectedSession(selected, candidate, branchDestinations)
		}
		reportExportProgress(progress, i+1, len(checkpoints), len(selected))
	}
	if requestedCheckpointDetails > 0 && readableCheckpointDetails == 0 {
		return nil, warnings, fmt.Errorf("%s: none of %d listed checkpoints had readable detail metadata", checkpointScopeIncompleteCode, requestedCheckpointDetails)
	}
	return selected, warnings, nil
}

func shouldReplaceSession(current, candidate selectedSession) bool {
	if candidate.CreatedAt.After(current.CreatedAt) {
		return true
	}
	if current.CreatedAt.Equal(candidate.CreatedAt) {
		if candidate.CheckpointID > current.CheckpointID {
			return true
		}
		if candidate.CheckpointID == current.CheckpointID && candidate.SessionIndex > current.SessionIndex {
			return true
		}
	}
	return false
}

func sessionBranch(sessionBranch, checkpointBranch string) string {
	if strings.TrimSpace(sessionBranch) != "" {
		return strings.TrimSpace(sessionBranch)
	}
	return strings.TrimSpace(checkpointBranch)
}

func selectedSessionKey(branch, sessionID string) string {
	return branch + "\x00" + sessionID
}

func flattenSessions(selected map[string]selectedSession) []exportSession {
	sessions := make([]exportSession, 0, len(selected))
	for _, session := range selected {
		sessions = append(sessions, exportSession{
			SessionID:            session.SessionID,
			Branch:               session.Branch,
			SourceBranch:         session.SourceBranch,
			Agent:                session.Agent,
			Model:                session.Model,
			Kind:                 session.Kind,
			ReviewSkills:         session.ReviewSkills,
			LatestCheckpoint:     session.CheckpointID,
			SessionIndex:         session.SessionIndex,
			CreatedAt:            session.CreatedAt.UTC(),
			TurnID:               session.TurnID,
			IsTask:               session.IsTask,
			ToolUseID:            session.ToolUseID,
			FilesTouched:         session.FilesTouched,
			TokenUsage:           session.TokenUsage,
			Summary:              session.Summary,
			CheckpointsCount:     session.CheckpointsCount,
			SourceTranscriptPath: session.SourceTranscriptPath,
			SourceKey:            session.SourceKey,
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		if !sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) {
			return sessions[i].CreatedAt.Before(sessions[j].CreatedAt)
		}
		if sessions[i].Branch != sessions[j].Branch {
			return sessions[i].Branch < sessions[j].Branch
		}
		return sessions[i].SessionID < sessions[j].SessionID
	})
	return sessions
}

func applySessionAuthors(sessions []exportSession, authorIndex checkpointAuthorIndex) {
	for i := range sessions {
		sessions[i].Authors = authorIndex.AuthorsFor(sessions[i].LatestCheckpoint)
	}
}

func (i checkpointAuthorIndex) AuthorsFor(checkpointID string) []exportAuthor {
	if len(i.AuthorsByCheckpoint) == 0 {
		return nil
	}
	authors := i.AuthorsByCheckpoint[checkpointID]
	if len(authors) == 0 {
		return nil
	}
	return append([]exportAuthor(nil), authors...)
}

func writeSessionTranscripts(ctx context.Context, runner CommandRunner, repoDir, outputDir, entireBinary string, raw bool, sessions []exportSession, branchDirs map[string]string, cursor *exportCursor) ([]string, error) {
	transcriptFlag := "--transcript"
	extension := ".jsonl"
	if raw {
		transcriptFlag = "--raw-transcript"
	}

	var warnings []string
	for i := range sessions {
		session := &sessions[i]
		name := sessionFileName(session.CreatedAt, session.Agent, session.SessionID, session.LatestCheckpoint, extension)
		relPath := filepath.Join(branchDirs[session.Branch], name)
		if reuseTranscriptFromCursor(outputDir, cursor, *session, relPath, transcriptMode(raw)) {
			session.TranscriptPath = filepath.ToSlash(relPath)
			continue
		}

		stdout, stderr, err := runner.Run(ctx, repoDir, entireBinary, "checkpoint", "explain", transcriptFlag, "--session-index", strconv.Itoa(session.SessionIndex), session.LatestCheckpoint)
		if err != nil {
			return nil, fmt.Errorf("export transcript for session %s from checkpoint %s: %w", session.SessionID, session.LatestCheckpoint, err)
		}
		for _, warning := range warningLines("session "+session.SessionID, stderr) {
			warnings = append(warnings, warning)
		}

		if err := writeTranscriptFile(outputDir, relPath, stdout); err != nil {
			return nil, fmt.Errorf("write transcript for session %s: %w", session.SessionID, err)
		}
		session.TranscriptPath = filepath.ToSlash(relPath)
	}
	return warnings, nil
}

func writeSnapshotSessionTranscripts(ctx context.Context, runner CommandRunner, snapshot *checkpointSnapshot, outputDir string, sessions []exportSession, branchDirs map[string]string, cursor *exportCursor) ([]exportSession, []string, error) {
	var warnings []string
	written := make([]exportSession, 0, len(sessions))
	var firstSourceErr error
	for i := range sessions {
		session := &sessions[i]
		name := sessionFileName(session.CreatedAt, session.Agent, session.SessionID, session.LatestCheckpoint, ".jsonl")
		relPath := filepath.Join(branchDirs[session.Branch], name)
		if reuseTranscriptFromCursor(outputDir, cursor, *session, relPath, snapshot.TranscriptMode) {
			session.TranscriptPath = filepath.ToSlash(relPath)
			written = append(written, *session)
			continue
		}

		sourcePath := session.SourceTranscriptPath
		if sourcePath == "" {
			sourcePath = snapshotTranscriptPath(session.LatestCheckpoint, session.SessionIndex, snapshot.TranscriptFileName)
		}

		transcript, err := readSnapshotTranscriptFromSource(ctx, runner, snapshot, session.SourceKey, sourcePath)
		if err != nil {
			if firstSourceErr == nil {
				firstSourceErr = err
			}
			warnings = append(warnings, fmt.Sprintf("%s: skipped unreadable local checkpoint %s session %d transcript", checkpointScopeIncompleteCode, session.LatestCheckpoint, session.SessionIndex))
			continue
		}

		if err := writeTranscriptFile(outputDir, relPath, transcript); err != nil {
			return nil, warnings, fmt.Errorf("write transcript for session %s: %w", session.SessionID, err)
		}
		session.TranscriptPath = filepath.ToSlash(relPath)
		written = append(written, *session)
	}
	if len(sessions) > 0 && len(written) == 0 && firstSourceErr != nil {
		return nil, warnings, fmt.Errorf("%w: %s: none of the requested checkpoint transcripts were readable: %v", errCheckpointSnapshotUnavailable, checkpointScopeIncompleteCode, firstSourceErr)
	}
	return written, warnings, nil
}

func reuseTranscriptFromCursor(outputDir string, cursor *exportCursor, session exportSession, relPath, transcriptMode string) bool {
	if cursor == nil {
		return false
	}
	entry := cursor.Sessions[selectedSessionKey(session.Branch, session.SessionID)]
	if entry.LatestCheckpoint != session.LatestCheckpoint || entry.TranscriptPath != filepath.ToSlash(relPath) {
		return false
	}
	if entry.TranscriptMode != transcriptMode {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	if err := rejectSymlinkPathComponents(outputDir, clean); err != nil {
		return false
	}
	info, err := os.Lstat(filepath.Join(outputDir, clean))
	return err == nil && !info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func writeTranscriptFile(outputDir, relPath string, data []byte) error {
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe transcript path: %s", relPath)
	}
	dir := filepath.Dir(clean)
	if dir != "." {
		if err := rejectExistingSymlinkPathComponents(outputDir, dir); err != nil {
			return fmt.Errorf("validate transcript directory %s: %w", dir, err)
		}
	}
	abs := filepath.Join(outputDir, clean)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return fmt.Errorf("create transcript directory: %w", err)
	}
	if err := rejectExistingSymlinkPathComponents(outputDir, clean); err != nil {
		return fmt.Errorf("validate transcript path %s: %w", clean, err)
	}
	if err := writeFileAtomic(abs, data, 0o600); err != nil {
		return err
	}
	return nil
}

func ensureExportDirectories(outputDir string, branchDirs map[string]string) error {
	for _, relPath := range branchDirs {
		clean := filepath.Clean(filepath.FromSlash(relPath))
		if filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe branch transcript directory: %s", relPath)
		}
		if err := rejectExistingSymlinkPathComponents(outputDir, clean); err != nil {
			return fmt.Errorf("validate branch transcript directory %s: %w", relPath, err)
		}
		if err := os.MkdirAll(filepath.Join(outputDir, clean), 0o700); err != nil {
			return fmt.Errorf("create branch transcript directory %s: %w", relPath, err)
		}
	}
	return nil
}

func readSnapshotTranscript(ctx context.Context, runner CommandRunner, snapshot *checkpointSnapshot, sourcePath string) ([]byte, error) {
	return readSnapshotTranscriptFromSource(ctx, runner, snapshot, "", sourcePath)
}

func readSnapshotTranscriptFromSource(ctx context.Context, runner CommandRunner, snapshot *checkpointSnapshot, sourceKey, sourcePath string) ([]byte, error) {
	if sourceKey != "" {
		source, ok := snapshot.Sources[sourceKey]
		if !ok {
			return nil, fmt.Errorf("checkpoint snapshot source %q not found", sourceKey)
		}
		return readCheckpointSourceTranscript(ctx, runner, source, sourcePath)
	}
	if _, ok := snapshot.TreePaths[sourcePath]; ok {
		return catFileWithPolicy(ctx, runner, snapshot.GitDir, snapshot.Ref, sourcePath, brainNoEgressMode())
	}

	chunks := snapshotTranscriptChunks(sourcePath, snapshot.TreePaths)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("transcript path %s not found in %s", sourcePath, snapshot.Ref)
	}

	var b strings.Builder
	for i, chunk := range chunks {
		data, err := catFileWithPolicy(ctx, runner, snapshot.GitDir, snapshot.Ref, chunk, brainNoEgressMode())
		if err != nil {
			return nil, fmt.Errorf("read transcript chunk %s: %w", chunk, err)
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.Write(data)
	}
	return []byte(b.String()), nil
}

func readCheckpointSourceTranscript(ctx context.Context, runner CommandRunner, source checkpointSnapshotSource, virtualPath string) ([]byte, error) {
	readOne := func(candidate string) ([]byte, error) {
		actualPath, err := source.actualPath(candidate)
		if err != nil {
			return nil, err
		}
		return catFileWithPolicy(ctx, runner, source.GitDir, source.Ref, actualPath, source.LocalOnly)
	}
	if _, ok := source.TreePaths[virtualPath]; ok {
		return readOne(virtualPath)
	}
	chunks := snapshotTranscriptChunks(virtualPath, source.TreePaths)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("transcript path %s not found in %s", virtualPath, source.Ref)
	}
	var b strings.Builder
	for i, chunk := range chunks {
		data, err := readOne(chunk)
		if err != nil {
			return nil, fmt.Errorf("read transcript chunk %s: %w", chunk, err)
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.Write(data)
	}
	return []byte(b.String()), nil
}

func (s checkpointSnapshotSource) actualPath(virtualPath string) (string, error) {
	virtualPath = strings.TrimSpace(filepath.ToSlash(virtualPath))
	if virtualPath == "" || strings.HasPrefix(virtualPath, "/") {
		return "", fmt.Errorf("unsafe checkpoint snapshot path %q", virtualPath)
	}
	clean := path.Clean(virtualPath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != virtualPath {
		return "", fmt.Errorf("unsafe checkpoint snapshot path %q", virtualPath)
	}
	root := strings.Trim(strings.TrimSpace(filepath.ToSlash(s.VirtualRoot)), "/")
	relative := clean
	if root != "" {
		prefix := root + "/"
		if !strings.HasPrefix(clean, prefix) {
			return "", fmt.Errorf("checkpoint snapshot path %q escapes source root %q", virtualPath, root)
		}
		relative = strings.TrimPrefix(clean, prefix)
	}
	actualRoot := strings.Trim(strings.TrimSpace(filepath.ToSlash(s.ActualRoot)), "/")
	actual := relative
	if actualRoot != "" {
		actual = path.Join(actualRoot, relative)
	}
	if actual == "." || actual == ".." || strings.HasPrefix(actual, "../") {
		return "", fmt.Errorf("unsafe checkpoint snapshot source path %q", actual)
	}
	return actual, nil
}

func snapshotTranscriptChunks(basePath string, treePaths map[string]struct{}) []string {
	var chunks []string
	if _, ok := treePaths[basePath]; ok {
		chunks = append(chunks, basePath)
	}
	for path := range treePaths {
		if path == basePath {
			continue
		}
		if parseChunkIndex(path, basePath) > 0 {
			chunks = append(chunks, path)
		}
	}
	sort.Slice(chunks, func(i, j int) bool {
		return parseChunkIndex(chunks[i], basePath) < parseChunkIndex(chunks[j], basePath)
	})
	return chunks
}

func parseChunkIndex(path, basePath string) int {
	if path == basePath {
		return 0
	}
	prefix := basePath + "."
	if !strings.HasPrefix(path, prefix) {
		return -1
	}
	suffix := strings.TrimPrefix(path, prefix)
	index, err := strconv.Atoi(suffix)
	if err != nil {
		return -1
	}
	if fmt.Sprintf("%03d", index) != suffix {
		return -1
	}
	return index
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func loadExportCursor(path string) *exportCursor {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cursor exportCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return nil
	}
	if cursor.Sessions == nil {
		cursor.Sessions = make(map[string]exportCursorSession)
	}
	return &cursor
}

func writeExportCursor(path string, manifest exportManifest) error {
	cursor := exportCursor{
		SchemaVersion: 1,
		UpdatedAt:     manifest.GeneratedAt,
		RepoRoot:      manifest.RepoRoot,
		Sessions:      make(map[string]exportCursorSession, len(manifest.Sessions)),
	}
	for _, session := range manifest.Sessions {
		cursor.Sessions[selectedSessionKey(session.Branch, session.SessionID)] = exportCursorSession{
			Branch:           session.Branch,
			SessionID:        session.SessionID,
			LatestCheckpoint: session.LatestCheckpoint,
			TranscriptMode:   manifest.TranscriptMode,
			TranscriptPath:   session.TranscriptPath,
			CreatedAt:        session.CreatedAt,
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create cursor directory: %w", err)
	}
	if err := writeJSONFile(path, cursor); err != nil {
		return fmt.Errorf("write export cursor: %w", err)
	}
	return nil
}

func cleanupStaleSessionFiles(outputDir string, sessions []exportSession) error {
	active := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		if session.TranscriptPath != "" {
			active[filepath.FromSlash(session.TranscriptPath)] = struct{}{}
		}
	}
	sessionsRoot := filepath.Join(outputDir, exportSessionsDirectory)
	if _, err := os.Stat(sessionsRoot); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(sessionsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !isTranscriptFile(path) {
			return nil
		}
		rel, err := filepath.Rel(outputDir, path)
		if err != nil {
			return err
		}
		if _, ok := active[rel]; ok {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale transcript %s: %w", rel, err)
		}
		return nil
	})
}

func isTranscriptFile(path string) bool {
	switch filepath.Ext(path) {
	case ".json", ".jsonl":
		return true
	default:
		return false
	}
}

func sessionFileName(createdAt time.Time, agent, sessionID, checkpointID, extension string) string {
	timestamp := createdAt.UTC().Format("20060102T150405Z")
	agentPart := safePathComponent(agent, "agent", 32)
	sessionPart := safePathComponent(sessionID, "session", 80)
	checkpointPart := safePathComponent(checkpointID, "checkpoint", 32)
	return strings.Join([]string{timestamp, agentPart, sessionPart, checkpointPart}, "_") + extension
}

func buildBranchDirectories(sessions []exportSession, defaultBranch string) map[string]string {
	branches := uniqueSessionBranches(sessions)
	if defaultBranch != "" {
		branches = append(branches, defaultBranch)
		sort.Strings(branches)
		branches = compactSortedStrings(branches)
	}
	defaultDir := filepath.Join(exportSessionsDirectory, safePathComponent(defaultBranch, "main", 80))

	dirs := make(map[string]string, len(branches))
	for _, branch := range branches {
		switch {
		case branch == defaultBranch:
			dirs[branch] = defaultDir
		case branch == "":
			dirs[branch] = filepath.Join(exportSessionsDirectory, exportUnknownDirectory)
		default:
			dirs[branch] = filepath.Join(exportSessionsDirectory, exportBranchesDirectory, stableBranchDirComponent(branch))
		}
	}
	return dirs
}

func stableBranchDirComponent(branch string) string {
	slug := safePathComponent(branch, "branch", 80)
	sum := sha256.Sum256([]byte(branch))
	return slug + "-" + hex.EncodeToString(sum[:])[:8]
}

func summarizeBranchExports(sessions []exportSession, branchDirs map[string]string, defaultBranch string) []exportBranch {
	counts := make(map[string]int)
	if defaultBranch != "" {
		counts[defaultBranch] = 0
	}
	for _, session := range sessions {
		counts[session.Branch]++
	}

	branches := make([]exportBranch, 0, len(counts))
	for _, branch := range sortedBranches(counts) {
		branches = append(branches, exportBranch{
			Branch:       branch,
			Directory:    filepath.ToSlash(branchDirs[branch]),
			SessionCount: counts[branch],
			Default:      branch != "" && branch == defaultBranch,
		})
	}
	return branches
}

func compactSortedStrings(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func uniqueSessionBranches(sessions []exportSession) []string {
	seen := make(map[string]struct{})
	for _, session := range sessions {
		seen[session.Branch] = struct{}{}
	}
	return sortedBranches(seen)
}

func sortedBranches[T any](values map[string]T) []string {
	branches := make([]string, 0, len(values))
	for branch := range values {
		branches = append(branches, branch)
	}
	sort.Slice(branches, func(i, j int) bool {
		if branches[i] == branches[j] {
			return false
		}
		if branches[i] == "" {
			return false
		}
		if branches[j] == "" {
			return true
		}
		return branches[i] < branches[j]
	})
	return branches
}

func safePathComponent(value, fallback string, max int) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-._")
	if out == "" {
		out = fallback
	}
	if max > 0 && len(out) > max {
		out = strings.Trim(out[:max], "-._")
		if out == "" {
			out = fallback
		}
	}
	return out
}

func transcriptMode(raw bool) string {
	if raw {
		return "raw"
	}
	return "compact"
}

func actualTranscriptMode(raw bool, snapshot *checkpointSnapshot) string {
	if snapshot != nil && snapshot.TranscriptMode != "" {
		return snapshot.TranscriptMode
	}
	return transcriptMode(raw)
}

func warningLines(prefix string, data []byte) []string {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	warnings := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			warnings = append(warnings, prefix+": "+line)
		}
	}
	return warnings
}

func usefulExportWarnings(warnings []string, debug bool) []string {
	if len(warnings) == 0 {
		return nil
	}
	useful := make([]string, 0, len(warnings))
	seen := make(map[string]struct{}, len(warnings))
	for _, warning := range warnings {
		warning = strings.TrimSpace(warning)
		if warning == "" {
			continue
		}
		if !debug && exportWarningIsDebugOnly(warning) {
			continue
		}
		warning = usefulExportWarningText(warning)
		if warning == "" {
			continue
		}
		if _, ok := seen[warning]; ok {
			continue
		}
		seen[warning] = struct{}{}
		useful = append(useful, warning)
	}
	return useful
}

func exportWarningIsDebugOnly(warning string) bool {
	switch {
	case strings.HasPrefix(warning, "checkpoint ref "):
		return true
	case strings.HasPrefix(warning, "checkpoint snapshot unavailable:"):
		return true
	case strings.HasPrefix(warning, "exporting raw transcripts directly from "):
		return true
	case strings.HasPrefix(warning, "exporting compact transcripts directly from "):
		return true
	case strings.HasPrefix(warning, "compact transcript unavailable for v1 checkpoints;"):
		return true
	case warning == "discovered checkpoint refs from configured checkpoint remote":
		return true
	case warning == "used Entire's complete routed checkpoint list because no local metadata refs were enumerable":
		return true
	case warning == "combined locally enumerated checkpoint refs with Entire's complete routed checkpoint list":
		return true
	default:
		return false
	}
}

func usefulExportWarningText(warning string) string {
	switch {
	case strings.HasPrefix(warning, "default branch checkpoint reachability unavailable:"):
		return "Could not determine which checkpoints are reachable from the default branch; branch folders may be less precise: " + strings.TrimSpace(strings.TrimPrefix(warning, "default branch checkpoint reachability unavailable:"))
	case strings.HasPrefix(warning, "checkpoint author index unavailable:"):
		return "Could not build the checkpoint author index; exported sessions may omit author metadata: " + strings.TrimSpace(strings.TrimPrefix(warning, "checkpoint author index unavailable:"))
	case strings.HasPrefix(warning, "all checkpoint discovery found no metadata refs; used branch-visible checkpoint list fallback"):
		return "Could not find all checkpoint metadata refs, so export used the current branch checkpoint list; sessions from other branches may be missing."
	case warning == "discovered checkpoint refs from configured checkpoint remote":
		return "Local checkpoint refs were unavailable, so export read checkpoints from the configured checkpoint remote."
	case strings.HasPrefix(warning, "checkpoint remote unavailable:"):
		return "Could not read the configured checkpoint remote; export used local checkpoint data only: " + strings.TrimSpace(strings.TrimPrefix(warning, "checkpoint remote unavailable:"))
	case strings.HasPrefix(warning, "direct checkpoint export unavailable:"):
		return "Could not export directly from checkpoint storage; export fell back to the Entire CLI checkpoint API: " + strings.TrimSpace(strings.TrimPrefix(warning, "direct checkpoint export unavailable:"))
	case strings.HasPrefix(warning, "compact transcript unavailable for v1 checkpoints;"):
		return "This repository uses v1 checkpoints, which only store raw full.jsonl transcripts; exported transcripts are raw."
	default:
		return warning
	}
}

func renderExportReadme(manifest exportManifest) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# Entire Brain Session Export")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Generated at: `%s`\n\n", manifest.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintln(&b, "This directory contains the newest known checkpoint version of each unique Entire session per branch.")
	fmt.Fprintln(&b, "Read `manifest.json` first, then inspect default-branch transcripts before branch-specific transcripts.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "## Summary")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- Sessions: %d\n", len(manifest.Sessions))
	fmt.Fprintf(&b, "- Checkpoints scanned: %d\n", manifest.CheckpointsScanned)
	fmt.Fprintf(&b, "- Transcript mode: %s\n", manifest.TranscriptMode)
	fmt.Fprintf(&b, "- Scope: %s\n", manifest.Scope)
	if manifest.DefaultBranch != "" {
		fmt.Fprintf(&b, "- Default branch: `%s`\n", manifest.DefaultBranch)
	}
	if manifest.RepoRoot != "" {
		fmt.Fprintf(&b, "- Repo root: `%s`\n", manifest.RepoRoot)
	}
	if manifest.EntireCLIVersion != "" {
		fmt.Fprintf(&b, "- Entire CLI version: `%s`\n", manifest.EntireCLIVersion)
	}
	fmt.Fprintln(&b)
	if len(manifest.Branches) > 0 {
		fmt.Fprintln(&b, "## Branch Folders")
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "| Branch | Sessions | Directory |")
		fmt.Fprintln(&b, "| --- | ---: | --- |")
		for _, branch := range manifest.Branches {
			label := branch.Branch
			if label == "" {
				label = "(unknown)"
			}
			if branch.Default {
				label += " (default)"
			}
			fmt.Fprintf(&b, "| %s | %d | `%s` |\n",
				escapeMarkdownTable(label),
				branch.SessionCount,
				branch.Directory,
			)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, "## Sessions")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Created | Branch | Authors | Agent | Model | Session | Checkpoint | Transcript |")
	fmt.Fprintln(&b, "| --- | --- | --- | --- | --- | --- | --- | --- |")
	for _, session := range manifest.Sessions {
		branch := session.Branch
		if branch == "" {
			branch = "(unknown)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | `%s` | `%s` | `%s` |\n",
			session.CreatedAt.Format(time.RFC3339),
			escapeMarkdownTable(branch),
			escapeMarkdownTable(formatAuthors(session.Authors)),
			escapeMarkdownTable(session.Agent),
			escapeMarkdownTable(session.Model),
			session.SessionID,
			session.LatestCheckpoint,
			session.TranscriptPath,
		)
	}
	if len(manifest.Warnings) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "## Warnings")
		fmt.Fprintln(&b)
		for _, warning := range manifest.Warnings {
			fmt.Fprintf(&b, "- %s\n", warning)
		}
	}
	return b.String()
}

func formatAuthors(authors []exportAuthor) string {
	if len(authors) == 0 {
		return ""
	}
	parts := make([]string, 0, len(authors))
	for _, author := range authors {
		name := strings.TrimSpace(author.Name)
		email := strings.TrimSpace(author.Email)
		switch {
		case name != "" && email != "":
			parts = append(parts, fmt.Sprintf("%s <%s>", name, email))
		case name != "":
			parts = append(parts, name)
		case email != "":
			parts = append(parts, email)
		}
	}
	return strings.Join(parts, ", ")
}

func escapeMarkdownTable(value string) string {
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "|", `\|`)
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

type checkpointListEntry struct {
	CheckpointID     string    `json:"checkpoint_id"`
	SessionID        string    `json:"session_id,omitempty"`
	Agent            string    `json:"agent,omitempty"`
	Date             time.Time `json:"date"`
	Message          string    `json:"message,omitempty"`
	IsTaskCheckpoint bool      `json:"is_task_checkpoint,omitempty"`
	IsLogsOnly       bool      `json:"is_logs_only,omitempty"`
	SessionCount     int       `json:"session_count,omitempty"`
	SessionIDs       []string  `json:"session_ids,omitempty"`
}

type checkpointExportEnvelope struct {
	CheckpointID     string                    `json:"checkpoint_id"`
	Strategy         string                    `json:"strategy,omitempty"`
	Branch           string                    `json:"branch,omitempty"`
	CheckpointsCount int                       `json:"checkpoints_count"`
	FilesTouched     []string                  `json:"files_touched,omitempty"`
	HasReview        bool                      `json:"has_review,omitempty"`
	SessionCount     int                       `json:"session_count"`
	Sessions         []checkpointExportSession `json:"sessions"`
	Partial          bool                      `json:"partial,omitempty"`
}

type checkpointExportSession struct {
	Index            int                   `json:"index"`
	CheckpointID     string                `json:"checkpoint_id,omitempty"`
	SessionID        string                `json:"session_id,omitempty"`
	Branch           string                `json:"branch,omitempty"`
	Agent            string                `json:"agent,omitempty"`
	Model            string                `json:"model,omitempty"`
	Kind             string                `json:"kind,omitempty"`
	ReviewSkills     []string              `json:"review_skills,omitempty"`
	CreatedAt        *time.Time            `json:"created_at,omitempty"`
	TurnID           string                `json:"turn_id,omitempty"`
	IsTask           bool                  `json:"is_task,omitempty"`
	ToolUseID        string                `json:"tool_use_id,omitempty"`
	FilesTouched     []string              `json:"files_touched,omitempty"`
	CheckpointsCount int                   `json:"checkpoints_count,omitempty"`
	TokenUsage       *checkpointTokenUsage `json:"token_usage,omitempty"`
	Summary          *checkpointSummary    `json:"summary,omitempty"`
	Error            string                `json:"error,omitempty"`
}

type checkpointTokenUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

type checkpointSummary struct {
	Intent  string `json:"intent,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

type checkpointSummaryPaths struct {
	Branch   string                   `json:"branch,omitempty"`
	Sessions []checkpointSessionPaths `json:"sessions,omitempty"`
}

type checkpointSessionPaths struct {
	Metadata   string `json:"metadata,omitempty"`
	Transcript string `json:"transcript,omitempty"`
}

type checkpointSnapshot struct {
	GitDir                string
	TempDir               string
	Ref                   string
	Version               int
	TranscriptMode        string
	TranscriptFileName    string
	Selected              map[string]selectedSession
	CheckpointCount       int
	TreePaths             map[string]struct{}
	Sources               map[string]checkpointSnapshotSource
	UnreadableCheckpoints map[string]struct{}
}

type checkpointSnapshotSource struct {
	GitDir      string
	Ref         string
	VirtualRoot string
	ActualRoot  string
	TreePaths   map[string]struct{}
	OIDs        map[string]string
	LocalOnly   bool
}

type checkpointBranchDestinations struct {
	DefaultBranch        string
	DefaultCheckpointIDs map[string]struct{}
}

type checkpointAuthorIndex struct {
	AuthorsByCheckpoint map[string][]exportAuthor
}

type checkpointAuthorAccumulator struct {
	Author       exportAuthor
	Commits      map[string]struct{}
	LastCommitAt time.Time
}

func (s *checkpointSnapshot) Cleanup() {
	if s != nil && s.TempDir != "" {
		_ = os.RemoveAll(s.TempDir)
	}
}

type entireSettingsFile struct {
	StrategyOptions entireStrategyOptions      `json:"strategy_options,omitempty"`
	Checkpoints     *entireCheckpointsSettings `json:"checkpoints,omitempty"`
}

type entireCheckpointsSettings struct {
	Primary entireCheckpointBackendSettings   `json:"primary"`
	Mirrors []entireCheckpointBackendSettings `json:"mirrors,omitempty"`
}

type entireCheckpointBackendSettings struct {
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}

type entireStrategyOptions struct {
	CheckpointRemote   *checkpointRemoteSettings `json:"checkpoint_remote,omitempty"`
	CheckpointsV2      *bool                     `json:"checkpoints_v2,omitempty"`
	CheckpointsVersion any                       `json:"checkpoints_version,omitempty"`
}

type checkpointRemoteSettings struct {
	Provider string `json:"provider"`
	Repo     string `json:"repo"`
}

type selectedSession struct {
	CheckpointID         string
	SessionIndex         int
	SessionID            string
	Branch               string
	SourceBranch         string
	Agent                string
	Model                string
	Kind                 string
	ReviewSkills         []string
	CreatedAt            time.Time
	TurnID               string
	IsTask               bool
	ToolUseID            string
	FilesTouched         []string
	TokenUsage           *checkpointTokenUsage
	Summary              *checkpointSummary
	CheckpointsCount     int
	SourceTranscriptPath string
	SourceKey            string
}

type exportManifest struct {
	SchemaVersion      int             `json:"schema_version"`
	GeneratedAt        time.Time       `json:"generated_at"`
	RepoRoot           string          `json:"repo_root,omitempty"`
	RepoKey            string          `json:"repo_key,omitempty"`
	EntireCLIVersion   string          `json:"entire_cli_version,omitempty"`
	DefaultBranch      string          `json:"default_branch,omitempty"`
	TranscriptMode     string          `json:"transcript_mode"`
	Scope              string          `json:"scope"`
	CheckpointLimit    int             `json:"checkpoint_limit"`
	CheckpointsScanned int             `json:"checkpoints_scanned"`
	Branches           []exportBranch  `json:"branches,omitempty"`
	Sessions           []exportSession `json:"sessions"`
	Warnings           []string        `json:"warnings,omitempty"`
	Sources            *brainSources   `json:"sources,omitempty"`
}

type exportBranch struct {
	Branch       string `json:"branch"`
	Directory    string `json:"directory"`
	SessionCount int    `json:"session_count"`
	Default      bool   `json:"default,omitempty"`
}

type exportSession struct {
	SessionID            string                `json:"session_id"`
	Branch               string                `json:"branch,omitempty"`
	SourceBranch         string                `json:"source_branch,omitempty"`
	Authors              []exportAuthor        `json:"authors,omitempty"`
	Agent                string                `json:"agent,omitempty"`
	Model                string                `json:"model,omitempty"`
	Kind                 string                `json:"kind,omitempty"`
	ReviewSkills         []string              `json:"review_skills,omitempty"`
	LatestCheckpoint     string                `json:"latest_checkpoint_id"`
	SessionIndex         int                   `json:"session_index"`
	CreatedAt            time.Time             `json:"created_at"`
	TurnID               string                `json:"turn_id,omitempty"`
	IsTask               bool                  `json:"is_task,omitempty"`
	ToolUseID            string                `json:"tool_use_id,omitempty"`
	FilesTouched         []string              `json:"files_touched,omitempty"`
	TokenUsage           *checkpointTokenUsage `json:"token_usage,omitempty"`
	Summary              *checkpointSummary    `json:"summary,omitempty"`
	CheckpointsCount     int                   `json:"checkpoints_count,omitempty"`
	TranscriptPath       string                `json:"transcript_path"`
	SourceTranscriptPath string                `json:"-"`
	SourceKey            string                `json:"-"`
}

type exportAuthor struct {
	Name         string     `json:"name,omitempty"`
	Email        string     `json:"email,omitempty"`
	Commits      int        `json:"commits,omitempty"`
	LastCommitAt *time.Time `json:"last_commit_at,omitempty"`
}

type exportCursor struct {
	SchemaVersion int                            `json:"schema_version"`
	UpdatedAt     time.Time                      `json:"updated_at"`
	RepoRoot      string                         `json:"repo_root,omitempty"`
	Sessions      map[string]exportCursorSession `json:"sessions"`
}

type exportCursorSession struct {
	Branch           string    `json:"branch,omitempty"`
	SessionID        string    `json:"session_id"`
	LatestCheckpoint string    `json:"latest_checkpoint_id"`
	TranscriptMode   string    `json:"transcript_mode"`
	TranscriptPath   string    `json:"transcript_path"`
	CreatedAt        time.Time `json:"created_at"`
}
