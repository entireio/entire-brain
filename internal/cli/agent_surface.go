package cli

import (
	"bufio"
	"bytes"
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
	// 4 MiB per line is far beyond any real history record while bounding the
	// buffer a single crafted line can force (was 16 MiB).
	brainInspectHistoryMaxLine = 4 * 1024 * 1024
)

type agentStatusOptions struct {
	json   bool
	failOn string
}

type brainBriefOptions struct {
	json        bool
	limit       int
	noSemantic  bool
	profileJSON string
	// packetFormat is set only by the MCP adapter. The CLI continues to select
	// between its existing text and JSON renderers with json above.
	packetFormat brainBriefPacketFormat
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
	GeneratedAt time.Time            `json:"generated_at"`
	Repo        brainStatusRepo      `json:"repo"`
	Brain       brainStatusBrain     `json:"brain"`
	Sources     brainStatusSources   `json:"sources"`
	Facts       *brainStatusFacts    `json:"facts,omitempty"`
	Semantic    *brainStatusSemantic `json:"semantic,omitempty"`
	Live        brainLiveState       `json:"live"`
	Warnings    []string             `json:"warnings,omitempty"`
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
	ActionChecklist []brainBriefAction  `json:"action_checklist,omitempty"`
	LikelyEditFiles []string            `json:"likely_edit_files,omitempty"`
	LikelyTestFiles []string            `json:"likely_test_files,omitempty"`
	LikelyFiles     []string            `json:"likely_files,omitempty"`
	Patterns        []patternView       `json:"patterns,omitempty"`
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
				if briefOpts.profileJSON != "" {
					return errors.New("--profile-json is only supported for task briefs, not --handoff")
				}
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
	cmd.Flags().IntVar(&briefOpts.limit, "limit", brainBriefDefaultLimit, "Maximum semantic records per section")
	cmd.Flags().BoolVar(&briefOpts.noSemantic, "no-semantic", false, "Disable embedding rerank for facts; use lexical ranking only")
	cmd.Flags().StringVar(&briefOpts.profileJSON, "profile-json", "", "Atomically write a privacy-safe performance profile sidecar (mode 0600)")
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
	impactOpts := semanticImpactOptions{limit: 200, depth: 1}
	cmd := &cobra.Command{
		Use:   "impact <symbol-or-text>",
		Short: "Traverse semantic relations for an impact set",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSemanticImpact(cmd.Context(), cmd, opts, impactOpts, args[0])
		},
	}
	// The impact set shares one budget with the matched roots, so a file query
	// that resolves to many roots can fill the budget before any downstream
	// neighbor is added. A generous default keeps room for the impacted set.
	cmd.Flags().IntVar(&impactOpts.limit, "limit", 200, "Maximum symbols to include")
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
	populateBrainStatusSemanticDetail(ctx, opts, &report)
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

// populateBrainStatusSemanticDetail fills the expensive semantic sections —
// store/snapshot coverage and blind spots — for the status command only.
// brief/overview/regressions share buildBrainStatusReport and must not pay for
// a store scan they never render.
func populateBrainStatusSemanticDetail(ctx context.Context, opts Options, report *brainStatusReport) {
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
	if spots, err := brainBlindSpotsForRepo(ctx, opts, report.Repo.Root); err != nil {
		report.Warnings = append(report.Warnings, "blind spots unavailable: "+err.Error())
	} else {
		report.Semantic.BlindSpots = spots
	}
}

func brainStatusFreshnessSeverity(report brainStatusReport) string {
	if report.Semantic == nil || report.Semantic.Freshness == nil {
		return ""
	}
	return report.Semantic.Freshness.Severity
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
	fmt.Fprintf(out, "  sources: seed=%t sessions=%t semantic=%t history=%t facts=%t\n",
		report.Sources.Seed, report.Sources.Sessions, report.Sources.Semantic, report.Sources.History, report.Sources.Facts)
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
	fmt.Fprintln(out, "\nLive")
	fmt.Fprintf(out, "  branch: %s\n", valueOrUnset(report.Live.Branch))
	fmt.Fprintf(out, "  head: %s\n", valueOrUnset(report.Live.Head))
	fmt.Fprintf(out, "  dirty: %t\n", report.Live.Dirty)
	if report.Live.DiffStat != "" {
		fmt.Fprintf(out, "  diff: %s\n", strings.TrimSpace(report.Live.DiffStat))
	}
	warnings := append(append([]string(nil), report.Warnings...), report.Live.Warnings...)
	if len(warnings) > 0 {
		fmt.Fprintln(out, "\nWarnings")
		for _, warning := range warnings {
			fmt.Fprintf(out, "  - %s\n", warning)
		}
	}
}

type brainBriefRawHistoryMatcher func(string, string, []brainTextMatch, int, *brainBriefProfileRawHistory) ([]brainTextMatch, error)

func runBrainBrief(ctx context.Context, cmd *cobra.Command, opts Options, briefOpts brainBriefOptions, task string) error {
	return runBrainBriefWithRawHistoryMatcher(ctx, cmd, opts, briefOpts, task, brainBriefRawHistoryMatchesObserved)
}

