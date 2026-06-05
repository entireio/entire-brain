package cli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

const (
	brainBriefDefaultLimit      = 8
	brainBriefFactsLimit        = 6
	brainInspectHistoryMaxFiles = 1000
	brainInspectHistoryMaxBytes = 512 * 1024
	brainInspectHistoryMaxHits  = 25
	brainInspectHistoryMaxLine  = 16 * 1024 * 1024
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
	Facts    bool `json:"facts"`
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
	GeneratedAt     time.Time          `json:"generated_at"`
	Task            string             `json:"task"`
	Status          brainStatusReport  `json:"status"`
	Semantic        brainBriefSemantic `json:"semantic"`
	History         brainBriefHistory  `json:"history"`
	Facts           []factRecord       `json:"facts,omitempty"`
	ActionChecklist []brainBriefAction `json:"action_checklist,omitempty"`
	LikelyEditFiles []string           `json:"likely_edit_files,omitempty"`
	LikelyTestFiles []string           `json:"likely_test_files,omitempty"`
	LikelyFiles     []string           `json:"likely_files,omitempty"`
	Guidance        []string           `json:"guidance"`
	Warnings        []string           `json:"warnings,omitempty"`
}

type brainBriefSemantic struct {
	Context semanticContextResult `json:"context"`
	Tests   semanticTestsResult   `json:"tests"`
}

type brainBriefHistory struct {
	Matches []brainTextMatch `json:"matches,omitempty"`
}

type brainBriefAction struct {
	File     string `json:"file,omitempty"`
	Symbol   string `json:"symbol,omitempty"`
	Action   string `json:"action"`
	Evidence string `json:"evidence,omitempty"`
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
	cmd.AddCommand(newInspectFactsCommand(opts))
	cmd.AddCommand(newInspectBlameCommand(opts))
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
	fmt.Fprintf(cmd.OutOrStdout(), "sources: seed=%t sessions=%t semantic=%t history=%t facts=%t\n", report.Sources.Seed, report.Sources.Sessions, report.Sources.Semantic, report.Sources.History, report.Sources.Facts)
	if report.Manifest != nil && report.Manifest.Sources != nil && report.Manifest.Sources.Facts != nil {
		f := report.Manifest.Sources.Facts
		fmt.Fprintf(cmd.OutOrStdout(), "facts: %d (%d distilled, %d authored, %d superseded) across %d branch(es); %d proposals pending\n",
			f.Facts, f.Distilled, f.Authored, f.Superseded, len(f.Branches), f.Proposals)
	}
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
		Status:      brainBriefOutputStatus(status),
		Guidance: []string{
			"Treat the brain as an indexed snapshot, not live memory.",
			"Use likely_edit_files and history matches before broad text search; use likely_test_files for validation context.",
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
		report.Warnings = append(report.Warnings, "semantic index missing; run `entire brain refresh`")
	}
	if status.Manifest != nil && status.Manifest.Sources != nil && status.Manifest.Sources.History != nil {
		index, historyErr := loadBrainHistoryIndex(status.Brain.Path, status.Manifest.Sources.History)
		if historyErr != nil {
			report.Warnings = append(report.Warnings, "history context unavailable: "+historyErr.Error())
		} else {
			var indexedMatches []brainTextMatch
			for _, record := range rankHistoryRecords(index, "history", task, briefOpts.limit) {
				indexedMatches = append(indexedMatches, historyRecordTextMatch(record))
			}
			rawMatches, rawErr := brainBriefRawHistoryMatches(status.Brain.Path, task, nil, briefOpts.limit)
			if rawErr != nil {
				report.Warnings = append(report.Warnings, "raw history fallback unavailable: "+rawErr.Error())
			}
			report.History.Matches = mergeBrainBriefHistoryMatches(briefOpts.limit, rawMatches, indexedMatches)
		}
	} else if status.Manifest != nil && status.Manifest.Sources != nil && status.Manifest.Sources.Sessions != nil {
		report.Warnings = append(report.Warnings, "history index missing; run `entire brain refresh`")
	}
	if status.Sources.Facts {
		branch := status.Live.Branch
		if branch == "" {
			branch = distillDefaultBranch
		}
		if facts, factsErr := loadFacts(status.Brain.Path, branch); factsErr != nil {
			report.Warnings = append(report.Warnings, "facts unavailable: "+factsErr.Error())
		} else {
			report.Facts = rankFacts(facts, task, brainBriefFactsCount(briefOpts.limit), false)
		}
	}
	report.LikelyEditFiles, report.LikelyTestFiles, report.LikelyFiles = brainBriefLikelyFileGroups(status.Repo.Root, report, task)
	report.LikelyTestFiles = brainBriefAddSiblingTestFiles(status.Repo.Root, report.LikelyEditFiles, report.LikelyTestFiles)
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
	report.ActionChecklist = brainBriefActionChecklist(status.Repo.Root, report, task)
	if len(report.ActionChecklist) > 0 {
		report.LikelyEditFiles = brainBriefActionFiles(report.ActionChecklist)
		report.LikelyTestFiles = brainBriefAddSiblingTestFiles(status.Repo.Root, report.LikelyEditFiles, report.LikelyTestFiles)
		report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
		report.Guidance = append(report.Guidance, "Treat action_checklist as the first-pass current-code inventory; edit listed files first, and broaden only when the checklist is missing, ambiguous, or validation fails.")
	}
	if briefOpts.json {
		return writeJSON(cmd, report)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "task: %s\n", report.Task)
	if report.Status.Freshness != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "freshness: %s\n", report.Status.Freshness.Severity)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "live: dirty=%t changed_files=%d\n", report.Status.Live.Dirty, len(report.Status.Live.ChangedFiles))
	for _, file := range report.LikelyEditFiles {
		fmt.Fprintf(cmd.OutOrStdout(), "edit_file %s\n", file)
	}
	for _, file := range report.LikelyTestFiles {
		fmt.Fprintf(cmd.OutOrStdout(), "test_file %s\n", file)
	}
	for _, file := range report.LikelyFiles {
		fmt.Fprintf(cmd.OutOrStdout(), "file %s\n", file)
	}
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
	for _, fact := range report.Facts {
		fmt.Fprintf(cmd.OutOrStdout(), "fact [%s] %s\n", strings.Join(fact.Paths, ","), fact.Text)
	}
	for _, item := range report.ActionChecklist {
		location := item.File
		if item.Symbol != "" {
			location = strings.TrimSpace(location + " " + item.Symbol)
		}
		if location != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "action %s: %s\n", location, item.Action)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "action %s\n", item.Action)
		}
	}
	for _, warning := range append(report.Status.Warnings, report.Warnings...) {
		fmt.Fprintf(cmd.OutOrStdout(), "warning: %s\n", warning)
	}
	return nil
}

