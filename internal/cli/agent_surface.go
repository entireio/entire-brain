package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	brainBriefDefaultLimit      = 8
	brainInspectHistoryMaxFiles = 200
	brainInspectHistoryMaxBytes = 512 * 1024
	brainInspectHistoryMaxHits  = 25
)

type agentStatusOptions struct {
	json bool
}

type brainBriefOptions struct {
	json  bool
	limit int
}

type brainShowOptions struct {
	json bool
}

type brainStatusReport struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Repo        brainStatusRepo    `json:"repo"`
	Brain       brainStatusBrain   `json:"brain"`
	Sources     brainStatusSources `json:"sources"`
	Freshness   *staleReport       `json:"freshness,omitempty"`
	Live        brainLiveState     `json:"live"`
	Warnings    []string           `json:"warnings,omitempty"`
	Manifest    *exportManifest    `json:"manifest,omitempty"`
}

type brainStatusRepo struct {
	Root string `json:"root"`
	Key  string `json:"key"`
}

type brainStatusBrain struct {
	Path        string `json:"path"`
	Schema      int    `json:"schema_version"`
	GeneratedAt string `json:"generated_at,omitempty"`
}

type brainStatusSources struct {
	Seed     bool `json:"seed"`
	Sessions bool `json:"sessions"`
	Semantic bool `json:"semantic"`
	History  bool `json:"history"`
}

type brainLiveState struct {
	Branch             string           `json:"branch,omitempty"`
	Head               string           `json:"head,omitempty"`
	Dirty              bool             `json:"dirty"`
	Staged             []string         `json:"staged,omitempty"`
	Unstaged           []string         `json:"unstaged,omitempty"`
	Untracked          []string         `json:"untracked,omitempty"`
	DiffStat           string           `json:"diff_stat,omitempty"`
	ChangedFiles       []string         `json:"changed_files,omitempty"`
	ChangedSymbolHints []semanticRecord `json:"changed_symbol_hints,omitempty"`
	Warnings           []string         `json:"warnings,omitempty"`
}

type brainBriefReport struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Task        string             `json:"task"`
	Status      brainStatusReport  `json:"status"`
	Semantic    brainBriefSemantic `json:"semantic"`
	History     brainBriefHistory  `json:"history"`
	Guidance    []string           `json:"guidance"`
	Warnings    []string           `json:"warnings,omitempty"`
}

type brainBriefSemantic struct {
	Context semanticContextResult `json:"context"`
	Tests   semanticTestsResult   `json:"tests"`
}

type brainBriefHistory struct {
	Matches []brainTextMatch `json:"matches,omitempty"`
}

type brainShowReport struct {
	Freshness staleReport    `json:"freshness"`
	Record    semanticRecord `json:"record"`
}

type brainTextMatch struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Excerpt string `json:"excerpt"`
}

type brainHistoryInspectReport struct {
	Kind       string           `json:"kind"`
	Query      string           `json:"query"`
	BrainPath  string           `json:"brain_path"`
	Matches    []brainTextMatch `json:"matches"`
	Truncated  bool             `json:"truncated,omitempty"`
	Scanned    int              `json:"scanned_files"`
	ScanErrors []string         `json:"scan_errors,omitempty"`
}

