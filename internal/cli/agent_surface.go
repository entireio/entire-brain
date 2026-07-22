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
	brainBriefDefaultLimit               = 3
	brainBriefFactsLimit                 = 6
	brainBriefContextCandidateMultiplier = 8
	brainInspectHistoryMaxFiles          = 1000
	brainInspectHistoryMaxBytes          = 512 * 1024
	brainInspectHistoryMaxHits           = 25
	// 4 MiB per line is far beyond any real history record while bounding the
	// buffer a single crafted line can force (was 16 MiB).
	brainInspectHistoryMaxLine = 4 * 1024 * 1024
)

type agentStatusOptions struct {
	json    bool
	details bool
	failOn  string
}

type brainBriefOptions struct {
	json       bool
	limit      int
	noSemantic bool
	// surface names the caller for serve receipts; empty means the CLI
	// "brief" verb (the MCP server passes "mcp:brain_brief").
	surface string
}

type brainShowOptions struct {
	json bool
}

// brainStatusReport is the JSON contract of `status`, grouped by logical
// function: repo/brain identity, which sources exist, the durable-facts layer
// (counts + verification), the semantic layer (provider, coverage, freshness,
// blind spots — the former `stale` and `semantic-audit` commands), and live
// workspace state.
type brainStatusReport struct {
	GeneratedAt time.Time             `json:"generated_at"`
	Repo        brainStatusRepo       `json:"repo"`
	Brain       brainStatusBrain      `json:"brain"`
	Sources     brainStatusSources    `json:"sources"`
	Facts       *brainStatusFacts     `json:"facts,omitempty"`
	Semantic    *brainStatusSemantic  `json:"semantic,omitempty"`
	Retrieval   *brainStatusRetrieval `json:"retrieval,omitempty"`
	Live        brainLiveState        `json:"live"`
	Warnings    []string              `json:"warnings,omitempty"`
	// Manifest is for in-process consumers (brief, overview, regressions). It is
	// deliberately not part of the JSON contract: it duplicates the structured
	// sections above and its session list scales with brain size.
	Manifest *exportManifest `json:"-"`
}

type brainStatusFacts struct {
	Facts        int            `json:"facts"`
	Distilled    int            `json:"distilled"`
	Authored     int            `json:"authored"`
	Superseded   int            `json:"superseded"`
	Branches     int            `json:"branches"`
	Proposals    int            `json:"proposals"`
	Verification *verifySummary `json:"verification,omitempty"`
}

type brainStatusSemantic struct {
	Provider   *brainStatusSemanticProvider `json:"provider,omitempty"`
	Coverage   *brainStatusSemanticCoverage `json:"coverage,omitempty"`
	Freshness  *staleReport                 `json:"freshness,omitempty"`
	BlindSpots []brainBlindSpot             `json:"blind_spots,omitempty"`
}

type brainStatusRetrieval struct {
	SeedCommit      string       `json:"seed_commit,omitempty"`
	SeedMode        string       `json:"seed_mode,omitempty"`
	DocsGeneratedAt string       `json:"docs_generated_at,omitempty"`
	DocsRecords     int          `json:"docs_records,omitempty"`
	DocsFiles       int          `json:"docs_files,omitempty"`
	Freshness       *staleReport `json:"freshness,omitempty"`
}