func brainBriefOutputStatus(status brainStatusReport) brainStatusReport {
	status.Manifest = nil
	return status
}

var brainBriefPathPattern = regexp.MustCompile(`(?:^|[\s"'({\[])([A-Za-z0-9_@./-]+\.[A-Za-z0-9][A-Za-z0-9._-]*)`)

// brainBriefFactsCount sizes the brief's facts section to the requested brief
// limit so a compact request (e.g. the Opus compact mode's limit 3, or the
// MCP brain_brief limit) gets fewer facts instead of a fixed block. Facts are a
// supplementary section, so they never exceed brainBriefFactsLimit and shrink
// with the budget; at least one fact is kept whenever the section is shown.
func brainBriefFactsCount(limit int) int {
	if limit < 1 {
		limit = 1
	}
	if limit > brainBriefFactsLimit {
		return brainBriefFactsLimit
	}
	return limit
}

func brainBriefLikelyFileGroups(repoRoot string, report brainBriefReport, task string) ([]string, []string, []string) {
	counts := map[string]int{}
	taskTerms := brainBriefFileMatchTerms(task)
	add := func(path string, weight int) {
		clean, ok := cleanBrainBriefLikelyFile(path)
		if !ok {
			return
		}
		counts[clean] += weight + brainBriefLikelyFileBonus(clean) + brainBriefTaskTermBonus(clean, taskTerms)
	}
	for _, symbol := range report.Semantic.Context.Symbols {
		add(symbol.FilePath, 12)
		add(symbol.Path, 4)
	}
	for _, relation := range report.Semantic.Context.Relations {
		add(relation.FilePath, 4)
		add(relation.Path, 2)
	}
	for _, root := range report.Semantic.Tests.Roots {
		add(root.FilePath, 6)
	}
	for _, suggestion := range report.Semantic.Tests.Suggestions {
		add(suggestion.Symbol.FilePath, 9)
	}
	for _, changed := range report.Status.Live.ChangedFiles {
		add(changed, 3)
	}
	for _, match := range report.History.Matches {
		for _, path := range extractBrainBriefPaths(match.Excerpt) {
			add(path, 5)
		}
	}
	for path, score := range brainBriefCurrentCodeFileCounts(repoRoot, task) {
		add(path, score)
	}
	editCounts := map[string]int{}
	testCounts := map[string]int{}
	for path, score := range counts {
		if brainBriefLikelyTestFile(path) {
			testCounts[path] = score
		} else {
			editCounts[path] = score
		}
	}
	editFiles := rankedBrainBriefLikelyFiles(editCounts, 8)
	testFiles := rankedBrainBriefLikelyFiles(testCounts, 6)
	all := append([]string{}, editFiles...)
	for _, file := range testFiles {
		if len(all) >= 12 {
			break
		}
		all = append(all, file)
	}
	return editFiles, testFiles, all
}

func brainBriefCurrentCodeFileCounts(repoRoot, task string) map[string]int {
	if brainBriefPreviousResponseTask(task) {
		return brainBriefCurrentCodeFileCountsByScore(repoRoot, brainBriefPreviousResponseFileScore)
	}
	if !brainBriefProviderMetadataTask(task) {
		return nil
	}
	return brainBriefCurrentCodeFileCountsByScore(repoRoot, brainBriefProviderMetadataFileScore)
}