func newAgentStatusCommand(opts Options) *cobra.Command {
	statusOpts := agentStatusOptions{}
	cmd := &cobra.Command{
		Use:   "status [path]",
		Short: "Summarize brain availability, freshness, and live workspace state",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := agentSurfaceTarget(opts, args)
			return runAgentStatus(cmd.Context(), cmd, opts, statusOpts, target)
		},
	}
	cmd.Flags().BoolVar(&statusOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newBrainBriefCommand(opts Options) *cobra.Command {
	briefOpts := brainBriefOptions{limit: brainBriefDefaultLimit}
	cmd := &cobra.Command{
		Use:   "brief <task>",
		Short: "Build a bounded task packet from brain context and live state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrainBrief(cmd.Context(), cmd, opts, briefOpts, args[0])
		},
	}
	cmd.Flags().BoolVar(&briefOpts.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().IntVar(&briefOpts.limit, "limit", brainBriefDefaultLimit, "Maximum semantic records per section")
	return cmd
}

func newBrainSearchCommand(opts Options) *cobra.Command {
	queryOpts := semanticQueryOptions{limit: 20}
	cmd := &cobra.Command{
		Use:   "search <symbol-or-text>",
		Short: "Search the local semantic brain",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticQuery(cmd.Context(), cmd, opts, queryOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&queryOpts.limit, "limit", 20, "Maximum results to return")
	cmd.Flags().IntVar(&queryOpts.offset, "offset", 0, "Results to skip before returning a page")
	cmd.Flags().BoolVar(&queryOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newBrainShowCommand(opts Options) *cobra.Command {
	showOpts := brainShowOptions{}
	cmd := &cobra.Command{
		Use:   "show <semantic-id-or-name>",
		Short: "Show one semantic brain record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrainShow(cmd.Context(), cmd, opts, showOpts, args[0])
		},
	}
	cmd.Flags().BoolVar(&showOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newBrainGuideCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "guide",
		Short: "Print the recommended coding-agent brain command set",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSpace(`
Start with:
  entire brain brief "<task>" --json

Small top-level surface:
  entire brain status [repo] --json
  entire brain brief "<task>" --json
  entire brain search "<query>" --json
  entire brain show <id> --json
  entire brain refresh [repo] --json
  entire brain guide
  entire brain path [repo]

Specialist tools:
  entire brain inspect code "<query>" --json
  entire brain inspect context <symbol-or-id> --json
  entire brain inspect impact <symbol-or-file> --json
  entire brain inspect changes --json
  entire brain inspect tests "<query>" --json
  entire brain inspect decisions "<query>" --json
  entire brain inspect history "<query>" --json
  entire brain inspect sessions "<query>" --json
  entire brain inspect validation "<query>" --json
  entire brain inspect tool-paths "<query>" --json
  entire brain inspect architecture "<query>" --json
  entire brain inspect boundaries --kind route|tool|workflow --json
`))
		},
	}
}

func newBrainInspectCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Run specialist brain inspection commands",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newInspectCodeCommand(opts))
	cmd.AddCommand(newInspectContextCommand(opts))
	cmd.AddCommand(newInspectImpactCommand(opts))
	cmd.AddCommand(newInspectChangesCommand(opts))
	cmd.AddCommand(newInspectTestsCommand(opts))
	for _, kind := range []string{"decisions", "history", "sessions", "validation", "tool-paths", "architecture"} {
		cmd.AddCommand(newInspectHistoryCommand(opts, kind))
	}
	cmd.AddCommand(newInspectBoundariesCommand(opts))
	return cmd
}