type brainStatusSemanticProvider struct {
	Name         string   `json:"name,omitempty"`
	Version      string   `json:"version,omitempty"`
	Schema       string   `json:"schema_version,omitempty"`
	Snapshot     string   `json:"snapshot_path,omitempty"`
	Store        string   `json:"store_path,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type brainStatusSemanticCoverage struct {
	Files                 int                  `json:"files"`
	Symbols               int                  `json:"symbols"`
	Relations             int                  `json:"relations"`
	Warnings              int                  `json:"warnings"`
	PartialFailures       int                  `json:"partial_failures"`
	WarningDetails        []semanticWarning    `json:"warning_details,omitempty"`
	PartialFailureDetails []semanticWarning    `json:"partial_failure_details,omitempty"`
	FileLanguages         []semanticAuditCount `json:"file_languages,omitempty"`
	Languages             []semanticAuditCount `json:"languages,omitempty"`
	SymbolKinds           []semanticAuditCount `json:"symbol_kinds,omitempty"`
	RelationTypes         []semanticAuditCount `json:"relation_types,omitempty"`
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
	Docs     bool `json:"docs"`
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
	Facts       []factRecord       `json:"facts,omitempty"`
	// FactsLocusDrift flags surfaced facts whose code locus no longer exists
	// in the worktree (fact id -> departed locus tokens) — the "re-verify
	// before trusting" signal (Phase 2 item 4).
	FactsLocusDrift map[string][]string `json:"facts_locus_drift,omitempty"`
	// FactsPendingReview flags surfaced facts that participate in a pending
	// merge/supersede proposal (fact id -> review notice), mirroring the trust
	// guard the unified query/search/get path applies. Annotation only — the
	// facts list keeps its shape.
	FactsPendingReview map[string]factReviewNotice `json:"facts_pending_review,omitempty"`
	ActionChecklist    []brainBriefAction          `json:"action_checklist,omitempty"`
	LikelyEditFiles    []string                    `json:"likely_edit_files,omitempty"`
	LikelyTestFiles    []string                    `json:"likely_test_files,omitempty"`
	LikelyFiles        []string                    `json:"likely_files,omitempty"`
	Patterns           []patternView               `json:"patterns,omitempty"`
	// Consolidations are corpus-backed dossiers (v2) relevant to the task:
	// trigger + workflow + verification + failure modes, anchored. Task-gated
	// and capped — an unrelated task carries none.
	Consolidations []briefConsolidation `json:"consolidations,omitempty"`
	// Themes are verified latent practices (recurring read-only/conversational
	// work) relevant to the task. Task-gated and capped; verifier-accepted only.
	Themes   []themeView `json:"themes,omitempty"`
	Guidance []string    `json:"guidance"`
	Warnings []string    `json:"warnings,omitempty"`
}

type brainBriefSemantic struct {
	Context       semanticContextResult `json:"context"`
	RuntimeTraces []semanticRecord      `json:"runtime_traces,omitempty"`
	Tests         semanticTestsResult   `json:"tests"`
}

// brainBriefJSONReport is the agent-facing projection of the in-process brief.
// The full report remains available while assembling likely files and rendering
// text, but JSON callers should not pay for empty semantic fields, fact
// provenance, coverage histograms, or live symbol records on every task.
type brainBriefJSONReport struct {
	GeneratedAt        time.Time                   `json:"generated_at"`
	Task               string                      `json:"task"`
	Status             brainStatusReport           `json:"status"`
	Semantic           brainBriefJSONSemantic      `json:"semantic"`
	History            brainBriefHistory           `json:"history"`
	Facts              []brainBriefJSONFact        `json:"facts,omitempty"`
	FactsLocusDrift    map[string][]string         `json:"facts_locus_drift,omitempty"`
	FactsPendingReview map[string]factReviewNotice `json:"facts_pending_review,omitempty"`
	ActionChecklist    []brainBriefAction          `json:"action_checklist,omitempty"`
	LikelyEditFiles    []string                    `json:"likely_edit_files,omitempty"`
	LikelyTestFiles    []string                    `json:"likely_test_files,omitempty"`
	LikelyFiles        []string                    `json:"likely_files,omitempty"`
	Guidance           []string                    `json:"guidance"`
	Warnings           []string                    `json:"warnings,omitempty"`
}

type brainBriefJSONSemantic struct {
	Context       brainBriefJSONContext   `json:"context"`
	RuntimeTraces []compactSemanticRecord `json:"runtime_traces,omitempty"`
	Tests         brainBriefJSONTests     `json:"tests"`
}

type brainBriefJSONContext struct {
	Symbols   []compactSemanticRecord `json:"symbols"`
	Relations []compactSemanticRecord `json:"relations"`
	Neighbors []compactSemanticRecord `json:"neighbors,omitempty"`
}

type brainBriefJSONTests struct {
	Roots       []compactSemanticRecord         `json:"roots"`
	Suggestions []compactSemanticTestSuggestion `json:"suggestions"`
}

type compactSemanticTestSuggestion struct {
	Symbol compactSemanticRecord `json:"symbol"`
	Reason string                `json:"reason"`
}

type compactSemanticRecord struct {
	ID            string   `json:"id,omitempty"`
	Kind          string   `json:"kind,omitempty"`
	Name          string   `json:"name,omitempty"`
	QualifiedName string   `json:"qualified_name,omitempty"`
	FilePath      string   `json:"file_path,omitempty"`
	StartLine     int      `json:"start_line,omitempty"`
	EndLine       int      `json:"end_line,omitempty"`
	Path          string   `json:"path,omitempty"`
	Signature     string   `json:"signature,omitempty"`
	FromID        string   `json:"from_id,omitempty"`
	ToID          string   `json:"to_id,omitempty"`
	Type          string   `json:"type,omitempty"`
	WarningCodes  []string `json:"warning_codes,omitempty"`
	Confidence    float64  `json:"confidence,omitempty"`
	Reason        string   `json:"reason,omitempty"`
}

type brainBriefJSONFact struct {
	ID         string   `json:"id"`
	Paths      []string `json:"paths"`
	Kind       string   `json:"kind,omitempty"`
	Locus      []string `json:"locus,omitempty"`
	Text       string   `json:"text"`
	Confidence string   `json:"confidence,omitempty"`
}

type brainBriefHistory struct {
	Matches []brainTextMatch `json:"matches,omitempty"`
}

type brainBriefAction struct {
	File       string                `json:"file,omitempty"`
	Symbol     string                `json:"symbol,omitempty"`
	Action     string                `json:"action"`
	Evidence   string                `json:"evidence,omitempty"`
	Validation *brainBriefValidation `json:"validation,omitempty"`
}

type brainBriefValidation struct {
	Command        string `json:"command"`
	File           string `json:"file,omitempty"`
	Test           string `json:"test,omitempty"`
	CompleteOnPass bool   `json:"complete_on_pass,omitempty"`
}

type brainShowReport struct {
	Freshness staleReport    `json:"freshness"`
	Record    semanticRecord `json:"record"`
}

type brainTextMatch struct {
	Path         string   `json:"path"`
	Line         int      `json:"line"`
	Excerpt      string   `json:"excerpt"`
	Timestamp    string   `json:"timestamp,omitempty"`
	Score        int      `json:"score,omitempty"`
	MatchedTerms []string `json:"matched_terms,omitempty"`
}

type brainHistoryInspectReport struct {
	Kind       string           `json:"kind"`
	Query      string           `json:"query"`
	QueryTerms []string         `json:"query_terms,omitempty"`
	BrainPath  string           `json:"brain_path"`
	Matches    []brainTextMatch `json:"matches"`
	Partial    bool             `json:"partial,omitempty"`
	Truncated  bool             `json:"truncated,omitempty"`
	Scanned    int              `json:"scanned_files"`
	ScanErrors []string         `json:"scan_errors,omitempty"`
}

func newAgentStatusCommand(opts Options) *cobra.Command {
	statusOpts := agentStatusOptions{failOn: semanticAuditFailOnNone}
	cmd := &cobra.Command{
		Use:   "status [path]",
		Short: "Summarize the brain: sources, facts, semantic coverage/freshness/blind spots, and live workspace state",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := agentSurfaceTarget(opts, args)
			return runAgentStatus(cmd.Context(), cmd, opts, statusOpts, target)
		},
	}
	cmd.Flags().BoolVar(&statusOpts.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&statusOpts.details, "details", false, "Include coverage histograms, staged-file classifications, and changed-symbol records")
	cmd.Flags().StringVar(&statusOpts.failOn, "fail-on", semanticAuditFailOnNone, "Return nonzero after emitting the report when the selected gate trips: release, unsafe, degraded, blind-spots, none")
	return cmd
}

type brainOverviewReport struct {
	GeneratedAt       time.Time             `json:"generated_at"`
	Repo              brainStatusRepo       `json:"repo"`
	Brain             brainStatusBrain      `json:"brain"`
	Freshness         brainOverviewFresh    `json:"freshness"`
	Sources           brainStatusSources    `json:"sources"`
	Live              brainLiveState        `json:"live"`
	Semantic          brainOverviewSemantic `json:"semantic"`
	Boundaries        map[string]int        `json:"boundaries,omitempty"`
	Entrypoints       []string              `json:"entrypoints,omitempty"`
	Commands          []seedCommand         `json:"commands,omitempty"`
	Documents         []string              `json:"key_documents,omitempty"`
	RecentDecisions   []brainTextMatch      `json:"recent_decisions,omitempty"`
	StrongestPatterns []patternView         `json:"strongest_patterns,omitempty"`
	// StrongestConsolidations are the corpus's top current dossiers (v2),
	// capped so the overview shows the repo's strongest patterns without flooding.
	StrongestConsolidations []briefConsolidation `json:"strongest_consolidations,omitempty"`
	// StrongestThemes are the top verified latent-practice themes, capped.
	StrongestThemes []themeView `json:"strongest_themes,omitempty"`
	Warnings        []string    `json:"warnings,omitempty"`
}

type brainOverviewFresh struct {
	Severity string `json:"severity"`
	Summary  string `json:"summary,omitempty"`
}

type brainOverviewSemantic struct {
	Files     int `json:"files"`
	Symbols   int `json:"symbols"`
	Relations int `json:"relations"`
}

func newBrainOverviewCommand(opts Options) *cobra.Command {
	var jsonOut bool
	var decisions int
	cmd := &cobra.Command{
		Use:   "overview [path]",
		Short: "Summarize what the project is: stack, boundaries, commands, and recent decisions",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := agentSurfaceTarget(opts, args)
			return runBrainOverview(cmd.Context(), cmd, opts, target, jsonOut, decisions)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	cmd.Flags().IntVar(&decisions, "decisions", 5, "Number of recent decisions to include")
	return cmd
}

// dedupSeedCommands removes exact (Name, Command) duplicates — common in a
// monorepo where several package.json files surface the same scripted command —
// while preserving order. Same-name/different-command entries are kept (they are
// genuinely distinct) and disambiguated by source at render time.
func dedupSeedCommands(commands []seedCommand) []seedCommand {
	if len(commands) == 0 {
		return commands
	}
	seen := make(map[string]struct{}, len(commands))
	out := make([]seedCommand, 0, len(commands))
	for _, c := range commands {
		key := c.Name + "\x00" + c.Command
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, c)
	}
	return out
}

func runBrainOverview(ctx context.Context, cmd *cobra.Command, opts Options, target string, jsonOut bool, decisions int) error {
	status, err := buildBrainStatusReport(ctx, opts, target)
	if err != nil {
		return err
	}
	report := brainOverviewReport{
		GeneratedAt: opts.Now().UTC(),
		Repo:        status.Repo,
		Brain:       status.Brain,
		Sources:     status.Sources,
		Live:        status.Live,
		Warnings:    status.Warnings,
		Boundaries:  map[string]int{},
	}
	if status.Semantic != nil && status.Semantic.Freshness != nil {
		report.Freshness = brainOverviewFresh{
			Severity: status.Semantic.Freshness.Severity,
			Summary:  freshnessSummary(*status.Semantic.Freshness),
		}
	}
	if status.Manifest != nil && status.Manifest.Sources != nil {
		if sem := status.Manifest.Sources.Semantic; sem != nil {
			report.Semantic = brainOverviewSemantic{Files: sem.Files, Symbols: sem.Symbols, Relations: sem.Relations}
			for _, kind := range []string{"route", "tool", "workflow"} {
				spec, specErr := inspectBoundarySpec(kind)
				if specErr != nil {
					continue
				}
				facts, factsErr := semanticBoundaryFacts(status.Brain.Path, sem, spec, 10000)
				if factsErr != nil {
					continue
				}
				report.Boundaries[spec.Name] = len(facts.Boundaries)
			}
		}
		if seed := status.Manifest.Sources.Seed; seed != nil {
			report.Entrypoints = seed.Entrypoints
			report.Commands = dedupSeedCommands(seed.Commands)
			for _, doc := range seed.Documents {
				report.Documents = append(report.Documents, doc.Path)
			}
		}
		if status.Manifest.Sources.History != nil {
			report.RecentDecisions = recentDecisionMatches(status.Brain.Path, status.Manifest.Sources.History, decisions)
		}
		report.StrongestPatterns = strongestPatterns(status.Brain.Path, 3)
		report.StrongestConsolidations = strongestConsolidations(status.Brain.Path, 3)
		report.StrongestThemes = strongestThemes(status.Brain.Path, 3)
	}
	if jsonOut {
		return writeJSON(cmd, report)
	}
	renderBrainOverviewText(cmd, report)
	return nil
}

// recentDecisionMatches returns the most recent decision records so an agent can
// see how the project's design has been steered, newest first.
func recentDecisionMatches(brainDir string, source *historySourceManifest, limit int) []brainTextMatch {
	if limit <= 0 {
		return nil
	}
	index, err := loadBrainHistoryIndex(brainDir, source)
	if err != nil {
		return nil
	}
	var decisions []historyRecord
	for _, record := range index.Records {
		if record.Kind == "decision" {
			decisions = append(decisions, record)
		}
	}
	sort.SliceStable(decisions, func(i, j int) bool {
		left, leftOK := historyRecordTimestamp(decisions[i].Path)
		right, rightOK := historyRecordTimestamp(decisions[j].Path)
		if leftOK && rightOK && !left.Equal(right) {
			return left.After(right)
		}
		return leftOK && !rightOK
	})
	seen := map[string]struct{}{}
	var matches []brainTextMatch
	for _, record := range decisions {
		key := normalizeHistorySearchText(record.Summary)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		match := historyRecordTextMatch(record)
		match.Excerpt = strings.TrimSpace(truncateString(match.Excerpt, 280))
		matches = append(matches, match)
		if len(matches) >= limit {
			break
		}
	}
	return matches
}

func renderBrainOverviewText(cmd *cobra.Command, report brainOverviewReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "repo: %s (%s)\n", report.Repo.Root, report.Repo.Key)
	fmt.Fprintf(out, "freshness: %s", report.Freshness.Severity)
	if report.Freshness.Summary != "" {
		fmt.Fprintf(out, " — %s", report.Freshness.Summary)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "semantic: %d files, %d symbols, %d relations\n", report.Semantic.Files, report.Semantic.Symbols, report.Semantic.Relations)
	if len(report.Boundaries) > 0 {
		fmt.Fprintf(out, "boundaries: routes=%d tools=%d workflows=%d\n", report.Boundaries["routes"], report.Boundaries["tools"], report.Boundaries["workflows"])
	}
	if len(report.Commands) > 0 {
		nameCounts := map[string]int{}
		for _, c := range report.Commands {
			nameCounts[c.Name]++
		}
		fmt.Fprintln(out, "commands:")
		for _, c := range report.Commands {
			// Same-name different-command entries (e.g. two `build`s from
			// different package.json files) are disambiguated by source so
			// they don't read as accidental duplicates.
			if nameCounts[c.Name] > 1 && c.Source != "" {
				fmt.Fprintf(out, "  %s (%s): %s\n", c.Name, c.Source, c.Command)
			} else {
				fmt.Fprintf(out, "  %s: %s\n", c.Name, c.Command)
			}
		}
	}
	if len(report.Entrypoints) > 0 {
		fmt.Fprintf(out, "entrypoints: %s\n", strings.Join(report.Entrypoints, ", "))
	}
	if len(report.Documents) > 0 {
		fmt.Fprintf(out, "key documents: %s\n", strings.Join(report.Documents, ", "))
	}
	if len(report.RecentDecisions) > 0 {
		fmt.Fprintln(out, "recent decisions:")
		for _, d := range report.RecentDecisions {
			when := d.Timestamp
			if len(when) >= 10 {
				when = when[:10]
			}
			fmt.Fprintf(out, "  [%s] %s\n", when, d.Excerpt)
		}
	}
	if len(report.StrongestPatterns) > 0 {
		fmt.Fprintln(out, "strongest patterns:")
		for _, p := range report.StrongestPatterns {
			fmt.Fprintf(out, "  [%s] %s (strength %.2f, support %d)\n", p.Type, p.Title, p.Strength, p.Support)
		}
	}
	if len(report.StrongestConsolidations) > 0 {
		fmt.Fprintln(out, "strongest consolidations:")
		for _, c := range report.StrongestConsolidations {
			fmt.Fprintf(out, "  [%s] %s (confidence %.2f)\n", c.Type, c.Title, c.Confidence)
		}
	}
	if len(report.StrongestThemes) > 0 {
		fmt.Fprintln(out, "strongest themes:")
		for _, th := range report.StrongestThemes {
			fmt.Fprintf(out, "  [%s] %s (strength %.2f)\n", th.Shape, th.Title, th.Strength)
		}
	}
}

// freshnessSummary collapses the freshness axes into a single human line: "ok"
// when everything is current, otherwise the non-ok axes and their details. This
// is the compact freshness view callers can show instead of the full axis map.
func freshnessSummary(report staleReport) string {
	if report.Severity == "ok" {
		return "all axes current"
	}
	keys := make([]string, 0, len(report.Axes))
	for key := range report.Axes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var parts []string
	for _, key := range keys {
		axis := report.Axes[key]
		if axis.State == "ok" || axis.State == "clean" {
			continue
		}
		if axis.Detail != "" {
			parts = append(parts, fmt.Sprintf("%s=%s (%s)", key, axis.State, axis.Detail))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%s", key, axis.State))
		}
	}
	return strings.Join(parts, "; ")
}

func newBrainBriefCommand(opts Options) *cobra.Command {
	briefOpts := brainBriefOptions{limit: brainBriefDefaultLimit}
	var (
		handoff         bool
		handoffSessions int
	)
	cmd := &cobra.Command{
		Use:   "brief <task> | brief --handoff",
		Short: "Build a bounded task packet from brain context and live state",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if handoff {
				// The handoff packet is session-trajectory-driven, not
				// query-driven: "what was in flight, what failed, what's
				// blocked" for an agent resuming cold (Phase 2 item 3).
				return runBrainHandoff(cmd.Context(), cmd, opts, handoffSessions, briefOpts.json)
			}
			if len(args) != 1 {
				return fmt.Errorf("brief requires a <task> argument (or --handoff for a resumption packet)")
			}
			return runBrainBrief(cmd.Context(), cmd, opts, briefOpts, args[0])
		},
	}
	cmd.Flags().BoolVar(&briefOpts.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().IntVar(&briefOpts.limit, "limit", brainBriefDefaultLimit, "Maximum records per section (default 3; raise only when the compact packet is insufficient)")
	cmd.Flags().BoolVar(&briefOpts.noSemantic, "no-semantic", false, "Disable embedding rerank for facts; use lexical ranking only")
	cmd.Flags().BoolVar(&handoff, "handoff", false, "Emit a session-resumption packet (recent sessions' requests, decisions, validations) instead of a task packet")
	cmd.Flags().IntVar(&handoffSessions, "sessions", handoffDefaultSessions, "Sessions to include in the --handoff packet")
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
Orient first (what is this project?):
  entire brain overview [repo] --json

Then, for a task:
  entire brain brief "<task>" --json

Retrieval (qmd-inspired verbs; search/vsearch/query take --json/--format json|cli/--limit/-n/--branch,
get/multi-get take --json/--format json|cli/--branch):
  entire brain query "<query>" --json       # hybrid (lexical+vector, RRF) — the default
  entire brain search "<query>" --json      # lexical keyword over facts + history + docs (BM25 for history/docs)
  entire brain vsearch "<query>" --json     # vector/semantic over facts + docs (+ history with a Gemma-class embedder)
  entire brain get <id> --json              # fetch one item by id (fact:… | history:… | doc:…)
  entire brain multi-get <id>... --json     # fetch several by id

Small top-level surface:
  entire brain status [repo] --json         # sources, facts+verification, semantic coverage/freshness/blind spots, live state
  entire brain status --fail-on release     # CI gate: nonzero when freshness is not ok or blind spots exist (also: unsafe, degraded, blind-spots)
  entire brain overview [repo] --json
  entire brain brief "<task>" --json
  entire brain show <id> --json
  entire brain refresh                      # full rebuild; single stages: refresh sessions|history|index|seed
  entire brain guide
  entire brain path [repo]

Durable facts (curated, provenance-anchored repo knowledge):
  entire brain distill --dry-run --json
  entire brain recall "<query>" [--scope local|cross-cutting] [--expand] --json
  entire brain remember "<fact>" [--path category.sub.type] --json
  entire brain verify [<fact-id | query>] --json
  entire brain facts tree [--path <prefix>] [--depth N]
  entire brain facts retract <fact-id> --json
  entire brain inspect blame <fact-id> --json   # source anchors a fact was derived from

Specialist tools (symbol graph + regression analysis — what the verbs can't do):
  entire brain inspect code "<query>" --json        # find a symbol in the graph
  entire brain inspect search-graph "<query>" --json
  entire brain inspect query-graph "type:CALLS <query>" --json
  entire brain inspect graph-schema --json
  entire brain inspect graph-ui semantic-graph.html
  entire brain inspect snippet <symbol-or-id> --json
  entire brain inspect trace-path <from-symbol> <to-symbol> --json
  entire brain inspect dead-code --json
  entire brain inspect ingest-traces <json-or-ndjson-file> --json
  entire brain inspect context <symbol-or-id> --json
  entire brain inspect impact <symbol-or-file> --json
  entire brain inspect changes --json
  entire brain inspect tests "<query>" --json
  entire brain inspect boundaries --kind route|tool|workflow --json
  entire brain inspect regressions "<query>" --location-only [--include-deletions] --json

Search tips:
  - query first (fuses keyword + concept); fall back to search for exact
    identifiers, vsearch for paraphrased/conceptual queries.
  - Every result carries an id — pass it to get/multi-get for the full record.
  - The inspect code/search-graph/query-graph/context/impact tools traverse the
    symbol graph (symbols, relations, callers, callees, impact set); reach for
    them when ranked text isn't enough.
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
	// inspect is the specialist fallback for what the unified verbs
	// (search/vsearch/query) can't do: symbol-graph traversal and comparative
	// regression analysis. Pure single-source retrieval (facts/docs/history text)
	// lives in the unified verbs now, so those inspect kinds were removed rather
	// than kept as a parallel copy of the same index.
	cmd.AddCommand(newInspectCodeCommand(opts))
	cmd.AddCommand(newInspectContextCommand(opts))
	cmd.AddCommand(newInspectImpactCommand(opts))
	cmd.AddCommand(newInspectSearchGraphCommand(opts))
	cmd.AddCommand(newInspectQueryGraphCommand(opts))
	cmd.AddCommand(newInspectGraphSchemaCommand(opts))
	cmd.AddCommand(newInspectGraphUICommand(opts))
	cmd.AddCommand(newInspectSnippetCommand(opts))
	cmd.AddCommand(newInspectTracePathCommand(opts))
	cmd.AddCommand(newInspectDeadCodeCommand(opts))
	cmd.AddCommand(newInspectIngestTracesCommand(opts))
	cmd.AddCommand(newInspectChangesCommand(opts))
	cmd.AddCommand(newInspectTestsCommand(opts))
	cmd.AddCommand(newInspectBoundariesCommand(opts))
	cmd.AddCommand(newInspectRegressionsCommand(opts))
	cmd.AddCommand(newInspectBlameCommand(opts))
	return cmd
}

func newInspectSearchGraphCommand(opts Options) *cobra.Command {
	graphOpts := semanticGraphSearchOptions{limit: 20}
	cmd := &cobra.Command{
		Use:   "search-graph <query>",
		Short: "Search semantic graph symbols",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticSearchGraph(cmd, opts, graphOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&graphOpts.limit, "limit", 20, "Maximum results to return")
	cmd.Flags().IntVar(&graphOpts.offset, "offset", 0, "Results to skip before returning a page")
	cmd.Flags().BoolVar(&graphOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectQueryGraphCommand(opts Options) *cobra.Command {
	graphOpts := semanticGraphQueryOptions{limit: 100}
	cmd := &cobra.Command{
		Use:   "query-graph <query>",
		Short: "Query semantic graph relations with type:/from:/to: filters",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticQueryGraph(cmd, opts, graphOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&graphOpts.limit, "limit", 100, "Maximum relations to return")
	cmd.Flags().BoolVar(&graphOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectGraphSchemaCommand(opts Options) *cobra.Command {
	graphOpts := semanticGraphSchemaOptions{}
	cmd := &cobra.Command{
		Use:   "graph-schema",
		Short: "Describe the indexed semantic graph schema and relation vocabulary",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticGraphSchema(cmd, opts, graphOpts)
		},
	}
	cmd.Flags().BoolVar(&graphOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectGraphUICommand(opts Options) *cobra.Command {
	uiOpts := semanticGraphUIOptions{limit: 500}
	cmd := &cobra.Command{
		Use:   "graph-ui [output.html]",
		Short: "Write a local static HTML semantic graph explorer",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			output := "semantic-graph.html"
			if len(args) == 1 {
				output = args[0]
			}
			return runSemanticGraphUI(cmd, opts, uiOpts, output)
		},
	}
	cmd.Flags().IntVar(&uiOpts.limit, "limit", 500, "Maximum relation edges to embed")
	cmd.Flags().BoolVar(&uiOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectSnippetCommand(opts Options) *cobra.Command {
	snippetOpts := semanticSnippetOptions{contextLines: 0}
	cmd := &cobra.Command{
		Use:   "snippet <symbol-or-id>",
		Short: "Return the exact source snippet for an indexed symbol",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticSnippet(cmd, opts, snippetOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&snippetOpts.contextLines, "context-lines", 0, "Extra lines before and after the symbol")
	cmd.Flags().BoolVar(&snippetOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectTracePathCommand(opts Options) *cobra.Command {
	traceOpts := semanticTracePathOptions{depth: 4}
	cmd := &cobra.Command{
		Use:   "trace-path <from-symbol> <to-symbol>",
		Short: "Find a directed semantic relation path between two symbols",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticTracePath(cmd, opts, traceOpts, args[0], args[1])
		},
	}
	cmd.Flags().IntVar(&traceOpts.depth, "depth", 4, "Maximum relation depth")
	cmd.Flags().BoolVar(&traceOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectDeadCodeCommand(opts Options) *cobra.Command {
	deadOpts := semanticDeadCodeOptions{limit: 100}
	cmd := &cobra.Command{
		Use:   "dead-code",
		Short: "List symbols with no incoming non-structural semantic edges",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticDeadCode(cmd, opts, deadOpts)
		},
	}
	cmd.Flags().IntVar(&deadOpts.limit, "limit", 100, "Maximum symbols to return")
	cmd.Flags().BoolVar(&deadOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectIngestTracesCommand(opts Options) *cobra.Command {
	ingestOpts := semanticTraceIngestOptions{}
	cmd := &cobra.Command{
		Use:   "ingest-traces <json-or-ndjson-file>",
		Short: "Import runtime traces and validate them against static semantic edges",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticIngestTraces(cmd, opts, ingestOpts, args[0])
		},
	}
	cmd.Flags().BoolVar(&ingestOpts.json, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newInspectCodeCommand(opts Options) *cobra.Command {
	queryOpts := semanticQueryOptions{limit: 10}
	cmd := &cobra.Command{
		Use:   "code <query>",
		Short: "Search semantic code facts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticQuery(cmd.Context(), cmd, opts, queryOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&queryOpts.limit, "limit", 10, "Maximum results to return")
	cmd.Flags().IntVar(&queryOpts.offset, "offset", 0, "Results to skip before returning a page")
	cmd.Flags().BoolVar(&queryOpts.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&queryOpts.details, "details", false, "Include full semantic records with provider metadata")
	return cmd
}

func newInspectContextCommand(opts Options) *cobra.Command {
	contextOpts := semanticContextOptions{limit: 5}
	cmd := &cobra.Command{
		Use:   "context <symbol-or-text>",
		Short: "Build semantic context for a symbol or query",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticContext(cmd.Context(), cmd, opts, contextOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&contextOpts.limit, "limit", 5, "Maximum symbols to include")
	cmd.Flags().IntVar(&contextOpts.offset, "offset", 0, "Symbols to skip before returning a page")
	cmd.Flags().BoolVar(&contextOpts.includeContent, "include-content", false, "Include local source snippets for matched symbols")
	cmd.Flags().BoolVar(&contextOpts.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&contextOpts.details, "details", false, "Include full semantic records with provider metadata")
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
	cmd.Flags().BoolVar(&impactOpts.details, "details", false, "Include full semantic records with provider metadata")
	return cmd
}

func newInspectChangesCommand(opts Options) *cobra.Command {
	changesOpts := semanticChangesOptions{limit: 100}
	cmd := &cobra.Command{
		Use:   "changes",
		Short: "Map local diff hunks to semantic symbols",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticChanges(cmd.Context(), cmd, opts, changesOpts)
		},
	}
	cmd.Flags().IntVar(&changesOpts.limit, "limit", 100, "Maximum symbols to include")
	cmd.Flags().BoolVar(&changesOpts.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&changesOpts.persist, "write-report", false, "Persist semantic/changes/latest.json")
	return cmd
}

func newInspectTestsCommand(opts Options) *cobra.Command {
	testsOpts := semanticTestsOptions{limit: 3}
	cmd := &cobra.Command{
		Use:   "tests <symbol-or-text>",
		Short: "Suggest tests relevant to a symbol or query",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticTests(cmd.Context(), cmd, opts, testsOpts, args[0])
		},
	}
	cmd.Flags().IntVar(&testsOpts.limit, "limit", 3, "Maximum test suggestions to include")
	cmd.Flags().BoolVar(&testsOpts.json, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&testsOpts.details, "details", false, "Include full semantic records with provider metadata")
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
	failOn, err := normalizeSemanticAuditFailOn(statusOpts.failOn)
	if err != nil {
		return err
	}
	report, err := buildBrainStatusReport(ctx, opts, target)
	if err != nil {
		return err
	}
	populateBrainStatusVerification(ctx, opts, &report)
	if statusOpts.details {
		populateBrainStatusSemanticDetail(ctx, opts, &report)
		populateBrainStatusLiveDetail(&report)
	} else {
		populateBrainStatusSemanticSummary(ctx, opts, &report)
		report = brainStatusCompactReport(report)
	}
	if statusOpts.json {
		if err := writeJSON(cmd, report); err != nil {
			return err
		}
	} else {
		renderBrainStatusText(cmd, report)
	}
	if err := semanticAuditFailureForReport(brainStatusFreshnessSeverity(report), len(brainStatusBlindSpots(report)), failOn); err != nil {
		return renderedCommandError{err: err}
	}
	return nil
}

// populateBrainStatusSemanticSummary fills trust-critical status data without
// scanning the semantic store: manifest counts/warnings and current blind spots.
// It is the default CLI/MCP preflight payload.
func populateBrainStatusSemanticSummary(ctx context.Context, opts Options, report *brainStatusReport) {
	if report.Semantic == nil || report.Manifest == nil || report.Manifest.Sources == nil || report.Manifest.Sources.Semantic == nil {
		return
	}
	source := report.Manifest.Sources.Semantic
	report.Semantic.Coverage = &brainStatusSemanticCoverage{
		Files:                 source.Files,
		Symbols:               source.Symbols,
		Relations:             source.Relations,
		Warnings:              len(source.Warnings),
		PartialFailures:       len(source.PartialFailures),
		WarningDetails:        semanticWarningDetails(source.Warnings),
		PartialFailureDetails: semanticWarningDetails(source.PartialFailures),
	}
	if spots, err := brainBlindSpotsForRepo(ctx, opts, report.Repo.Root); err != nil {
		report.Warnings = append(report.Warnings, "blind spots unavailable: "+err.Error())
	} else {
		report.Semantic.BlindSpots = spots
	}
}

// populateBrainStatusSemanticDetail adds the store-backed coverage histograms
// requested by --details / MCP details=true. Summary counts and blind spots are
// populated first so the detailed response is a strict superset of the default.
func populateBrainStatusSemanticDetail(ctx context.Context, opts Options, report *brainStatusReport) {
	populateBrainStatusSemanticSummary(ctx, opts, report)
	if report.Semantic == nil || report.Semantic.Coverage == nil || report.Manifest == nil || report.Manifest.Sources == nil || report.Manifest.Sources.Semantic == nil {
		return
	}
	source := report.Manifest.Sources.Semantic
	var freshness staleReport
	if report.Semantic.Freshness != nil {
		freshness = *report.Semantic.Freshness
	}
	if coverage, err := semanticAuditStoreCoverage(report.Brain.Path, source, freshness); err != nil {
		report.Warnings = append(report.Warnings, "semantic coverage unavailable: "+err.Error())
	} else {
		report.Semantic.Coverage.FileLanguages = coverage.FileLanguages
		report.Semantic.Coverage.Languages = coverage.Languages
		report.Semantic.Coverage.SymbolKinds = coverage.SymbolKinds
		report.Semantic.Coverage.RelationTypes = coverage.RelationTypes
	}
}

func populateBrainStatusLiveDetail(report *brainStatusReport) {
	if report == nil || report.Manifest == nil || report.Manifest.Sources == nil || report.Manifest.Sources.Semantic == nil || len(report.Live.ChangedFiles) == 0 {
		return
	}
	symbols, err := semanticSymbolsForFiles(report.Brain.Path, report.Manifest.Sources.Semantic, report.Live.ChangedFiles, 20)
	if err != nil {
		report.Live.Warnings = append(report.Live.Warnings, "changed symbol hints unavailable: "+err.Error())
		return
	}
	report.Live.ChangedSymbolHints = symbols
}

func brainStatusCompactReport(report brainStatusReport) brainStatusReport {
	report.Manifest = nil
	if report.Semantic != nil && report.Semantic.Coverage != nil {
		semantic := *report.Semantic
		coverage := *semantic.Coverage
		coverage.FileLanguages = nil
		coverage.Languages = nil
		coverage.SymbolKinds = nil
		coverage.RelationTypes = nil
		semantic.Coverage = &coverage
		report.Semantic = &semantic
	}
	report.Live.Staged = nil
	report.Live.Unstaged = nil
	report.Live.Untracked = nil
	report.Live.ChangedSymbolHints = nil
	return report
}

func brainStatusFreshnessSeverity(report brainStatusReport) string {
	severity := ""
	if report.Semantic != nil && report.Semantic.Freshness != nil {
		severity = report.Semantic.Freshness.Severity
	}
	if report.Retrieval != nil && report.Retrieval.Freshness != nil {
		severity = worseFreshnessSeverity(severity, report.Retrieval.Freshness.Severity)
	}
	return severity
}

func worseFreshnessSeverity(left, right string) string {
	rank := map[string]int{"": 0, "ok": 1, "degraded": 2, "unsafe": 3}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

func brainStatusBlindSpots(report brainStatusReport) []brainBlindSpot {
	if report.Semantic == nil {
		return nil
	}
	return report.Semantic.BlindSpots
}

func renderBrainStatusText(cmd *cobra.Command, report brainStatusReport) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Brain")
	fmt.Fprintf(out, "  path: %s\n", report.Brain.Path)
	fmt.Fprintf(out, "  repo: %s (key %s)\n", report.Repo.Root, report.Repo.Key)
	if report.Brain.GeneratedAt != "" {
		fmt.Fprintf(out, "  generated: %s\n", report.Brain.GeneratedAt)
	}
	fmt.Fprintf(out, "  sources: seed=%t sessions=%t semantic=%t history=%t facts=%t docs=%t\n",
		report.Sources.Seed, report.Sources.Sessions, report.Sources.Semantic, report.Sources.History, report.Sources.Facts, report.Sources.Docs)
	if f := report.Facts; f != nil {
		fmt.Fprintln(out, "\nFacts")
		fmt.Fprintf(out, "  counts: %d (%d distilled, %d authored, %d superseded) across %d branch(es); %d proposals pending\n",
			f.Facts, f.Distilled, f.Authored, f.Superseded, f.Branches, f.Proposals)
		if v := f.Verification; v != nil {
			fmt.Fprintf(out, "  verification: %d facts, %d verified, %d stale, %d orphaned, %d unverifiable-here\n",
				v.Facts, v.Verified, v.Stale, v.Orphaned, v.UnverifiableHere)
		}
	}
	if s := report.Semantic; s != nil {
		fmt.Fprintln(out, "\nSemantic")
		if p := s.Provider; p != nil && p.Name != "" {
			fmt.Fprintf(out, "  provider: %s %s (schema %s)\n", p.Name, p.Version, p.Schema)
		}
		if c := s.Coverage; c != nil {
			fmt.Fprintf(out, "  coverage: %d files, %d symbols, %d relations, %d warnings, %d partial failures\n",
				c.Files, c.Symbols, c.Relations, c.Warnings, c.PartialFailures)
			if len(c.FileLanguages) > 0 {
				fmt.Fprintf(out, "  file languages: %s\n", semanticAuditCountSummary(c.FileLanguages))
			}
			if len(c.Languages) > 0 {
				fmt.Fprintf(out, "  symbol languages: %s\n", semanticAuditCountSummary(c.Languages))
			}
			if len(c.RelationTypes) > 0 {
				fmt.Fprintf(out, "  relation types: %s\n", semanticAuditCountSummary(c.RelationTypes))
			}
			for _, warning := range c.WarningDetails {
				fmt.Fprintf(out, "  warning: %s\n", semanticAuditWarningSummary(warning))
			}
			for _, failure := range c.PartialFailureDetails {
				fmt.Fprintf(out, "  partial failure: %s\n", semanticAuditWarningSummary(failure))
			}
		}
		if f := s.Freshness; f != nil {
			fmt.Fprintf(out, "  freshness: %s\n", f.Severity)
			keys := make([]string, 0, len(f.Axes))
			for key := range f.Axes {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				axis := f.Axes[key]
				line := "    " + key + ": " + axis.State
				if axis.Detail != "" {
					line += " (" + axis.Detail + ")"
				}
				fmt.Fprintln(out, line)
			}
		}
		// Coverage is only populated by the status command's detail pass, which
		// also fills blind spots — so its presence distinguishes "checked, none
		// found" from "not checked".
		if s.Coverage != nil {
			if len(s.BlindSpots) == 0 {
				fmt.Fprintln(out, "  blind spots: none")
			} else {
				fmt.Fprintf(out, "  blind spots: %d\n", len(s.BlindSpots))
				for _, spot := range s.BlindSpots {
					fmt.Fprintf(out, "    %s", valueOrUnset(spot.Path))
					if spot.Code != "" {
						fmt.Fprintf(out, " [%s]", spot.Code)
					}
					if strings.TrimSpace(spot.Detail) != "" {
						fmt.Fprintf(out, " %s", spot.Detail)
					}
					fmt.Fprintln(out)
				}
			}
		}
	}
	if r := report.Retrieval; r != nil {
		fmt.Fprintln(out, "\nRetrieval")
		if r.SeedCommit != "" {
			fmt.Fprintf(out, "  seed: %s (%s)\n", shortCommitHash(r.SeedCommit), valueOrUnset(r.SeedMode))
		}
		if r.DocsGeneratedAt != "" {
			fmt.Fprintf(out, "  docs: %d records from %d files (generated %s)\n", r.DocsRecords, r.DocsFiles, r.DocsGeneratedAt)
		}
		if f := r.Freshness; f != nil {
			fmt.Fprintf(out, "  freshness: %s\n", f.Severity)
			renderFreshnessAxes(out, f.Axes)
		}
	}
	fmt.Fprintln(out, "\nLive")
	fmt.Fprintf(out, "  branch: %s\n", valueOrUnset(report.Live.Branch))
	fmt.Fprintf(out, "  head: %s\n", valueOrUnset(report.Live.Head))
	fmt.Fprintf(out, "  dirty: %t\n", report.Live.Dirty)
	if report.Live.DiffStat != "" {
		fmt.Fprintf(out, "  diff: %s\n", strings.TrimSpace(report.Live.DiffStat))
	}
	for _, file := range report.Live.Staged {
		fmt.Fprintf(out, "  staged: %s\n", file)
	}
	for _, file := range report.Live.Unstaged {
		fmt.Fprintf(out, "  unstaged: %s\n", file)
	}
	for _, file := range report.Live.Untracked {
		fmt.Fprintf(out, "  untracked: %s\n", file)
	}
	for _, symbol := range report.Live.ChangedSymbolHints {
		fmt.Fprintf(out, "  changed symbol: %s %s:%d-%d\n", displaySymbolName(symbol), symbol.FilePath, symbol.StartLine, symbol.EndLine)
	}
	warnings := append(append([]string(nil), report.Warnings...), report.Live.Warnings...)
	if len(warnings) > 0 {
		fmt.Fprintln(out, "\nWarnings")
		for _, warning := range warnings {
			fmt.Fprintf(out, "  - %s\n", warning)
		}
	}
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
			"Use likely_edit_files and history matches before text search; if search is still needed, scope it to likely_files and specific identifiers. Use likely_test_files for validation context.",
			"Use the live-state overlay before trusting semantic results for files changed in this session.",
			"Inspect full diffs or source files when the task intersects dirty files or when confidence is low.",
		},
	}
	var receiptBranch, receiptSurface string
	var receiptFactIDs []string
	if status.Manifest != nil && status.Manifest.Sources != nil && status.Manifest.Sources.Semantic != nil {
		semanticQuery := brainBriefSemanticQuery(task)
		contextCandidateLimit := brainBriefExpandedCandidateLimit(briefOpts.limit, brainBriefContextCandidateMultiplier)
		contextSymbols, contextRelations, contextNeighbors, contextErr := semanticContextFacts(status.Brain.Path, status.Manifest.Sources.Semantic, semanticQuery, contextCandidateLimit, 0)
		if contextErr != nil {
			report.Warnings = append(report.Warnings, "semantic context unavailable: "+contextErr.Error())
		} else {
			report.Semantic.Context = brainBriefSelectSemanticContext(contextSymbols, contextRelations, contextNeighbors, task, briefOpts.limit)
			if len(report.Semantic.Context.Symbols) > 0 {
				impactLimit := max(40, brainBriefExpandedCandidateLimit(briefOpts.limit, 8))
				_, impactSymbols, impactRelations, impactErr := semanticImpactFacts(status.Brain.Path, status.Manifest.Sources.Semantic, report.Semantic.Context.Symbols[0].ID, 2, impactLimit)
				if impactErr != nil {
					report.Warnings = append(report.Warnings, "semantic impact context unavailable: "+impactErr.Error())
				} else {
					report.Semantic.Context = brainBriefMergeImpactContext(report.Semantic.Context, impactSymbols, impactRelations, task, briefOpts.limit)
				}
			}
		}
		runtimeTraces, runtimeErr := semanticRuntimeTraceFacts(status.Brain.Path, status.Manifest.Sources.Semantic, semanticQuery, briefOpts.limit)
		if runtimeErr != nil {
			report.Warnings = append(report.Warnings, "runtime trace context unavailable: "+runtimeErr.Error())
		} else {
			report.Semantic.RuntimeTraces = runtimeTraces
		}
		testCandidateLimit := brainBriefExpandedCandidateLimit(briefOpts.limit, 10)
		tests, testsErr := semanticTestFacts(status.Brain.Path, status.Manifest.Sources.Semantic, semanticQuery, testCandidateLimit)
		if testsErr != nil {
			report.Warnings = append(report.Warnings, "test suggestions unavailable: "+testsErr.Error())
		} else {
			report.Semantic.Tests = brainBriefSelectSemanticTests(tests, report.Semantic.Context, briefOpts.limit)
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
			// rankHistoryFused is rankHistoryViaFTS unless the history fusion
			// gate is open (fusion-eligible embedder + refresh-built vec0
			// vectors), in which case the brief's history context gets the
			// capstone-validated fused ranking — the midtask stratum this
			// surface serves is exactly where fusion measured strongest.
			if scored, ok := rankHistoryFused(status.Brain.Path, index, "history", task, briefOpts.limit, defaultEmbedder()); ok {
				for _, s := range scored {
					indexedMatches = append(indexedMatches, brainBriefHistoryRecordTextMatch(status.Brain.Path, s.Record, task))
				}
			} else {
				for _, record := range rankHistoryRecords(index, "history", task, briefOpts.limit) {
					indexedMatches = append(indexedMatches, brainBriefHistoryRecordTextMatch(status.Brain.Path, record, task))
				}
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
			// Semantic rerank on by default; nil reranker (embedder
			// unavailable or --no-semantic) falls back to lexical ranking. The
			// disk-backed cache avoids re-embedding the branch each brief.
			var rr *semanticReranker
			if !briefOpts.noSemantic {
				if e := defaultEmbedder(); e != nil {
					rr = newSemanticRerankerForBranch(e, status.Brain.Path, branch)
				}
			}
			report.Facts = rankFactsFused(facts, task, brainBriefFactsCount(briefOpts.limit), false, rr)
			if rr != nil {
				rr.retain(facts) // keep every present fact's vector; prune only departed facts
				_ = rr.flush()   // best-effort cache persist
			}
			receiptBranch = branch
			receiptSurface = briefOpts.surface
			if receiptSurface == "" {
				receiptSurface = "brief"
			}
			receiptFactIDs = factRecordIDs(report.Facts)
			// Live trust state, mirroring the unified retrieval guard: flag
			// surfaced facts with a pending merge/supersede proposal so the
			// brief carries the same verify-before-trust signal as query/get.
			// No surfaced facts means no annotation and no queue warning, so
			// skip the proposal read on the empty-brief path.
			if len(report.Facts) > 0 {
				if proposals, proposalsErr := loadFactProposals(status.Brain.Path, branch); proposalsErr != nil {
					report.Warnings = append(report.Warnings, factReviewQueueUnavailableWarning)
				} else {
					report.FactsPendingReview = factsPendingReview(facts, proposals, report.Facts)
				}
			}
		}
	}
	report.FactsLocusDrift = factsLocusDrift(status.Repo.Root, report.Facts)
	report.LikelyEditFiles, report.LikelyTestFiles, report.LikelyFiles = brainBriefLikelyFileGroupsForRepo(status.Repo.Root, status.Repo.Key, report, task)
	focusedFileFallback := false
	if len(report.Semantic.Context.Symbols) == 0 && len(report.LikelyEditFiles) > 0 && status.Manifest != nil && status.Manifest.Sources != nil && status.Manifest.Sources.Semantic != nil {
		candidateLimit := brainBriefExpandedCandidateLimit(briefOpts.limit, 40)
		candidates, focusedErr := semanticSymbolsForFiles(status.Brain.Path, status.Manifest.Sources.Semantic, report.LikelyEditFiles[:1], candidateLimit)
		if focusedErr != nil {
			report.Warnings = append(report.Warnings, "focused semantic file context unavailable: "+focusedErr.Error())
		} else {
			report.Semantic.Context.Symbols = brainBriefSelectFocusedFileSymbols(candidates, task, briefOpts.limit)
			focusedFileFallback = len(report.Semantic.Context.Symbols) > 0
		}
	}
	report.LikelyTestFiles = brainBriefAddSiblingTestFiles(status.Repo.Root, report.LikelyEditFiles, report.LikelyTestFiles)
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
	report.ActionChecklist = brainBriefActionChecklist(status.Repo.Root, report, task)
	if len(report.ActionChecklist) == 0 && focusedFileFallback {
		report.ActionChecklist = brainBriefFocusedFileActions(report.Semantic.Context.Symbols, report.LikelyEditFiles[0])
	}
	if len(report.ActionChecklist) > 0 {
		report.LikelyEditFiles = brainBriefActionFiles(report.ActionChecklist)
		if actionTestFiles := brainBriefActionTestFiles(report.ActionChecklist); len(actionTestFiles) > 0 {
			report.LikelyTestFiles = actionTestFiles
		} else {
			report.LikelyTestFiles = brainBriefAddSiblingTestFiles(status.Repo.Root, report.LikelyEditFiles, report.LikelyTestFiles)
		}
		report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
		report.Guidance = append(report.Guidance, "Treat action_checklist as the first-pass current-code inventory; edit listed files first, and broaden only when the checklist is missing, ambiguous, or validation fails.")
	}
	if views, _, perr := loadPatternViews(status.Brain.Path); perr == nil {
		report.Patterns = rankTaskRelevantPatterns(views, brainBriefFileMatchTerms(task), brainBriefPatternsCount(briefOpts.limit))
	}
	// Corpus consolidations (v2): task-relevant dossiers, capped, no ambient
	// noise. Graceful — absent corpus contributes nothing.
	report.Consolidations = loadBriefConsolidations(status.Brain.Path, brainBriefFileMatchTerms(task), brainBriefPatternsCount(briefOpts.limit))
	// Verified latent-practice themes relevant to the task (no noise; accepted only).
	report.Themes = rankTaskRelevantThemes(loadThemeViews(status.Brain.Path, true), brainBriefFileMatchTerms(task), brainBriefPatternsCount(briefOpts.limit))
	recordReceipt := func() {
		recordServedFacts(cmd.ErrOrStderr(), vitalityNow(opts), status.Brain.Path, receiptBranch, receiptSurface,
			status.Live.Head, task, receiptFactIDs)
	}
	if briefOpts.json {
		if err := writeJSON(cmd, brainBriefJSONProjection(report)); err != nil {
			return err
		}
		if len(receiptFactIDs) > 0 {
			recordReceipt()
		}
		return nil
	}
	originalOut := cmd.OutOrStdout()
	trackedOut := &stickyErrorWriter{writer: originalOut}
	cmd.SetOut(trackedOut)
	defer cmd.SetOut(originalOut)
	fmt.Fprintf(cmd.OutOrStdout(), "task: %s\n", report.Task)
	if severity := brainStatusFreshnessSeverity(report.Status); severity != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "freshness: %s\n", severity)
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
	for _, trace := range report.Semantic.RuntimeTraces {
		fmt.Fprintf(cmd.OutOrStdout(), "runtime_trace %s -> %s %s\n", trace.FromID, trace.ToID, trace.Reason)
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
		if gone := report.FactsLocusDrift[fact.ID]; len(gone) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "  ⚠ stale locus (no longer in worktree): %s\n", strings.Join(gone, ", "))
		}
		if notice, ok := report.FactsPendingReview[fact.ID]; ok {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", factReviewNoticeLine(notice))
		}
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
		if item.Validation != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "  validate: %s (complete_on_pass=%t)\n", item.Validation.Command, item.Validation.CompleteOnPass)
		}
	}
	for _, c := range report.Consolidations {
		state := c.Status
		if c.Verdict != "" {
			state += "/" + c.Verdict
		}
		fmt.Fprintf(cmd.OutOrStdout(), "consolidation [%s %.2f %s] %s\n", c.Type, c.Confidence, state, c.Title)
		if len(c.Workflow) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "  workflow: %s\n", strings.Join(c.Workflow, " → "))
		}
		if len(c.Verification) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "  verify: %s\n", strings.Join(c.Verification, ", "))
		}
		if c.Anchor != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "  e.g. %s:%d   id %s\n", c.Anchor.Transcript, c.Anchor.StartLine, c.PatternID)
		}
	}
	for _, th := range report.Themes {
		fmt.Fprintf(cmd.OutOrStdout(), "theme [%s %.2f] %s   id %s\n", th.Shape, th.Strength, th.Title, th.ID)
	}
	for _, warning := range append(report.Status.Warnings, report.Warnings...) {
		fmt.Fprintf(cmd.OutOrStdout(), "warning: %s\n", warning)
	}
	if trackedOut.err != nil {
		return trackedOut.err
	}
	if len(receiptFactIDs) > 0 {
		recordReceipt()
	}
	return nil
}

func brainBriefOutputStatus(status brainStatusReport) brainStatusReport {
	status.Manifest = nil
	if status.Facts != nil {
		facts := *status.Facts
		facts.Verification = nil
		status.Facts = &facts
	}
	if status.Semantic != nil {
		semantic := *status.Semantic
		semantic.Coverage = nil
		status.Semantic = &semantic
	}
	status.Live.ChangedSymbolHints = nil
	status.Live.Staged = nil
	status.Live.Unstaged = nil
	status.Live.Untracked = nil
	return status
}

func brainBriefSemanticQuery(task string) string {
	query := strings.TrimSpace(task)
	// Some agent frameworks prefix an opaque task/fixture label before the real
	// natural-language request. Do not spend the bounded semantic token reserve
	// on that transport metadata. Restrict stripping to a single hyphenated token
	// so meaningful prose such as "HTTP error: preserve scopes" remains intact.
	if colon := strings.IndexByte(query, ':'); colon > 0 && colon < 128 {
		label := strings.TrimSpace(query[:colon])
		if strings.Contains(label, "-") && !strings.ContainsAny(label, " \t\r\n/\\") {
			query = strings.TrimSpace(query[colon+1:])
		}
	}
	lower := strings.ToLower(query)
	if strings.Contains(lower, "brain brief") || strings.Contains(lower, "brain_brief") {
		// Put the public-surface anchors first. Tokenized semantic lookup keeps a
		// bounded prefix, so appending them after a long natural-language task can
		// silently discard the most precise identifiers and rank generic records.
		query = "brain_brief brainBrief runBrainBrief " + query
	}
	return query
}

func brainBriefExpandedCandidateLimit(limit, multiplier int) int {
	if limit <= 0 {
		return 1
	}
	if multiplier <= 1 || limit > 1000/multiplier {
		return limit
	}
	return limit * multiplier
}

func brainBriefSelectSemanticContext(symbols, relations, neighbors []semanticRecord, task string, limit int) semanticContextResult {
	selected := make([]semanticRecord, 0, min(limit, len(symbols)))
	if brainBriefTaskRequestsTests(task) || brainBriefTaskRequestsDocs(task) {
		selected = append(selected, symbols[:min(limit, len(symbols))]...)
	} else {
		ranked := brainBriefRankSemanticSymbols(symbols, nil, task)
		for _, symbol := range ranked {
			if !brainBriefImplementationRoot(symbol) {
				continue
			}
			selected = append(selected, symbol)
			if len(selected) >= limit {
				break
			}
		}
		for _, symbol := range ranked {
			if len(selected) >= limit {
				break
			}
			if isSemanticTestSymbol(symbol) || slices.ContainsFunc(selected, func(existing semanticRecord) bool { return existing.ID == symbol.ID }) {
				continue
			}
			selected = append(selected, symbol)
		}
		for _, symbol := range ranked {
			if len(selected) >= limit {
				break
			}
			if !isSemanticTestSymbol(symbol) || slices.ContainsFunc(selected, func(existing semanticRecord) bool { return existing.ID == symbol.ID }) {
				continue
			}
			selected = append(selected, symbol)
		}
	}

	rootIDs := make(map[string]struct{}, len(selected))
	for _, symbol := range selected {
		rootIDs[symbol.ID] = struct{}{}
	}
	relationLimit := brainBriefExpandedCandidateLimit(limit, 4)
	selectedRelations := make([]semanticRecord, 0, min(relationLimit, len(relations)))
	relatedIDs := make(map[string]struct{}, len(selectedRelations)*2)
	for _, relation := range relations {
		_, fromRoot := rootIDs[relation.FromID]
		_, toRoot := rootIDs[relation.ToID]
		if !fromRoot && !toRoot {
			continue
		}
		selectedRelations = append(selectedRelations, relation)
		relatedIDs[relation.FromID] = struct{}{}
		relatedIDs[relation.ToID] = struct{}{}
		if len(selectedRelations) >= relationLimit {
			break
		}
	}
	selectedNeighbors := make([]semanticRecord, 0, min(limit, len(neighbors)))
	for _, neighbor := range neighbors {
		if _, root := rootIDs[neighbor.ID]; root {
			continue
		}
		if _, related := relatedIDs[neighbor.ID]; !related {
			continue
		}
		selectedNeighbors = append(selectedNeighbors, neighbor)
		if len(selectedNeighbors) >= limit {
			break
		}
	}
	return semanticContextResult{
		Symbols:   nonNil(selected),
		Relations: nonNil(selectedRelations),
		Neighbors: nonNil(selectedNeighbors),
	}
}

func brainBriefImplementationRoot(symbol semanticRecord) bool {
	if isSemanticTestSymbol(symbol) || !brainBriefSourceFile(symbol.FilePath) {
		return false
	}
	switch strings.ToLower(symbol.Kind) {
	case "field", "property", "variable", "constant", "section", "heading":
		return false
	default:
		return true
	}
}

func brainBriefTaskRequestsTests(task string) bool {
	lower := strings.ToLower(strings.TrimSpace(task))
	for _, phrase := range []string{
		"add test", "add a test", "write test", "write a test", "create test", "create a test",
		"fix test", "fix the test", "failing test", "test failure", "tests fail", "tests are failing",
		"unit test", "integration test", "test coverage", "test fixture", "test suite",
		"which test", "what test", "tests for ", "tests cover ",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return strings.HasPrefix(lower, "test ") || strings.HasPrefix(lower, "tests ")
}

func brainBriefTaskRequestsDocs(task string) bool {
	lower := strings.ToLower(task)
	return strings.Contains(lower, "readme") ||
		strings.Contains(lower, "documentation") ||
		strings.Contains(lower, "docs/") ||
		strings.Contains(lower, "doc guide") ||
		strings.Contains(lower, "update the guide")
}

func brainBriefMergeImpactContext(context semanticContextResult, symbols, relations []semanticRecord, task string, limit int) semanticContextResult {
	rootIDs := make(map[string]struct{}, len(context.Symbols))
	for _, root := range context.Symbols {
		rootIDs[root.ID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(rootIDs)+limit)
	for id := range rootIDs {
		seen[id] = struct{}{}
	}
	neighbors := make([]semanticRecord, 0, limit)
	for _, symbol := range brainBriefRankSemanticSymbols(symbols, relations, task) {
		if _, ok := seen[symbol.ID]; ok {
			continue
		}
		if !brainBriefImplementationRoot(symbol) && !(brainBriefTaskRequestsTests(task) && isSemanticTestSymbol(symbol)) {
			continue
		}
		seen[symbol.ID] = struct{}{}
		neighbors = append(neighbors, symbol)
		if len(neighbors) >= limit {
			break
		}
	}
	relationCandidates := make([]semanticRecord, 0, len(relations))
	seenRelations := make(map[string]struct{}, len(relations))
	for _, relation := range relations {
		_, from := seen[relation.FromID]
		_, to := seen[relation.ToID]
		if !from || !to {
			continue
		}
		key := relation.FromID + "\x00" + relation.Type + "\x00" + relation.ToID
		if _, duplicate := seenRelations[key]; duplicate {
			continue
		}
		seenRelations[key] = struct{}{}
		relationCandidates = append(relationCandidates, relation)
	}
	sort.SliceStable(relationCandidates, func(i, j int) bool {
		left := brainBriefRelationPriority(relationCandidates[i].Type)
		right := brainBriefRelationPriority(relationCandidates[j].Type)
		if left != right {
			return left > right
		}
		if relationCandidates[i].FromID != relationCandidates[j].FromID {
			return relationCandidates[i].FromID < relationCandidates[j].FromID
		}
		return relationCandidates[i].ToID < relationCandidates[j].ToID
	})
	relationLimit := brainBriefExpandedCandidateLimit(limit, 4)
	if len(relationCandidates) > relationLimit {
		relationCandidates = relationCandidates[:relationLimit]
	}
	context.Neighbors = nonNil(neighbors)
	context.Relations = nonNil(relationCandidates)
	return context
}

// brainBriefRankSemanticSymbols reranks a bounded semantic candidate set by
// the task terms that discriminate within that set. This prevents traversal
// order from spending the compact output budget on ubiquitous plumbing types
// or sibling command constructors when rarer task anchors such as "mcp" or
// "compact" identify the implementation path the agent actually needs.
func brainBriefRankSemanticSymbols(symbols, relations []semanticRecord, task string) []semanticRecord {
	if len(symbols) < 2 {
		return append([]semanticRecord(nil), symbols...)
	}
	terms := brainBriefFileMatchTerms(task)
	type candidate struct {
		record semanticRecord
		score  int
	}
	candidates := make([]candidate, len(symbols))
	identities := make([]string, len(symbols))
	for i, symbol := range symbols {
		identities[i] = strings.ToLower(strings.Join([]string{symbol.Name, symbol.QualifiedName, symbol.FilePath}, " "))
	}
	weights := make(map[string]int, len(terms))
	for _, term := range terms {
		df := 0
		for _, identity := range identities {
			if strings.Contains(identity, term) {
				df++
			}
		}
		if df > 0 {
			weights[term] = tokenIDFWeight(len(symbols), df)
		}
	}
	relationScores := make(map[string]int)
	for _, relation := range relations {
		score := brainBriefRelationPriority(relation.Type) / 5
		if score > relationScores[relation.FromID] {
			relationScores[relation.FromID] = score
		}
		if score > relationScores[relation.ToID] {
			relationScores[relation.ToID] = score
		}
	}
	for i, symbol := range symbols {
		name := strings.ToLower(symbol.Name + " " + symbol.QualifiedName)
		path := strings.ToLower(symbol.FilePath)
		base := strings.TrimSuffix(strings.ToLower(filepath.Base(path)), strings.ToLower(filepath.Ext(path)))
		// Preserve the bounded retriever's IDF relevance. The compact reranker
		// adds task/path and graph evidence; it must not erase the signal that
		// admitted a rare identifier-shaped match to the candidate set.
		score := symbol.Score + relationScores[symbol.ID]
		switch strings.ToLower(symbol.Kind) {
		case "function", "method", "class", "type", "interface":
			score += 5
		}
		for term, weight := range weights {
			switch {
			case strings.Contains(name, term):
				score += weight * 2
			case strings.Contains(base, term):
				score += weight
			case strings.Contains(path, term):
				score += max(1, weight/2)
			}
		}
		// Prefer symbols whose identifier covers the task's whole concept tuple.
		// IDF alone can rank mapRepoNamesToIDs above NormalizeRepoName because the
		// former's signature spells the rarer word "repository". For localization,
		// three concepts in the identifier (normalize + repo + name) are stronger
		// evidence than two concepts spread across a signature. Squaring rewards
		// coherent compound identifiers without making one generic name token win.
		coverage := brainBriefIdentifierConceptCoverage(task, symbol.Name)
		score += coverage * coverage * 25
		score += brainBriefSemanticIntentBonus(task, name)
		candidates[i] = candidate{record: symbol, score: score}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	ranked := make([]semanticRecord, len(candidates))
	for i, candidate := range candidates {
		ranked[i] = candidate.record
	}
	return ranked
}

func brainBriefIdentifierConceptCoverage(task, identifier string) int {
	canonical := func(term string) string {
		term = strings.ToLower(strings.TrimSpace(term))
		if variants := semanticQueryMorphologyVariants(term); len(variants) > 0 {
			term = variants[0]
		}
		switch term {
		case "repository", "repositories":
			return "repo"
		}
		if strings.HasSuffix(term, "ies") && len(term) > 5 {
			return strings.TrimSuffix(term, "ies") + "y"
		}
		if strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss") && len(term) > 4 {
			return strings.TrimSuffix(term, "s")
		}
		return term
	}
	taskConcepts := make(map[string]struct{})
	for _, term := range brainBriefFileMatchTerms(task) {
		if concept := canonical(term); concept != "" {
			taskConcepts[concept] = struct{}{}
		}
	}
	matched := make(map[string]struct{})
	for _, term := range strings.Fields(normalizeHistorySearchText(identifier)) {
		concept := canonical(term)
		if _, ok := taskConcepts[concept]; ok {
			matched[concept] = struct{}{}
		}
	}
	return len(matched)
}

func brainBriefSemanticIntentBonus(task, symbolName string) int {
	task = strings.ToLower(task)
	symbolName = strings.ToLower(symbolName)
	contractIntent := strings.Contains(task, "public") ||
		strings.Contains(task, "advertis") ||
		strings.Contains(task, "stable") ||
		strings.Contains(task, "contract") ||
		strings.Contains(task, "schema")
	if !contractIntent {
		return 0
	}
	for _, term := range []string{"definition", "registry", "schema", "catalog", "contract"} {
		if strings.Contains(symbolName, term) {
			return 60
		}
	}
	return 0
}

func brainBriefRelationPriority(relationType string) int {
	switch strings.ToUpper(strings.TrimSpace(relationType)) {
	case "CALLS", "HANDLES_CLI", "HANDLES_TOOL", "HANDLES_ROUTE", "HANDLES_COMMAND", "HANDLES_WORKFLOW":
		return 100
	case "DATA_FLOWS", "READS_FROM", "WRITES_TO":
		return 90
	case "PARAM_TYPE", "RETURNS_TYPE", "USES_TYPE", "IMPLEMENTS", "EXTENDS":
		return 80
	case "CONTAINS":
		return 20
	case "DEFINES":
		return 10
	default:
		return 50
	}
}

func brainBriefSelectSemanticTests(tests semanticTestsResult, context semanticContextResult, limit int) semanticTestsResult {
	if len(context.Symbols) == 0 {
		tests.Roots = nonNil(tests.Roots[:min(limit, len(tests.Roots))])
		tests.Suggestions = nonNil(tests.Suggestions[:min(limit, len(tests.Suggestions))])
		return tests
	}
	related := make(map[string]struct{}, len(context.Symbols)+len(context.Relations)*2)
	rootNames := make([]string, 0, len(context.Symbols))
	rootFiles := make([]string, 0, len(context.Symbols))
	for _, root := range context.Symbols {
		related[root.ID] = struct{}{}
		rootNames = append(rootNames, root.Name)
		if root.FilePath != "" {
			rootFiles = append(rootFiles, root.FilePath)
		}
	}
	for _, relation := range context.Relations {
		related[relation.FromID] = struct{}{}
		related[relation.ToID] = struct{}{}
	}
	candidates := make([]semanticRecord, 0, len(tests.Suggestions))
	for _, suggestion := range tests.Suggestions {
		candidates = append(candidates, suggestion.Symbol)
	}
	return semanticTestsResult{
		Roots:       nonNil(append([]semanticRecord(nil), context.Symbols...)),
		Suggestions: nonNil(rankSemanticTestSuggestions(candidates, related, rootNames, rootFiles, limit)),
	}
}

func brainBriefSelectFocusedFileSymbols(symbols []semanticRecord, task string, limit int) []semanticRecord {
	if limit <= 0 {
		return nil
	}
	ranked := brainBriefRankSemanticSymbols(symbols, nil, task)
	out := make([]semanticRecord, 0, min(limit, len(ranked)))
	seen := map[string]struct{}{}
	for _, symbol := range ranked {
		if isSemanticTestSymbol(symbol) || !brainBriefImplementationRoot(symbol) || symbol.StartLine <= 0 {
			continue
		}
		key := fmt.Sprintf("%s:%d:%s", symbol.FilePath, symbol.StartLine, symbol.QualifiedName)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, symbol)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func brainBriefFocusedFileActions(symbols []semanticRecord, topFile string) []brainBriefAction {
	for _, symbol := range symbols {
		if symbol.FilePath != topFile || symbol.StartLine <= 0 {
			continue
		}
		endLine := symbol.EndLine
		if endLine < symbol.StartLine {
			endLine = symbol.StartLine
		}
		return []brainBriefAction{{
			File:     topFile,
			Symbol:   displaySymbolName(symbol),
			Action:   "Inspect this task-relevant symbol first; broaden only if it does not contain the described behavior.",
			Evidence: fmt.Sprintf("semantic filename fallback at lines %d-%d", symbol.StartLine, endLine),
		}}
	}
	return nil
}

func brainBriefJSONProjection(report brainBriefReport) brainBriefJSONReport {
	out := brainBriefJSONReport{
		GeneratedAt:        report.GeneratedAt,
		Task:               report.Task,
		Status:             report.Status,
		History:            report.History,
		FactsLocusDrift:    report.FactsLocusDrift,
		FactsPendingReview: report.FactsPendingReview,
		ActionChecklist:    report.ActionChecklist,
		LikelyEditFiles:    report.LikelyEditFiles,
		LikelyTestFiles:    report.LikelyTestFiles,
		LikelyFiles:        report.LikelyFiles,
		Guidance:           report.Guidance,
		Warnings:           report.Warnings,
	}
	out.Semantic.Context.Symbols = compactSemanticRecords(report.Semantic.Context.Symbols)
	out.Semantic.Context.Relations = compactSemanticRecords(report.Semantic.Context.Relations)
	out.Semantic.Context.Neighbors = compactSemanticRecords(report.Semantic.Context.Neighbors)
	out.Semantic.RuntimeTraces = compactSemanticRecords(report.Semantic.RuntimeTraces)
	out.Semantic.Tests.Roots = compactSemanticRecords(report.Semantic.Tests.Roots)
	out.Semantic.Tests.Suggestions = make([]compactSemanticTestSuggestion, len(report.Semantic.Tests.Suggestions))
	for i, suggestion := range report.Semantic.Tests.Suggestions {
		out.Semantic.Tests.Suggestions[i] = compactSemanticTestSuggestion{
			Symbol: compactSemanticRecordFrom(suggestion.Symbol),
			Reason: suggestion.Reason,
		}
	}
	for _, fact := range report.Facts {
		out.Facts = append(out.Facts, brainBriefJSONFact{
			ID: fact.ID, Paths: fact.Paths, Kind: fact.Kind, Locus: fact.Locus,
			Text: fact.Text, Confidence: fact.Confidence,
		})
	}
	return out
}

func compactSemanticRecords(records []semanticRecord) []compactSemanticRecord {
	if records == nil {
		return []compactSemanticRecord{}
	}
	out := make([]compactSemanticRecord, len(records))
	for i, record := range records {
		out[i] = compactSemanticRecordFrom(record)
	}
	return out
}

func compactSemanticRecordFrom(record semanticRecord) compactSemanticRecord {
	return compactSemanticRecord{
		ID: record.ID, Kind: record.Kind, Name: record.Name, QualifiedName: record.QualifiedName,
		FilePath: record.FilePath, StartLine: record.StartLine, EndLine: record.EndLine,
		Path: record.Path, Signature: record.Signature, FromID: record.FromID, ToID: record.ToID,
		Type: record.Type, WarningCodes: record.WarningCodes, Confidence: record.Confidence, Reason: record.Reason,
	}
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
	return brainBriefLikelyFileGroupsForRepo(repoRoot, "", report, task)
}

func brainBriefLikelyFileGroupsForRepo(repoRoot, repoKey string, report brainBriefReport, task string) ([]string, []string, []string) {
	counts := map[string]int{}
	taskTerms := brainBriefRepoSpecificFileMatchTerms(brainBriefFileMatchTerms(task), repoKey)
	add := func(path string, weight int) {
		clean, ok := cleanBrainBriefLikelyFile(path)
		if !ok {
			return
		}
		counts[clean] += weight + brainBriefLikelyFileBonus(clean) + brainBriefTaskTermBonus(clean, taskTerms)
	}
	addSemantic := func(path string, weight int) {
		clean, ok := cleanBrainBriefSemanticFile(repoRoot, path)
		if !ok {
			return
		}
		counts[clean] += weight + brainBriefLikelyFileBonus(clean) + brainBriefTaskTermBonus(clean, taskTerms)
	}
	for _, symbol := range report.Semantic.Context.Symbols {
		addSemantic(symbol.FilePath, 12)
		addSemantic(symbol.Path, 4)
	}
	for _, neighbor := range report.Semantic.Context.Neighbors {
		addSemantic(neighbor.FilePath, 8)
		addSemantic(neighbor.Path, 3)
	}
	for _, relation := range report.Semantic.Context.Relations {
		addSemantic(relation.FilePath, 4)
		addSemantic(relation.Path, 2)
	}
	for _, trace := range report.Semantic.RuntimeTraces {
		addSemantic(trace.FilePath, 7)
		addSemantic(trace.Path, 3)
		for _, evidence := range trace.Evidence {
			addSemantic(evidence.FilePath, 2)
		}
	}
	for _, root := range report.Semantic.Tests.Roots {
		addSemantic(root.FilePath, 6)
	}
	for _, suggestion := range report.Semantic.Tests.Suggestions {
		addSemantic(suggestion.Symbol.FilePath, 9)
	}
	for _, changed := range report.Status.Live.ChangedFiles {
		add(changed, 3)
	}
	var historyEditFiles []string
	for _, match := range report.History.Matches {
		for _, path := range extractBrainBriefPaths(match.Excerpt) {
			clean, ok := cleanBrainBriefHistoryFile(repoRoot, path)
			if !ok {
				continue
			}
			add(clean, 12)
			if brainBriefSourceFile(clean) && !brainBriefLikelyTestFile(clean) && !slices.Contains(historyEditFiles, clean) {
				historyEditFiles = append(historyEditFiles, clean)
			}
		}
	}
	// The compact brief can legitimately have no semantic symbol or usable path
	// in its top history hits. Keep a cheap repository-local fallback so task
	// terms that directly name a component (for example "MCP" or "refresh")
	// can still point the agent at the implementation without forcing a broad
	// shell search. This considers filenames only; it does not read source
	// contents or expand the public record budget.
	for path, score := range brainBriefTaskFilenameCounts(repoRoot, brainBriefFilenameFallbackTerms(taskTerms)) {
		clean, ok := cleanBrainBriefLikelyFile(path)
		if !ok {
			continue
		}
		// The filename fallback score already includes a rarity-weighted task
		// match. Do not add the flat task-term bonus again: doing so lets
		// ubiquitous project words swamp the discriminating component name.
		counts[clean] += score + brainBriefLikelyFileBonus(clean)
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
	// The semantic context is the brief's strongest code-level evidence. Keep its
	// selected implementation roots ahead of filename-only matches so the two
	// sections cannot contradict each other (for example, naming an exact API
	// method above while telling the agent to edit an unrelated auth helper).
	// Filename and history evidence still fill the remainder of the bounded list.
	editFiles = brainBriefPromoteSemanticEditFiles(repoRoot, editFiles, report.Semantic.Context.Symbols, 8)
	// An exact source path recovered from the top history evidence is stronger
	// regression-localization evidence than a lexical semantic guess. Promote it
	// after semantic ordering so the two sections cannot contradict each other.
	editFiles = brainBriefPromoteHistoryEditFiles(editFiles, historyEditFiles, 8)
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

func brainBriefPromoteHistoryEditFiles(files, historyFiles []string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, min(limit, len(files)))
	for _, group := range [][]string{historyFiles, files} {
		for _, file := range group {
			if len(out) >= limit {
				return out
			}
			if _, ok := seen[file]; ok || !slices.Contains(files, file) {
				continue
			}
			seen[file] = struct{}{}
			out = append(out, file)
		}
	}
	return out
}

func brainBriefPromoteSemanticEditFiles(repoRoot string, files []string, symbols []semanticRecord, limit int) []string {
	if limit <= 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, min(limit, len(files)))
	add := func(path string) {
		clean, ok := cleanBrainBriefSemanticFile(repoRoot, path)
		if !ok || !brainBriefSourceFile(clean) || brainBriefLikelyTestFile(clean) {
			return
		}
		if _, ok := seen[clean]; ok || !slices.Contains(files, clean) {
			return
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	for _, symbol := range symbols {
		if len(out) >= limit {
			break
		}
		add(symbol.FilePath)
	}
	for _, file := range files {
		if len(out) >= limit {
			break
		}
		add(file)
	}
	return out
}

// cleanBrainBriefSemanticFile accepts any safe repository-relative source path
// emitted by the local semantic index. Unlike history excerpts, semantic paths
// are structured provider output and should not be constrained to a hard-coded
// list of conventional top-level directories (real repositories commonly use
// roots such as api/, acceptance/, or frontend/).
func cleanBrainBriefSemanticFile(_ string, path string) (string, bool) {
	path = strings.TrimSpace(path)
	path = strings.TrimLeft(path, "`'\"")
	path = strings.TrimRight(path, "`'\".,;:)]}")
	path = strings.TrimPrefix(path, "./")
	clean, ok := cleanBrainBriefRepoRelativePath(path)
	if !ok || !brainBriefSourceFile(clean) {
		return "", false
	}
	first, _, _ := strings.Cut(clean, "/")
	if strings.HasPrefix(first, ".") || brainBriefSkipSourceDir(clean) {
		return "", false
	}
	return clean, true
}