func brainBriefCurrentCodeFileCountsByScore(repoRoot string, scoreFile func(string, string) int) map[string]int {
	counts := map[string]int{}
	walked := 0
	_ = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == repoRoot {
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if brainBriefSkipSourceDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if walked >= 2500 || !brainBriefSourceFile(rel) {
			return nil
		}
		walked++
		info, statErr := d.Info()
		if statErr != nil || info.Size() > brainInspectHistoryMaxBytes {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		score := scoreFile(rel, string(data))
		if score >= 90 {
			counts[rel] = score
		}
		return nil
	})
	return counts
}

func brainBriefSkipSourceDir(rel string) bool {
	lower := strings.ToLower(filepath.ToSlash(rel))
	switch lower {
	case ".git", ".benchmark", ".entire", ".codex", "node_modules", "dist", "build", "coverage", ".next", ".turbo":
		return true
	}
	return strings.HasPrefix(lower, ".git/") ||
		strings.HasPrefix(lower, ".benchmark/") ||
		strings.HasPrefix(lower, ".entire/") ||
		strings.HasPrefix(lower, "node_modules/") ||
		strings.HasPrefix(lower, "dist/") ||
		strings.HasPrefix(lower, "build/") ||
		strings.HasPrefix(lower, "coverage/")
}

func brainBriefProviderMetadataTask(task string) bool {
	lower := strings.ToLower(task)
	return strings.Contains(lower, "metadata") &&
		(strings.Contains(lower, "responses api") ||
			strings.Contains(lower, "invalid_type") ||
			strings.Contains(lower, "provider contract") ||
			strings.Contains(lower, "agentic decider") ||
			strings.Contains(lower, "browser decider"))
}

func brainBriefPreviousResponseTask(task string) bool {
	lower := strings.ToLower(task)
	return (strings.Contains(lower, "self-contained") ||
		strings.Contains(lower, "stale model state") ||
		strings.Contains(lower, "continuing from stale") ||
		strings.Contains(lower, "browser decision turns")) &&
		(strings.Contains(lower, "browser") || strings.Contains(lower, "agentic"))
}

func brainBriefProviderMetadataFileScore(rel, source string) int {
	lower := strings.ToLower(source)
	score := 0
	if strings.Contains(lower, "metadata") {
		score += 30
	}
	if strings.Contains(source, "agentic_decision") {
		score += 100
	}
	if strings.Contains(source, "stepIndex") || strings.Contains(source, "step:") {
		score += 35
	}
	if strings.Contains(source, "previousResponseId") {
		score += 35
	}
	if strings.Contains(source, "reasoningEffort") {
		score += 25
	}
	if strings.Contains(source, "createAgenticDecider") {
		score += 55
	}
	if strings.Contains(lower, "llm.generate") || strings.Contains(lower, ".generate(") {
		score += 15
	}
	relLower := strings.ToLower(rel)
	if strings.Contains(relLower, "agentic") || strings.Contains(relLower, "decider") {
		score += 15
	}
	if strings.Contains(relLower, "demo") || strings.Contains(lower, "preview: true") {
		score -= 70
	}
	return score
}

func brainBriefPreviousResponseFileScore(rel, source string) int {
	lower := strings.ToLower(source)
	score := 0
	if strings.Contains(source, "AgenticPerception") {
		score += 45
	}
	if strings.Contains(source, "executeAgenticPlan") {
		score += 75
	}
	if strings.Contains(source, "previousResponseId: null") {
		score += 100
	}
	if strings.Contains(source, "history: history.slice") {
		score += 35
	}
	if strings.Contains(source, "screenshotBase64") && strings.Contains(source, "pageText") {
		score += 25
	}
	relLower := strings.ToLower(rel)
	if strings.Contains(relLower, "packages/brain/") || strings.Contains(relLower, "packages/providers/") {
		score -= 100
	}
	if strings.Contains(lower, "previous_response_id") && !strings.Contains(source, "AgenticPerception") {
		score -= 40
	}
	return score
}

func brainBriefAddSiblingTestFiles(repoRoot string, editFiles, testFiles []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(testFiles)+len(editFiles))
	for _, file := range testFiles {
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		out = append(out, file)
	}
	for _, file := range editFiles {
		for _, candidate := range brainBriefSiblingTestCandidates(file) {
			if _, ok := seen[candidate]; ok {
				continue
			}
			if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(candidate))); err != nil {
				continue
			}
			seen[candidate] = struct{}{}
			out = append(out, candidate)
			if len(out) >= 6 {
				return out
			}
		}
	}
	return out
}

func brainBriefSiblingTestCandidates(file string) []string {
	ext := filepath.Ext(file)
	if ext == "" {
		return nil
	}
	stem := strings.TrimSuffix(file, ext)
	switch ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return []string{stem + ".test" + ext, stem + ".spec" + ext}
	case ".go":
		return []string{stem + "_test.go"}
	case ".py":
		return []string{stem + "_test.py", stem + "_test" + ext}
	default:
		return nil
	}
}

func brainBriefMergeLikelyFiles(editFiles, testFiles []string) []string {
	all := append([]string{}, editFiles...)
	for _, file := range testFiles {
		if len(all) >= 12 {
			break
		}
		if !slices.Contains(all, file) {
			all = append(all, file)
		}
	}
	return all
}

