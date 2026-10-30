// Package brainreview hosts the hidden diff-less review command surface as it
// moves out of package cli (COR-2268). The shared regression engine
// (detectRegressionAnomaliesCapped and the raw-history scan) stays in cli and
// is injected through Deps; the review contract types and summary helpers live
// here, with aliases in cli for the engine and workspace call sites.
package brainreview

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/entire-brain/internal/cli/internal/base"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"github.com/spf13/cobra"
)

// Config carries the CLI options plus the injected cli-bound seams.
type Config struct {
	Options base.Options
	Deps    Deps
}

// Deps are the cli-bound seams the review command still needs. root.go wires
// them from package cli; tests may supply fakes.
type Deps struct {
	// AgentSurfaceTarget resolves the repository path argument for a command.
	AgentSurfaceTarget func(opts base.Options, args []string) string
	// BrainStatus resolves the brain directory, the repository root, and the
	// declared semantic source manifest (nil when no semantic index exists)
	// for the target repository.
	BrainStatus func(ctx context.Context, opts base.Options, target string) (brainPath, repoRoot string, semSource *bmanifest.SemanticSource, err error)
	// DetectAnomalies is the shared regression engine
	// (detectRegressionAnomaliesCapped in cli), which stays there with its
	// ~24 callers; only this command surface moved.
	DetectAnomalies func(brainDir, repoRoot string, semSource *bmanifest.SemanticSource, query string, limit int, includeDeletions bool) ([]Anomaly, int, []string, ScanCaps)
	// RuntimeTraceFacts returns query-matching runtime trace records for
	// review context.
	RuntimeTraceFacts func(brainDir string, semSource *bmanifest.SemanticSource, query string, limit int) ([]semstore.Record, error)
	// RequirePrivacyDerivedRead fails closed when derived reads are not
	// verified clean.
	RequirePrivacyDerivedRead func(brainDir string) error
	// RequirePatternCorpusAvailable fails when the V2 pattern corpus is
	// absent.
	RequirePatternCorpusAvailable func(brainDir string) error
	// LoadReviewPatternContextChecked loads V2 corpus risk/practice/task
	// context for the finding files.
	LoadReviewPatternContextChecked func(brainDir string, files []string, limit int) ([]PatternRef, error)
	// BufferRetrievalOutput buffers rendered output behind the retrieval
	// privacy gate.
	BufferRetrievalOutput func(cmd *cobra.Command, policies []brainstore.RetrievalPrivacyPolicy, render func() error) error
}

type Anomaly struct {
	File             string   `json:"file"`
	Line             int      `json:"line,omitempty"`
	Kind             string   `json:"kind"` // "changed" | "deleted"
	Current          string   `json:"current"`
	Expected         string   `json:"expected"`
	Identifier       string   `json:"identifier"`
	Symbol           string   `json:"symbol,omitempty"`
	RelatedLocations []string `json:"related_locations,omitempty"`
	Confidence       float64  `json:"confidence"`
	Reason           string   `json:"reason"`
	Evidence         string   `json:"evidence"`
	// RankBoost is engine-internal ranking state (set and read only by the
	// cli-side detector); exported solely because the type's home moved here,
	// and kept out of the JSON contract.
	RankBoost int `json:"-"`
}

type DetectorOptions struct {
	Limit            int
	JSON             bool
	IncludeDeletions bool // deletions are higher-recall but noisier (no recency data); opt-in
	LocationOnly     bool // emit file:line only, NOT expected/current — so a fair A/B can't paste the answer
	Patterns         bool // opt-in: attach V2 risk/practice/task corpus context for the finding files
}

// ScanCaps records whether a hard limit stopped the raw-history walk before it
// reached the end of the brain. A fired cap makes the scan PARTIAL: from that
// point on, the absence of a signal is no longer evidence that no such signal
// exists, so every caller must say so rather than print a clean bill of health.
type ScanCaps struct {
	// FileCap is true when the walk hit maxFiles opened history files.
	FileCap bool
	// SignalCap is true when both the distinct-change and distinct-delete signal caps filled.
	SignalCap bool
	// Scanned is the number of history files actually opened before the walk ended.
	Scanned int
	// MaxFiles / MaxSignals are the limits in force, reported so the warning is self-explaining.
	MaxFiles   int
	MaxSignals int
}