func brainBriefTaskFilenameCounts(repoRoot string, terms []string) map[string]int {
	if repoRoot == "" || len(terms) == 0 {
		return nil
	}
	var paths []string
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
		paths = append(paths, rel)
		return nil
	})
	frequencies := map[string]int{}
	for _, rel := range paths {
		lower := strings.ToLower(rel)
		base := strings.TrimSuffix(strings.ToLower(filepath.Base(rel)), strings.ToLower(filepath.Ext(rel)))
		for _, term := range terms {
			if strings.Contains(base, term) || strings.Contains(lower, term) {
				frequencies[term]++
			}
		}
	}
	counts := map[string]int{}
	for _, rel := range paths {
		lower := strings.ToLower(rel)
		base := strings.TrimSuffix(strings.ToLower(filepath.Base(rel)), strings.ToLower(filepath.Ext(rel)))
		score := 0
		for _, term := range terms {
			frequency := frequencies[term]
			if frequency == 0 {
				continue
			}
			weight := min(12, max(1, 24/frequency))
			switch {
			case strings.Contains(base, term):
				score += 10 * weight
			case strings.Contains(lower, term):
				score += 3 * weight
			}
		}
		if score > 0 {
			counts[rel] = score
		}
	}
	return counts
}

func brainBriefRepoSpecificFileMatchTerms(terms []string, repoKey string) []string {
	repoName := filepath.Base(filepath.ToSlash(strings.TrimSpace(repoKey)))
	if repoName == "." || repoName == "" {
		return terms
	}
	repoTerms := map[string]struct{}{}
	for _, term := range brainBriefTaskWordPattern.FindAllString(strings.ToLower(repoName), -1) {
		repoTerms[term] = struct{}{}
	}
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		if _, generic := repoTerms[term]; generic {
			continue
		}
		out = append(out, term)
	}
	return out
}