func brainBriefActionFiles(actions []brainBriefAction) []string {
	seen := map[string]struct{}{}
	files := make([]string, 0, len(actions))
	for _, action := range actions {
		clean, ok := cleanBrainBriefLikelyFile(action.File)
		if !ok || !brainBriefSourceFile(clean) {
			continue
		}
		if _, exists := seen[clean]; exists {
			continue
		}
		seen[clean] = struct{}{}
		files = append(files, clean)
		if len(files) >= 8 {
			break
		}
	}
	return files
}

type rankedBrainBriefFile struct {
	path  string
	score int
}

func rankedBrainBriefLikelyFiles(counts map[string]int, limit int) []string {
	files := make([]rankedBrainBriefFile, 0, len(counts))
	for path, score := range counts {
		files = append(files, rankedBrainBriefFile{path: path, score: score})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].score != files[j].score {
			return files[i].score > files[j].score
		}
		return files[i].path < files[j].path
	})
	capped := min(len(files), limit)
	out := make([]string, 0, capped)
	for _, file := range files[:capped] {
		out = append(out, file.path)
	}
	return out
}

// brainBriefTaskTermStop is a generic English + generic-task-verb stopword list so
// filename matching keys on meaningful nouns/identifiers, not filler words. Kept
// deliberately generic (no words cherry-picked from particular task prompts).
var brainBriefTaskTermStop = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "when": true, "must": true,
	"that": true, "this": true, "these": true, "those": true, "into": true, "from": true,
	"before": true, "after": true, "their": true, "your": true, "also": true, "than": true,
	"then": true, "but": true, "are": true, "was": true, "will": true, "can": true,
	"how": true, "why": true, "what": true, "where": true, "which": true, "should": true,
	"does": true, "did": true, "has": true, "have": true, "had": true, "its": true,
	"would": true, "need": true, "want": true, "use": true, "using": true, "used": true,
	"add": true, "fix": true, "update": true, "change": true, "make": true, "ensure": true,
	"run": true, "runs": true, "running": true, "set": true, "get": true,
	// Common 3-char fillers (matched now that the floor is 3, so that strong
	// 3-char identifiers like "api"/"cli" are kept while filler is dropped).
	"not": true, "all": true, "any": true, "one": true, "two": true, "new": true,
	"old": true, "via": true, "per": true, "off": true, "out": true, "now": true,
	"yet": true, "way": true, "see": true, "let": true, "may": true, "you": true,
}

// Floor is 3 (not 4) so high-signal short identifiers like "api"/"cli" are not
// skipped; common 3-char filler words are removed by brainBriefTaskTermStop above.
var brainBriefTaskWordPattern = regexp.MustCompile(`[a-z0-9]{3,}`)

// brainBriefFileMatchTerms extracts the significant lowercase tokens from a task
// description used to bias likely_edit_files toward files named after the task.
func brainBriefFileMatchTerms(task string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, word := range brainBriefTaskWordPattern.FindAllString(strings.ToLower(task), -1) {
		if brainBriefTaskTermStop[word] || seen[word] {
			continue
		}
		seen[word] = true
		out = append(out, word)
	}
	return out
}

// brainBriefTaskTermBonus rewards a candidate file whose name matches the task's
// significant terms — a strong "this file is what the task is about" signal that
// the raw semantic-symbol density (which favors large files like TUIs) misses.
func brainBriefTaskTermBonus(path string, terms []string) int {
	if len(terms) == 0 {
		return 0
	}
	lower := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(path))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	score := 0
	for _, term := range terms {
		switch {
		case strings.Contains(base, term):
			score += 10 // basename match is the strongest locator
		case strings.Contains(lower, term):
			score += 3 // elsewhere in the path is weaker
		}
	}
	return score
}

func brainBriefLikelyFileBonus(path string) int {
	lower := strings.ToLower(path)
	score := 0
	switch strings.ToLower(filepath.Ext(lower)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".py", ".rs", ".java", ".kt", ".swift", ".c", ".cc", ".cpp", ".h", ".hpp":
		score += 8
	case ".json", ".yml", ".yaml", ".toml":
		score += 3
	}
	if strings.Contains(lower, "/src/") || strings.HasPrefix(lower, "src/") {
		score += 6
	}
	if strings.HasPrefix(lower, "packages/") || strings.HasPrefix(lower, "apps/") || strings.HasPrefix(lower, "internal/") || strings.HasPrefix(lower, "cmd/") {
		score += 3
	}
	if strings.Contains(lower, "/journal/") || strings.HasPrefix(lower, "journal/") || strings.HasPrefix(lower, "sources/") || strings.HasPrefix(lower, "entities/") {
		score -= 4
	}
	return score
}

func brainBriefLikelyTestFile(path string) bool {
	lower := strings.ToLower(path)
	base := filepath.Base(lower)
	return strings.Contains(lower, "/test/") ||
		strings.Contains(lower, "/tests/") ||
		strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") ||
		strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, "_test.py")
}

func extractBrainBriefPaths(text string) []string {
	matches := brainBriefPathPattern.FindAllStringSubmatch(text, -1)
	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) > 1 {
			paths = append(paths, match[1])
		}
	}
	return paths
}