// Truncated reports whether the walk ended early, i.e. whether the scan is partial.
func (c ScanCaps) Truncated() bool { return c.FileCap || c.SignalCap }

// Warning renders the one line a reader needs to know the scan was partial. Empty when it was not.
func (c ScanCaps) Warning() string {
	switch {
	case c.FileCap && c.SignalCap:
		return fmt.Sprintf("history scan stopped early: hit both the %d-file cap and the %d-distinct-signal cap; results are partial and absence of a finding is not evidence of absence", c.MaxFiles, c.MaxSignals)
	case c.FileCap:
		return fmt.Sprintf("history scan stopped early: hit the %d-file cap after %d files; results are partial and absence of a finding is not evidence of absence", c.MaxFiles, c.Scanned)
	case c.SignalCap:
		return fmt.Sprintf("history scan stopped early: hit the %d-distinct-signal cap after %d files; results are partial and absence of a finding is not evidence of absence", c.MaxSignals, c.Scanned)
	default:
		return ""
	}
}

// ---- Diff-less review: the versioned contract `entire review` / `labs investigate` are INTENDED to
//      consume (cross-repo, NOT yet wired) ----
//
// This is the machine contract, not a human verb (the human review surface is `entire review` in the
// cli; this command is hidden — see root.go). When wired, the cli's `entire review` WOULD gain a
// diff-less mode that shells `entire-brain review <query> --json`, checks Report.schema_version,
// and folds these findings into the review prompt — reviewing the working tree against the brain's
// memory instead of a branch-vs-base diff (the graceful upgrade entire-brain gets from entire-graph).
// That CLI consumer is not landed. Full contract + consumer design:
// docs/diffless_review_seam.md.
//
// Consumer status (cross-repo, in entireio/cli — NOT in this repo, do not claim either is shipped):
//   - `entire review`            : consumer integration is NOT landed.
//   - `entire labs investigate`  : DESIGNED, NOT WIRED.
//
// TODO(diffless-seam): wire `entire labs investigate` to consume this contract — fold the suspected
// regressions for the investigation topic into the per-turn shared context so every brainstorming
// agent sees them. No diff concept there, so it fires whenever the brain is installed. Design lives in
// docs/diffless_review_seam.md; the CLI-side hook is not landed here.

// ReportSchemaVersion is the contract version `entire review` is intended to bind to (the
// consumer is not yet landed; see above). Bump on any breaking change to Report / Finding
// (field rename/removal/semantics).
const ReportSchemaVersion = 1

type Finding struct {
	Severity   string  `json:"severity"` // high (reserved) | medium (changed) | low (deleted)
	File       string  `json:"file"`
	Line       int     `json:"line,omitempty"`
	Title      string  `json:"title"`
	Detail     string  `json:"detail"`
	Evidence   string  `json:"evidence"`
	Confidence float64 `json:"confidence"`
}

type Report struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Mode          string    `json:"mode"`
	Query         string    `json:"query"`
	RepoPath      string    `json:"repo_path"`
	BrainPath     string    `json:"brain_path"`
	Summary       string    `json:"summary"`
	// Checked reports whether the review actually compared anything. False means
	// the brain held nothing to check this tree against — history carried no
	// precise code assertions for these identifiers, or there were no current
	// files to read — so an empty Findings list means UNKNOWN, not clean. A
	// consumer that reads len(findings) == 0 as a pass must read this first;
	// FilesScanned is the evidence behind it.
	//
	// Additive: nothing was renamed or removed and the contract has no landed
	// consumer (see ReportSchemaVersion), so the schema version is
	// unchanged.
	Checked       bool              `json:"checked"`
	FilesScanned  int               `json:"files_scanned"`
	Findings      []Finding         `json:"findings"`
	Warnings      []string          `json:"warnings,omitempty"`
	RuntimeTraces []semstore.Record `json:"runtime_traces,omitempty"`
	// PatternContext is OPT-IN (only with --patterns): V2 corpus risk/practice/
	// task patterns whose recorded evidence touched the finding files — diff-less
	// regression context. omitempty keeps the default review output byte-identical
	// for the versioned schema's existing consumers.
	PatternContext []PatternRef `json:"pattern_context,omitempty"`
}