// runBrainBriefWithRawHistoryMatcher keeps the complete packet-building path
// measurable against the retained multi-scan reference. Product callers use
// runBrainBrief above, which always supplies the default matcher.
func runBrainBriefWithRawHistoryMatcher(ctx context.Context, cmd *cobra.Command, opts Options, briefOpts brainBriefOptions, task string, rawHistoryMatcher brainBriefRawHistoryMatcher) error {
	if briefOpts.limit <= 0 {
		return errors.New("--limit must be greater than zero")
	}
	var profile *brainBriefProfile
	if briefOpts.profileJSON != "" {
		profile = newBrainBriefProfiler()
	}
	target := agentSurfaceTarget(opts, nil)
	statusStarted := profile.start()
	status, err := buildBrainStatusReport(ctx, opts, target)
	statusErrors := 0
	if err != nil {
		statusErrors = 1
	}
	if profile != nil {
		statusOutputs := 1
		if err != nil {
			statusOutputs = 0
		}
		profile.finishStage(&profile.StatusBuildState, statusStarted, 1, statusOutputs, statusErrors)
	}
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
		semanticSource := status.Manifest.Sources.Semantic
		semanticInputCount := semanticSource.Symbols + semanticSource.Relations
		semanticLimit := briefOpts.limit
		if brainBriefSemanticNeedsCurrentBoundary(report.Status) {
			// A bounded overfetch lets the current-worktree boundary refill slots
			// which stale top-ranked records would otherwise consume.
			semanticLimit = brainBriefSemanticCandidateLimit(briefOpts.limit, semanticSource.Symbols)
		}
		contextStarted := profile.start()
		contextSymbols, contextRelations, contextNeighbors, contextErr := semanticContextFacts(status.Brain.Path, semanticSource, task, semanticLimit, 0)
		if contextErr != nil {
			report.Warnings = append(report.Warnings, "semantic context unavailable: "+contextErr.Error())
		} else {
			report.Semantic.Context = semanticContextResult{
				Symbols:   nonNil(contextSymbols),
				Relations: nonNil(contextRelations),
				Neighbors: nonNil(contextNeighbors),
			}
		}
		if profile != nil {
			contextErrors := 0
			if contextErr != nil {
				contextErrors = 1
			}
			profile.finishStage(&profile.Semantic.Context, contextStarted, semanticInputCount, len(contextSymbols)+len(contextRelations)+len(contextNeighbors), contextErrors)
		}
		runtimeStarted := profile.start()
		runtimeTraces, runtimeErr := semanticRuntimeTraceFacts(status.Brain.Path, semanticSource, task, semanticLimit)
		if runtimeErr != nil {
			report.Warnings = append(report.Warnings, "runtime trace context unavailable: "+runtimeErr.Error())
		} else {
			report.Semantic.RuntimeTraces = runtimeTraces
		}
		if profile != nil {
			runtimeErrors := 0
			if runtimeErr != nil {
				runtimeErrors = 1
			}
			profile.finishStage(&profile.Semantic.RuntimeTraces, runtimeStarted, semanticInputCount, len(runtimeTraces), runtimeErrors)
		}
		testsStarted := profile.start()
		tests, testsErr := semanticTestFacts(status.Brain.Path, semanticSource, task, semanticLimit)
		if testsErr != nil {
			report.Warnings = append(report.Warnings, "test suggestions unavailable: "+testsErr.Error())
		} else {
			report.Semantic.Tests = tests
		}
		if profile != nil {
			testErrors := 0
			if testsErr != nil {
				testErrors = 1
			}
			profile.finishStage(&profile.Semantic.Tests, testsStarted, semanticInputCount, len(tests.Roots)+len(tests.Suggestions), testErrors)
		}
	} else {
		report.Warnings = append(report.Warnings, "semantic index missing; run `entire brain refresh`")
	}
	if status.Manifest != nil && status.Manifest.Sources != nil && status.Manifest.Sources.History != nil {
		source := status.Manifest.Sources.History
		historyEmbedder := defaultEmbedder()
		fusionEnabled := historySemanticEmbedder(historyEmbedder) != nil
		var scoredHistory []scoredHistoryRecord
		var historyErr error

		if !fusionEnabled {
			directStarted := profile.start()
			var used bool
			derivedCorrupt := false
			scoredHistory, used, historyErr = rankHistoryViaFreshFTSCutoffDetailed(status.Brain.Path, source, "history", task, briefOpts.limit, historyFTSRelevanceCutoff)
			if errors.Is(historyErr, errHistoryFTSPayloadCorrupt) {
				derivedCorrupt = true
				historyErr = nil
			}
			if used || historyErr != nil {
				if profile != nil {
					historyErrors := 0
					if historyErr != nil {
						historyErrors = 1
					}
					profile.finishStage(&profile.History.IndexedRank, directStarted, source.Records, len(scoredHistory), historyErrors)
				}
			} else {
				// Legacy/stale/corrupt derived caches retain the exact old path:
				// verify and load index.json, rebuild FTS if possible, then use the
				// in-memory scorer if SQLite remains unavailable.
				indexLoadStarted := profile.start()
				var index historyIndex
				index, historyErr = loadBrainHistoryIndex(status.Brain.Path, source)
				if profile != nil {
					historyErrors := 0
					if historyErr != nil {
						historyErrors = 1
					}
					profile.finishStage(&profile.History.IndexLoad, indexLoadStarted, 1, len(index.Records), historyErrors)
				}
				if historyErr == nil {
					rankStarted := profile.start()
					if derivedCorrupt {
						if rebuildErr := rebuildHistoryFTSFromTruth(status.Brain.Path, index); rebuildErr == nil {
							if rebuilt, ok := rankHistoryViaFTS(status.Brain.Path, index, "history", task, briefOpts.limit); ok {
								scoredHistory = rebuilt
							} else {
								scoredHistory = rankHistoryRecordsScored(index, "history", task, briefOpts.limit, 0)
							}
						} else {
							scoredHistory = rankHistoryRecordsScored(index, "history", task, briefOpts.limit, 0)
						}
					} else {
						var ok bool
						scoredHistory, ok = rankHistoryViaFTS(status.Brain.Path, index, "history", task, briefOpts.limit)
						if !ok {
							scoredHistory = rankHistoryRecordsScored(index, "history", task, briefOpts.limit, 0)
						}
					}
					if profile != nil {
						profile.finishStage(&profile.History.IndexedRank, rankStarted, len(index.Records), len(scoredHistory), 0)
					}
				}
			}
		} else {
			// The validated semantic arm can return arbitrary record IDs, so it
			// deliberately keeps the full index mapping rather than hydrating only
			// the lexical top-k payload window.
			indexLoadStarted := profile.start()
			var index historyIndex
			index, historyErr = loadBrainHistoryIndex(status.Brain.Path, source)
			if profile != nil {
				historyErrors := 0
				if historyErr != nil {
					historyErrors = 1
				}
				profile.finishStage(&profile.History.IndexLoad, indexLoadStarted, 1, len(index.Records), historyErrors)
			}
			if historyErr == nil {
				rankStarted := profile.start()
				if fused, ok := rankHistoryFused(status.Brain.Path, index, "history", task, briefOpts.limit, historyEmbedder); ok {
					scoredHistory = fused
				} else {
					scoredHistory = rankHistoryRecordsScored(index, "history", task, briefOpts.limit, 0)
				}
				if profile != nil {
					profile.finishStage(&profile.History.IndexedRank, rankStarted, len(index.Records), len(scoredHistory), 0)
				}
			}
		}
		if historyErr != nil {
			report.Warnings = append(report.Warnings, "history context unavailable: "+historyErr.Error())
		} else {
			indexedMatches := make([]brainTextMatch, 0, len(scoredHistory))
			for _, scored := range scoredHistory {
				indexedMatches = append(indexedMatches, historyRecordTextMatch(scored.Record))
			}
			var rawProfile *brainBriefProfileRawHistory
			if profile != nil {
				rawProfile = &profile.History.RawFallback
			}
			rawMatches, rawErr := rawHistoryMatcher(status.Brain.Path, task, nil, briefOpts.limit, rawProfile)
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
		factsLoadStarted := profile.start()
		facts, factsErr := loadFacts(status.Brain.Path, branch)
		if profile != nil {
			factsLoadErrors := 0
			if factsErr != nil {
				factsLoadErrors = 1
			}
			profile.finishStage(&profile.Facts.Load, factsLoadStarted, 1, len(facts), factsLoadErrors)
		}
		if factsErr != nil {
			report.Warnings = append(report.Warnings, "facts unavailable: "+factsErr.Error())
		} else {
			// Semantic rerank on by default; nil reranker (embedder
			// unavailable or --no-semantic) falls back to lexical ranking. The
			// disk-backed cache avoids re-embedding the branch each brief.
			var rr *semanticReranker
			if !briefOpts.noSemantic {
				if e := defaultEmbedder(); e != nil {
					if profile != nil {
						e = &brainBriefProfilingEmbedder{Embedder: e, profile: &profile.Facts.Embed}
					}
					cacheLoadStarted := profile.start()
					rr = newSemanticRerankerForBranch(e, status.Brain.Path, branch)
					if profile != nil {
						profile.finishStage(&profile.Facts.VectorCacheLoad, cacheLoadStarted, 1, rr.loaded, 0)
					}
				}
			}
			embedBefore := int64(0)
			if profile != nil {
				embedBefore = profile.Facts.Embed.DurationNS
			}
			factsRankStarted := profile.start()
			report.Facts = rankFactsFused(facts, task, brainBriefFactsCount(briefOpts.limit), false, rr)
			if profile != nil {
				profile.finishStage(&profile.Facts.Rank, factsRankStarted, len(facts), len(report.Facts), 0)
				embedDuringRank := profile.Facts.Embed.DurationNS - embedBefore
				if embedDuringRank > 0 {
					// Rank is exclusive compute time; embed calls are reported in
					// their own nested stage rather than double-counted here.
					profile.Facts.Rank.DurationNS = max(0, profile.Facts.Rank.DurationNS-embedDuringRank)
				}
			}
			if rr != nil {
				rr.retain(facts) // keep every present fact's vector; prune only departed facts
				flushStarted := profile.start()
				flushErr := rr.flush() // best-effort cache persist
				if profile != nil {
					flushErrors := 0
					if flushErr != nil {
						flushErrors = 1
					}
					profile.finishStage(&profile.Facts.CacheFlush, flushStarted, len(facts), len(rr.cache), flushErrors)
				}
			}
		}
	}
	likelyFilesStarted := profile.start()
	report.FactsLocusDrift = factsLocusDrift(status.Repo.Root, report.Facts)
	// A non-current semantic index is still useful for intent and graph shape,
	// but its repository loci must cross the live worktree boundary before the
	// packet can present them as current context. The durable index is unchanged.
	if brainBriefSemanticNeedsCurrentBoundary(report.Status) {
		brainBriefFilterDepartedSemantic(status.Repo.Root, report.Status.Live, &report.Semantic)
		brainBriefCapSemantic(&report.Semantic, briefOpts.limit)
	}
	report.LikelyEditFiles, report.LikelyTestFiles, report.LikelyFiles = brainBriefLikelyFileGroups(status.Repo.Root, report, task)
	brainBriefApplyLayoutGuidance(status.Repo.Root, task, &report)
	if profile != nil {
		likelyInputs := len(report.Semantic.Context.Symbols) + len(report.Semantic.Context.Relations) + len(report.Semantic.RuntimeTraces) + len(report.Semantic.Tests.Suggestions) + len(report.History.Matches) + len(report.Facts)
		profile.finishStage(&profile.Synthesis.LikelyFiles, likelyFilesStarted, likelyInputs, len(report.LikelyEditFiles)+len(report.LikelyTestFiles)+len(report.LikelyFiles), 0)
	}
	actionStarted := profile.start()
	report.ActionChecklist = brainBriefActionChecklist(status.Repo.Root, report, task)
	if len(report.ActionChecklist) > 0 {
		brainBriefPrioritizeActionTargets(status.Repo.Root, &report)
		report.Guidance = append(report.Guidance, "Treat action_checklist as the first-pass current-code inventory; edit listed files first, and broaden only when the checklist is missing, ambiguous, or validation fails.")
	}
	if profile != nil {
		profile.finishStage(&profile.Synthesis.ActionChecklist, actionStarted, len(report.LikelyFiles)+len(report.History.Matches), len(report.ActionChecklist), 0)
	}
	patternsStarted := profile.start()
	views, _, perr := loadPatternViews(status.Brain.Path)
	if perr == nil {
		report.Patterns = rankTaskRelevantPatterns(views, brainBriefFileMatchTerms(task), brainBriefPatternsCount(briefOpts.limit))
	}
	if profile != nil {
		patternErrors := 0
		if perr != nil {
			patternErrors = 1
		}
		profile.finishStage(&profile.Knowledge.Patterns, patternsStarted, len(views), len(report.Patterns), patternErrors)
	}
	// Corpus consolidations (v2): task-relevant dossiers, capped, no ambient
	// noise. Graceful — absent corpus contributes nothing.
	consolidationsStarted := profile.start()
	report.Consolidations = loadBriefConsolidations(status.Brain.Path, brainBriefFileMatchTerms(task), brainBriefPatternsCount(briefOpts.limit))
	if profile != nil {
		profile.finishStage(&profile.Knowledge.Consolidations, consolidationsStarted, 0, len(report.Consolidations), 0)
	}
	// Verified latent-practice themes relevant to the task (no noise; accepted only).
	themesStarted := profile.start()
	report.Themes = rankTaskRelevantThemes(loadThemeViews(status.Brain.Path, true), brainBriefFileMatchTerms(task), brainBriefPatternsCount(briefOpts.limit))
	if profile != nil {
		profile.finishStage(&profile.Knowledge.Themes, themesStarted, 0, len(report.Themes), 0)
	}
	packetFormat := briefOpts.resolvedPacketFormat()
	if profile == nil {
		return emitBrainBriefPacket(cmd, report, packetFormat)
	}
	serializationStarted := profile.start()
	var packet bytes.Buffer
	packetCmd := &cobra.Command{}
	packetCmd.SetOut(&packet)
	serializationErr := emitBrainBriefPacket(packetCmd, report, packetFormat)
	serializationErrors := 0
	if serializationErr != nil {
		serializationErrors = 1
	}
	serializationOutputs := 1
	if serializationErr != nil {
		serializationOutputs = 0
	}
	profile.finishStage(&profile.Packet.Serialization, serializationStarted, 1, serializationOutputs, serializationErrors)
	if serializationErr != nil {
		return serializationErr
	}
	profile.Packet.Format = string(packetFormat)
	profile.Packet.ByteCount = packet.Len()
	profile.Packet.Counts = brainBriefProfilePacketCounts(report, packetFormat)
	profile.finishTotal()
	if err := writeBrainBriefProfile(briefOpts.profileJSON, *profile); err != nil {
		return err
	}
	_, err = io.Copy(cmd.OutOrStdout(), &packet)
	return err
}