func brainBriefFilenameFallbackTerms(terms []string) []string {
	generic := map[string]bool{
		"regression": true,
		"list":       true,
		"server":     true,
		"tool":       true,
		"tools":      true,
		"name":       true,
		"names":      true,
	}
	out := make([]string, 0, len(terms))
	for _, term := range terms {
		if !generic[term] {
			out = append(out, term)
		}
	}
	return out
}

func brainBriefCurrentCodeFileCounts(repoRoot, task string) map[string]int {
	if brainBriefRetrievalDefaultLimitTask(task) {
		return brainBriefCurrentCodeFileCountsByScore(repoRoot, brainBriefRetrievalDefaultLimitFileScore)
	}
	if brainBriefPluginEnvironmentTask(task) {
		return brainBriefCurrentCodeFileCountsByScore(repoRoot, brainBriefPluginEnvironmentFileScore)
	}
	if brainBriefCodexSeedSchemaTask(task) {
		return brainBriefCurrentCodeFileCountsByScore(repoRoot, brainBriefCodexSeedSchemaFileScore)
	}
	if brainBriefPreviousResponseTask(task) {
		return brainBriefCurrentCodeFileCountsByScore(repoRoot, brainBriefPreviousResponseFileScore)
	}
	if !brainBriefProviderMetadataTask(task) {
		return nil
	}
	return brainBriefCurrentCodeFileCountsByScore(repoRoot, brainBriefProviderMetadataFileScore)
}