type PatternRef struct {
	File  string `json:"file"`
	Type  string `json:"type"`
	Title string `json:"title"`
	ID    string `json:"id"`
}

// regressionSeverity maps detector confidence to a review severity. Every finding the current
// heuristic detector emits is *suspected* — `changed` (0.8) is precise but rename-ambiguous and
// `deleted` (0.65) is noisier/opt-in — so none warrant "high". That tier is reserved for a future
// high-confidence signal (a durable fact bound to a locus with recency), so the machine severity
// never contradicts the "could be a rename — verify" hedge: changed -> medium, deleted -> low.
func regressionSeverity(conf float64) string {
	switch {
	case conf >= 0.9:
		return "high" // reserved: no heuristic finding reaches this today
	case conf >= 0.7:
		return "medium"
	default:
		return "low"
	}
}

// ReviewSummary is the one line a reader takes away, and it may never say more
// than the run actually established.
//
// Zero findings has two completely different causes and they were reported with
// the same sentence: "no suspected regressions (current tree matches the brain's
// memory)". On a brain that was never built, or whose history holds no precise
// code assertions for the query, the detector returns before it
// opens a single file — scanned == 0 — and that sentence became a clean bill of
// health for a comparison that never happened. The contradiction was printed
// directly underneath it, as "note: history holds no precise code assertions for
// these identifiers", but the summary is the line people quote.
//
// scanned is the number of current files actually read and compared, so
// scanned == 0 is the precise, non-string-matched test for "nothing was
// checked".
func ReviewSummary(findings, scanned int, historyTruncated bool) string {
	switch {
	case historyTruncated && scanned > 0:
		return fmt.Sprintf("Diff-less review: PARTIAL — %d suspected regression(s) in %d file(s) compared; the history scan stopped at a cap, so this is not a clean result and additional regressions may exist. See the notes below.", findings, scanned)
	case findings > 0:
		return fmt.Sprintf("Diff-less review: %d suspected regression(s) — verify each before acting.", findings)
	case scanned == 0:
		return "Diff-less review: INCONCLUSIVE — nothing was compared (the brain holds no code assertions to check this tree against). This is not a clean result; see the notes below."
	default:
		return fmt.Sprintf("Diff-less review: no suspected regressions (%d file(s) compared against the brain's memory).", scanned)
	}
}

func AnomalyToReviewFinding(a Anomaly) Finding {
	detail := "suspected regression at this location — verify against the brain's history."
	if a.Expected != "" {
		detail = fmt.Sprintf("history shows `%s`; current is `%s` — verify this change is intentional (could also be a rename).", a.Expected, a.Current)
	}
	return Finding{
		Severity:   regressionSeverity(a.Confidence),
		File:       a.File,
		Line:       a.Line,
		Title:      fmt.Sprintf("Suspected regression: `%s` %s", a.Identifier, a.Kind),
		Detail:     detail,
		Evidence:   a.Evidence,
		Confidence: a.Confidence,
	}
}