func emitBrainBriefReport(cmd *cobra.Command, report brainBriefReport, jsonOutput bool) error {
	if jsonOutput {
		return writeJSON(cmd, report)
	}
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
	var currentFileCache map[string]bool
	taskTerms := brainBriefFileMatchTerms(task)
	add := func(path string, weight int, requireCurrentFile bool) {
		clean, ok := cleanBrainBriefLikelyFile(path)
		if !ok {
			return
		}
		// Semantic results come from an indexed snapshot, so a syntactically
		// valid path may have departed from the current worktree. Live changed
		// paths and history are intentionally exempt: a deleted live file can be
		// a restoration target, and history may name a file the task must create.
		if requireCurrentFile && repoRoot != "" {
			if currentFileCache == nil {
				currentFileCache = make(map[string]bool)
			}
			exists, cached := currentFileCache[clean]
			if !cached {
				exists = brainBriefRepoFileExists(repoRoot, clean)
				currentFileCache[clean] = exists
			}
			if !exists {
				return
			}
		}
		counts[clean] += weight + brainBriefLikelyFileBonus(clean) + brainBriefTaskTermBonus(clean, taskTerms)
	}
	for _, symbol := range report.Semantic.Context.Symbols {
		add(symbol.FilePath, 12, true)
		add(symbol.Path, 4, true)
	}
	for _, relation := range report.Semantic.Context.Relations {
		add(relation.FilePath, 4, true)
		add(relation.Path, 2, true)
	}
	for _, trace := range report.Semantic.RuntimeTraces {
		add(trace.FilePath, 7, true)
		add(trace.Path, 3, true)
		for _, evidence := range trace.Evidence {
			add(evidence.FilePath, 2, true)
		}
	}
	for _, root := range report.Semantic.Tests.Roots {
		add(root.FilePath, 6, true)
	}
	for _, suggestion := range report.Semantic.Tests.Suggestions {
		add(suggestion.Symbol.FilePath, 9, true)
	}
	for _, changed := range report.Status.Live.ChangedFiles {
		add(changed, 3, false)
	}
	for _, match := range report.History.Matches {
		for _, path := range extractBrainBriefPaths(match.Excerpt) {
			add(path, 5, false)
		}
	}
	for path, score := range brainBriefCurrentCodeFileCounts(repoRoot, task) {
		add(path, score, false)
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
	return brainBriefAddSiblingTestFilesWithGoSuffixes(repoRoot, editFiles, testFiles, nil)
}

const brainBriefGoTestSourceProbeLimit = 6

// brainBriefAddGoTestSourceCompanions closes the common inverse-layout gap in
// semantic retrieval: an exact test hit such as mcp_test.go is useful evidence
// for its existing mcp.go implementation even when no implementation symbol
// matched the task terms. This is an always-live, bounded postprocess over the
// already-ranked test files; it never scans a directory or trusts an indexed
// path without checking the current worktree.
func brainBriefAddGoTestSourceCompanions(repoRoot string, editFiles, testFiles []string) []string {
	if len(editFiles) >= 8 || len(testFiles) == 0 {
		return editFiles
	}
	var (
		repoFiles brainBriefRepoFileChecker
		checked   bool
		out       []string
	)
	for i, testFile := range testFiles {
		if i >= brainBriefGoTestSourceProbeLimit {
			break
		}
		candidate, ok := brainBriefGoTestSourceCandidate(testFile)
		if !ok || slices.Contains(editFiles, candidate) || slices.Contains(out, candidate) {
			continue
		}
		if !checked {
			repoFiles, ok = newBrainBriefRepoFileChecker(repoRoot)
			if !ok {
				return editFiles
			}
			checked = true
		}
		if !repoFiles.exists(candidate) {
			continue
		}
		if out == nil {
			out = append([]string(nil), editFiles...)
		}
		out = append(out, candidate)
		if len(out) >= 8 {
			return out
		}
	}
	if out == nil {
		return editFiles
	}
	return out
}

func brainBriefGoTestSourceCandidate(testFile string) (string, bool) {
	clean, ok := cleanBrainBriefLikelyFile(testFile)
	if !ok {
		return "", false
	}
	base := filepath.Base(clean)
	stem, ok := strings.CutSuffix(base, "_test.go")
	if !ok || stem == "" || strings.HasPrefix(stem, "_") || strings.Contains(stem, ".") || strings.HasSuffix(stem, "_test") {
		return "", false
	}
	return strings.TrimSuffix(clean, "_test.go") + ".go", true
}

func brainBriefAddSiblingTestFilesWithGoSuffixes(
	repoRoot string,
	editFiles, testFiles []string,
	goTestSuffixes map[string][]string,
) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(testFiles)+len(editFiles))
	for _, file := range testFiles {
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		out = append(out, file)
		if len(out) >= 6 {
			return out
		}
	}
	repoFiles, ok := newBrainBriefRepoFileChecker(repoRoot)
	if !ok {
		return out
	}
	goProbes := 0
	for _, file := range editFiles {
		directFound := false
		for _, candidate := range brainBriefSiblingTestCandidates(file) {
			if _, ok := seen[candidate]; ok {
				directFound = true
				continue
			}
			if !repoFiles.exists(candidate) {
				continue
			}
			directFound = true
			seen[candidate] = struct{}{}
			out = append(out, candidate)
			if len(out) >= 6 {
				return out
			}
			break
		}
		if directFound {
			continue
		}
		// A common-layout test is a fallback, not an invitation to fill the
		// packet with every naming variant. The first safe existing match wins.
		for _, candidate := range brainBriefNestedTestCandidates(file) {
			if _, ok := seen[candidate]; ok {
				break
			}
			if !repoFiles.exists(candidate) {
				continue
			}
			seen[candidate] = struct{}{}
			out = append(out, candidate)
			if len(out) >= 6 {
				return out
			}
			break
		}
		for _, suffix := range goTestSuffixes[file] {
			if goProbes >= brainBriefGoTestCandidateLimit {
				break
			}
			goProbes++
			candidate := strings.TrimSuffix(file, ".go") + "_" + suffix + "_test.go"
			if _, ok := seen[candidate]; ok {
				break
			}
			if !repoFiles.exists(candidate) {
				continue
			}
			seen[candidate] = struct{}{}
			out = append(out, candidate)
			if len(out) >= 6 {
				return out
			}
			break
		}
	}
	return out
}

