package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

	v2MainRef        = "refs/entire/checkpoints/v2/main"
	v1MainRef        = "refs/heads/entire/checkpoints/v1"
	v1OriginRef      = "refs/remotes/origin/entire/checkpoints/v1"
	v1RemoteFetchRef = "refs/heads/entire/checkpoints/v1"
	v1RemoteRef      = v1RemoteFetchRef

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

var checkpointTrailerRegex = regexp.MustCompile(checkpointTrailerKey + `:\s*([a-f0-9]{12})(?:\s|$)`)

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
		Use:   "export",
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
	if !outputExplicit {
		if storage, storageErr := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir); storageErr == nil {
			metadataCachePath = filepath.Join(storage.BrainDir, filepath.FromSlash(checkpointMetadataCachePath))
			metadataCache = loadCheckpointMetadataCache(metadataCachePath)
		}
	}

	if exportOpts.scope == exportScopeAll {
		loadedSnapshot, snapshotWarnings, snapshotErr := loadConfiguredCheckpointSnapshot(ctx, opts.Runner, repoDir, exportOpts.rawTranscript, exportOpts.checkpointLimit, branchDestinations, exportOpts.progress, metadataCache)
		if snapshotErr == nil {
			snapshot = loadedSnapshot
			defer snapshot.Cleanup()
			selected = loadedSnapshot.Selected
			checkpointsScanned = loadedSnapshot.CheckpointCount
			warnings = append(warnings, snapshotWarnings...)
			reportExportProgress(exportOpts.progress, checkpointsScanned, checkpointsScanned, len(selected))
			if metadataCachePath != "" {
				saveCheckpointMetadataCache(metadataCachePath, metadataCache)
			}
		} else if !errors.Is(snapshotErr, errCheckpointSnapshotUnavailable) {
			warnings = append(warnings, "direct checkpoint export unavailable: "+snapshotErr.Error())
		}
	}

	if selected == nil {
		checkpoints, discoverWarnings, err := discoverCheckpoints(ctx, opts.Runner, repoDir, exportOpts.entireBinary, exportOpts.scope, exportOpts.checkpointLimit)
		if err != nil {
			return err
		}
		warnings = append(warnings, discoverWarnings...)
		checkpointsScanned = len(checkpoints)

		selected = make(map[string]selectedSession)
		for i, checkpoint := range checkpoints {
			reportExportProgress(exportOpts.progress, i+1, len(checkpoints), len(selected))
			if !checkpoint.IsLogsOnly {
				warnings = append(warnings, fmt.Sprintf("skipped non-committed checkpoint list entry %s", checkpoint.CheckpointID))
				continue
			}
			if checkpoint.CheckpointID == "" {
				warnings = append(warnings, "skipped checkpoint list entry with no checkpoint_id")
				continue
			}

			detail, detailErr := checkpointDetail(ctx, opts.Runner, repoDir, exportOpts.entireBinary, checkpoint.CheckpointID)
			if detailErr != nil {
				return detailErr
			}

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
			reportExportProgress(exportOpts.progress, i+1, len(checkpoints), len(selected))
		}
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

	outputDir, err = prepareExportDir(outputDir, persistentBrain)
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
	if persistentBrain {
		if err := cleanupStaleSessionFiles(outputDir, sessions); err != nil {
			return err
		}
	}

	if persistentBrain {
		if err := writeBrainSessionSource(outputDir, repoKey, manifest); err != nil {
			return err
		}
	} else {
		if err := writeBrainManifestAndReadme(outputDir, manifest); err != nil {
			return err
		}
	}
	if exportOpts.historyIndex {
		if _, err := writeBrainHistoryIndexAndSource(outputDir, opts.Now().UTC(), nil); err != nil {
			return err
		}
	}
	if persistentBrain {
		if err := writeExportCursor(cursorFile, manifest); err != nil {
			return err
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
		info, statErr := os.Stat(abs)
		if statErr == nil && !info.IsDir() {
			return "", fmt.Errorf("output path exists and is not a directory: %s", abs)
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

	info, err := os.Stat(abs)
	switch {
	case err == nil:
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

	stdout, stderr, err := runner.Run(ctx, repoDir, "git", "log", "--format=%B", "--grep", checkpointTrailerKey+":", defaultRef)
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
	stdout, stderr, err := runner.Run(ctx, repoDir, "git", "log", "--all", "--format=%H%x00%an%x00%ae%x00%aI%x00%B%x1e", "--grep", checkpointTrailerKey+":")
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
		return listBranchCheckpoints(ctx, runner, repoDir, entireBinary, limit)
	}

	checkpoints, warnings, err := listAllCheckpointRefs(ctx, runner, repoDir, limit)
	if err != nil {
		return nil, nil, err
	}
	if len(checkpoints) > 0 {
		return checkpoints, warnings, nil
	}

	fallback, fallbackWarnings, fallbackErr := listBranchCheckpoints(ctx, runner, repoDir, entireBinary, limit)
	if fallbackErr != nil {
		return nil, warnings, fmt.Errorf("all checkpoint discovery found no metadata refs and branch fallback failed: %w", fallbackErr)
	}
	warnings = append(warnings, "all checkpoint discovery found no metadata refs; used branch-visible checkpoint list fallback")
	warnings = append(warnings, fallbackWarnings...)
	return fallback, warnings, nil
}

func listBranchCheckpoints(ctx context.Context, runner CommandRunner, repoDir, entireBinary string, limit int) ([]checkpointListEntry, []string, error) {
	args := []string{"checkpoint", "explain", "--json"}
	if limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	}
	stdout, stderr, err := runner.Run(ctx, repoDir, entireBinary, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list checkpoints: %w", err)
	}

	var checkpoints []checkpointListEntry
	if err := json.Unmarshal(stdout, &checkpoints); err != nil {
		return nil, nil, fmt.Errorf("parse checkpoint list json: %w", err)
	}
	return checkpoints, warningLines("checkpoint list", stderr), nil
}

func listAllCheckpointRefs(ctx context.Context, runner CommandRunner, repoDir string, limit int) ([]checkpointListEntry, []string, error) {
	settings, settingsErr := readEntireSettings(repoDir)
	includeV2 := settingsErr == nil && settings.CheckpointsV2Enabled()
	ids, warnings := listLocalCheckpointRefIDs(ctx, runner, repoDir, localCheckpointRefs(includeV2))
	if len(ids) == 0 {
		localWarnings := warnings
		remoteURL, remoteErr := settings.CheckpointRemoteFetchURL()
		if settingsErr != nil {
			remoteErr = settingsErr
		}
		if remoteErr == nil {
			var remoteWarnings []string
			ids, remoteWarnings = listRemoteCheckpointRefIDs(ctx, runner, remoteURL, remoteCheckpointRefs(includeV2))
			if len(ids) > 0 {
				warnings = remoteWarnings
				warnings = append(warnings, "discovered checkpoint refs from configured checkpoint remote")
			} else {
				warnings = append(localWarnings, remoteWarnings...)
			}
		} else {
			warnings = append(warnings, "checkpoint remote unavailable: "+remoteErr.Error())
		}
	}

	return checkpointEntriesFromIDs(ids, limit), warningsFromLimit(ids, limit, warnings), nil
}

func localCheckpointRefs(includeV2 bool) []string {
	if includeV2 {
		return []string{v2MainRef, v1MainRef, v1OriginRef}
	}
	return []string{v1MainRef, v1OriginRef}
}

func remoteCheckpointRefs(includeV2 bool) []string {
	if includeV2 {
		return []string{v2MainRef, v1RemoteFetchRef}
	}
	return []string{v1RemoteFetchRef}
}

func listLocalCheckpointRefIDs(ctx context.Context, runner CommandRunner, repoDir string, refs []string) (map[string]struct{}, []string) {
	ids := make(map[string]struct{})
	var refErrors []string
	var warnings []string

	for _, ref := range refs {
		stdout, stderr, err := runner.Run(ctx, repoDir, "git", "ls-tree", "-r", "--name-only", ref)
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

func listRemoteCheckpointRefIDs(ctx context.Context, runner CommandRunner, remoteURL string, refs []string) (map[string]struct{}, []string) {
	ids := make(map[string]struct{})
	var warnings []string

	tmpDir, err := os.MkdirTemp("", "entire-brain-checkpoints-*")
	if err != nil {
		return ids, []string{"create checkpoint discovery temp repo: " + err.Error()}
	}
	defer os.RemoveAll(tmpDir)

	if _, stderr, err := runner.Run(ctx, tmpDir, "git", "init", "-q"); err != nil {
		warnings = append(warnings, "initialize checkpoint discovery temp repo: "+err.Error())
		warnings = append(warnings, warningLines("git init", stderr)...)
		return ids, warnings
	}

	for _, ref := range refs {
		localRef := ref
		refspec := "+" + ref + ":" + localRef
		_, stderr, fetchErr := runner.Run(ctx, tmpDir, "git", "fetch", "--no-tags", "--depth=1", "--filter=blob:none", remoteURL, refspec)
		if fetchErr != nil {
			warnings = append(warnings, fmt.Sprintf("checkpoint remote ref %s unavailable: %v", ref, fetchErr))
			warnings = append(warnings, warningLines("checkpoint remote "+ref, stderr)...)
			continue
		}

		stdout, stderr, treeErr := runner.Run(ctx, tmpDir, "git", "ls-tree", "-r", "--name-only", localRef)
		if treeErr != nil {
			warnings = append(warnings, fmt.Sprintf("checkpoint remote ref %s unreadable after fetch: %v", ref, treeErr))
			warnings = append(warnings, warningLines("checkpoint remote "+ref, stderr)...)
			continue
		}
		warnings = append(warnings, warningLines("checkpoint remote "+ref, stderr)...)
		for _, id := range checkpointIDsFromTreeListing(stdout) {
			ids[id] = struct{}{}
		}
	}

	return ids, warnings
}

func checkpointEntriesFromIDs(ids map[string]struct{}, limit int) []checkpointListEntry {
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
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
	data, err := os.ReadFile(filepath.Join(repoDir, ".entire", "settings.json"))
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
		return "https://github.com/" + strings.Trim(strings.TrimSpace(remote.Repo), "/") + ".git", nil
	default:
		return "", fmt.Errorf("unsupported checkpoint_remote provider %q", remote.Provider)
	}
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
	settings, err := readEntireSettings(repoDir)
	if err != nil {
		return loadDefaultLocalV1CheckpointSnapshot(ctx, runner, repoDir, raw, limit, branchDestinations, progress, cache)
	}

	version := checkpointStorageV1
	refs := []string{v1MainRef, v1OriginRef}
	transcriptFileName := v1TranscriptFileName
	mode := "raw"
	if settings.CheckpointsV2Enabled() {
		version = checkpointStorageV2
		refs = []string{v2MainRef}
		transcriptFileName = v2TranscriptFileName
		mode = "compact"
		if raw {
			return nil, nil, fmt.Errorf("%w: direct v2 raw transcript export is not implemented", errCheckpointSnapshotUnavailable)
		}
	}

	snapshot, ref, warnings, err := loadCheckpointSnapshotFromGitDirRefs(ctx, runner, repoDir, refs, version, transcriptFileName, mode, limit, branchDestinations, progress, cache)
	if err == nil {
		warnings = append(warnings, fmt.Sprintf("exporting %s transcripts directly from local checkpoint ref %s", mode, ref))
		if version == checkpointStorageV1 && !raw {
			warnings = append(warnings, "compact transcript unavailable for v1 checkpoints; exported raw full.jsonl logs")
		}
		return snapshot, warnings, nil
	}

	remoteURL, remoteErr := settings.CheckpointRemoteFetchURL()
	if remoteErr != nil {
		return nil, warnings, fmt.Errorf("%w: local checkpoint refs unavailable and checkpoint remote unavailable: %v", errCheckpointSnapshotUnavailable, remoteErr)
	}

	remoteRef := remoteCheckpointRefs(version == checkpointStorageV2)[0]
	remoteSnapshot, remoteWarnings, remoteErr := loadCheckpointSnapshotFromRemoteRef(ctx, runner, remoteURL, remoteRef, version, transcriptFileName, mode, limit, branchDestinations, cache)
	if remoteErr != nil {
		warnings = append(warnings, remoteWarnings...)
		return nil, warnings, remoteErr
	}
	warnings = remoteWarnings
	warnings = append(warnings, fmt.Sprintf("exporting %s transcripts directly from configured checkpoint remote ref %s", mode, remoteRef))
	if version == checkpointStorageV1 && !raw {
		warnings = append(warnings, "compact transcript unavailable for v1 checkpoints; exported raw full.jsonl logs")
	}
	return remoteSnapshot, warnings, nil
}

func loadDefaultLocalV1CheckpointSnapshot(ctx context.Context, runner CommandRunner, repoDir string, raw bool, limit int, branchDestinations checkpointBranchDestinations, progress func(exportProgress), cache *checkpointMetadataCache) (*checkpointSnapshot, []string, error) {
	snapshot, ref, warnings, err := loadCheckpointSnapshotFromGitDirRefs(ctx, runner, repoDir, []string{v1MainRef, v1OriginRef}, checkpointStorageV1, v1TranscriptFileName, "raw", limit, branchDestinations, progress, cache)
	if err != nil {
		return nil, warnings, err
	}
	warnings = append(warnings, fmt.Sprintf("exporting raw transcripts directly from local checkpoint ref %s", ref))
	if !raw {
		warnings = append(warnings, "compact transcript unavailable for v1 checkpoints; exported raw full.jsonl logs")
	}
	return snapshot, warnings, nil
}

func loadCheckpointSnapshotFromGitDirRefs(ctx context.Context, runner CommandRunner, gitDir string, refs []string, version int, transcriptFileName, mode string, limit int, branchDestinations checkpointBranchDestinations, progress func(exportProgress), cache *checkpointMetadataCache) (*checkpointSnapshot, string, []string, error) {
	var warnings []string
	for _, ref := range refs {
		snapshot, refWarnings, err := loadCheckpointSnapshotFromGitDir(ctx, runner, gitDir, ref, version, transcriptFileName, mode, limit, branchDestinations, progress, cache)
		if err == nil {
			return snapshot, ref, append(warnings, refWarnings...), nil
		}
		warnings = append(warnings, refWarnings...)
		warnings = append(warnings, err.Error())
	}
	return nil, "", warnings, fmt.Errorf("%w: no local checkpoint refs readable", errCheckpointSnapshotUnavailable)
}

func loadCheckpointSnapshotFromGitDir(ctx context.Context, runner CommandRunner, gitDir, ref string, version int, transcriptFileName, mode string, limit int, branchDestinations checkpointBranchDestinations, progress func(exportProgress), cache *checkpointMetadataCache) (*checkpointSnapshot, []string, error) {
	reportExportStatus(progress, "reading checkpoint tree")
	stdout, stderr, err := runner.Run(ctx, gitDir, "git", "ls-tree", "-r", "--name-only", ref)
	if err != nil {
		warnings := warningLines("checkpoint ref "+ref, stderr)
		return nil, warnings, fmt.Errorf("%w: read checkpoint ref %s: %v", errCheckpointSnapshotUnavailable, ref, err)
	}
	treePaths := treePathSet(stdout)
	reader := &checkpointBlobReader{runner: runner, gitDir: gitDir, ref: ref}
	if cache != nil {
		// One extra ls-tree resolves the object id for each metadata path,
		// keying the cache. Negligible next to the thousands of cat-file calls
		// it lets us skip. Failure to read object ids just disables caching.
		if oidOut, _, oidErr := runner.Run(ctx, gitDir, "git", "ls-tree", "-r", ref); oidErr == nil {
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
	return &checkpointSnapshot{
		GitDir:             gitDir,
		Ref:                ref,
		Version:            version,
		TranscriptMode:     mode,
		TranscriptFileName: transcriptFileName,
		Selected:           selected,
		CheckpointCount:    checkpointCount,
		TreePaths:          treePaths,
	}, warnings, nil
}

func loadCheckpointSnapshotFromRemoteRef(ctx context.Context, runner CommandRunner, remoteURL, ref string, version int, transcriptFileName, mode string, limit int, branchDestinations checkpointBranchDestinations, cache *checkpointMetadataCache) (*checkpointSnapshot, []string, error) {
	tmpDir, err := os.MkdirTemp("", "entire-brain-checkpoints-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create checkpoint snapshot temp repo: %w", err)
	}
	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			os.RemoveAll(tmpDir)
		}
	}()

	if _, stderr, err := runner.Run(ctx, tmpDir, "git", "init", "-q"); err != nil {
		return nil, warningLines("git init", stderr), fmt.Errorf("initialize checkpoint snapshot temp repo: %w", err)
	}

	refspec := "+" + ref + ":" + ref
	if _, stderr, err := runner.Run(ctx, tmpDir, "git", "fetch", "--no-tags", "--depth=1", "--filter="+checkpointRemoteBlobFilter, remoteURL, refspec); err != nil {
		warnings := warningLines("checkpoint remote "+ref, stderr)
		return nil, warnings, fmt.Errorf("fetch checkpoint remote ref %s: %w", ref, err)
	}

	snapshot, warnings, err := loadCheckpointSnapshotFromGitDir(ctx, runner, tmpDir, ref, version, transcriptFileName, mode, limit, branchDestinations, nil, cache)
	if err != nil {
		return nil, warnings, err
	}
	cleanupOnError = false
	snapshot.TempDir = tmpDir
	return snapshot, warnings, nil
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

	rootMetadataByCheckpoint, transcriptPathsByMetadata, branchByCheckpoint, rootWarnings := readRootMetadataForSnapshot(ctx, reader, rootMetadataPaths, allowed, func() {
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
		return nil, min(len(checkpointIDs), limit), warnings, fmt.Errorf("%w: checkpoint ref %s contained no readable sessions", errCheckpointSnapshotUnavailable, reader.ref)
	}

	if len(checkpointIDs) > limit {
		warnings = append(warnings, fmt.Sprintf("checkpoint ref discovery capped at %d checkpoints; rerun with --checkpoint-limit <N> to inspect more", limit))
	}
	return selected, min(len(checkpointIDs), limit), warnings, nil
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

func readRootMetadataForSnapshot(ctx context.Context, reader *checkpointBlobReader, rootMetadataPaths []string, allowed map[string]struct{}, progress func()) (map[string][]byte, map[string]string, map[string]string, []string) {
	rootMetadataByCheckpoint := make(map[string][]byte)
	transcriptPathsByMetadata := make(map[string]string)
	branchByCheckpoint := make(map[string]string)
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
		for _, session := range summary.Sessions {
			metadataPath := normalizeSnapshotPath(session.Metadata)
			transcriptPath := normalizeSnapshotPath(session.Transcript)
			if metadataPath != "" && transcriptPath != "" {
				transcriptPathsByMetadata[metadataPath] = transcriptPath
			}
		}
	}

	return rootMetadataByCheckpoint, transcriptPathsByMetadata, branchByCheckpoint, warnings
}

func addSnapshotSession(selected map[string]selectedSession, checkpointID string, sessionIndex int, transcriptPath string, meta checkpointExportSession, checkpointBranch string, branchDestinations checkpointBranchDestinations) {
	if meta.SessionID == "" {
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
	if len(sorted) > limit {
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
	return checkpointID, isCheckpointID(checkpointID)
}

func sessionMetadataPathParts(path string) (string, int, bool) {
	parts := strings.Split(path, "/")
	if len(parts) != 4 || parts[3] != "metadata.json" {
		return "", 0, false
	}
	checkpointID := parts[0] + parts[1]
	if !isCheckpointID(checkpointID) {
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
		if isCheckpointID(id) {
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
	var file checkpointMetadataCacheFile
	if err := json.NewDecoder(gz).Decode(&file); err != nil || file.Version != checkpointMetadataCacheVersion || file.Blobs == nil {
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
	runner CommandRunner
	gitDir string
	ref    string
	oids   map[string]string
	cache  *checkpointMetadataCache
}

func (r *checkpointBlobReader) read(ctx context.Context, path string) ([]byte, error) {
	if r.cache == nil {
		return catFile(ctx, r.runner, r.gitDir, r.ref, path)
	}
	oid := r.oids[path]
	if oid != "" {
		if data, ok := r.cache.prev[oid]; ok {
			r.cache.next[oid] = data
			return data, nil
		}
	}
	data, err := catFile(ctx, r.runner, r.gitDir, r.ref, path)
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

func normalizeSnapshotPath(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(path)), "/")
}

func hasSnapshotTranscript(basePath string, treePaths map[string]struct{}) bool {
	if _, ok := treePaths[basePath]; ok {
		return true
	}
	return len(snapshotTranscriptChunks(basePath, treePaths)) > 0
}

func catFile(ctx context.Context, runner CommandRunner, gitDir, ref, path string) ([]byte, error) {
	stdout, _, err := runner.Run(ctx, gitDir, "git", "cat-file", "-p", ref+":"+path)
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
	for i := range sessions {
		session := &sessions[i]
		name := sessionFileName(session.CreatedAt, session.Agent, session.SessionID, session.LatestCheckpoint, ".jsonl")
		relPath := filepath.Join(branchDirs[session.Branch], name)
		if reuseTranscriptFromCursor(outputDir, cursor, *session, relPath, snapshot.TranscriptMode) {
			session.TranscriptPath = filepath.ToSlash(relPath)
			continue
		}

		sourcePath := session.SourceTranscriptPath
		if sourcePath == "" {
			sourcePath = snapshotTranscriptPath(session.LatestCheckpoint, session.SessionIndex, snapshot.TranscriptFileName)
		}

		transcript, err := readSnapshotTranscript(ctx, runner, snapshot, sourcePath)
		if err != nil {
			return nil, warnings, fmt.Errorf("export transcript for session %s from checkpoint %s: %w", session.SessionID, session.LatestCheckpoint, err)
		}

		if err := writeTranscriptFile(outputDir, relPath, transcript); err != nil {
			return nil, warnings, fmt.Errorf("write transcript for session %s: %w", session.SessionID, err)
		}
		session.TranscriptPath = filepath.ToSlash(relPath)
	}
	return sessions, warnings, nil
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
	info, err := os.Stat(filepath.Join(outputDir, relPath))
	return err == nil && !info.IsDir()
}

func writeTranscriptFile(outputDir, relPath string, data []byte) error {
	abs := filepath.Join(outputDir, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return fmt.Errorf("create transcript directory: %w", err)
	}
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		return err
	}
	return nil
}

func ensureExportDirectories(outputDir string, branchDirs map[string]string) error {
	for _, relPath := range branchDirs {
		if err := os.MkdirAll(filepath.Join(outputDir, relPath), 0o700); err != nil {
			return fmt.Errorf("create branch transcript directory %s: %w", relPath, err)
		}
	}
	return nil
}

func readSnapshotTranscript(ctx context.Context, runner CommandRunner, snapshot *checkpointSnapshot, sourcePath string) ([]byte, error) {
	if _, ok := snapshot.TreePaths[sourcePath]; ok {
		return catFile(ctx, runner, snapshot.GitDir, snapshot.Ref, sourcePath)
	}

	chunks := snapshotTranscriptChunks(sourcePath, snapshot.TreePaths)
	if len(chunks) == 0 {
		return nil, fmt.Errorf("transcript path %s not found in %s", sourcePath, snapshot.Ref)
	}

	var b strings.Builder
	for i, chunk := range chunks {
		data, err := catFile(ctx, runner, snapshot.GitDir, snapshot.Ref, chunk)
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
	if err := os.WriteFile(path, data, 0o600); err != nil {
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

	slugCounts := make(map[string]int)
	for _, branch := range branches {
		if branch == "" || branch == defaultBranch {
			continue
		}
		slugCounts[safePathComponent(branch, "branch", 100)]++
	}

	nextCollisionIndex := make(map[string]int)
	dirs := make(map[string]string, len(branches))
	for _, branch := range branches {
		switch {
		case branch == defaultBranch:
			dirs[branch] = defaultDir
		case branch == "":
			dirs[branch] = filepath.Join(exportSessionsDirectory, exportUnknownDirectory)
		default:
			slug := safePathComponent(branch, "branch", 100)
			if slugCounts[slug] > 1 {
				nextCollisionIndex[slug]++
				slug = fmt.Sprintf("%s-%d", slug, nextCollisionIndex[slug])
			}
			dirs[branch] = filepath.Join(exportSessionsDirectory, exportBranchesDirectory, slug)
		}
	}
	return dirs
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
	GitDir             string
	TempDir            string
	Ref                string
	Version            int
	TranscriptMode     string
	TranscriptFileName string
	Selected           map[string]selectedSession
	CheckpointCount    int
	TreePaths          map[string]struct{}
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
	StrategyOptions entireStrategyOptions `json:"strategy_options,omitempty"`
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