func RunBrainReview(ctx context.Context, cmd *cobra.Command, cfg Config, ro DetectorOptions, query string) error {
	if ro.Limit <= 0 {
		ro.Limit = 20
	}
	target := cfg.Deps.AgentSurfaceTarget(cfg.Options, nil)
	brainPath, repoRoot, semSource, err := cfg.Deps.BrainStatus(ctx, cfg.Options, target)
	if err != nil {
		return err
	}
	privacyPolicy, err := brainstore.CaptureRetrievalPrivacyPolicy(brainPath)
	if err != nil {
		return err
	}
	if ro.Patterns {
		privacyPolicy.RequireDerivedClean = true
		if err := cfg.Deps.RequirePrivacyDerivedRead(brainPath); err != nil {
			return err
		}
		if err := cfg.Deps.RequirePatternCorpusAvailable(brainPath); err != nil {
			return err
		}
	}
	anomalies, scanned, warnings, caps := cfg.Deps.DetectAnomalies(brainPath, repoRoot, semSource, query, ro.Limit, ro.IncludeDeletions)
	var runtimeTraces []semstore.Record
	if semSource != nil {
		traces, err := cfg.Deps.RuntimeTraceFacts(brainPath, semSource, query, ro.Limit)
		if err != nil {
			warnings = append(warnings, "runtime trace facts unavailable for review context")
		} else {
			for _, trace := range traces {
				if trace.Score > 1 {
					runtimeTraces = append(runtimeTraces, trace)
				}
			}
		}
	}
	if ro.LocationOnly {
		// Parity with `inspect regressions --location-only`: hand the suspected site, not the fix,
		// so a fair consumer/A-B measures detection rather than pasting a harness-computed value.
		for i := range anomalies {
			anomalies[i].Expected = ""
			anomalies[i].Current = ""
			anomalies[i].Reason = "suspected regression site (location only)"
		}
	}
	findings := make([]Finding, 0, len(anomalies))
	for _, a := range anomalies {
		findings = append(findings, AnomalyToReviewFinding(a))
	}
	report := Report{
		SchemaVersion: ReportSchemaVersion,
		GeneratedAt:   cfg.Options.Now().UTC(),
		Mode:          "diff-less (brain memory vs current tree)",
		Query:         query,
		RepoPath:      repoRoot,
		BrainPath:     brainPath,
		Summary:       ReviewSummary(len(findings), scanned, caps.Truncated()),
		Checked:       scanned > 0,
		FilesScanned:  scanned,
		Findings:      findings,
		Warnings:      warnings,
		RuntimeTraces: runtimeTraces,
	}
	if ro.Patterns {
		files := make([]string, 0, len(findings))
		for _, f := range findings {
			files = append(files, f.File)
		}
		report.PatternContext, err = cfg.Deps.LoadReviewPatternContextChecked(brainPath, files, ro.Limit)
		if err != nil {
			return err
		}
	}
	render := func() error {
		if ro.JSON {
			return cmdutil.WriteJSON(cmd, report)
		}
		out := cmd.OutOrStdout()
		fmt.Fprintln(out, report.Summary)
		for _, f := range findings {
			fmt.Fprintf(out, "\n  [%s] %s\n    %s:%d\n    %s\n    evidence: %s\n",
				strings.ToUpper(f.Severity), f.Title, f.File, f.Line, f.Detail, f.Evidence)
		}
		for _, pc := range report.PatternContext {
			fmt.Fprintf(out, "  pattern context [%s] %s — %s   id %s\n", pc.Type, pc.File, pc.Title, pc.ID)
		}
		for _, w := range warnings {
			fmt.Fprintf(out, "  note: %s\n", w)
		}
		return nil
	}
	return cfg.Deps.BufferRetrievalOutput(cmd, []brainstore.RetrievalPrivacyPolicy{privacyPolicy}, render)
}

func New(cfg Config) *cobra.Command {
	ro := DetectorOptions{Limit: 20}
	cmd := &cobra.Command{
		Use:   "review <query>",
		Short: "Diff-less review: flag suspected regressions in the current tree vs the brain's memory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunBrainReview(cmd.Context(), cmd, cfg, ro, args[0])
		},
	}
	cmd.Flags().IntVar(&ro.Limit, "limit", 20, "Maximum findings")
	cmd.Flags().BoolVar(&ro.JSON, "json", false, "Emit machine-readable JSON")
	cmd.Flags().BoolVar(&ro.Patterns, "patterns", false, "Attach V2 corpus risk/practice/task context for the finding files (additive)")
	cmd.Flags().BoolVar(&ro.IncludeDeletions, "include-deletions", false, "Also flag deleted assignments (lower confidence, noisier)")
	cmd.Flags().BoolVar(&ro.LocationOnly, "location-only", false, "Emit only the suspected file:line, not the expected/current values")
	return cmd
}