// brainBriefApplyLayoutGuidance runs on every task brief, before optional action
// scanners. Exact test layouts lead noisier retrieved tests but remain capped.
func brainBriefApplyLayoutGuidance(repoRoot, task string, report *brainBriefReport) {
	if report == nil {
		return
	}
	report.LikelyEditFiles = brainBriefAddGoTestSourceCompanions(repoRoot, report.LikelyEditFiles, report.LikelyTestFiles)
	if len(report.LikelyEditFiles) > 0 {
		goSuffixes := brainBriefGoTaskTestSuffixes(task, report.LikelyEditFiles)
		inferred := brainBriefAddSiblingTestFilesWithGoSuffixes(repoRoot, report.LikelyEditFiles, nil, goSuffixes)
		report.LikelyTestFiles = brainBriefMergePrioritizedFiles(inferred, report.LikelyTestFiles, 6)
	}
	brainBriefAddCxxHeaderLayoutGuidance(repoRoot, task, report)
}

// brainBriefRepoFileChecker reuses the resolved repository root for one test-
// guidance pass. Comparing each fully resolved candidate with its expected
// physical path rejects every symlink component without separately resolving
// the root for each candidate. Candidate results are deliberately not cached,
// so file changes are observed and no state survives beyond this invocation.
type brainBriefRepoFileChecker struct {
	rootResolved string
}