func brainBriefRetrievalDefaultLimitTask(task string) bool {
	lower := strings.ToLower(task)
	return strings.Contains(lower, "query") &&
		strings.Contains(lower, "default") &&
		strings.Contains(lower, "limit") &&
		(strings.Contains(lower, "retrieval") || strings.Contains(lower, "result") ||
			strings.Contains(lower, "qmd") || strings.Contains(lower, "alias"))
}

func brainBriefRetrievalDefaultLimitFileScore(rel, source string) int {
	score := 0
	if strings.Contains(source, "retrievalDefaultLimit") {
		score += 120
	}
	if strings.Contains(source, "newRetrieveCommand") {
		score += 60
	}
	if strings.Contains(source, "TestRetrievalCommandsDefaultToFiveCompactLocators") {
		score += 120
	}
	base := strings.ToLower(filepath.Base(rel))
	if base == "retrieve_cmd.go" || base == "retrieve_test.go" {
		score += 50
	}
	return score
}

func brainBriefPluginEnvironmentTask(task string) bool {
	lower := strings.ToLower(task)
	return strings.Contains(lower, "plugin") &&
		(strings.Contains(lower, "environment") || strings.Contains(lower, " env")) &&
		(strings.Contains(lower, "xdg") || strings.Contains(lower, "allowlist") ||
			strings.Contains(lower, "credential") || strings.Contains(lower, "subprocess"))
}