func newInspectCodeCommand(opts Options) *cobra.Command {
	queryOpts := semanticQueryOptions{limit: 20}
	cmd := &cobra.Command{
		Use:   "code <query>",
		Short: "Search semantic code facts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticQuery(cmd.Context(), cmd, opts, queryOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&queryOpts.limit, "limit", 20, "Maximum results to return")
	cmd.Flags().IntVar(&queryOpts.offset, "offset", 0, "Results to skip before returning a page")
	cmd.Flags().BoolVar(&queryOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectContextCommand(opts Options) *cobra.Command {
	contextOpts := semanticContextOptions{limit: 10}
	cmd := &cobra.Command{
		Use:   "context <symbol-or-text>",
		Short: "Build semantic context for a symbol or query",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticContext(cmd.Context(), cmd, opts, contextOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&contextOpts.limit, "limit", 10, "Maximum symbols to include")
	cmd.Flags().IntVar(&contextOpts.offset, "offset", 0, "Symbols to skip before returning a page")
	cmd.Flags().BoolVar(&contextOpts.includeContent, "include-content", false, "Include local source snippets for matched symbols")
	cmd.Flags().BoolVar(&contextOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectImpactCommand(opts Options) *cobra.Command {
	impactOpts := semanticImpactOptions{limit: 20, depth: 1}
	cmd := &cobra.Command{
		Use:   "impact <symbol-or-text>",
		Short: "Traverse semantic relations for an impact set",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticImpact(cmd.Context(), cmd, opts, impactOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&impactOpts.limit, "limit", 20, "Maximum symbols to include")
	cmd.Flags().IntVar(&impactOpts.depth, "depth", 1, "Relation traversal depth")
	cmd.Flags().BoolVar(&impactOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectChangesCommand(opts Options) *cobra.Command {
	changesOpts := semanticChangesOptions{limit: 100}
	cmd := &cobra.Command{
		Use:   "changes",
		Short: "Map local changes to semantic symbols",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticChanges(cmd.Context(), cmd, opts, changesOpts)
		},
	}
	cmd.Flags().IntVar(&changesOpts.limit, "limit", 100, "Maximum symbols to include")
	cmd.Flags().BoolVar(&changesOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectTestsCommand(opts Options) *cobra.Command {
	testsOpts := semanticTestsOptions{limit: 20}
	cmd := &cobra.Command{
		Use:   "tests <symbol-or-text>",
		Short: "Suggest tests relevant to a symbol or query",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticTests(cmd.Context(), cmd, opts, testsOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&testsOpts.limit, "limit", 20, "Maximum test suggestions to include")
	cmd.Flags().BoolVar(&testsOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectHistoryCommand(opts Options, kind string) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   kind + " <query>",
		Short: "Search exported brain " + kind + " text",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrainHistoryInspect(cmd.Context(), cmd, opts, kind, args[0], jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectBoundariesCommand(opts Options) *cobra.Command {
	var kind string
	boundaryOpts := semanticBoundaryOptions{limit: 50}
	cmd := &cobra.Command{
		Use:   "boundaries",
		Short: "List route, tool, or workflow boundaries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := inspectBoundarySpec(kind)
			if err != nil {
				return err
			}
			return runSemanticBoundary(cmd.Context(), cmd, opts, boundaryOpts, spec)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "tool", "Boundary kind: route, tool, or workflow")
	cmd.Flags().IntVar(&boundaryOpts.limit, "limit", 50, "Maximum boundary symbols to include")
	cmd.Flags().BoolVar(&boundaryOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func inspectBoundarySpec(kind string) (semanticBoundarySpec, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "route", "routes":
		return semanticBoundarySpec{Name: "routes", Use: "boundaries", Short: "List route boundaries", SymbolKinds: []string{"route", "http_route"}, RelationTypes: []string{"HANDLES_ROUTE"}}, nil
	case "tool", "tools":
		return semanticBoundarySpec{Name: "tools", Use: "boundaries", Short: "List tool boundaries", SymbolKinds: []string{"tool", "mcp_tool", "cli_command", "command"}, RelationTypes: []string{"HANDLES_TOOL", "HANDLES_CLI", "HANDLES_COMMAND"}}, nil
	case "workflow", "workflows":
		return semanticBoundarySpec{Name: "workflows", Use: "boundaries", Short: "List workflow boundaries", SymbolKinds: []string{"workflow", "job", "pipeline"}, RelationTypes: []string{"HANDLES_WORKFLOW", "PART_OF_WORKFLOW"}}, nil
	default:
		return semanticBoundarySpec{}, fmt.Errorf("--kind must be route, tool, or workflow")
	}
}

func runAgentStatus(ctx context.Context, cmd *cobra.Command, opts Options, statusOpts agentStatusOptions, target string) error {
	report, err := buildBrainStatusReport(ctx, opts, target)
	if err != nil {
		return err
	}
	if statusOpts.json {
		return writeJSON(cmd, report)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "brain: %s\n", report.Brain.Path)
	fmt.Fprintf(cmd.OutOrStdout(), "repo: %s\n", report.Repo.Root)
	fmt.Fprintf(cmd.OutOrStdout(), "sources: seed=%t sessions=%t semantic=%t history=%t\n", report.Sources.Seed, report.Sources.Sessions, report.Sources.Semantic, report.Sources.History)
	if report.Freshness != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "freshness: %s\n", report.Freshness.Severity)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "live: branch=%s head=%s dirty=%t\n", valueOrUnset(report.Live.Branch), valueOrUnset(report.Live.Head), report.Live.Dirty)
	if report.Live.DiffStat != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "diff: %s\n", report.Live.DiffStat)
	}
	for _, warning := range append(report.Warnings, report.Live.Warnings...) {
		fmt.Fprintf(cmd.OutOrStdout(), "warning: %s\n", warning)
	}
	return nil
}

func runBrainBrief(ctx context.Context, cmd *cobra.Command, opts Options, briefOpts brainBriefOptions, task string) error {
	if briefOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	target := agentSurfaceTarget(opts, nil)
	status, err := buildBrainStatusReport(ctx, opts, target)
	if err != nil {
		return err
	}
	report := brainBriefReport{
		GeneratedAt: opts.Now().UTC(),
		Task:        task,
		Status:      status,
		Guidance: []string{
			"Treat the brain as an indexed snapshot, not live memory.",
			"Use the live-state overlay before trusting semantic results for files changed in this session.",
			"Inspect full diffs or source files when the task intersects dirty files or when confidence is low.",
		},
	}
	if status.Manifest != nil && status.Manifest.Sources != nil && status.Manifest.Sources.Semantic != nil {
		contextSymbols, contextRelations, contextErr := semanticContextFacts(status.Brain.Path, status.Manifest.Sources.Semantic, task, briefOpts.limit, 0)
		if contextErr != nil {
			report.Warnings = append(report.Warnings, "semantic context unavailable: "+contextErr.Error())
		} else {
			report.Semantic.Context = semanticContextResult{Symbols: contextSymbols, Relations: contextRelations}
		}
		tests, testsErr := semanticTestFacts(status.Brain.Path, status.Manifest.Sources.Semantic, task, briefOpts.limit)
		if testsErr != nil {
			report.Warnings = append(report.Warnings, "test suggestions unavailable: "+testsErr.Error())
		} else {
			report.Semantic.Tests = tests
		}
	} else {
		report.Warnings = append(report.Warnings, "semantic index missing; run `entire brain refresh --semantic` or `entire brain index`")
	}
	if status.Manifest != nil && status.Manifest.Sources != nil && status.Manifest.Sources.History != nil {
		index, historyErr := loadBrainHistoryIndex(status.Brain.Path, status.Manifest.Sources.History)
		if historyErr != nil {
			report.Warnings = append(report.Warnings, "history context unavailable: "+historyErr.Error())
		} else {
			for _, record := range rankHistoryRecords(index, "history", task, briefOpts.limit) {
				report.History.Matches = append(report.History.Matches, historyRecordTextMatch(record))
			}
		}
	} else if status.Manifest != nil && status.Manifest.Sources != nil && status.Manifest.Sources.Sessions != nil {
		report.Warnings = append(report.Warnings, "history index missing; run `entire brain history-index` or `entire brain refresh --history-index`")
	}
	if briefOpts.json {
		return writeJSON(cmd, report)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "task: %s\n", report.Task)
	if report.Status.Freshness != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "freshness: %s\n", report.Status.Freshness.Severity)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "live: dirty=%t changed_files=%d\n", report.Status.Live.Dirty, len(report.Status.Live.ChangedFiles))
	for _, symbol := range report.Semantic.Context.Symbols {
		fmt.Fprintf(cmd.OutOrStdout(), "symbol %s %s:%d-%d\n", displaySymbolName(symbol), symbol.FilePath, symbol.StartLine, symbol.EndLine)
	}
	for _, suggestion := range report.Semantic.Tests.Suggestions {
		symbol := suggestion.Symbol
		fmt.Fprintf(cmd.OutOrStdout(), "test %s %s:%d-%d %s\n", displaySymbolName(symbol), symbol.FilePath, symbol.StartLine, symbol.EndLine, suggestion.Reason)
	}
	for _, match := range report.History.Matches {
		fmt.Fprintf(cmd.OutOrStdout(), "history %s:%d %s\n", match.Path, match.Line, match.Excerpt)
	}
	for _, warning := range append(report.Status.Warnings, report.Warnings...) {
		fmt.Fprintf(cmd.OutOrStdout(), "warning: %s\n", warning)
	}
	return nil
}

func runBrainShow(ctx context.Context, cmd *cobra.Command, opts Options, showOpts brainShowOptions, idOrName string) error {
	repoDir, err := exportRepoDir(opts.Env)
	if err != nil {
		return err
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return err
	}
	if manifest.Sources == nil || manifest.Sources.Semantic == nil {
		return errors.New("semantic index missing; run `entire brain index`")
	}
	freshness, err := semanticStaleReport(ctx, opts, repoDir)
	if err != nil {
		return err
	}
	record, err := findSemanticRecordByIDOrName(storage.BrainDir, manifest.Sources.Semantic, idOrName)
	if err != nil {
		return err
	}
	report := brainShowReport{Freshness: freshness, Record: record}
	if showOpts.json {
		return writeJSON(cmd, report)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s:%d-%d\n", record.Kind, displaySymbolName(record), record.FilePath, record.StartLine, record.EndLine)
	if record.Signature != "" {
		fmt.Fprintln(cmd.OutOrStdout(), record.Signature)
	}
	return nil
}

func runBrainHistoryInspect(ctx context.Context, cmd *cobra.Command, opts Options, kind, query string, jsonOut bool) error {
	target := agentSurfaceTarget(opts, nil)
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return err
	}
	if !local {
		return fmt.Errorf("inspect %s requires a local repository path: %s", kind, target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return err
	}
	report, err := inspectBrainText(storage.BrainDir, kind, query)
	if err != nil {
		return err
	}
	if jsonOut {
		return writeJSON(cmd, report)
	}
	for _, match := range report.Matches {
		fmt.Fprintf(cmd.OutOrStdout(), "%s:%d: %s\n", match.Path, match.Line, match.Excerpt)
	}
	if len(report.Matches) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "no %s matches for %q\n", kind, query)
	}
	return nil
}

func buildBrainStatusReport(ctx context.Context, opts Options, target string) (brainStatusReport, error) {
	repoDir, local, err := resolveLocalTargetRepoDir(ctx, opts.Runner, target)
	if err != nil {
		return brainStatusReport{}, err
	}
	if !local {
		return brainStatusReport{}, fmt.Errorf("status requires a local repository path: %s", target)
	}
	storage, err := repoStoragePaths(ctx, opts.Runner, opts.Env, repoDir)
	if err != nil {
		return brainStatusReport{}, err
	}
	manifest, err := loadBrainManifest(storage.BrainDir)
	if err != nil {
		return brainStatusReport{}, err
	}
	report := brainStatusReport{
		GeneratedAt: opts.Now().UTC(),
		Repo:        brainStatusRepo{Root: repoDir, Key: storage.Key},
		Brain: brainStatusBrain{
			Path:        storage.BrainDir,
			Schema:      manifest.SchemaVersion,
			GeneratedAt: manifest.GeneratedAt.Format(time.RFC3339),
		},
		Manifest: manifest,
	}
	if manifest.GeneratedAt.IsZero() {
		report.Brain.GeneratedAt = ""
	}
	if manifest.Sources != nil {
		report.Sources.Seed = manifest.Sources.Seed != nil
		report.Sources.Sessions = manifest.Sources.Sessions != nil
		report.Sources.Semantic = manifest.Sources.Semantic != nil
		report.Sources.History = manifest.Sources.History != nil
	}
	live, liveErr := brainLiveStateReport(ctx, opts.Runner, repoDir, storage.BrainDir, manifest)
	if liveErr != nil {
		report.Warnings = append(report.Warnings, "live state unavailable: "+liveErr.Error())
	} else {
		report.Live = live
	}
	if report.Sources.Semantic {
		freshness, freshnessErr := semanticStaleReport(ctx, opts, repoDir)
		if freshnessErr != nil {
			report.Warnings = append(report.Warnings, "freshness unavailable: "+freshnessErr.Error())
		} else {
			report.Freshness = &freshness
		}
	}
	return report, nil
}

func brainLiveStateReport(ctx context.Context, runner CommandRunner, repoDir, brainDir string, manifest *exportManifest) (brainLiveState, error) {
	var live brainLiveState
	if head, err := gitScalar(ctx, runner, repoDir, "rev-parse", "HEAD"); err == nil {
		live.Head = head
	} else {
		live.Warnings = append(live.Warnings, "HEAD unavailable: "+err.Error())
	}
	if branch, err := gitScalar(ctx, runner, repoDir, "branch", "--show-current"); err == nil {
		live.Branch = branch
	} else {
		live.Warnings = append(live.Warnings, "branch unavailable: "+err.Error())
	}
	status, err := gitStatusPorcelainAll(ctx, runner, repoDir)
	if err != nil {
		return live, err
	}
	live.Staged, live.Unstaged, live.Untracked = parseBrainLiveStatus(status)
	live.Dirty = len(live.Staged)+len(live.Unstaged)+len(live.Untracked) > 0
	if stat, err := gitScalar(ctx, runner, repoDir, "diff", "--shortstat", "HEAD"); err == nil {
		live.DiffStat = stat
	} else {
		live.Warnings = append(live.Warnings, "diff stat unavailable: "+err.Error())
	}
	if manifest != nil && manifest.Sources != nil && manifest.Sources.Semantic != nil {
		files, err := changedSemanticFiles(ctx, runner, repoDir)
		if err == nil {
			live.ChangedFiles = files
			if symbols, symbolsErr := semanticSymbolsForFiles(brainDir, manifest.Sources.Semantic, files, 20); symbolsErr == nil {
				live.ChangedSymbolHints = symbols
			} else {
				live.Warnings = append(live.Warnings, "changed symbol hints unavailable: "+symbolsErr.Error())
			}
		} else {
			live.Warnings = append(live.Warnings, "changed file list unavailable: "+err.Error())
		}
	}
	return live, nil
}

func parseBrainLiveStatus(status []byte) (staged, unstaged, untracked []string) {
	for _, line := range strings.Split(string(status), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		paths := worktreeStatusPaths(line)
		if len(paths) == 0 {
			continue
		}
		if strings.HasPrefix(line, "?? ") {
			untracked = append(untracked, paths...)
			continue
		}
		index := byte(' ')
		worktree := byte(' ')
		if len(line) > 0 {
			index = line[0]
		}
		if len(line) > 1 {
			worktree = line[1]
		}
		if index != ' ' && index != '?' {
			staged = append(staged, paths...)
		}
		if worktree != ' ' && worktree != '?' {
			unstaged = append(unstaged, paths...)
		}
	}
	sort.Strings(staged)
	sort.Strings(unstaged)
	sort.Strings(untracked)
	return staged, unstaged, untracked
}

func findSemanticRecordByIDOrName(brainDir string, source *semanticSourceManifest, idOrName string) (semanticRecord, error) {
	if source.StorePath != "" {
		storePath, err := validateSemanticDeclaredStore(brainDir, source)
		if err != nil {
			return semanticRecord{}, err
		}
		return findSemanticRecordByIDOrNameSQLite(storePath, idOrName)
	}
	snapshotPath, err := validateSemanticSnapshotPath(source.SnapshotPath)
	if err != nil {
		return semanticRecord{}, err
	}
	if err := rejectSymlinkPathComponents(brainDir, snapshotPath); err != nil {
		return semanticRecord{}, err
	}
	return findSemanticRecordByIDOrNameSnapshot(filepath.Join(brainDir, snapshotPath), idOrName)
}

func findSemanticRecordByIDOrNameSQLite(storePath, idOrName string) (semanticRecord, error) {
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		return semanticRecord{}, err
	}
	defer db.Close()
	row := db.QueryRow(`SELECT id, kind, name, qualified_name, file_path, start_line, end_line, signature, language, stable_id_version
FROM symbols
WHERE id = ? OR name = ? OR qualified_name = ?
ORDER BY CASE WHEN id = ? THEN 0 WHEN qualified_name = ? THEN 1 WHEN name = ? THEN 2 ELSE 3 END
LIMIT 1`, idOrName, idOrName, idOrName, idOrName, idOrName, idOrName)
	var record semanticRecord
	record.RecordType = "symbol"
	if err := row.Scan(&record.ID, &record.Kind, &record.Name, &record.QualifiedName, &record.FilePath, &record.StartLine, &record.EndLine, &record.Signature, &record.Language, &record.StableIDVersion); err != nil {
		return semanticRecord{}, fmt.Errorf("semantic record not found: %s", idOrName)
	}
	return record, nil
}

func findSemanticRecordByIDOrNameSnapshot(path, idOrName string) (semanticRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return semanticRecord{}, fmt.Errorf("open semantic snapshot: %w", err)
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	for {
		var record semanticRecord
		if err := decoder.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return semanticRecord{}, err
		}
		if record.RecordType != "symbol" {
			continue
		}
		if record.ID == idOrName || record.Name == idOrName || record.QualifiedName == idOrName {
			return record, nil
		}
	}
	return semanticRecord{}, fmt.Errorf("semantic record not found: %s", idOrName)
}

func inspectBrainText(brainDir, kind, query string) (brainHistoryInspectReport, error) {
	report := brainHistoryInspectReport{Kind: kind, Query: query, BrainPath: brainDir}
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return report, errors.New("query must not be empty")
	}
	if indexed, ok := inspectBrainHistoryIndex(brainDir, kind, query); ok {
		return indexed, nil
	}
	err := filepath.WalkDir(brainDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			report.ScanErrors = append(report.ScanErrors, err.Error())
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == semanticDirName || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if report.Scanned >= brainInspectHistoryMaxFiles || len(report.Matches) >= brainInspectHistoryMaxHits {
			report.Truncated = true
			return filepath.SkipAll
		}
		ext := filepath.Ext(path)
		if ext != ".md" && ext != ".json" && ext != ".jsonl" && ext != ".txt" {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			report.ScanErrors = append(report.ScanErrors, statErr.Error())
			return nil
		}
		if info.Size() > brainInspectHistoryMaxBytes {
			return nil
		}
		report.Scanned++
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			report.ScanErrors = append(report.ScanErrors, readErr.Error())
			return nil
		}
		rel, _ := filepath.Rel(brainDir, path)
		for i, line := range strings.Split(string(data), "\n") {
			if !historyTextMatchesQuery(line, query) {
				continue
			}
			report.Matches = append(report.Matches, brainTextMatch{
				Path:    filepath.ToSlash(rel),
				Line:    i + 1,
				Excerpt: strings.TrimSpace(truncateString(line, 500)),
			})
			if len(report.Matches) >= brainInspectHistoryMaxHits {
				report.Truncated = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	return report, nil
}

func inspectBrainHistoryIndex(brainDir, kind, query string) (brainHistoryInspectReport, bool) {
	manifest, err := loadBrainManifest(brainDir)
	if err != nil || manifest.Sources == nil || manifest.Sources.History == nil {
		return brainHistoryInspectReport{}, false
	}
	index, err := loadBrainHistoryIndex(brainDir, manifest.Sources.History)
	if err != nil {
		return brainHistoryInspectReport{}, false
	}
	report := brainHistoryInspectReport{Kind: kind, Query: query, BrainPath: brainDir, Scanned: len(index.Records)}
	records := inspectHistoryRecords(index, kind, query, brainInspectHistoryMaxHits)
	for _, record := range records {
		report.Matches = append(report.Matches, historyRecordTextMatch(record))
	}
	if len(report.Matches) >= brainInspectHistoryMaxHits {
		report.Truncated = true
	}
	return report, true
}

func inspectHistoryRecords(index historyIndex, kind, query string, limit int) []historyRecord {
	switch kind {
	case "history", "sessions", "architecture":
		return rankHistoryRecords(index, kind, query, limit)
	default:
		allowed := historyInspectKinds(kind)
		var records []historyRecord
		seen := map[string]struct{}{}
		for _, record := range index.Records {
			if len(allowed) > 0 {
				if _, ok := allowed[record.Kind]; !ok {
					continue
				}
			}
			if !historyRecordMatchesQuery(record, query) {
				continue
			}
			matchKey := normalizeHistorySearchText(record.Summary)
			if _, ok := seen[matchKey]; ok {
				continue
			}
			seen[matchKey] = struct{}{}
			records = append(records, record)
			if len(records) >= limit {
				break
			}
		}
		return records
	}
}

func historyRecordTextMatch(record historyRecord) brainTextMatch {
	return brainTextMatch{
		Path:    record.Path,
		Line:    record.Line,
		Excerpt: record.Summary,
	}
}

func historyInspectKinds(kind string) map[string]struct{} {
	switch kind {
	case "decisions":
		return map[string]struct{}{"decision": {}}
	case "validation":
		return map[string]struct{}{"validation": {}}
	case "tool-paths":
		return map[string]struct{}{"tool_call": {}}
	case "architecture":
		return map[string]struct{}{"architecture": {}, "decision": {}, "learning": {}}
	case "history", "sessions":
		return nil
	default:
		return nil
	}
}

func agentSurfaceTarget(opts Options, args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	if opts.Env.RepoRoot != "" {
		return opts.Env.RepoRoot
	}
	return "."
}

func truncateString(value string, max int) string {
	if len(value) <= max {
		return value
	}
	if max <= 3 {
		return value[:max]
	}
	return value[:max-3] + "..."
}