func newBrainBriefRepoFileChecker(repoRoot string) (brainBriefRepoFileChecker, bool) {
	rootForEval := repoRoot
	if rootForEval == "" {
		rootForEval = "."
	}
	rootResolved, err := filepath.EvalSymlinks(rootForEval)
	if err != nil {
		return brainBriefRepoFileChecker{}, false
	}
	return brainBriefRepoFileChecker{rootResolved: rootResolved}, true
}

func (files brainBriefRepoFileChecker) exists(rel string) bool {
	clean, ok := cleanBrainBriefRepoRelativePath(rel)
	if !ok {
		return false
	}
	nativeRel := filepath.FromSlash(clean)
	path := filepath.Join(files.rootResolved, nativeRel)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	pathResolved, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Clean(pathResolved) != path {
		return false
	}
	return true
}

// brainBriefNestedTestCandidates covers a small set of common test layouts that
// are not direct source siblings. It is consulted only when no direct sibling
// was recommended, and callers require a safe existing regular repository file.
func brainBriefNestedTestCandidates(file string) []string {
	ext := filepath.Ext(file)
	switch ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		nativeFile := filepath.FromSlash(file)
		nestedStem := filepath.ToSlash(filepath.Join(
			filepath.Dir(nativeFile),
			"__tests__",
			strings.TrimSuffix(filepath.Base(nativeFile), ext),
		))
		return []string{nestedStem + ".test" + ext, nestedStem + ".spec" + ext}
	case ".py":
		clean, ok := cleanBrainBriefRepoRelativePath(file)
		if !ok || !strings.HasPrefix(clean, "src/") {
			return nil
		}
		sourceRelative := filepath.FromSlash(strings.TrimPrefix(clean, "src/"))
		testName := "test_" + strings.TrimSuffix(filepath.Base(sourceRelative), ext) + ext
		return []string{filepath.ToSlash(filepath.Join("tests", filepath.Dir(sourceRelative), testName))}
	case ".java":
		clean, ok := cleanBrainBriefRepoRelativePath(file)
		if !ok {
			return nil
		}
		modulePrefix, sourceRelative, found := strings.Cut(clean, "src/main/java/")
		if !found || (modulePrefix != "" && !strings.HasSuffix(modulePrefix, "/")) || sourceRelative == "" {
			return nil
		}
		testName := strings.TrimSuffix(filepath.Base(sourceRelative), ext) + "Test" + ext
		return []string{filepath.ToSlash(filepath.Join(
			filepath.FromSlash(modulePrefix),
			"src", "test", "java",
			filepath.Dir(filepath.FromSlash(sourceRelative)),
			testName,
		))}
	default:
		return nil
	}
}

