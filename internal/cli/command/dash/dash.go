package dash

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/entire-brain/factmerge"
	"github.com/entireio/entire-brain/internal/cli/internal/brainstore"
	"github.com/entireio/entire-brain/internal/cli/internal/cmdutil"
	"github.com/entireio/entire-brain/internal/cli/internal/factstore"
	"github.com/entireio/entire-brain/internal/cli/internal/historystore"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/cli/internal/retrieval"
	"github.com/entireio/entire-brain/internal/cli/internal/semstore"
	"github.com/entireio/entire-brain/internal/tui"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

// dashDefaultLimit caps how many entries per tab the dashboard loads. Large
// brains can hold thousands of facts/symbols; the cap keeps the snapshot bounded
// and a Note records what was dropped so the truncation is never silent. A
// non-positive --limit means no cap.
const dashDefaultLimit = 500

// dashSearchLimit caps results from an in-dashboard search (the `s` key),
// matching the `entire brain search` default surface.
const dashSearchLimit = 25

// Flags are the dash command's flag values. Fields are exported so the
// cli-staying transport tests can drive Run directly.
type Flags struct {
	Theme  string
	Tab    string
	Branch string
	Limit  int
	JSON   bool
	Plain  bool
}

// New builds the `dash` command.
func New(cfg Config) *cobra.Command {
	var flags Flags
	cmd := &cobra.Command{
		Use:   "dash [path]",
		Short: "Browse the brain in an interactive dashboard",
		Long: `dash opens an interactive terminal dashboard over the brain: a Home tab
summarizing source health, freshness, and blind spots, plus explorer tabs that
browse individual facts, sessions, history records, and code symbols, each
with a detail page. It is read-only and makes no agent calls.

On a non-TTY (piped output) it prints a plain summary instead; --json emits the
loaded snapshot.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return Run(cmd.Context(), cmd, cfg, flags, cfg.Deps.AgentSurfaceTarget(cfg.Options, args))
		},
	}
	cmd.Flags().StringVar(&flags.Theme, "theme", "", "Color theme: "+strings.Join(tui.ThemeNames(), ", ")+" (or set ENTIRE_BRAIN_THEME)")
	cmd.Flags().StringVar(&flags.Tab, "tab", "", "Tab to open on: home, facts, sessions, history, semantic, search")
	cmd.Flags().StringVar(&flags.Branch, "branch", "", "Branch whose memories to load (default: current branch)")
	cmd.Flags().IntVar(&flags.Limit, "limit", dashDefaultLimit, "Max entries loaded per tab (0 = no cap)")
	cmd.Flags().BoolVar(&flags.JSON, "json", false, "Emit the loaded snapshot as JSON instead of opening the dashboard")
	cmd.Flags().BoolVar(&flags.Plain, "plain", false, "Print a plain text summary instead of opening the dashboard")
	return cmd
}

// Run executes the dash command. It is exported so the cli transport tests
// can exercise the --json privacy-emission path directly.
func Run(ctx context.Context, cmd *cobra.Command, cfg Config, flags Flags, target string) error {
	startTab, ok := tui.ParseTab(flags.Tab)
	if !ok && flags.Tab != "" {
		return fmt.Errorf("unknown_tab: %q (want home, facts, sessions, history, semantic, or search)", flags.Tab)
	}

	// Validate the theme up front (like the tab) so a typo fails fast with the
	// valid names, in any output mode, rather than silently falling back.
	theme, err := resolveDashTheme(flags.Theme)
	if err != nil {
		return err
	}

	repoDir, brainDir, branch, err := cfg.Deps.ResolveFactsTarget(ctx, cfg.Options, target, flags.Branch)
	if err != nil {
		return err
	}
	privacyPolicy, err := brainstore.CaptureRetrievalPrivacyPolicy(brainDir)
	if err != nil {
		return err
	}

	snap, err := AssembleBrainSnapshot(ctx, cfg, target, repoDir, brainDir, branch, flags.Limit)
	if err != nil {
		return err
	}

	// A dumb or unset TERM is a terminal that cannot render escape sequences at
	// all, so the dashboard must not be started on one: it would arrive as
	// literal \x1b[?1049h, rounded box-drawing and 256-colour SGR bytes. The
	// plain summary is the same content in a form that terminal can show, and
	// the note goes to stderr so stdout stays byte-identical to --plain.
	dumbTerminal := commandOutputIsTTY(cmd) && !tui.SupportsFullScreen(os.Getenv)
	if flags.JSON || flags.Plain || !commandOutputIsTTY(cmd) || dumbTerminal {
		if dumbTerminal && !flags.JSON && !flags.Plain {
			fmt.Fprintln(cmd.ErrOrStderr(), "note: TERM is unset or dumb — printing the plain summary; the dashboard needs a terminal that renders escape sequences")
		}
		return cfg.Deps.BufferRetrievalOutput(cmd, []brainstore.RetrievalPrivacyPolicy{privacyPolicy}, func() error {
			if flags.JSON {
				return cmdutil.WriteJSON(cmd, snap)
			}
			printDashPlain(cmd, snap)
			return nil
		})
	}
	return tui.Run(snap, theme, brainSearchFunc(cfg.Deps, repoDir, brainDir, branch), startTab,
		cfg.Deps.TerminalWriter(cmd.OutOrStdout(), privacyPolicy), cfg.Deps.BrainSourceReader(ctx, cfg.Options.Runner, brainDir))
}

// brainSearchFunc adapts the lexical retrieval (`entire brain search`) into the
// callback the dashboard's `s` key invokes, so search runs the same code path as
// the CLI verb while keeping the TUI free of file IO.
func brainSearchFunc(deps Deps, repoDir, brainDir, branch string) tui.SearchFunc {
	return func(query string) ([]tui.SearchResult, error) {
		results, err := deps.RetrieveUnified(repoDir, brainDir, branch, query, dashSearchLimit, retrieval.ModeLexical)
		if err != nil {
			return nil, err
		}
		out := make([]tui.SearchResult, 0, len(results))
		for _, r := range results {
			out = append(out, SearchResultFromUnified(r, brainDir))
		}
		return out, nil
	}
}

// SearchResultFromUnified maps a retrieval hit to the TUI view model and decides
// its "open source" target. Only history and doc hits carry a real
// brain-relative file path; a fact's Path is its taxonomy labels (not a file),
// so a fact hit gets no open target rather than a bogus one.
func SearchResultFromUnified(r retrieval.Result, brainDir string) tui.SearchResult {
	text := r.Text
	if r.VerificationRequired {
		text = "VERIFY AGAINST CURRENT EVIDENCE: " + text
	}
	sr := tui.SearchResult{
		Source:  r.Source,
		ID:      r.ID,
		Path:    r.Path,
		Heading: r.Heading,
		Line:    r.Line,
		Text:    text,
		Score:   r.Score,
	}
	if r.Path != "" && (r.Source == "history" || r.Source == "doc") {
		sr.OpenPath = containedPath(brainDir, r.Path)
		sr.OpenLine = r.Line
	}
	return sr
}

// AssembleBrainSnapshot is the only place that touches the brain on disk. It
// reuses the existing loaders to build a read-only tui.Snapshot; no agent calls.
func AssembleBrainSnapshot(ctx context.Context, cfg Config, target, repoDir, brainDir, branch string, limit int) (tui.Snapshot, error) {
	snap := tui.Snapshot{Repo: repoDir, Branch: branch}

	status, err := cfg.Deps.StatusHome(ctx, cfg.Options, target, branch, brainDir)
	if err != nil {
		return snap, err
	}
	if status.RepoRoot != "" {
		snap.Repo = status.RepoRoot
	}
	snap.GeneratedAt = status.GeneratedAt
	manifest := status.Manifest
	guard := status.Guard
	snap.Home = status.Home

	// Facts (per branch).
	if facts, err := factstore.Load(brainDir, branch); err != nil {
		markSourceUnreadable(&snap.Home, "Facts", "facts unavailable: "+err.Error())
	} else {
		facts = factstore.GuardRecords(guard, facts)
		snap.Facts = FactViews(facts, brainDir, limit)
		snap.Notes = appendCapNote(snap.Notes, "facts", len(facts), len(snap.Facts))
	}

	if manifest != nil && manifest.Sources != nil {
		// Sessions (from the manifest).
		if s := manifest.Sources.Sessions; s != nil {
			visibleSessions := bmanifest.GuardSessions(guard, s.Sessions)
			snap.Sessions = SessionViews(visibleSessions, brainDir, limit, guard)
			snap.Notes = appendCapNote(snap.Notes, "sessions", len(visibleSessions), len(snap.Sessions))
		}
		// History (index.json).
		if h := manifest.Sources.History; h != nil {
			if idx, err := historystore.LoadIndex(brainDir, h); err != nil {
				markSourceUnreadable(&snap.Home, "History", "history unavailable: "+err.Error())
			} else {
				records := historystore.FilterRecords(idx.Records, func(record historystore.Record) bool { return !historystore.GuardBlocksRecord(guard, record) })
				snap.History = HistoryViews(records, brainDir, limit)
				snap.Notes = appendCapNote(snap.Notes, "history", len(records), len(snap.History))
			}
		}
		// Semantic symbols (snapshot.ndjson).
		if sem := manifest.Sources.Semantic; sem != nil {
			if syms, err := semstore.LoadSymbols(brainDir, sem, limit); err != nil {
				markSourceUnreadable(&snap.Home, "Code", "code index unavailable: "+err.Error())
			} else {
				snap.Semantic = dashSemanticViews(syms, repoDir)
				snap.Notes = appendCapNote(snap.Notes, "code symbols", sem.Symbols, len(snap.Semantic))
			}
		}
	}

	return snap, nil
}

// markSourceUnreadable records that a source the manifest declares could not
// actually be read: the Sources row flips from "present" to "unreadable", its
// detail leads with the failure, and the warning is still raised.
//
// The Sources list is built from the manifest, which only says what the brain
// CLAIMS to hold. A corrupt semantic snapshot therefore rendered as "Semantic
// present (8 symbols · 18 relations · 2 files)" — manifest counts quoted with
// full confidence — directly above a warning that the same file could not be
// parsed, and above a tab bar reading "Semantic (0)". Three lines, two of them
// true. The row has to carry the bad news itself, because the row is what a
// reader scans.
func markSourceUnreadable(home *tui.HomeView, name, warning string) {
	home.Warnings = append(home.Warnings, warning)
	for i := range home.Sources {
		if home.Sources[i].Name != name {
			continue
		}
		home.Sources[i].Unreadable = true
		if claim := home.Sources[i].Detail; claim != "" {
			home.Sources[i].Detail = warning + " (manifest claims " + claim + ")"
		} else {
			home.Sources[i].Detail = warning
		}
		return
	}
}

func FactViews(facts []factmerge.Record, brainDir string, limit int) []tui.FactView {
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
			view.Source = containedPath(brainDir, f.Provenance[0].Transcript)
			view.SourceLine = f.Provenance[0].Line
		}
		out = append(out, view)
	}
	return out
}

func SessionViews(sessions []bmanifest.Session, brainDir string, limit int, guard brainstore.SessionReadGuard) []tui.SessionView {
	sessions = bmanifest.GuardSessions(guard, sessions)
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
			view.Source = containedPath(brainDir, s.TranscriptPath)
		}
		out = append(out, view)
	}
	return out
}

func HistoryViews(records []historystore.Record, brainDir string, limit int) []tui.HistoryView {
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
			view.Source = containedPath(brainDir, r.Path)
			view.SourceLine = r.Line
		}
		out = append(out, view)
	}
	return out
}

func dashSemanticViews(syms []semstore.Record, repoDir string) []tui.SemanticView {
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
		// Contain the snapshot-provided path: a tampered snapshot must not make
		// `o` open a file outside the repo (absolute / traversal paths rejected).
		if repoDir != "" {
			if src := containedPath(repoDir, s.FilePath); src != "" {
				view.Source = src
				view.SourceLine = s.StartLine
			}
		}
		out = append(out, view)
	}
	return out
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
		line := fmt.Sprintf("  %-9s %s", s.Name, s.State())
		if s.Detail != "" {
			line += "  (" + s.Detail + ")"
		}
		fmt.Fprintln(out, line)
	}
	if snap.Home.Freshness != "" {
		fmt.Fprintf(out, "freshness: %s\n", snap.Home.Freshness)
	}
	fmt.Fprintf(out, "\nFacts (%d) · Sessions (%d) · History (%d) · Code (%d)\n",
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
// ENTIRE_BRAIN_THEME, then the default scheme. A non-empty name that matches no
// known theme is an error (so a typo is surfaced, not silently ignored); an
// empty name resolves to the default without error.
func resolveDashTheme(flagValue string) (tui.Theme, error) {
	name := flagValue
	if name == "" {
		name = os.Getenv("ENTIRE_BRAIN_THEME")
	}
	theme, ok := tui.ThemeByName(name)
	if !ok && name != "" {
		return theme, fmt.Errorf("unknown_theme: %q (want %s)", name, strings.Join(tui.ThemeNames(), ", "))
	}
	return theme, nil
}

// commandOutputIsTTY reports whether the command's stdout is a terminal.
func commandOutputIsTTY(cmd *cobra.Command) bool {
	if f, ok := cmd.OutOrStdout().(*os.File); ok {
		return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
	}
	return false
}

// --- small helpers ---------------------------------------------------------

// containedPath joins a relative path to a base dir, returning "" for an empty,
// absolute, rooted, or escaping path. Path values come straight out of on-disk
// brain/snapshot files, so a "../"-laden or rooted value must never resolve to an
// open target outside the base (the `o` key hands the result to the OS opener).
func containedPath(base, rel string) string {
	// Reject absolute and rooted paths. filepath.IsAbs is OS-specific (on Windows
	// a leading-slash path like "/etc/passwd" is NOT absolute), so also reject a
	// leading "/" or "\" explicitly — a rooted path is never a valid relative
	// anchor on any platform.
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return ""
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ""
	}
	full := filepath.Join(base, clean)
	if r, err := filepath.Rel(base, full); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
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

func orEmpty(v string) string {
	if v == "" {
		return "(unknown)"
	}
	return v
}