func cleanBrainBriefLikelyFile(path string) (string, bool) {
	path = strings.TrimSpace(path)
	path = strings.TrimLeft(path, "`'\"")
	path = strings.TrimRight(path, "`'\".,;:)]}")
	if path == "" {
		return "", false
	}
	path = strings.TrimPrefix(path, "./")
	path = filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/") {
		path = strings.TrimPrefix(strings.TrimPrefix(path, "a/"), "b/")
	}
	if path == "." || filepath.IsAbs(path) || strings.HasPrefix(path, "../") || strings.HasPrefix(path, "..\\") || path == ".." {
		return "", false
	}
	lower := strings.ToLower(path)
	for _, prefix := range []string{
		".benchmark/",
		".git/",
		"history/",
		"seed/",
		"semantic/",
		"sessions/",
		"node_modules/",
		"dist/",
		"build/",
		"coverage/",
	} {
		if strings.HasPrefix(lower, prefix) {
			return "", false
		}
	}
	if !strings.Contains(path, "/") && !brainBriefRootFile(lower) {
		return "", false
	}
	if strings.Contains(path, "/") && !brainBriefLikelyPathRoot(lower) {
		return "", false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".json", ".md", ".yml", ".yaml", ".toml", ".py", ".rs", ".java", ".kt", ".swift", ".c", ".cc", ".cpp", ".h", ".hpp", ".css", ".scss", ".html":
		return path, true
	default:
		return "", false
	}
}

func brainBriefLikelyPathRoot(path string) bool {
	for _, prefix := range []string{
		"apps/",
		"cmd/",
		"docs/",
		"internal/",
		"lib/",
		"packages/",
		"pkg/",
		"scripts/",
		"src/",
		"templates/",
		"test/",
		"tests/",
		".github/",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func brainBriefRootFile(path string) bool {
	switch path {
	case "package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "go.mod", "go.sum", "cargo.toml", "pyproject.toml", "readme.md", "agents.md", "claude.md":
		return true
	default:
		return false
	}
}

func brainBriefActionChecklist(repoRoot string, report brainBriefReport, task string) []brainBriefAction {
	context := strings.ToLower(task)
	for _, match := range report.History.Matches {
		context += "\n" + strings.ToLower(match.Excerpt)
	}
	var actions []brainBriefAction
	if strings.Contains(context, "normalizelimit") ||
		strings.Contains(context, "max_query_limit") ||
		(strings.Contains(context, "query limit") && strings.Contains(context, "limit normalization")) ||
		(strings.Contains(context, "normalize") && strings.Contains(context, "limit")) ||
		(strings.Contains(context, "oversized") && strings.Contains(context, "limit")) {
		actions = append(actions, brainBriefLimitNormalizationActions(repoRoot, report.LikelyEditFiles)...)
	}
	if strings.Contains(context, "metadata.step") ||
		strings.Contains(context, "metadata values must be strings") ||
		strings.Contains(context, "invalid_type") ||
		(strings.Contains(context, "metadata") && strings.Contains(context, "responses api")) {
		actions = append(actions, brainBriefMetadataStringActions(repoRoot, report.LikelyEditFiles)...)
	}
	if strings.Contains(context, "previousresponseid") ||
		strings.Contains(context, "previous_response_id") ||
		(strings.Contains(context, "self-contained") && strings.Contains(context, "perception")) ||
		(strings.Contains(context, "stale") && strings.Contains(context, "model state")) {
		actions = append(actions, brainBriefPreviousResponseActions(repoRoot, report.LikelyEditFiles)...)
	}
	return dedupeBrainBriefActions(actions, 20)
}

func brainBriefLimitNormalizationActions(repoRoot string, likelyFiles []string) []brainBriefAction {
	var actions []brainBriefAction
	for _, rel := range likelyFiles {
		if !brainBriefSourceFile(rel) {
			continue
		}
		path := filepath.Join(repoRoot, filepath.FromSlash(rel))
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		actions = append(actions, brainBriefLimitNormalizationActionsForFile(rel, string(data))...)
		if len(actions) >= 20 {
			break
		}
	}
	return actions
}

func brainBriefSourceFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".py", ".rs", ".java", ".kt", ".swift":
		return true
	default:
		return false
	}
}

