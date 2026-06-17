package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/ashtom/entire-brain/internal/tui"
)

// dashDefaultLimit caps how many entries per tab the dashboard loads. Large
// brains can hold thousands of facts/symbols; the cap keeps the snapshot bounded
// and a Note records what was dropped so the truncation is never silent. A
// non-positive --limit means no cap.
const dashDefaultLimit = 500

// dashSearchLimit caps results from an in-dashboard search (the `s` key),
// matching the `entire brain search` default surface.
const dashSearchLimit = 25

type dashFlags struct {
	theme  string
	tab    string
	branch string
	limit  int
	json   bool
	plain  bool
}

func newDashCommand(opts Options) *cobra.Command {
	var flags dashFlags
	cmd := &cobra.Command{
		Use:   "dash [path]",
		Short: "Browse the brain in an interactive dashboard (status, facts, sessions, history, semantic)",
		Long: `dash opens an interactive terminal dashboard over the brain: a Home tab
summarizing source health, freshness, and blind spots, plus explorer tabs that
browse individual facts, sessions, history records, and semantic symbols, each
with a detail page. It is read-only and makes no agent calls.

On a non-TTY (piped output) it prints a plain summary instead; --json emits the
loaded snapshot.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDash(cmd.Context(), cmd, opts, flags, agentSurfaceTarget(opts, args))
		},
	}
	cmd.Flags().StringVar(&flags.theme, "theme", "", "Color theme: "+strings.Join(tui.ThemeNames(), ", ")+" (or set ENTIRE_BRAIN_THEME)")
	cmd.Flags().StringVar(&flags.tab, "tab", "", "Tab to open on: home, facts, sessions, history, semantic")
	cmd.Flags().StringVar(&flags.branch, "branch", "", "Branch whose facts to load (default: current branch)")
	cmd.Flags().IntVar(&flags.limit, "limit", dashDefaultLimit, "Max entries loaded per tab (0 = no cap)")
	cmd.Flags().BoolVar(&flags.json, "json", false, "Emit the loaded snapshot as JSON instead of opening the dashboard")
	cmd.Flags().BoolVar(&flags.plain, "plain", false, "Print a plain text summary instead of opening the dashboard")
	return cmd
}

func runDash(ctx context.Context, cmd *cobra.Command, opts Options, flags dashFlags, target string) error {
	startTab, ok := tui.ParseTab(flags.tab)
	if !ok && flags.tab != "" {
		return fmt.Errorf("unknown_tab: %q (want home, facts, sessions, history, or semantic)", flags.tab)
	}

	repoDir, brainDir, branch, err := resolveFactsTarget(ctx, opts, target, flags.branch)
	if err != nil {
		return err
	}

	snap, err := assembleBrainSnapshot(ctx, opts, target, repoDir, brainDir, branch, flags.limit)
	if err != nil {
		return err
	}

	if flags.json {
		return writeJSON(cmd, snap)
	}
	if flags.plain || !commandOutputIsTTY(cmd) {
		printDashPlain(cmd, snap)
		return nil
	}
	return tui.Run(snap, resolveDashTheme(flags.theme), brainSearchFunc(brainDir, branch), startTab, cmd.OutOrStdout())
}

// brainSearchFunc adapts the lexical retrieval (`entire brain search`) into the
// callback the dashboard's `s` key invokes, so search runs the same code path as
// the CLI verb while keeping the TUI free of file IO.
func brainSearchFunc(brainDir, branch string) tui.SearchFunc {
	return func(query string) ([]tui.SearchResult, error) {
		results, err := retrieveUnified(brainDir, branch, query, dashSearchLimit, modeLexical)
		if err != nil {
			return nil, err
		}
		out := make([]tui.SearchResult, 0, len(results))
		for _, r := range results {
			out = append(out, searchResultFromUnified(r, brainDir))
		}
		return out, nil
	}
}

// searchResultFromUnified maps a retrieval hit to the TUI view model and decides
// its "open source" target. Only history and doc hits carry a real
// brain-relative file path; a fact's Path is its taxonomy labels (not a file),
// so a fact hit gets no open target rather than a bogus one.
func searchResultFromUnified(r unifiedResult, brainDir string) tui.SearchResult {
	sr := tui.SearchResult{
		Source:  r.Source,
		ID:      r.ID,
		Path:    r.Path,
		Heading: r.Heading,
		Line:    r.Line,
		Text:    r.Text,
		Score:   r.Score,
	}
	if r.Path != "" && (r.Source == "history" || r.Source == "doc") {
		sr.OpenPath = brainRelPath(brainDir, r.Path)
		sr.OpenLine = r.Line
	}
	return sr
}

// assembleBrainSnapshot is the only place that touches the brain on disk. It
// reuses the existing loaders to build a read-only tui.Snapshot; no agent calls.
func assembleBrainSnapshot(ctx context.Context, opts Options, target, repoDir, brainDir, branch string, limit int) (tui.Snapshot, error) {
	snap := tui.Snapshot{Repo: repoDir, Branch: branch}

	status, err := buildBrainStatusReport(ctx, opts, target)
	if err != nil {
		return snap, err
	}
	if status.Repo.Root != "" {
		snap.Repo = status.Repo.Root
	}
	snap.GeneratedAt = status.Brain.GeneratedAt
	manifest := status.Manifest
	snap.Home = dashHomeView(status, manifest, branch)

	// Facts (per branch).
	if facts, err := loadFacts(brainDir, branch); err != nil {
		snap.Home.Warnings = append(snap.Home.Warnings, "facts unavailable: "+err.Error())
	} else {
		snap.Facts = dashFactViews(facts, brainDir, limit)
		snap.Notes = appendCapNote(snap.Notes, "facts", len(facts), len(snap.Facts))
	}

	if manifest != nil && manifest.Sources != nil {
		// Sessions (from the manifest).
		if s := manifest.Sources.Sessions; s != nil {
			snap.Sessions = dashSessionViews(s.Sessions, brainDir, limit)
			snap.Notes = appendCapNote(snap.Notes, "sessions", len(s.Sessions), len(snap.Sessions))
		}
		// History (index.json).
		if h := manifest.Sources.History; h != nil {
			if idx, err := loadBrainHistoryIndex(brainDir, h); err != nil {
				snap.Home.Warnings = append(snap.Home.Warnings, "history unavailable: "+err.Error())
			} else {
				snap.History = dashHistoryViews(idx.Records, brainDir, limit)
				snap.Notes = appendCapNote(snap.Notes, "history", len(idx.Records), len(snap.History))
			}
		}
		// Semantic symbols (snapshot.ndjson).
		if sem := manifest.Sources.Semantic; sem != nil {
			if syms, err := loadSemanticSymbols(brainDir, sem, limit); err != nil {
				snap.Home.Warnings = append(snap.Home.Warnings, "semantic unavailable: "+err.Error())
			} else {
				snap.Semantic = dashSemanticViews(syms, repoDir)
				snap.Notes = appendCapNote(snap.Notes, "semantic symbols", sem.Symbols, len(snap.Semantic))
			}
		}
	}

	return snap, nil
}

// dashHomeView maps the status report into the Home tab view model.
func dashHomeView(status brainStatusReport, manifest *exportManifest, branch string) tui.HomeView {
	home := tui.HomeView{
		Freshness:  brainStatusFreshnessSeverity(status),
		Warnings:   append([]string(nil), status.Warnings...),
		BlindSpots: dashBlindSpotLabels(brainStatusBlindSpots(status)),
	}

	present := status.Sources
	detail := dashSourceDetails(manifest)
	home.Sources = []tui.SourceHealth{
		{Name: "Seed", Present: present.Seed, Detail: detail["seed"]},
		{Name: "Sessions", Present: present.Sessions, Detail: detail["sessions"]},
		{Name: "Semantic", Present: present.Semantic, Detail: detail["semantic"]},
		{Name: "History", Present: present.History, Detail: detail["history"]},
		{Name: "Facts", Present: present.Facts, Detail: detail["facts"]},
	}

	if status.Semantic != nil && status.Semantic.Freshness != nil {
		for name, axis := range status.Semantic.Freshness.Axes {
			home.Axes = append(home.Axes, tui.FreshnessAxis{Name: name, State: axis.State, Detail: axis.Detail})
		}
		sortFreshnessAxes(home.Axes)
	}

	home.Live = tui.LiveState{
		Branch:  firstNonEmpty(status.Live.Branch, branch),
		Head:    status.Live.Head,
		Dirty:   status.Live.Dirty,
		Summary: dashLiveSummary(status.Live),
	}

	if manifest != nil && manifest.GeneratedAt.IsZero() && !anySource(present) {
		home.Warnings = append(home.Warnings, "brain not built yet — run `entire brain refresh`")
	}
	return home
}

func dashSourceDetails(manifest *exportManifest) map[string]string {
	out := map[string]string{}
	if manifest == nil || manifest.Sources == nil {
		return out
	}
	src := manifest.Sources
	if s := src.Sessions; s != nil {
		out["sessions"] = fmt.Sprintf("%d sessions · %d checkpoints scanned", len(s.Sessions), s.CheckpointsScanned)
	}
	if s := src.Semantic; s != nil {
		out["semantic"] = fmt.Sprintf("%d symbols · %d relations · %d files", s.Symbols, s.Relations, s.Files)
	}
	if h := src.History; h != nil {
		out["history"] = fmt.Sprintf("%d records (%d decisions · %d learnings)", h.Records, h.Decisions, h.Learnings)
	}
	if f := src.Facts; f != nil {
		out["facts"] = fmt.Sprintf("%d facts across %d branch(es)", f.Facts, len(f.Branches))
	}
	if s := src.Seed; s != nil {
		out["seed"] = fmt.Sprintf("%d entrypoints · %d commands", len(s.Entrypoints), len(s.Commands))
	}
	return out
}

func dashBlindSpotLabels(spots []brainBlindSpot) []string {
	if len(spots) == 0 {
		return nil
	}
	out := make([]string, 0, len(spots))
	for _, s := range spots {
		label := s.Detail
		if label == "" {
			label = s.Code
		}
		if s.Path != "" {
			if label == "" {
				label = s.Path
			} else {
				label = s.Path + ": " + label
			}
		}
		if label == "" {
			continue
		}
		out = append(out, label)
	}
	return out
}

func dashLiveSummary(live brainLiveState) string {
	if !live.Dirty {
		return "clean"
	}
	parts := make([]string, 0, 3)
	if n := len(live.Staged); n > 0 {
		parts = append(parts, fmt.Sprintf("%d staged", n))
	}
	if n := len(live.Unstaged); n > 0 {
		parts = append(parts, fmt.Sprintf("%d unstaged", n))
	}
	if n := len(live.Untracked); n > 0 {
		parts = append(parts, fmt.Sprintf("%d untracked", n))
	}
	if len(parts) == 0 {
		return "dirty"
	}
	return strings.Join(parts, " · ")
}

func dashFactViews(facts []factRecord, brainDir string, limit int) []tui.FactView {
	if limit > 0 && len(facts) > limit {
		facts = facts[:limit]
	}
	out := make([]tui.FactView, 0, len(facts))
	for _, f := range facts {
		view := tui.FactView{
			ID:         f.ID,
			Kind:       f.Kind,
			Text:       f.Text,
			Paths:      f.Paths,
			Locus:      f.Locus,
			Status:     f.Status,
			Origin:     f.Origin,
			Confidence: f.Confidence,
			Related:    f.RelatedIDs,
		}
		for _, a := range f.Provenance {
			view.Provenance = append(view.Provenance, tui.Anchor{
				SessionID:  a.SessionID,
				Commit:     a.Commit,
				Transcript: a.Transcript,
				Line:       a.Line,
				Verified:   a.Verified,
			})
		}
		if len(f.Provenance) > 0 && f.Provenance[0].Transcript != "" {
			view.Source = brainRelPath(brainDir, f.Provenance[0].Transcript)
			view.SourceLine = f.Provenance[0].Line
		}
		out = append(out, view)
	}
	return out
}

func dashSessionViews(sessions []exportSession, brainDir string, limit int) []tui.SessionView {
	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	out := make([]tui.SessionView, 0, len(sessions))
	for _, s := range sessions {
		view := tui.SessionView{
			ID:          s.SessionID,
			Branch:      s.Branch,
			Agent:       s.Agent,
			Model:       s.Model,
			Kind:        s.Kind,
			Files:       s.FilesTouched,
			Checkpoints: s.CheckpointsCount,
		}
		if !s.CreatedAt.IsZero() {
			view.Created = s.CreatedAt.UTC().Format("2006-01-02 15:04Z")
		}
		if s.TokenUsage != nil {
			view.InputTok = s.TokenUsage.InputTokens
			view.OutputTok = s.TokenUsage.OutputTokens
		}
		if s.Summary != nil {
			view.Intent = s.Summary.Intent
			view.Outcome = s.Summary.Outcome
		}
		if s.TranscriptPath != "" {
			view.Source = brainRelPath(brainDir, s.TranscriptPath)
		}
		out = append(out, view)
	}
	return out
}

func dashHistoryViews(records []historyRecord, brainDir string, limit int) []tui.HistoryView {
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}
	out := make([]tui.HistoryView, 0, len(records))
	for _, r := range records {
		view := tui.HistoryView{
			ID:      r.ID,
			Kind:    r.Kind,
			Branch:  r.Branch,
			Summary: r.Summary,
			Path:    r.Path,
			Line:    r.Line,
		}
		if r.Path != "" {
			view.Source = brainRelPath(brainDir, r.Path)
			view.SourceLine = r.Line
		}
		out = append(out, view)
	}
	return out
}

func dashSemanticViews(syms []semanticRecord, repoDir string) []tui.SemanticView {
	out := make([]tui.SemanticView, 0, len(syms))
	for _, s := range syms {
		view := tui.SemanticView{
			Kind:          s.Kind,
			Name:          s.Name,
			QualifiedName: s.QualifiedName,
			FilePath:      s.FilePath,
			StartLine:     s.StartLine,
			EndLine:       s.EndLine,
			Signature:     s.Signature,
			Language:      s.Language,
		}
		if s.FilePath != "" && repoDir != "" {
			view.Source = filepath.Join(repoDir, filepath.FromSlash(s.FilePath))
			view.SourceLine = s.StartLine
		}
		out = append(out, view)
	}
	return out
}

// loadSemanticSymbols reads the symbol records from the active semantic snapshot
// (NDJSON: a header line followed by one record per line), reusing the snapshot
// scanner and path validation. A missing snapshot yields no symbols.
func loadSemanticSymbols(brainDir string, source *semanticSourceManifest, limit int) ([]semanticRecord, error) {
	if source == nil || source.SnapshotPath == "" {
		return nil, nil
	}
	rel, err := validateSemanticSnapshotPath(source.SnapshotPath)
	if err != nil {
		return nil, err
	}
	full := filepath.Join(brainDir, rel)
	// A missing snapshot is not an error: an inconsistent or not-yet-built brain
	// just yields no symbols rather than a scary warning.
	if _, statErr := os.Stat(full); os.IsNotExist(statErr) {
		return nil, nil
	}
	if err := rejectSymlinkPathComponents(brainDir, rel); err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	scanner := newSemanticScanner(f)
	first := true
	var out []semanticRecord
	for scanner.Scan() {
		if first { // skip the header line
			first = false
			continue
		}
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var rec semanticRecord
		if err := json.Unmarshal(text, &rec); err != nil {
			return nil, err
		}
		if rec.RecordType != "symbol" {
			continue
		}
		out = append(out, rec)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, scanner.Err()
}

// printDashPlain renders the snapshot as plain text for non-TTY / --plain use,
// reusing the brain summary shape.
func printDashPlain(cmd *cobra.Command, snap tui.Snapshot) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Brain dashboard — %s (branch %s)\n", orEmpty(snap.Repo), orEmpty(snap.Branch))
	if snap.GeneratedAt != "" {
		fmt.Fprintf(out, "built: %s\n", snap.GeneratedAt)
	}
	fmt.Fprintln(out, "\nSources")
	for _, s := range snap.Home.Sources {
		mark := "absent"
		if s.Present {
			mark = "present"
		}
		line := fmt.Sprintf("  %-9s %s", s.Name, mark)
		if s.Detail != "" {
			line += "  (" + s.Detail + ")"
		}
		fmt.Fprintln(out, line)
	}
	if snap.Home.Freshness != "" {
		fmt.Fprintf(out, "freshness: %s\n", snap.Home.Freshness)
	}
	fmt.Fprintf(out, "\nFacts (%d) · Sessions (%d) · History (%d) · Semantic (%d)\n",
		len(snap.Facts), len(snap.Sessions), len(snap.History), len(snap.Semantic))
	for _, bs := range snap.Home.BlindSpots {
		fmt.Fprintf(out, "  blind spot: %s\n", bs)
	}
	for _, w := range snap.Home.Warnings {
		fmt.Fprintf(out, "  warning: %s\n", w)
	}
	for _, n := range snap.Notes {
		fmt.Fprintf(out, "  note: %s\n", n)
	}
}

// resolveDashTheme picks the theme from the --theme flag, falling back to
// ENTIRE_BRAIN_THEME, then the default scheme.
func resolveDashTheme(flagValue string) tui.Theme {
	name := flagValue
	if name == "" {
		name = os.Getenv("ENTIRE_BRAIN_THEME")
	}
	theme, _ := tui.ThemeByName(name)
	return theme
}

// commandOutputIsTTY reports whether the command's stdout is a terminal.
func commandOutputIsTTY(cmd *cobra.Command) bool {
	if f, ok := cmd.OutOrStdout().(*os.File); ok {
		return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
	}
	return false
}

// --- small helpers ---------------------------------------------------------

// brainRelPath joins a brain-relative path to the brain dir, returning "" for an
// absolute, empty, or escaping path. Anchor paths come straight out of on-disk
// brain files, so a "../"-laden value must never resolve to an open target
// outside the brain (the `o` key hands the result to the OS opener).
func brainRelPath(brainDir, rel string) string {
	// Reject absolute and rooted paths. filepath.IsAbs is OS-specific (on Windows
	// a leading-slash path like "/etc/passwd" is NOT absolute), so also reject a
	// leading "/" or "\" explicitly — a rooted path is never a valid
	// brain-relative anchor on any platform.
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return ""
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ""
	}
	full := filepath.Join(brainDir, clean)
	if r, err := filepath.Rel(brainDir, full); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return ""
	}
	return full
}

func appendCapNote(notes []string, label string, total, shown int) []string {
	if total > shown {
		return append(notes, fmt.Sprintf("%s: showing %d of %d", label, shown, total))
	}
	return notes
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func anySource(s brainStatusSources) bool {
	return s.Seed || s.Sessions || s.Semantic || s.History || s.Facts
}

func orEmpty(v string) string {
	if v == "" {
		return "(unknown)"
	}
	return v
}

func sortFreshnessAxes(axes []tui.FreshnessAxis) {
	for i := 1; i < len(axes); i++ {
		for j := i; j > 0 && axes[j-1].Name > axes[j].Name; j-- {
			axes[j-1], axes[j] = axes[j], axes[j-1]
		}
	}
}