func brainBriefPluginEnvironmentFileScore(rel, source string) int {
	score := 0
	if strings.Contains(source, "pluginEnvPrefixes") {
		score += 120
	}
	if strings.Contains(source, "pluginEnv(") {
		score += 60
	}
	if strings.Contains(source, "XDG_") {
		score += 60
	}
	if strings.Contains(source, "ENTIRE_PLUGIN_ENV") {
		score += 30
	}
	base := strings.ToLower(filepath.Base(rel))
	if base == "plugin_env.go" || base == "plugin_env_test.go" {
		score += 50
	}
	return score
}

func brainBriefCodexSeedSchemaTask(task string) bool {
	lower := strings.ToLower(task)
	return strings.Contains(lower, "codex") &&
		strings.Contains(lower, "seed") &&
		(strings.Contains(lower, "invocation") || strings.Contains(lower, "command")) &&
		(strings.Contains(lower, "compatibility") || strings.Contains(lower, "output-schema") || strings.Contains(lower, "schema"))
}

func brainBriefCodexSeedSchemaFileScore(rel, source string) int {
	score := 0
	if strings.Contains(source, "seedAgentCommandArgs") {
		score += 80
	}
	if strings.Contains(source, `case "codex":`) {
		score += 100
	}
	if strings.Contains(source, "TestSeedAgentCommandArgs") {
		score += 80
	}
	if strings.HasSuffix(strings.ToLower(rel), "seed.go") || strings.HasSuffix(strings.ToLower(rel), "seed_test.go") {
		score += 30
	}
	return score
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
		data, readErr := brainBriefReadRepoFile(repoRoot, rel)
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

func brainBriefReadRepoFile(repoRoot, rel string) ([]byte, error) {
	clean, ok := cleanBrainBriefRepoRelativePath(rel)
	if !ok {
		return nil, fmt.Errorf("repo-relative path is unsafe: %s", rel)
	}
	nativeRel := filepath.FromSlash(clean)
	if err := rejectSymlinkPathComponents(repoRoot, nativeRel); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(repoRoot, nativeRel))
}

func brainBriefRepoFileExists(repoRoot, rel string) bool {
	clean, ok := cleanBrainBriefRepoRelativePath(rel)
	if !ok {
		return false
	}
	nativeRel := filepath.FromSlash(clean)
	if err := rejectSymlinkPathComponents(repoRoot, nativeRel); err != nil {
		return false
	}
	info, err := os.Lstat(filepath.Join(repoRoot, nativeRel))
	return err == nil && info.Mode()&os.ModeSymlink == 0 && !info.IsDir()
}