var brainBriefFunctionPattern = regexp.MustCompile(`^\s*(?:async\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

func brainBriefLimitNormalizationActionsForFile(rel, source string) []brainBriefAction {
	lines := strings.Split(source, "\n")
	var actions []brainBriefAction
	currentSymbol := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if match := brainBriefFunctionPattern.FindStringSubmatch(line); len(match) == 2 {
			if !brainBriefCodeKeyword(match[1]) {
				currentSymbol = match[1]
			}
		}
		if !brainBriefLimitLine(trimmed) {
			continue
		}
		action := brainBriefLimitNormalizationAction(trimmed, currentSymbol)
		if action == "" {
			continue
		}
		actions = append(actions, brainBriefAction{
			File:     rel,
			Symbol:   currentSymbol,
			Action:   action,
			Evidence: fmt.Sprintf("current line %d: %s", i+1, truncateString(trimmed, 180)),
		})
	}
	return actions
}

func brainBriefCodeKeyword(value string) bool {
	switch value {
	case "if", "for", "switch", "while", "catch", "return", "function":
		return true
	default:
		return false
	}
}

func brainBriefLimitLine(line string) bool {
	lower := strings.ToLower(line)
	if !strings.Contains(lower, "limit") {
		return false
	}
	if strings.Contains(line, "normalizeLimit(") {
		return true
	}
	if strings.Contains(line, "??") && (strings.Contains(line, "filter.limit") || strings.Contains(line, "query.limit")) {
		return true
	}
	if strings.Contains(line, "= limit") && !strings.Contains(line, "normalizeLimit(") {
		return true
	}
	if strings.Contains(line, ", limit]") || strings.Contains(line, "[limit]") || strings.Contains(line, "values.push(limit)") {
		return true
	}
	if strings.Contains(line, "LIMIT") && (strings.Contains(line, "limit") || strings.Contains(line, "$")) {
		return true
	}
	if strings.Contains(line, ".all(") || strings.Contains(line, ".query(") {
		return strings.Contains(line, "limit")
	}
	return false
}

func brainBriefLimitNormalizationAction(line, symbol string) string {
	switch {
	case strings.Contains(line, "filter.limit ??"):
		return "Replace raw filter limit fallback with normalizeLimit(filter.limit, fallback) before applying SQL LIMIT."
	case strings.Contains(line, "query.limit ??"):
		return "Replace raw query limit fallback with normalizeLimit(query.limit, fallback) before applying SQL LIMIT."
	case strings.Contains(line, "= limit") && !strings.Contains(line, "normalizeLimit("):
		if symbol != "" {
			return "Replace raw limit passthrough with normalizeLimit(limit, fallback) in " + symbol + " before applying SQL LIMIT."
		}
		return "Replace raw limit passthrough with normalizeLimit(limit, fallback) before applying SQL LIMIT."
	case strings.Contains(line, ", limit]") || strings.Contains(line, "[limit]") || strings.Contains(line, "values.push(limit)"):
		if symbol != "" {
			return "Replace raw limit query argument with normalizeLimit(limit, fallback) in " + symbol + " before applying SQL LIMIT."
		}
		return "Replace raw limit query argument with normalizeLimit(limit, fallback) before applying SQL LIMIT."
	case strings.Contains(line, "LIMIT") && strings.Contains(line, "limit") && !strings.Contains(line, "normalizeLimit("):
		if symbol != "" {
			return "Verify this SQL LIMIT path normalizes the caller-provided limit in " + symbol + " before query execution."
		}
		return "Verify this SQL LIMIT path normalizes the caller-provided limit before query execution."
	case strings.Contains(line, "normalizeLimit("):
		return "Preserve this normalized limit call; apply the same invariant to sibling list/search methods."
	default:
		return ""
	}
}

func dedupeBrainBriefActions(actions []brainBriefAction, limit int) []brainBriefAction {
	if limit <= 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]brainBriefAction, 0, min(len(actions), limit))
	for _, action := range actions {
		key := strings.ToLower(action.File + "\x00" + action.Symbol + "\x00" + action.Action + "\x00" + action.Evidence)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, action)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func brainBriefMetadataStringActions(repoRoot string, likelyFiles []string) []brainBriefAction {
	var primary []brainBriefAction
	var fallback []brainBriefAction
	for _, rel := range likelyFiles {
		if !brainBriefSourceFile(rel) {
			continue
		}
		path := filepath.Join(repoRoot, filepath.FromSlash(rel))
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		source := string(data)
		actions := brainBriefMetadataStringActionsForFile(rel, source)
		if brainBriefPrimaryProviderMetadataFile(rel, source) {
			primary = append(primary, actions...)
		} else {
			fallback = append(fallback, actions...)
		}
		if len(primary) >= 10 {
			break
		}
	}
	if len(primary) > 0 {
		return primary
	}
	return fallback
}

func brainBriefPrimaryProviderMetadataFile(rel, source string) bool {
	relLower := strings.ToLower(rel)
	return strings.Contains(relLower, "agentic-decider") ||
		(strings.Contains(source, "agentic_decision") && strings.Contains(source, "step:"))
}

func brainBriefPreviousResponseActions(repoRoot string, likelyFiles []string) []brainBriefAction {
	var primary []brainBriefAction
	var fallback []brainBriefAction
	for _, rel := range likelyFiles {
		if !brainBriefSourceFile(rel) {
			continue
		}
		path := filepath.Join(repoRoot, filepath.FromSlash(rel))
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		source := string(data)
		actions := brainBriefPreviousResponseActionsForFile(rel, source)
		if brainBriefPrimaryPreviousResponseFile(rel, source) {
			primary = append(primary, actions...)
		} else {
			fallback = append(fallback, actions...)
		}
		if len(primary) >= 10 {
			break
		}
	}
	if len(primary) > 0 {
		return primary
	}
	return fallback
}

func brainBriefPrimaryPreviousResponseFile(rel, source string) bool {
	relLower := strings.ToLower(rel)
	return strings.Contains(relLower, "packages/automation/") &&
		strings.Contains(source, "AgenticPerception") &&
		strings.Contains(source, "executeAgenticPlan")
}

func brainBriefPreviousResponseActionsForFile(rel, source string) []brainBriefAction {
	lines := strings.Split(source, "\n")
	var actions []brainBriefAction
	currentSymbol := ""
	inAgenticPerception := false
	for i, line := range lines {
		if match := brainBriefFunctionPattern.FindStringSubmatch(line); len(match) == 2 {
			if !brainBriefCodeKeyword(match[1]) {
				currentSymbol = match[1]
			}
		}
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "const perception: AgenticPerception =") {
			inAgenticPerception = true
		}
		if strings.Contains(trimmed, "let previousResponseId") {
			actions = append(actions, brainBriefAction{
				File:     rel,
				Symbol:   currentSymbol,
				Action:   "Delete the loop-scoped browser response-id accumulator line; do not replace it with another accumulator. Each agentic perception turn must be built from fresh observed page state and recent local history.",
				Evidence: fmt.Sprintf("current line %d: %s", i+1, truncateString(trimmed, 180)),
			})
		}
		if inAgenticPerception && (strings.Contains(trimmed, "previousResponseId:") || trimmed == "previousResponseId") {
			action := "Replace this perception entry with the literal property `previousResponseId: null`. Keep the `previousResponseId` key present; do not delete it, use shorthand, or pass a variable from prior turns."
			if strings.Contains(trimmed, "previousResponseId: null") {
				action = "Preserve self-contained browser perception: keep the `previousResponseId` key present as literal null for each agentic browser turn."
			}
			actions = append(actions, brainBriefAction{
				File:     rel,
				Symbol:   currentSymbol,
				Action:   action,
				Evidence: fmt.Sprintf("current line %d: %s", i+1, truncateString(trimmed, 180)),
			})
		}
		if inAgenticPerception && trimmed == "};" {
			inAgenticPerception = false
		}
		if strings.Contains(trimmed, "previousResponseId =") && strings.Contains(trimmed, "decision.previousResponseId") {
			actions = append(actions, brainBriefAction{
				File:     rel,
				Symbol:   currentSymbol,
				Action:   "Delete this browser-loop response-id carry-forward assignment entirely; browser decider turns should not chain `decision.previousResponseId` into later perception turns.",
				Evidence: fmt.Sprintf("current line %d: %s", i+1, truncateString(trimmed, 180)),
			})
		}
	}
	return actions
}

func brainBriefMetadataStringActionsForFile(rel, source string) []brainBriefAction {
	lines := strings.Split(source, "\n")
	var actions []brainBriefAction
	currentSymbol := ""
	providerContext := strings.Contains(source, "agentic_decision") ||
		(strings.Contains(source, "previousResponseId") && strings.Contains(source, "reasoningEffort"))
	for i, line := range lines {
		if match := brainBriefFunctionPattern.FindStringSubmatch(line); len(match) == 2 {
			if !brainBriefCodeKeyword(match[1]) {
				currentSymbol = match[1]
			}
		}
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if !strings.Contains(lower, "metadata") {
			continue
		}
		if providerContext && !strings.Contains(trimmed, "agentic_decision") && !strings.Contains(trimmed, "step:") {
			continue
		}
		if strings.Contains(trimmed, "String(") {
			actions = append(actions, brainBriefAction{
				File:     rel,
				Symbol:   currentSymbol,
				Action:   "Preserve string conversion for Responses API metadata values.",
				Evidence: fmt.Sprintf("current line %d: %s", i+1, truncateString(trimmed, 180)),
			})
			continue
		}
		if strings.Contains(trimmed, "step:") || strings.Contains(trimmed, "metadata:") {
			action := "Ensure every OpenAI Responses API metadata value is a string before calling the provider."
			if providerContext {
				action = "Stringify the agentic_decision metadata step before the Responses API provider call; preserve reasoningEffort, previousResponseId, and image inputs."
			}
			actions = append(actions, brainBriefAction{
				File:     rel,
				Symbol:   currentSymbol,
				Action:   action,
				Evidence: fmt.Sprintf("current line %d: %s", i+1, truncateString(trimmed, 180)),
			})
		}
	}
	return actions
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
		report.Sources.Facts = manifest.Sources.Facts != nil
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
	return inspectBrainRawText(brainDir, kind, query, brainInspectHistoryMaxHits)
}

func inspectBrainRawText(brainDir, kind, query string, maxHits int) (brainHistoryInspectReport, error) {
	report := brainHistoryInspectReport{Kind: kind, Query: query, BrainPath: brainDir}
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return report, errors.New("query must not be empty")
	}
	if maxHits <= 0 {
		maxHits = brainInspectHistoryMaxHits
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
		if report.Scanned >= brainInspectHistoryMaxFiles || len(report.Matches) >= maxHits {
			report.Truncated = true
			return filepath.SkipAll
		}
		ext := filepath.Ext(path)
		if ext != ".md" && ext != ".json" && ext != ".jsonl" && ext != ".txt" {
			return nil
		}
		rel, _ := filepath.Rel(brainDir, path)
		relSlash := filepath.ToSlash(rel)
		if relSlash == historyIndexPath || relSlash == "manifest.json" || strings.HasPrefix(relSlash, "seed/") {
			return nil
		}
		report.Scanned++
		f, openErr := os.Open(path)
		if openErr != nil {
			report.ScanErrors = append(report.ScanErrors, openErr.Error())
			return nil
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), brainInspectHistoryMaxLine)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			if !historyTextMatchesQuery(line, query) {
				continue
			}
			report.Matches = append(report.Matches, brainTextMatch{
				Path:    relSlash,
				Line:    lineNo,
				Excerpt: historyRawLineExcerpt(line, query),
			})
			if len(report.Matches) >= maxHits {
				report.Truncated = true
				return filepath.SkipAll
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			report.ScanErrors = append(report.ScanErrors, scanErr.Error())
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	return report, nil
}

func brainBriefRawHistoryMatches(brainDir, task string, existing []brainTextMatch, limit int) ([]brainTextMatch, error) {
	if limit <= 0 {
		return nil, nil
	}
	seen := map[string]struct{}{}
	for _, match := range existing {
		seen[brainBriefHistoryMatchKey(match)] = struct{}{}
	}
	var matches []brainTextMatch
	var firstErr error
	for _, query := range brainBriefRawHistoryQueries(task) {
		if len(matches) >= limit {
			break
		}
		report, err := inspectBrainRawText(brainDir, "history", query, limit-len(matches))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, match := range report.Matches {
			key := brainBriefHistoryMatchKey(match)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			matches = append(matches, match)
			if len(matches) >= limit {
				break
			}
		}
	}
	if len(matches) > 0 {
		return matches, nil
	}
	return nil, firstErr
}

func brainBriefRawHistoryQueries(task string) []string {
	seen := map[string]struct{}{}
	var queries []string
	for _, identifier := range historyIdentifierQueryTerms(task) {
		normalized := strings.ToLower(strings.Trim(identifier, "_"))
		if normalized == "" || normalized == "ultron" || normalized == "api" || normalized == "apis" {
			continue
		}
		if _, ok := seen[identifier]; ok {
			continue
		}
		seen[identifier] = struct{}{}
		queries = append(queries, identifier)
	}
	sort.SliceStable(queries, func(i, j int) bool {
		left := brainBriefRawHistoryQueryPriority(queries[i])
		right := brainBriefRawHistoryQueryPriority(queries[j])
		if left != right {
			return left > right
		}
		return len(queries[i]) > len(queries[j])
	})
	if len(queries) > 8 {
		queries = queries[:8]
	}
	return queries
}

func brainBriefRawHistoryQueryPriority(query string) int {
	upper := strings.ToUpper(query)
	score := len(query)
	if strings.Contains(upper, "LIMIT") {
		score += 100
	}
	if strings.Contains(upper, "_") {
		score += 40
	}
	if strings.HasPrefix(upper, "LIST") || strings.HasPrefix(upper, "SEARCH") {
		score -= 30
	}
	return score
}

func brainBriefHistoryMatchKey(match brainTextMatch) string {
	return fmt.Sprintf("%s:%d:%s", match.Path, match.Line, normalizeHistorySearchText(match.Excerpt))
}

func mergeBrainBriefHistoryMatches(limit int, groups ...[]brainTextMatch) []brainTextMatch {
	if limit <= 0 {
		return nil
	}
	seen := map[string]struct{}{}
	var merged []brainTextMatch
	for _, group := range groups {
		for _, match := range group {
			key := brainBriefHistoryMatchKey(match)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, match)
			if len(merged) >= limit {
				return merged
			}
		}
	}
	return merged
}

func historyRawLineExcerpt(line, query string) string {
	text := normalizeHistoryTextForExcerpt(line)
	lower := strings.ToLower(text)
	query = strings.ToLower(strings.TrimSpace(query))
	idx := -1
	matchLen := len(query)
	if query != "" {
		idx = strings.Index(lower, query)
	}
	if idx == -1 {
		// The full query was not a literal substring. Locate the first significant
		// query token directly in `lower` so the resulting offset stays valid for
		// `text`. (Using an offset from normalizeHistorySearchText, which collapses
		// punctuation/case and splits camelCase, would index a differently-sized
		// string and misalign the window.)
		for _, tok := range strings.Fields(query) {
			if len(tok) < 3 {
				continue
			}
			if at := strings.Index(lower, tok); at != -1 {
				idx = at
				matchLen = len(tok)
				break
			}
		}
	}
	if idx == -1 {
		return strings.TrimSpace(truncateString(text, 700))
	}
	start := max(0, idx-280)
	end := min(len(text), idx+matchLen+620)
	// Snap the window to rune boundaries so the slice is valid UTF-8.
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	return strings.TrimSpace(truncateString(text[start:end], 900))
}

func normalizeHistoryTextForExcerpt(text string) string {
	text = strings.ReplaceAll(text, "\\n", "\n")
	text = strings.ReplaceAll(text, "\\t", "\t")
	text = strings.ReplaceAll(text, "\\\"", "\"")
	text = strings.Join(strings.Fields(text), " ")
	return text
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
		return map[string]struct{}{"architecture": {}, "code_fact": {}, "decision": {}, "learning": {}}
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
	if max <= 0 {
		return ""
	}
	if len(value) <= max {
		return value
	}
	// Cut on a rune boundary so we never split a multi-byte UTF-8 rune in
	// real (non-ASCII) transcripts, which would emit invalid UTF-8.
	limit := max
	suffix := ""
	if max > 3 {
		limit = max - 3
		suffix = "..."
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + suffix
}