const brainBriefGoTestCandidateLimit = 24

// brainBriefGoTaskTestSuffixes derives bounded focused Go test names from the
// task. Adjacent pairs lead their singles so "single read" selects single_read.
func brainBriefGoTaskTestSuffixes(task string, editFiles []string) map[string][]string {
	var byFile map[string][]string
	var terms []string
	remaining := brainBriefGoTestCandidateLimit
	for _, editFile := range editFiles {
		if remaining == 0 {
			break
		}
		file, ok := cleanBrainBriefLikelyFile(editFile)
		if !ok || filepath.Ext(file) != ".go" {
			continue
		}
		if byFile == nil {
			byFile = make(map[string][]string)
			terms = brainBriefFileMatchTerms(task)
		}
		sourceTerms := make(map[string]bool)
		for _, term := range brainBriefFileMatchTerms(strings.TrimSuffix(filepath.Base(file), ".go")) {
			sourceTerms[term] = true
		}
		seen := make(map[string]bool)
		for _, suffix := range byFile[file] {
			seen[suffix] = true
		}
		add := func(suffix string) {
			if suffix != "" && len(suffix) <= 80 && !seen[suffix] && remaining > 0 {
				seen[suffix] = true
				byFile[file] = append(byFile[file], suffix)
				remaining--
			}
		}
		filtered := make([]string, 0, len(terms))
		for _, term := range terms {
			if !sourceTerms[term] {
				filtered = append(filtered, term)
			}
		}
		for i := 0; i+1 < len(filtered); i++ {
			add(filtered[i] + "_" + filtered[i+1])
		}
		for _, term := range filtered {
			add(term)
		}
	}
	return byFile
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
		return []string{stem + "_test.py"}
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

func brainBriefActionFiles(actions []brainBriefAction, candidates []string) []string {
	eligible := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if clean, ok := cleanBrainBriefLikelyFile(candidate); ok {
			eligible[clean] = struct{}{}
		}
	}
	seen := map[string]struct{}{}
	files := make([]string, 0, len(actions))
	for _, action := range actions {
		clean, ok := cleanBrainBriefLikelyFile(action.File)
		if !ok || !brainBriefSourceFile(clean) {
			continue
		}
		// The action scanners only read report.LikelyEditFiles. Requiring the
		// path to remain in that same bounded candidate set prevents a stale or
		// injected action from consuming a slot without another filesystem walk.
		if _, ok := eligible[clean]; !ok {
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

func brainBriefCxxHeaderCandidates(file string) []string {
	clean, ok := cleanBrainBriefRepoRelativePath(file)
	ext := strings.ToLower(filepath.Ext(clean))
	if !ok || (ext != ".c" && ext != ".cc" && ext != ".cpp") {
		return nil
	}
	stem := strings.TrimSuffix(clean, filepath.Ext(clean))
	marker := strings.LastIndex(stem, "src/")
	if marker < 0 || (marker > 0 && stem[marker-1] != '/') || marker+len("src/") == len(stem) {
		return nil
	}
	includeStem := stem[:marker] + "include/" + stem[marker+len("src/"):]
	return []string{includeStem + ".h", includeStem + ".hpp"}
}

func brainBriefCxxDeclarationChangeTask(task string) bool {
	lower := strings.ToLower(task)
	for _, negative := range []string{
		"without changing public api", "without changing the public api", "without changing its public api",
		"without touching header", "without touching the header",
		"signature unchanged", "do not change signature", "do not change the signature",
		"do not change public api", "do not change the public api",
		"preserve public api", "preserve the public api",
	} {
		if strings.Contains(lower, negative) {
			return false
		}
	}
	change, declaration := false, false
	for _, word := range brainBriefTaskWordPattern.FindAllString(lower, -1) {
		switch {
		case strings.HasPrefix(word, "chang"), strings.HasPrefix(word, "updat"), strings.HasPrefix(word, "modif"),
			strings.HasPrefix(word, "remov"), strings.HasPrefix(word, "renam"), strings.HasPrefix(word, "extend"),
			strings.HasPrefix(word, "replac"), strings.HasPrefix(word, "alter"),
			word == "add", word == "adds", word == "added", word == "adding":
			change = true
		case strings.HasPrefix(word, "signatur"), strings.HasPrefix(word, "declarat"), strings.HasPrefix(word, "prototyp"):
			declaration = true
		}
	}
	return change && declaration
}

func brainBriefAddCxxHeaderLayoutGuidance(repoRoot, task string, report *brainBriefReport) {
	if report == nil {
		return
	}
	if brainBriefCxxDeclarationChangeTask(task) {
		var repoFiles *brainBriefRepoFileChecker
	search:
		for i, source := range report.LikelyEditFiles {
			if i+1 >= 8 {
				break
			}
			candidates := brainBriefCxxHeaderCandidates(source)
			if len(candidates) == 0 {
				continue
			}
			if repoFiles == nil {
				files, ok := newBrainBriefRepoFileChecker(repoRoot)
				if !ok {
					break
				}
				repoFiles = &files
			}
			companion := ""
			for _, candidate := range candidates {
				if slices.Contains(report.LikelyEditFiles, candidate) {
					break search
				}
				if repoFiles.exists(candidate) {
					if companion != "" {
						companion = ""
						break
					}
					companion = candidate
				}
			}
			if companion == "" {
				continue
			}
			guided := append([]string{}, report.LikelyEditFiles[:i+1]...)
			guided = append(guided, companion)
			guided = append(guided, report.LikelyEditFiles[i+1:]...)
			report.LikelyEditFiles = guided[:min(len(guided), 8)]
			break search
		}
	}
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
}

// brainBriefPrioritizeActionTargets keeps concrete current-code actions first
// without turning the checklist into a lossy replacement for other evidence.
// Action paths come from safe current-file reads and must still belong to the
// bounded candidate set; live-deleted and history-backed intended-create paths
// are retained from that set even though they may not exist yet. The upstream
// edit/test caps remain hard after reprioritization.
func brainBriefPrioritizeActionTargets(repoRoot string, report *brainBriefReport) {
	if report == nil || len(report.ActionChecklist) == 0 {
		return
	}
	actionFiles := brainBriefActionFiles(report.ActionChecklist, report.LikelyEditFiles)
	report.LikelyEditFiles = brainBriefMergePrioritizedFiles(actionFiles, report.LikelyEditFiles, 8)
	actionTests := brainBriefAddSiblingTestFiles(repoRoot, actionFiles, nil)
	report.LikelyTestFiles = brainBriefMergePrioritizedFiles(actionTests, report.LikelyTestFiles, 6)
	report.LikelyFiles = brainBriefMergeLikelyFiles(report.LikelyEditFiles, report.LikelyTestFiles)
}

func brainBriefMergePrioritizedFiles(primary, fallback []string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	out := make([]string, 0, min(len(primary)+len(fallback), limit))
	seen := make(map[string]struct{}, cap(out))
	appendFiles := func(files []string) bool {
		for _, path := range files {
			clean, ok := cleanBrainBriefLikelyFile(path)
			if !ok {
				continue
			}
			if _, exists := seen[clean]; exists {
				continue
			}
			seen[clean] = struct{}{}
			out = append(out, clean)
			if len(out) >= limit {
				return true
			}
		}
		return false
	}
	if appendFiles(primary) {
		return out
	}
	appendFiles(fallback)
	return out
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
	"these": true, "those": true, "after": true, "before": true, "their": true, "your": true,
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
		"include/",
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
	}
	live, liveErr := brainLiveStateReport(ctx, opts.Runner, repoDir, storage.BrainDir, manifest)
	if liveErr != nil {
		report.Warnings = append(report.Warnings, "live state unavailable: "+liveErr.Error())
	} else {
		report.Live = live
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
	db, err := sql.Open(sqliteDriverName, storePath)
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

type brainBriefRawScanObservation struct {
	scannedBytes int64
}

type brainBriefCountingReader struct {
	reader io.Reader
	bytes  int64
}

func (r *brainBriefCountingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	return n, err
}

func inspectBrainRawText(brainDir, kind, query string, maxHits int) (brainHistoryInspectReport, error) {
	return inspectBrainRawTextObserved(brainDir, kind, query, maxHits, nil)
}

func inspectBrainRawTextObserved(brainDir, kind, query string, maxHits int, observation *brainBriefRawScanObservation) (brainHistoryInspectReport, error) {
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
		var reader io.Reader = f
		if observation != nil {
			counting := &brainBriefCountingReader{reader: f}
			reader = counting
			defer func() {
				observation.scannedBytes += counting.bytes
			}()
		}
		scanner := bufio.NewScanner(reader)
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
	return brainBriefRawHistoryMatchesObserved(brainDir, task, existing, limit, nil)
}

func brainBriefRawHistoryMatchesObserved(brainDir, task string, existing []brainTextMatch, limit int, profile *brainBriefProfileRawHistory) ([]brainTextMatch, error) {
	if profile == nil {
		return brainBriefRawHistoryMatchesSingleScan(brainDir, task, existing, limit)
	}
	return brainBriefRawHistoryMatchesMultiScan(brainDir, task, existing, limit, profile)
}

// brainBriefRawHistoryMatchesMultiScan preserves the observation contract for
// opt-in profiling: each query has its own measured filesystem walk and byte
// count. The default product path uses brainBriefRawHistoryMatchesSingleScan,
// which produces the same ordered matches with one walk.
func brainBriefRawHistoryMatchesMultiScan(brainDir, task string, existing []brainTextMatch, limit int, profile *brainBriefProfileRawHistory) ([]brainTextMatch, error) {
	var profileStarted time.Time
	if profile != nil {
		profile.Invoked = true
		profileStarted = time.Now()
		defer func() {
			d := time.Since(profileStarted)
			if d > 0 {
				profile.DurationNS = d.Nanoseconds()
			}
		}()
	}
	if limit <= 0 {
		return nil, nil
	}
	seen := map[string]struct{}{}
	for _, match := range existing {
		seen[brainBriefHistoryMatchKey(match)] = struct{}{}
	}
	var matches []brainTextMatch
	var firstErr error
	for ordinal, query := range brainBriefRawHistoryQueries(task) {
		if len(matches) >= limit {
			break
		}
		queryStarted := time.Time{}
		var observation *brainBriefRawScanObservation
		if profile != nil {
			queryStarted = time.Now()
			observation = &brainBriefRawScanObservation{}
		}
		report, err := inspectBrainRawTextObserved(brainDir, "history", query, limit-len(matches), observation)
		if profile != nil {
			queryErrors := len(report.ScanErrors)
			if err != nil {
				queryErrors++
			}
			queryDuration := time.Since(queryStarted)
			if queryDuration < 0 {
				queryDuration = 0
			}
			queryProfile := brainBriefProfileRawHistoryQuery{
				Ordinal:          ordinal + 1,
				DurationNS:       queryDuration.Nanoseconds(),
				ScannedFileCount: report.Scanned,
				ScannedByteCount: observation.scannedBytes,
				MatchCount:       len(report.Matches),
				Truncated:        report.Truncated,
				ErrorCount:       queryErrors,
			}
			profile.Queries = append(profile.Queries, queryProfile)
			profile.QueryCount++
			profile.ScannedFileCount += queryProfile.ScannedFileCount
			profile.ScannedByteCount += queryProfile.ScannedByteCount
			profile.MatchCount += queryProfile.MatchCount
			if queryProfile.Truncated {
				profile.TruncationCount++
			}
			profile.ErrorCount += queryProfile.ErrorCount
		}
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

type brainBriefRawHistoryQuery struct {
	original   string
	lower      string
	normalized string
	matches    []brainTextMatch
}

// brainBriefRawHistoryMatchesSingleScan evaluates the same prioritized raw
// history queries in a single deterministic WalkDir pass. The legacy path
// walks the identical files up to eight times. Collecting the first limit hits
// for every query is sufficient to replay its exact behavior afterward:
// query n can request at most limit-len(matches) hits, and the legacy scanner
// stops before deduplication at precisely that prefix length.
func brainBriefRawHistoryMatchesSingleScan(brainDir, task string, existing []brainTextMatch, limit int) ([]brainTextMatch, error) {
	if limit <= 0 {
		return nil, nil
	}
	matchCapacity := min(limit, brainInspectHistoryMaxHits)
	queryStrings := brainBriefRawHistoryQueries(task)
	if len(queryStrings) == 0 {
		return nil, nil
	}
	queries := make([]brainBriefRawHistoryQuery, 0, len(queryStrings))
	for _, query := range queryStrings {
		lower := strings.ToLower(strings.TrimSpace(query))
		if lower == "" {
			continue
		}
		queries = append(queries, brainBriefRawHistoryQuery{
			original:   query,
			lower:      lower,
			normalized: normalizeHistorySearchText(lower),
			matches:    make([]brainTextMatch, 0, matchCapacity),
		})
	}
	if len(queries) == 0 {
		return nil, nil
	}

	fullQueries := 0
	scannedFiles := 0
	scanBuffer := make([]byte, 64*1024)
	err := filepath.WalkDir(brainDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == semanticDirName || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if fullQueries == len(queries) {
			return filepath.SkipAll
		}
		if scannedFiles >= brainInspectHistoryMaxFiles {
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
		scannedFiles++
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(scanBuffer, brainInspectHistoryMaxLine)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			lowerLine := strings.ToLower(line)
			normalizedLine := ""
			normalized := false
			for i := range queries {
				query := &queries[i]
				if len(query.matches) >= limit {
					continue
				}
				matched := strings.Contains(lowerLine, query.lower)
				if !matched && query.normalized != "" {
					if !normalized {
						normalizedLine = normalizeHistorySearchText(lowerLine)
						normalized = true
					}
					matched = strings.Contains(normalizedLine, query.normalized)
				}
				if !matched {
					continue
				}
				match := brainTextMatch{
					Path:    relSlash,
					Line:    lineNo,
					Excerpt: historyRawLineExcerpt(line, query.original),
				}
				if ts, ok := historyRecordTimestamp(relSlash); ok {
					match.Timestamp = ts.Format(time.RFC3339)
				}
				query.matches = append(query.matches, match)
				if len(query.matches) == limit {
					fullQueries++
				}
			}
			if fullQueries == len(queries) {
				break
			}
		}
		_ = f.Close()
		return nil
	})
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(existing)+matchCapacity)
	for _, match := range existing {
		seen[brainBriefHistoryMatchKey(match)] = struct{}{}
	}
	matches := make([]brainTextMatch, 0, matchCapacity)
	for _, query := range queries {
		remaining := limit - len(matches)
		if remaining <= 0 {
			break
		}
		candidates := query.matches
		if len(candidates) > remaining {
			candidates = candidates[:remaining]
		}
		for _, match := range candidates {
			key := brainBriefHistoryMatchKey(match)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			matches = append(matches, match)
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	return matches, nil
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