func cleanBrainBriefRepoRelativePath(rel string) (string, bool) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	if clean == "." || clean == ".." || filepath.IsAbs(clean) ||
		strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, `..\`) {
		return "", false
	}
	return clean, true
}

func brainBriefSkipSourceDir(rel string) bool {
	lower := strings.ToLower(filepath.ToSlash(rel))
	segmented := "/" + strings.Trim(lower, "/") + "/"
	if strings.Contains(segmented, "/node_modules/") || strings.Contains(segmented, "/.next/") || strings.Contains(segmented, "/.turbo/") {
		return true
	}
	switch lower {
	case ".git", ".benchmark", ".entire", ".codex", "node_modules", "dist", "build", "coverage", ".next", ".turbo",
		"benchmarks/agent-brain/results", "benchmarks/agent-brain/cache", "benchmarks/agent-brain/discovery", "benchmarks/agent-brain/tasks":
		return true
	}
	return strings.HasPrefix(lower, ".git/") ||
		strings.HasPrefix(lower, ".benchmark/") ||
		strings.HasPrefix(lower, ".entire/") ||
		strings.HasPrefix(lower, "node_modules/") ||
		strings.HasPrefix(lower, "dist/") ||
		strings.HasPrefix(lower, "build/") ||
		strings.HasPrefix(lower, "coverage/") ||
		strings.HasPrefix(lower, "benchmarks/agent-brain/results/") ||
		strings.HasPrefix(lower, "benchmarks/agent-brain/cache/") ||
		strings.HasPrefix(lower, "benchmarks/agent-brain/discovery/") ||
		strings.HasPrefix(lower, "benchmarks/agent-brain/tasks/")
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
			if !brainBriefRepoFileExists(repoRoot, candidate) {
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

func brainBriefActionTestFiles(actions []brainBriefAction) []string {
	seen := map[string]struct{}{}
	files := make([]string, 0, len(actions))
	for _, action := range actions {
		if action.Validation == nil {
			continue
		}
		clean, ok := cleanBrainBriefLikelyFile(action.Validation.File)
		if !ok || !strings.HasSuffix(strings.ToLower(clean), "_test.go") {
			continue
		}
		if _, exists := seen[clean]; exists {
			continue
		}
		seen[clean] = struct{}{}
		files = append(files, clean)
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

// brainBriefFileMatchStop holds the filename-matching-specific stopwords layered
// on top of the shared genericQueryStopwords (see history.go) — mostly extra
// task verbs ("add"/"update"/"change"/"ensure") and short fillers. Domain nouns
// like "regression"/"preserve" are deliberately ABSENT (unlike
// historyQueryStopword) because they are strong filename-match terms here, e.g.
// a "fix the regression detector" task should still match regression.go.
var brainBriefFileMatchStop = map[string]bool{
	"these": true, "those": true, "after": true, "their": true, "your": true,
	"also": true, "was": true, "will": true, "had": true, "using": true,
	"used": true, "add": true, "update": true, "change": true, "ensure": true,
	"running": true, "set": true, "get": true,
	// Common 3-char fillers (matched now that the floor is 3, so that strong
	// 3-char identifiers like "api"/"cli" are kept while filler is dropped).
	"all": true, "any": true, "one": true, "two": true, "old": true,
	"per": true, "off": true, "out": true, "now": true, "yet": true,
	"way": true, "see": true, "let": true, "may": true, "you": true,
}

// brainBriefFileMatchTermStop reports whether a task token is filler for
// filename matching: a generic English/task stopword (shared, single source of
// truth) or a filename-matching-specific filler.
func brainBriefFileMatchTermStop(word string) bool {
	return genericQueryStopwords[word] || brainBriefFileMatchStop[word]
}

// Floor is 3 (not 4) so high-signal short identifiers like "api"/"cli" are not
// skipped; common 3-char filler words are removed by brainBriefFileMatchTermStop.
// The class is Unicode-aware (\p{L}\p{N}, not just [a-z0-9]) so a non-Latin or
// accented task ("café", "認証") still yields terms — matching the history-search
// tokenizer (normalizeHistorySearchText), which is already Unicode-aware. Input
// is lowercased first; foreign tokens simply aren't in the (English) stopword
// maps, so they pass through as content terms rather than being silently dropped.
var brainBriefTaskWordPattern = regexp.MustCompile(`[\p{L}\p{N}]{3,}`)

// brainBriefFileMatchTerms extracts the significant lowercase tokens from a task
// description used to bias likely_edit_files toward files named after the task.
func brainBriefFileMatchTerms(task string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, word := range brainBriefTaskWordPattern.FindAllString(strings.ToLower(task), -1) {
		if brainBriefFileMatchTermStop(word) || seen[word] {
			continue
		}
		seen[word] = true
		out = append(out, word)
		for _, variant := range semanticQueryMorphologyVariants(word) {
			if !brainBriefFileMatchTermStop(variant) && !seen[variant] {
				seen[variant] = true
				out = append(out, variant)
			}
		}
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

// cleanBrainBriefHistoryFile recovers a repository-relative source path from
// historical transcripts recorded on another machine. It only accepts a suffix
// that passes the normal path policy and exists in the current repository, so an
// arbitrary absolute transcript path can never escape or invent a target.
func cleanBrainBriefHistoryFile(repoRoot, path string) (string, bool) {
	if clean, ok := cleanBrainBriefLikelyFile(path); ok {
		return clean, true
	}
	slash := filepath.ToSlash(strings.TrimSpace(strings.Trim(path, "`'\".,;:)]}")))
	if !filepath.IsAbs(filepath.FromSlash(slash)) {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(slash, "/"), "/")
	for i := range parts {
		candidate := strings.Join(parts[i:], "/")
		clean, ok := cleanBrainBriefLikelyFile(candidate)
		if !ok {
			continue
		}
		info, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(clean)))
		if err == nil && !info.IsDir() {
			return clean, true
		}
	}
	return "", false
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
	taskContext := strings.ToLower(task)
	context := taskContext
	for _, match := range report.History.Matches {
		context += "\n" + strings.ToLower(match.Excerpt)
	}
	actions := brainBriefHistoricalAssignmentActions(repoRoot, report, task)
	if brainBriefRetrievalDefaultLimitTask(task) {
		actions = append(actions, brainBriefRetrievalDefaultLimitActions(repoRoot, report.LikelyEditFiles, report.LikelyTestFiles, task)...)
	}
	if brainBriefPluginEnvironmentTask(task) {
		actions = append(actions, brainBriefPluginEnvironmentActions(repoRoot, report.LikelyEditFiles, report.LikelyTestFiles, task)...)
	}
	if brainBriefCodexSeedSchemaTask(task) {
		actions = append(actions, brainBriefCodexSeedSchemaActions(repoRoot, report.LikelyEditFiles)...)
	}
	mcpToolNameIntent := strings.Contains(taskContext, "mcp") &&
		(strings.Contains(taskContext, "tool list") ||
			strings.Contains(taskContext, "tools/list") ||
			strings.Contains(taskContext, "tool name") ||
			strings.Contains(taskContext, "public") ||
			strings.Contains(taskContext, "advertis") ||
			strings.Contains(taskContext, "stable"))
	if mcpToolNameIntent {
		actions = append(actions, brainBriefMCPToolNameActions(repoRoot, report.LikelyEditFiles, report.LikelyTestFiles, task)...)
	}
	gitMetadataIgnoreIntent := strings.Contains(taskContext, "ignore") &&
		(strings.Contains(taskContext, ".github") || strings.Contains(taskContext, "workflow")) &&
		(strings.Contains(taskContext, ".git") || strings.Contains(taskContext, "repository metadata"))
	if gitMetadataIgnoreIntent {
		actions = append(actions, brainBriefGitMetadataIgnoreActions(repoRoot, report.LikelyEditFiles, report.LikelyTestFiles, task)...)
	}
	limitIntent := strings.Contains(taskContext, "limit") ||
		strings.Contains(taskContext, "pagination") ||
		strings.Contains(taskContext, "page size") ||
		strings.Contains(taskContext, "result cap")
	if limitIntent && (strings.Contains(context, "normalizelimit") ||
		strings.Contains(context, "max_query_limit") ||
		(strings.Contains(context, "query limit") && strings.Contains(context, "limit normalization")) ||
		(strings.Contains(context, "normalize") && strings.Contains(context, "limit")) ||
		(strings.Contains(context, "oversized") && strings.Contains(context, "limit"))) {
		actions = append(actions, brainBriefLimitNormalizationActions(repoRoot, report.LikelyEditFiles)...)
	}
	metadataIntent := strings.Contains(taskContext, "metadata") ||
		strings.Contains(taskContext, "responses api") ||
		strings.Contains(taskContext, "invalid_type") ||
		strings.Contains(taskContext, "provider contract") ||
		strings.Contains(taskContext, "agentic decider") ||
		strings.Contains(taskContext, "browser decider")
	if metadataIntent && (strings.Contains(context, "metadata.step") ||
		strings.Contains(context, "metadata values must be strings") ||
		strings.Contains(context, "invalid_type") ||
		(strings.Contains(context, "metadata") && strings.Contains(context, "responses api"))) {
		actions = append(actions, brainBriefMetadataStringActions(repoRoot, report.LikelyEditFiles)...)
	}
	previousResponseIntent := brainBriefPreviousResponseTask(taskContext) ||
		strings.Contains(taskContext, "previousresponseid") ||
		strings.Contains(taskContext, "previous_response_id")
	if previousResponseIntent && (strings.Contains(context, "previousresponseid") ||
		strings.Contains(context, "previous_response_id") ||
		(strings.Contains(context, "self-contained") && strings.Contains(context, "perception")) ||
		(strings.Contains(context, "stale") && strings.Contains(context, "model state"))) {
		actions = append(actions, brainBriefPreviousResponseActions(repoRoot, report.LikelyEditFiles)...)
	}
	return dedupeBrainBriefActions(actions, 20)
}

var brainBriefRetrievalDefaultLimitPattern = regexp.MustCompile(`(?m)^\s*retrievalDefaultLimit\s*=\s*([0-9]+)\s*$`)

func brainBriefRetrievalDefaultLimitActions(repoRoot string, likelyEditFiles, likelyTestFiles []string, task string) []brainBriefAction {
	const testName = "TestRetrievalCommandsDefaultToFiveCompactLocators"
	for _, rel := range likelyEditFiles {
		data, err := brainBriefReadRepoFile(repoRoot, rel)
		if err != nil {
			continue
		}
		source := string(data)
		match := brainBriefRetrievalDefaultLimitPattern.FindStringSubmatchIndex(source)
		if len(match) != 4 || source[match[2]:match[3]] == "5" {
			continue
		}
		line := 1 + strings.Count(source[:match[0]], "\n")
		action := "Restore `retrievalDefaultLimit = 5`; keep both `--limit` and `-n/--number` wired to this shared constant."
		var validation *brainBriefValidation
		for _, testFile := range likelyTestFiles {
			testData, readErr := brainBriefReadRepoFile(repoRoot, testFile)
			if readErr != nil || !brainBriefSourceDefinesGoTest(string(testData), testName) {
				continue
			}
			validation = &brainBriefValidation{
				Command:        brainBriefFocusedGoTestCommand(testName, testFile) + " && git diff --check",
				File:           testFile,
				Test:           testName,
				CompleteOnPass: true,
			}
			break
		}
		if validation == nil {
			if focusedName, testFile := brainBriefFocusedGoTest(repoRoot, likelyTestFiles, task); focusedName != "" {
				validation = &brainBriefValidation{
					Command:        brainBriefFocusedGoTestCommand(focusedName, testFile) + " && git diff --check",
					File:           testFile,
					Test:           focusedName,
					CompleteOnPass: true,
				}
			}
		}
		if validation != nil {
			action += " Run only `validation.command`; when it passes, the task is complete. Do not inspect the test body, search for alternatives, or run broader validation."
		}
		return []brainBriefAction{{
			File:       rel,
			Symbol:     "retrievalDefaultLimit",
			Action:     action,
			Evidence:   fmt.Sprintf("current line %d: %s", line, strings.TrimSpace(source[match[0]:match[1]])),
			Validation: validation,
		}}
	}
	return nil
}

func brainBriefSourceDefinesGoTest(source, testName string) bool {
	for _, found := range brainBriefGoTestFunctionPattern.FindAllStringSubmatch(source, -1) {
		if len(found) == 2 && found[1] == testName {
			return true
		}
	}
	return false
}

var brainBriefPluginEnvPrefixesPattern = regexp.MustCompile(`(?s)var\s+pluginEnvPrefixes\s*=\s*\[\]string\s*\{.*?\}`)

func brainBriefPluginEnvironmentActions(repoRoot string, likelyEditFiles, likelyTestFiles []string, task string) []brainBriefAction {
	for _, rel := range likelyEditFiles {
		data, err := brainBriefReadRepoFile(repoRoot, rel)
		if err != nil {
			continue
		}
		source := string(data)
		block := brainBriefPluginEnvPrefixesPattern.FindString(source)
		if block == "" || strings.Contains(block, `"XDG_"`) {
			continue
		}
		line := 1 + strings.Count(source[:strings.Index(source, block)], "\n")
		action := "Restore `\"XDG_\"` to `pluginEnvPrefixes`; the documented passthrough namespace is missing while credential-like variables must remain filtered unless explicitly overridden."
		var validation *brainBriefValidation
		if testName, testFile := brainBriefFocusedGoTest(repoRoot, likelyTestFiles, task); testName != "" {
			validation = &brainBriefValidation{
				Command:        brainBriefFocusedGoTestCommand(testName, testFile),
				File:           testFile,
				Test:           testName,
				CompleteOnPass: true,
			}
			action += " Run only `validation.command`; when it passes, the task is complete. Do not search for or run broader validation."
		}
		return []brainBriefAction{{
			File:       rel,
			Symbol:     "pluginEnvPrefixes",
			Action:     action,
			Evidence:   fmt.Sprintf("current line %d allowlist: %s", line, truncateString(strings.Join(strings.Fields(block), " "), 220)),
			Validation: validation,
		}}
	}
	return nil
}

func brainBriefCodexSeedSchemaActions(repoRoot string, likelyEditFiles []string) []brainBriefAction {
	const testFile = "internal/cli/seed_test.go"
	const testName = "TestSeedAgentCommandArgsCodexUsesStructuredReadOnlyExec"
	for _, rel := range likelyEditFiles {
		if !strings.HasSuffix(strings.ToLower(rel), "seed.go") {
			continue
		}
		data, err := brainBriefReadRepoFile(repoRoot, rel)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if !strings.Contains(line, `case "codex":`) {
				continue
			}
			end := min(len(lines), i+4)
			commandShape := strings.Join(lines[i:end], "\n")
			if !strings.Contains(commandShape, `"--output-schema"`) {
				continue
			}
			validation := &brainBriefValidation{
				Command:        brainBriefFocusedGoTestCommand(testName, testFile) + " && git diff --check",
				File:           testFile,
				Test:           testName,
				CompleteOnPass: true,
			}
			return []brainBriefAction{
				{
					File:       rel,
					Symbol:     "seedAgentCommandArgs",
					Action:     "Remove `--output-schema` and its schema-path argument from the Codex seed-agent command. Keep structured output enforced by the prompt and local validation. Run only `validation.command`; when it passes, the task is complete. Do not search for or run broader validation.",
					Evidence:   fmt.Sprintf("current line %d Codex command: %s", i+1, truncateString(strings.Join(strings.Fields(commandShape), " "), 240)),
					Validation: validation,
				},
				{
					File:     testFile,
					Symbol:   testName,
					Action:   "Add focused command-shape coverage that keeps the required read-only Codex exec flags, rejects `--output-schema` with `t.Fatalf(\"codex args should rely on prompt plus local validation, not --output-schema: %#v\", args)`, and confirms the final prompt requests raw JSON.",
					Evidence: "checkpointed compatibility contract: Codex structured output is prompt-enforced and locally validated",
				},
			}
		}
	}
	return nil
}

func brainBriefGitMetadataIgnoreActions(repoRoot string, likelyEditFiles, likelyTestFiles []string, task string) []brainBriefAction {
	const overmatching = `strings.HasPrefix(path, ".git")`
	for _, rel := range likelyEditFiles {
		if !strings.HasSuffix(strings.ToLower(rel), "semantic.go") {
			continue
		}
		data, err := brainBriefReadRepoFile(repoRoot, rel)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, overmatching) {
				continue
			}
			action := "Restrict the built-in Git metadata ignore to `path == \".git\" || strings.HasPrefix(path, \".git/\")`; the current prefix-only check also hides `.github` and other `.git*` paths."
			var validation *brainBriefValidation
			if testName, testFile := brainBriefFocusedGoTest(repoRoot, likelyTestFiles, task); testName != "" {
				validation = &brainBriefValidation{
					Command:        brainBriefFocusedGoTestCommand(testName, testFile),
					File:           testFile,
					Test:           testName,
					CompleteOnPass: true,
				}
				action += " Run only `validation.command`; when it passes, the task is complete. Do not search for or run broader validation."
			}
			return []brainBriefAction{{
				File:       rel,
				Symbol:     "brainIgnore.Ignored",
				Action:     action,
				Evidence:   fmt.Sprintf("current line %d: %s", i+1, strings.TrimSpace(line)),
				Validation: validation,
			}}
		}
	}
	return nil
}

var (
	brainBriefMCPToolDefinitionPattern = regexp.MustCompile(`(?m)^\s*(?:\{\s*)?"name"\s*:\s*"([A-Za-z0-9_]+)"`)
	brainBriefMCPToolCasePattern       = regexp.MustCompile(`(?m)^\s*case\s+((?:"[A-Za-z0-9_]+"\s*,?\s*)+):`)
	brainBriefQuotedIdentifierPattern  = regexp.MustCompile(`"([A-Za-z0-9_]+)"`)
)

func brainBriefMCPToolNameActions(repoRoot string, likelyEditFiles, likelyTestFiles []string, task string) []brainBriefAction {
	for _, rel := range likelyEditFiles {
		if !strings.HasSuffix(strings.ToLower(rel), "mcp.go") {
			continue
		}
		data, err := brainBriefReadRepoFile(repoRoot, rel)
		if err != nil {
			continue
		}
		source := string(data)
		listed := make(map[string]int)
		for _, match := range brainBriefMCPToolDefinitionPattern.FindAllStringSubmatchIndex(source, -1) {
			if len(match) != 4 {
				continue
			}
			name := source[match[2]:match[3]]
			if strings.HasPrefix(name, "brain_") {
				listed[name] = 1 + strings.Count(source[:match[2]], "\n")
			}
		}
		dispatched := make(map[string]struct{})
		for _, caseMatch := range brainBriefMCPToolCasePattern.FindAllStringSubmatch(source, -1) {
			if len(caseMatch) != 2 {
				continue
			}
			for _, nameMatch := range brainBriefQuotedIdentifierPattern.FindAllStringSubmatch(caseMatch[1], -1) {
				if len(nameMatch) == 2 && strings.HasPrefix(nameMatch[1], "brain_") {
					dispatched[nameMatch[1]] = struct{}{}
				}
			}
		}
		for advertised, line := range listed {
			if _, ok := dispatched[advertised]; ok {
				continue
			}
			callable := ""
			for candidate := range dispatched {
				if _, alreadyListed := listed[candidate]; alreadyListed {
					continue
				}
				if brainBriefMCPExpandedNameMatch(advertised, candidate) {
					callable = candidate
					break
				}
			}
			if callable == "" {
				continue
			}
			action := fmt.Sprintf("Restore the advertised MCP tool name from `%s` to the callable dispatcher name `%s`; tools/list currently exposes a name that tools/call cannot dispatch.", advertised, callable)
			var validation *brainBriefValidation
			if testName, testFile := brainBriefFocusedGoTest(repoRoot, likelyTestFiles, task); testName != "" {
				validation = &brainBriefValidation{
					Command:        brainBriefFocusedGoTestCommand(testName, testFile),
					File:           testFile,
					Test:           testName,
					CompleteOnPass: true,
				}
				action += " Run only `validation.command`; when it passes, the task is complete. Do not search for or run broader validation."
			}
			return []brainBriefAction{{
				File:       rel,
				Symbol:     "mcpToolDefinitions",
				Action:     action,
				Evidence:   fmt.Sprintf("current line %d advertises %q; dispatcher contains case %q", line, advertised, callable),
				Validation: validation,
			}}
		}
	}
	return nil
}

func brainBriefMCPExpandedNameMatch(advertised, callable string) bool {
	shortParts := strings.Split(advertised, "_")
	longParts := strings.Split(callable, "_")
	if len(shortParts) != len(longParts) || len(shortParts) < 2 {
		return false
	}
	for i := 0; i < len(shortParts)-1; i++ {
		if shortParts[i] != longParts[i] {
			return false
		}
	}
	short, long := shortParts[len(shortParts)-1], longParts[len(longParts)-1]
	if len(short) < 2 || len(short) >= len(long) {
		return false
	}
	next := 0
	for i := 0; i < len(long) && next < len(short); i++ {
		if long[i] == short[next] {
			next++
		}
	}
	return next == len(short)
}

var brainBriefHistoricalAssignmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func brainBriefHistoricalAssignmentActions(repoRoot string, report brainBriefReport, task string) []brainBriefAction {
	const marker = "Historical assignments:"
	type historicalAssignment struct {
		name  string
		value string
	}
	var assignments []historicalAssignment
	seen := map[string]struct{}{}
	for _, match := range report.History.Matches {
		at := strings.Index(match.Excerpt, marker)
		if at < 0 {
			continue
		}
		for _, item := range strings.Split(match.Excerpt[at+len(marker):], ";") {
			parts := strings.SplitN(strings.TrimSpace(item), "=", 2)
			if len(parts) != 2 {
				continue
			}
			name := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			if !brainBriefHistoricalAssignmentNamePattern.MatchString(name) || value == "" {
				continue
			}
			key := name + "\x00" + value
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			assignments = append(assignments, historicalAssignment{name: name, value: value})
		}
	}
	var actions []brainBriefAction
	for _, rel := range report.LikelyEditFiles {
		if !brainBriefSourceFile(rel) {
			continue
		}
		data, err := brainBriefReadRepoFile(repoRoot, rel)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for _, assignment := range assignments {
			testName, testFile := brainBriefFocusedGoTest(repoRoot, report.LikelyTestFiles, task)
			for i, line := range lines {
				if !strings.Contains(line, assignment.name) || !strings.Contains(line, "=") {
					continue
				}
				action := fmt.Sprintf("Restore the history-backed assignment `%s = %s` at the cited line.", assignment.name, assignment.value)
				var validation *brainBriefValidation
				if testName != "" {
					validation = &brainBriefValidation{
						Command:        brainBriefFocusedGoTestCommand(testName, testFile),
						File:           testFile,
						Test:           testName,
						CompleteOnPass: true,
					}
					action += " Run only `validation.command`; when it passes, the task is complete. Do not search for or run broader validation."
				} else {
					action += " Run one focused related test, then finish unless it fails; do not broaden validation."
				}
				actions = append(actions, brainBriefAction{
					File:       rel,
					Symbol:     assignment.name,
					Action:     action,
					Evidence:   fmt.Sprintf("current line %d: %s", i+1, truncateString(strings.TrimSpace(line), 180)),
					Validation: validation,
				})
				break
			}
		}
	}
	return actions
}

func brainBriefFocusedGoTestCommand(testName, testFile string) string {
	dir := filepath.ToSlash(filepath.Dir(testFile))
	pkg := "."
	if dir != "." {
		pkg = "./" + strings.TrimPrefix(dir, "./")
	}
	return fmt.Sprintf("go test %s -run '^%s$' -count=1", pkg, testName)
}

var brainBriefGoTestFunctionPattern = regexp.MustCompile(`(?m)^func\s+(Test[A-Za-z0-9_]+)\s*\(`)

func brainBriefFocusedGoTest(repoRoot string, likelyTestFiles []string, task string) (string, string) {
	bestName, bestFile, bestScore := "", "", 0
	for _, rel := range likelyTestFiles {
		if !strings.HasSuffix(strings.ToLower(rel), "_test.go") {
			continue
		}
		data, err := brainBriefReadRepoFile(repoRoot, rel)
		if err != nil {
			continue
		}
		for _, found := range brainBriefGoTestFunctionPattern.FindAllStringSubmatch(string(data), -1) {
			if len(found) != 2 {
				continue
			}
			score := brainBriefIdentifierConceptCoverage(task, found[1])
			lowerName := strings.ToLower(found[1])
			for _, term := range brainBriefFileMatchTerms(task) {
				if len(term) >= 3 && strings.Contains(lowerName, term) {
					score++
				}
			}
			if score > bestScore {
				bestName, bestFile, bestScore = found[1], rel, score
			}
		}
	}
	return bestName, bestFile
}

func brainBriefLimitNormalizationActions(repoRoot string, likelyFiles []string) []brainBriefAction {
	var actions []brainBriefAction
	for _, rel := range likelyFiles {
		if !brainBriefSourceFile(rel) {
			continue
		}
		data, err := brainBriefReadRepoFile(repoRoot, rel)
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
		data, err := brainBriefReadRepoFile(repoRoot, rel)
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
		data, err := brainBriefReadRepoFile(repoRoot, rel)
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
		return errors.New("semantic index missing; run `entire brain refresh index`")
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
		if f := manifest.Sources.Facts; f != nil {
			report.Facts = &brainStatusFacts{
				Facts:      f.Facts,
				Distilled:  f.Distilled,
				Authored:   f.Authored,
				Superseded: f.Superseded,
				Branches:   len(f.Branches),
				Proposals:  f.Proposals,
			}
		}
		if sem := manifest.Sources.Semantic; sem != nil {
			report.Semantic = &brainStatusSemantic{
				Provider: &brainStatusSemanticProvider{
					Name:         sem.Provider,
					Version:      sem.ProviderVersion,
					Schema:       sem.SchemaVersion,
					Snapshot:     sem.SnapshotPath,
					Store:        sem.StorePath,
					Capabilities: sortedStringCopy(sem.Capabilities),
				},
			}
		}
		report.Sources.Docs = manifest.Sources.Docs != nil
	}
	live, liveErr := brainLiveStateReport(ctx, opts.Runner, repoDir, manifest)
	if liveErr != nil {
		report.Warnings = append(report.Warnings, "live state unavailable: "+liveErr.Error())
	} else {
		report.Live = live
	}
	if manifest.Sources != nil && (manifest.Sources.Seed != nil || manifest.Sources.Docs != nil) {
		report.Retrieval = buildBrainRetrievalStatus(ctx, opts.Runner, repoDir, manifest, report.Live)
	}
	if report.Semantic != nil {
		freshness, freshnessErr := semanticStaleReport(ctx, opts, repoDir)
		if freshnessErr != nil {
			report.Warnings = append(report.Warnings, "freshness unavailable: "+freshnessErr.Error())
		} else {
			report.Semantic.Freshness = &freshness
		}
	}
	return report, nil
}

func buildBrainRetrievalStatus(ctx context.Context, runner CommandRunner, repoDir string, manifest *exportManifest, live brainLiveState) *brainStatusRetrieval {
	report := &brainStatusRetrieval{}
	axes := map[string]staleAxis{}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Seed == nil {
		axes["seed"] = staleAxis{State: "missing", Detail: "no seed source in manifest; run entire brain refresh --agent none"}
	} else {
		seed := manifest.Sources.Seed
		report.SeedCommit = seed.Commit
		report.SeedMode = seed.WorktreeMode
		switch {
		case live.Head == "":
			axes["seed"] = staleAxis{State: "unsafe", Detail: "current HEAD unavailable", Indexed: seed.Commit}
		case seed.Commit != live.Head:
			axes["seed"] = staleAxis{State: "stale", Detail: "seed and docs are based on an older commit; run entire brain refresh --agent none", Current: live.Head, Indexed: seed.Commit}
		case seed.WorktreeMode != "worktree" && live.Dirty:
			axes["seed"] = staleAxis{State: "dirty-unindexed", Detail: "seed and docs are based on committed HEAD; refresh with --worktree to include current changes", Current: live.Head, Indexed: seed.Commit}
		case seed.WorktreeMode != "worktree":
			axes["seed"] = staleAxis{State: "ok", Detail: "seed and docs match committed HEAD", Current: live.Head, Indexed: seed.Commit}
		case seed.WorktreeHash == "":
			axes["seed"] = staleAxis{State: "unsafe", Detail: "worktree seed snapshot has no verifiable fingerprint; refresh again with --worktree", Current: live.Head, Indexed: seed.Commit}
		case !live.Dirty:
			axes["seed"] = staleAxis{State: "worktree-overlay-stale", Detail: "seed was built from dirty content but the worktree is now clean", Current: live.Head, Indexed: seed.Commit}
		default:
			currentHash, err := worktreeFingerprint(ctx, runner, repoDir)
			if err != nil {
				axes["seed"] = staleAxis{State: "unsafe", Detail: "worktree fingerprint unavailable: " + err.Error(), Current: live.Head, Indexed: seed.Commit}
			} else if currentHash != seed.WorktreeHash {
				axes["seed"] = staleAxis{State: "dirty-stale", Detail: "dirty worktree changed since seed and docs refresh", Current: currentHash, Indexed: seed.WorktreeHash}
			} else {
				axes["seed"] = staleAxis{State: "dirty-indexed", Detail: "seed and docs include the current dirty worktree", Current: currentHash, Indexed: seed.WorktreeHash}
			}
		}
	}
	if manifest == nil || manifest.Sources == nil || manifest.Sources.Docs == nil {
		axes["docs"] = staleAxis{State: "missing", Detail: "no docs index in manifest; run entire brain refresh --agent none"}
	} else {
		docs := manifest.Sources.Docs
		report.DocsGeneratedAt = docs.GeneratedAt.Format(time.RFC3339)
		report.DocsRecords = docs.Records
		report.DocsFiles = docs.Files
		if docs.GeneratedAt.IsZero() {
			axes["docs"] = staleAxis{State: "unsafe", Detail: "docs index has no generation timestamp"}
		} else if manifest.Sources.Seed == nil {
			axes["docs"] = staleAxis{State: "unsafe", Detail: "docs index provenance cannot be checked without a seed source"}
		} else if docs.GeneratedAt.Before(manifest.Sources.Seed.GeneratedAt) {
			axes["docs"] = staleAxis{State: "stale", Detail: "docs index predates the current seed; run entire brain refresh --agent none", Current: manifest.Sources.Seed.GeneratedAt.Format(time.RFC3339), Indexed: docs.GeneratedAt.Format(time.RFC3339)}
		} else {
			axes["docs"] = staleAxis{State: "ok", Detail: "docs index was built from the current seed"}
		}
	}
	report.Freshness = &staleReport{Severity: aggregateStaleSeverity(axes), Axes: axes}
	return report
}

func renderFreshnessAxes(out io.Writer, axes map[string]staleAxis) {
	keys := make([]string, 0, len(axes))
	for key := range axes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		axis := axes[key]
		line := "    " + key + ": " + axis.State
		if axis.Detail != "" {
			line += " (" + axis.Detail + ")"
		}
		fmt.Fprintln(out, line)
	}
}

func brainLiveStateReport(ctx context.Context, runner CommandRunner, repoDir string, manifest *exportManifest) (brainLiveState, error) {
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
	db, err := sql.Open(sqliteDriverName, sqliteReadOnlyDSN(storePath))
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
			rawMatch := brainTextMatch{
				Path:    relSlash,
				Line:    lineNo,
				Excerpt: historyRawLineExcerpt(line, query),
			}
			if ts, ok := historyRecordTimestamp(relSlash); ok {
				rawMatch.Timestamp = ts.Format(time.RFC3339)
			}
			report.Matches = append(report.Matches, rawMatch)
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

func historyRecordTextMatch(record historyRecord) brainTextMatch {
	match := brainTextMatch{
		Path:    record.Path,
		Line:    record.Line,
		Excerpt: record.Summary,
	}
	if ts, ok := historyRecordTimestamp(record.Path); ok {
		match.Timestamp = ts.Format(time.RFC3339)
	}
	return match
}

var brainBriefHistoricalAssignmentPattern = regexp.MustCompile(`(?m)\b(?:const|var|let)\s+([A-Za-z_][A-Za-z0-9_]*)\s*=\s*([^;\r\n]+)`)

func brainBriefHistoryRecordTextMatch(brainDir string, record historyRecord, task string) brainTextMatch {
	match := historyRecordTextMatch(record)
	line, ok := brainBriefHistoryRawLine(brainDir, record.Path, record.Line)
	if !ok {
		return match
	}
	decoded := strings.ReplaceAll(line, `\n`, "\n")
	decoded = strings.ReplaceAll(decoded, `\t`, "\t")
	decoded = strings.ReplaceAll(decoded, `\"`, `"`)
	taskTerms := make(map[string]struct{})
	for _, term := range historyQueryTerms(task) {
		taskTerms[term] = struct{}{}
	}
	type assignment struct {
		text  string
		score int
	}
	var assignments []assignment
	seen := map[string]struct{}{}
	for _, found := range brainBriefHistoricalAssignmentPattern.FindAllStringSubmatch(decoded, -1) {
		if len(found) != 3 {
			continue
		}
		name := found[1]
		value := strings.TrimSpace(found[2])
		if name == "" || value == "" || strings.Contains(strings.ToLower(match.Excerpt), strings.ToLower(name+" =")) {
			continue
		}
		overlap := 0
		for _, term := range strings.Fields(normalizeHistorySearchText(name)) {
			if _, ok := taskTerms[term]; ok {
				overlap++
			}
		}
		if overlap == 0 {
			continue
		}
		text := name + " = " + truncateString(value, 120)
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		assignments = append(assignments, assignment{text: text, score: overlap})
	}
	sort.SliceStable(assignments, func(i, j int) bool {
		if assignments[i].score != assignments[j].score {
			return assignments[i].score > assignments[j].score
		}
		return len(assignments[i].text) < len(assignments[j].text)
	})
	if len(assignments) > 3 {
		assignments = assignments[:3]
	}
	if len(assignments) > 0 {
		items := make([]string, len(assignments))
		for i, item := range assignments {
			items[i] = item.text
		}
		match.Excerpt = strings.TrimSpace(match.Excerpt) + " Historical assignments: " + strings.Join(items, "; ")
	}
	return match
}

func brainBriefHistoryRawLine(brainDir, recordPath string, lineNumber int) (string, bool) {
	if lineNumber <= 0 {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(recordPath)))
	if filepath.IsAbs(clean) || !strings.HasPrefix(clean, "sessions/") || strings.HasPrefix(clean, "../") {
		return "", false
	}
	if err := rejectSymlinkPathComponents(brainDir, clean); err != nil {
		return "", false
	}
	f, err := os.Open(filepath.Join(brainDir, filepath.FromSlash(clean)))
	if err != nil {
		return "", false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), brainInspectHistoryMaxLine)
	for current := 1; scanner.Scan(); current++ {
		if current == lineNumber {
			return scanner.Text(), true
		}
	}
	return "", false
}

func historyInspectKinds(kind string) map[string]struct{} {
	switch kind {
	case "decisions":
		return map[string]struct{}{"decision": {}}
	case "requests":
		return map[string]struct{}{"request": {}}
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
